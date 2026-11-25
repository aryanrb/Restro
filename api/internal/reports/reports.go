// Package reports serves the owner dashboard, daily sales, the IRD-style sales
// book (BS dates), the cancelled-invoice report and the audit-log browser.
// Owner/manager only. CSV export is built in; Excel/PDF are Phase 2.
package reports

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/shift"
)

var nepal = time.FixedZone("NPT", 5*3600+45*60)

type Service struct {
	q   *sqlc.Queries
	log *zap.Logger
}

func NewService(pool *pgxpool.Pool, log *zap.Logger) *Service {
	return &Service{q: sqlc.New(pool), log: log}
}

func startOfTodayNPT() time.Time {
	n := time.Now().In(nepal)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, nepal)
}

func (s *Service) Dashboard(ctx context.Context) (*restaurantv1.DashboardResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	since := startOfTodayNPT()
	sales, err := s.q.TodaySalesTotal(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	openOrders, _ := s.q.CountOpenOrders(ctx)
	openTables, _ := s.q.CountOpenTables(ctx)
	discounts, _ := s.q.CountTodayDiscounts(ctx, since)
	top, err := s.q.TopItemsToday(ctx, sqlc.TopItemsTodayParams{AdDatetime: since, Limit: 5})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	byMethod, err := s.q.TodaySalesByMethod(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	discountTotal, err := s.q.TodayDiscountTotal(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	voids, err := s.q.TodayVoids(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	cancelled, err := s.q.TodayCancelled(ctx, pgtype.Timestamptz{Time: since, Valid: true})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.DashboardResponse{
		TodaySalesPaisa: sales.Total, TodayInvoiceCount: sales.InvoiceCount,
		OpenOrders: openOrders, OpenTables: openTables, VoidCount: voids.VoidCount, DiscountCount: discounts,
		DiscountPaisa: discountTotal, VoidValuePaisa: voids.VoidValue,
		CancelledCount: cancelled.CancelledCount, CancelledValuePaisa: cancelled.CancelledValue,
	}
	for _, t := range top {
		out.TopItems = append(out.TopItems, &restaurantv1.TopItem{Name: t.Name, Quantity: t.Quantity, AmountPaisa: t.AmountPaisa})
	}
	for _, m := range byMethod {
		out.ByMethod = append(out.ByMethod, &restaurantv1.MethodTotal{Method: paymentMethodEnum(m.Method), TotalPaisa: m.Total})
	}
	return out, nil
}

// SalesBook lists invoices in the range: tax invoices only unless
// include_estimates is set.
func (s *Service) SalesBook(ctx context.Context, req *restaurantv1.RangeRequest) (*restaurantv1.SalesBookResponse, error) {
	rows, err := s.salesBookRows(ctx, req)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.SalesBookResponse{}
	for _, r := range rows {
		out.Rows = append(out.Rows, toSalesBookRow(r))
	}
	return out, nil
}

func (s *Service) ExportSalesBookCsv(ctx context.Context, req *restaurantv1.RangeRequest) (*restaurantv1.CsvExport, error) {
	rows, err := s.salesBookRows(ctx, req)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Fiscal Year", "Bill No", "Mode", "BS Date", "Buyer", "PAN", "Subtotal", "Discount", "Taxable", "VAT", "Total", "Status", "Entered By"})
	for _, r := range rows {
		_ = w.Write([]string{
			r.FiscalYear, shift.DisplayNumber(r.Mode, r.FiscalYear, r.Number), r.Mode, r.BsDate, r.BuyerName, r.BuyerPan,
			rupees(r.SubtotalPaisa), rupees(r.DiscountPaisa), rupees(r.TaxablePaisa), rupees(r.VatPaisa), rupees(r.TotalPaisa), r.Status,
			r.EnteredByName,
		})
	}
	w.Flush()
	return &restaurantv1.CsvExport{Data: buf.Bytes(), Filename: "sales-book.csv"}, nil
}

func (s *Service) salesBookRows(ctx context.Context, req *restaurantv1.RangeRequest) ([]sqlc.SalesBookDetailRow, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.SalesBookDetail(ctx, sqlc.SalesBookDetailParams{
		FromTs: tsOf(req.GetFromUnix()), ToTs: tsOf(req.GetToUnix()), IncludeEstimates: req.GetIncludeEstimates(),
	})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return rows, nil
}

// DailySales is one row per Kathmandu day: issued-bill totals, discounts,
// service charge, takings by method, and how many of the day's bills were cancelled.
func (s *Service) DailySales(ctx context.Context, fromUnix, toUnix int64) (*restaurantv1.DailySalesResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.DailySalesDetail(ctx, sqlc.DailySalesDetailParams{FromTs: tsOf(fromUnix), ToTs: tsOf(toUnix)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	methods, err := s.q.DailySalesByMethod(ctx, sqlc.DailySalesByMethodParams{FromTs: tsOf(fromUnix), ToTs: tsOf(toUnix)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	byDay := map[string][]*restaurantv1.MethodTotal{}
	for _, m := range methods {
		byDay[m.AdDate] = append(byDay[m.AdDate], &restaurantv1.MethodTotal{Method: paymentMethodEnum(m.Method), TotalPaisa: m.Total})
	}
	out := &restaurantv1.DailySalesResponse{}
	for _, r := range rows {
		out.Rows = append(out.Rows, &restaurantv1.DailySalesRow{
			BsDate: r.BsDate, AdDate: r.AdDate, InvoiceCount: r.InvoiceCount, TotalPaisa: r.TotalPaisa, VatPaisa: r.VatPaisa,
			TaxablePaisa: r.TaxablePaisa, DiscountPaisa: r.DiscountPaisa, ServiceChargePaisa: r.ServiceChargePaisa,
			ByMethod: byDay[r.AdDate], CancelledCount: r.CancelledCount,
		})
	}
	return out, nil
}

func (s *Service) CancelledInvoices(ctx context.Context, fromUnix, toUnix int64) (*restaurantv1.CancelledInvoicesResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.CancelledInvoicesDetail(ctx, sqlc.CancelledInvoicesDetailParams{FromTs: tsPg(fromUnix), ToTs: tsPg(toUnix)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.CancelledInvoicesResponse{}
	for _, r := range rows {
		row := &restaurantv1.CancelledInvoiceRow{
			Number: r.Number, FiscalYear: r.FiscalYear, BsDate: r.BsDate, TotalPaisa: r.TotalPaisa,
			Reason: r.CancelReason, CancelledByUserId: pgUUID(r.CancelledByUserID),
			DisplayNumber: shift.DisplayNumber(r.Mode, r.FiscalYear, r.Number), InvoiceId: r.ID.String(),
			IssuedByUserId: pgUUID(r.CreatedByUserID), IssuedByName: r.IssuedByName, CancelledByName: r.CancelledByName,
			ApprovedByUserId: pgUUID(r.CancelApprovedByUserID), ApprovedByName: r.ApprovedByName,
		}
		if r.CancelledAt.Valid {
			row.CancelledAtUnix = r.CancelledAt.Time.Unix()
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

func (s *Service) AuditLog(ctx context.Context, req *restaurantv1.AuditLogRequest) (*restaurantv1.AuditLogResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	limit := req.GetLimit()
	if limit <= 0 {
		limit = 200
	}
	var actor pgtype.UUID
	if req.GetActorId() != "" {
		id, err := uuid.Parse(req.GetActorId())
		if err != nil {
			return nil, connecterr.Invalid("actor_id", "invalid id")
		}
		actor = pgtype.UUID{Bytes: id, Valid: true}
	}
	rows, err := s.q.QueryAuditLogDetail(ctx, sqlc.QueryAuditLogDetailParams{
		FromTs: tsOf(req.GetFromUnix()), ToTs: tsOf(req.GetToUnix()), Action: req.GetAction(), ActorID: actor, RowLimit: limit,
	})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.AuditLogResponse{}
	for _, r := range rows {
		out.Entries = append(out.Entries, &restaurantv1.AuditEntry{
			Id: r.ID, ActorUserId: pgUUID(r.ActorUserID), Action: r.Action, Entity: r.Entity,
			EntityId: r.EntityID.String, Reason: r.Reason.String, ApproverUserId: pgUUID(r.ApproverUserID), CreatedAtUnix: r.CreatedAt.Unix(),
			ActorName: r.ActorName, ApproverName: r.ApproverName, Subject: r.Subject, DeviceLabel: r.DeviceLabel.String,
			Detail: auditDetail(r.Reason.String, r.After),
		})
	}
	return out, nil
}

// auditDetail renders an audit row's reason and the well-known fields of its
// "after" JSON as one readable line, e.g. "CASH · Rs 150.50 · wrong table".
func auditDetail(reason string, after []byte) string {
	var parts []string
	var m map[string]any
	if len(after) > 0 && json.Unmarshal(after, &m) == nil {
		if v, ok := m["method"].(string); ok && v != "" {
			parts = append(parts, v)
		}
		for _, k := range []struct{ key, label string }{
			{"amount", ""}, {"total", "total "}, {"counted_cash", "counted "},
		} {
			if v, ok := m[k.key].(float64); ok {
				parts = append(parts, k.label+"Rs "+rupees(int64(v)))
			}
		}
		if v, ok := m["number"].(float64); ok {
			parts = append(parts, fmt.Sprintf("no. %d", int64(v)))
		}
		if v, ok := m["print_count"].(float64); ok {
			parts = append(parts, fmt.Sprintf("print #%d", int64(v)))
		}
	}
	if reason != "" {
		parts = append(parts, reason)
	}
	return strings.Join(parts, " · ")
}

// --- helpers ---

func tsOf(unix int64) time.Time { return time.Unix(unix, 0).UTC() }

func tsPg(unix int64) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Unix(unix, 0).UTC(), Valid: true}
}

func rupees(paisa int64) string {
	sign := ""
	if paisa < 0 {
		sign, paisa = "-", -paisa
	}
	return fmt.Sprintf("%s%d.%02d", sign, paisa/100, paisa%100)
}

func pgUUID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	var b [16]byte = u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func toSalesBookRow(r sqlc.SalesBookDetailRow) *restaurantv1.SalesBookRow {
	return &restaurantv1.SalesBookRow{
		FiscalYear: r.FiscalYear, Number: r.Number, Mode: r.Mode, BsDate: r.BsDate,
		BuyerName: r.BuyerName, BuyerPan: r.BuyerPan, SubtotalPaisa: r.SubtotalPaisa, DiscountPaisa: r.DiscountPaisa,
		TaxablePaisa: r.TaxablePaisa, VatPaisa: r.VatPaisa, TotalPaisa: r.TotalPaisa, Status: r.Status,
		DisplayNumber: shift.DisplayNumber(r.Mode, r.FiscalYear, r.Number), InvoiceId: r.ID.String(),
		EnteredByName: r.EnteredByName, PrintCount: r.PrintCount, AdDatetimeUnix: r.AdDatetime.Unix(),
	}
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
