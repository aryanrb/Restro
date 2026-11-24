// Package billing issues immutable, gap-free-numbered invoices with BS dates,
// records payments against an open shift, cancels invoices (reopening the order),
// and tracks prints. All money maths is in internal/money. CBMS rows are written
// DISABLED (the sender is Phase 2).
package billing

import (
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
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

func toInvoice(inv sqlc.Invoice, lines []sqlc.InvoiceLine, payments []sqlc.Payment) *restaurantv1.Invoice {
	out := &restaurantv1.Invoice{
		Id: inv.ID.String(), FiscalYear: inv.FiscalYear, Number: inv.Number, Mode: billingModeEnum(inv.Mode),
		BsDate: inv.BsDate, AdDatetimeUnix: inv.AdDatetime.Unix(), BuyerName: inv.BuyerName, BuyerPan: inv.BuyerPan,
		SubtotalPaisa: inv.SubtotalPaisa, DiscountPaisa: inv.DiscountPaisa, ServiceChargePaisa: inv.ServiceChargePaisa,
		TaxablePaisa: inv.TaxablePaisa, VatPaisa: inv.VatPaisa, RoundingPaisa: inv.RoundingPaisa, TotalPaisa: inv.TotalPaisa,
		Status: invoiceStatusEnum(inv.Status), PrintCount: inv.PrintCount,
	}
	if inv.OrderID.Valid {
		out.OrderId = uuid.UUID(inv.OrderID.Bytes).String()
	}
	for _, l := range lines {
		out.Lines = append(out.Lines, toLine(l))
	}
	for _, p := range payments {
		out.Payments = append(out.Payments, toPayment(p))
	}
	return out
}

func toLine(l sqlc.InvoiceLine) *restaurantv1.InvoiceLine {
	out := &restaurantv1.InvoiceLine{
		Name: local(l.NameEn, l.NameNe), VariantName: l.VariantName, Quantity: l.Quantity,
		UnitPricePaisa: l.UnitPricePaisa, LineTotalPaisa: l.LineTotalPaisa,
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
	default:
		return ""
	}
}
