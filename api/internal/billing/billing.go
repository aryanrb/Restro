// Package billing issues immutable, gap-free-numbered invoices with BS dates,
// records payments against an open shift, cancels invoices (reopening the order),
// and tracks prints. All money maths is in internal/money. CBMS rows are written
// DISABLED (the sender is Phase 2).
package billing

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/loyalty"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// cashierMaxDiscountBp is the discount a cashier may apply without a manager
// override (10%). Owners and managers are unlimited; waiters cannot discount.
const cashierMaxDiscountBp = 1000

type Service struct {
	pool      *pgxpool.Pool
	q         *sqlc.Queries
	hub       *realtime.Hub
	auth      *auth.Service
	providers map[string]PaymentProvider
	log       *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, authSvc *auth.Service, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, auth: authSvc, providers: defaultProviders(), log: log}
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

// lineMod is the shape stored in invoice_lines.modifiers_json.
type lineMod struct {
	NameEn     string `json:"name_en"`
	NameNe     string `json:"name_ne"`
	PriceDelta int64  `json:"price_delta_paisa"`
}

// --- converters ---

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

// displayNumber is the printed invoice number: "E-2083/84-12" for an estimate,
// "2083/84-12" for a tax invoice. fy is the stored fiscal year ("2083.084").
func displayNumber(mode, fy string, n int32) string {
	prefix := ""
	if mode == "ESTIMATE" {
		prefix = "E-"
	}
	return fmt.Sprintf("%s%s-%d", prefix, displayFiscalYear(fy), n)
}

// displayFiscalYear turns the stored "2083.084" into the printed "2083/84";
// anything else passes through unchanged.
func displayFiscalYear(fy string) string {
	start, end, ok := strings.Cut(fy, ".")
	if !ok || len(end) < 2 {
		return fy
	}
	return start + "/" + end[len(end)-2:]
}

func uuidStr(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

func toInvoice(inv sqlc.Invoice, lines []sqlc.InvoiceLine, payments []sqlc.Payment, prints []sqlc.ListInvoicePrintsRow) *restaurantv1.Invoice {
	out := &restaurantv1.Invoice{
		Id: inv.ID.String(), FiscalYear: inv.FiscalYear, Number: inv.Number, Mode: billingModeEnum(inv.Mode),
		BsDate: inv.BsDate, AdDatetimeUnix: inv.AdDatetime.Unix(), BuyerName: inv.BuyerName, BuyerPan: inv.BuyerPan,
		SubtotalPaisa: inv.SubtotalPaisa, DiscountPaisa: inv.DiscountPaisa, ServiceChargePaisa: inv.ServiceChargePaisa,
		TaxablePaisa: inv.TaxablePaisa, VatPaisa: inv.VatPaisa, RoundingPaisa: inv.RoundingPaisa, TotalPaisa: inv.TotalPaisa,
		Status: invoiceStatusEnum(inv.Status), PrintCount: inv.PrintCount,
		OrderId: uuidStr(inv.OrderID), OrderClientId: uuidStr(inv.OrderClientID),
		Seller: &restaurantv1.InvoiceSeller{
			Name: local(inv.SellerNameEn, inv.SellerNameNe), Address: inv.SellerAddress, PanNumber: inv.SellerPanNumber,
			VatRateBp: inv.SellerVatRateBp, LogoUrl: inv.SellerLogoUrl, VatRegistered: inv.SellerVatRegistered,
		},
		DisplayNumber:  inv.DisplayNumber,
		DiscountReason: inv.DiscountReason,
		IssuedByUserId: uuidStr(inv.CreatedByUserID), IssuedByName: inv.IssuedByName,
		Place:        inv.Place,
		CancelReason: inv.CancelReason, CancelledByUserId: uuidStr(inv.CancelledByUserID),
		CancelApprovedByUserId: uuidStr(inv.CancelApprovedByUserID),
		RewardPaisa:            inv.RewardPaisa, CustomerId: uuidStr(inv.LoyaltyCustomerID),
		LoyaltySnapshot: loyalty.SnapshotProto(inv.LoyaltySnapshot),
	}
	if out.DisplayNumber == "" {
		out.DisplayNumber = displayNumber(inv.Mode, inv.FiscalYear, inv.Number)
	}
	if inv.CancelledAt.Valid {
		out.CancelledAtUnix = inv.CancelledAt.Time.Unix()
	}
	for _, l := range lines {
		out.Lines = append(out.Lines, toLine(l))
	}
	for _, p := range payments {
		out.Payments = append(out.Payments, toPayment(p))
	}
	for _, p := range prints {
		out.Prints = append(out.Prints, &restaurantv1.InvoicePrint{AtUnix: p.PrintedAt.Unix(), ByUserId: p.PrintedByUserID.String(), ByName: p.PrintedByName})
	}
	return out
}

func toLine(l sqlc.InvoiceLine) *restaurantv1.InvoiceLine {
	out := &restaurantv1.InvoiceLine{
		Name: local(l.NameEn, l.NameNe), VariantName: l.VariantName, Quantity: l.Quantity,
		UnitPricePaisa: l.UnitPricePaisa, LineTotalPaisa: l.LineTotalPaisa,
		OrderItemId: uuidStr(l.OrderItemID), OrderItemClientId: uuidStr(l.OrderItemClientID),
	}
	if l.VariantName != "" || l.VariantNameNe != "" {
		out.Variant = local(l.VariantName, l.VariantNameNe)
	}
	var mods []lineMod
	if len(l.ModifiersJson) > 0 {
		_ = json.Unmarshal(l.ModifiersJson, &mods)
	}
	for _, m := range mods {
		out.Modifiers = append(out.Modifiers, local(m.NameEn, m.NameNe))
	}
	return out
}

func toPayment(p sqlc.Payment) *restaurantv1.Payment {
	return &restaurantv1.Payment{
		Id: p.ID.String(), Method: paymentMethodEnum(p.Method), AmountPaisa: p.AmountPaisa,
		Reference: p.Reference, ConfirmedByUserId: p.ConfirmedByUserID.String(), CreatedAtUnix: p.CreatedAt.Unix(),
		ShiftId:    p.ShiftID.String(),
		CustomerId: uuidStr(p.CustomerID), CustomerName: p.CustomerName, BalanceAfterPaisa: p.BalanceAfterPaisa,
	}
}

// --- enums ---

func billingModeEnum(s string) restaurantv1.BillingMode {
	if s == "TAX_INVOICE" {
		return restaurantv1.BillingMode_BILLING_MODE_TAX_INVOICE
	}
	return restaurantv1.BillingMode_BILLING_MODE_ESTIMATE
}

func invoiceStatusEnum(s string) restaurantv1.InvoiceStatus {
	if s == "CANCELLED" {
		return restaurantv1.InvoiceStatus_INVOICE_STATUS_CANCELLED
	}
	return restaurantv1.InvoiceStatus_INVOICE_STATUS_ISSUED
}

func paymentMethodEnum(s string) restaurantv1.PaymentMethod {
	switch s {
	case "CASH":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH
	case "CARD":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD
	case "QR_FONEPAY":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY
	case "QR_OTHER":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_OTHER
	case "ON_ACCOUNT":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_ON_ACCOUNT
	default:
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED
	}
}

func paymentMethodStr(m restaurantv1.PaymentMethod) string {
	switch m {
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH:
		return "CASH"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD:
		return "CARD"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY:
		return "QR_FONEPAY"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_OTHER:
		return "QR_OTHER"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_ON_ACCOUNT:
		return "ON_ACCOUNT"
	default:
		return ""
	}
}
