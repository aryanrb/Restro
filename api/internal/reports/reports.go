// Package reports serves the owner dashboard, daily sales, the IRD-style sales
// book (BS dates), the cancelled-invoice report and the audit-log browser.
// Owner/manager only. CSV export is built in; Excel/PDF are Phase 2.
package reports

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
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
	voids, _ := s.q.CountTodayVoids(ctx, since)
	discounts, _ := s.q.CountTodayDiscounts(ctx, since)
	top, err := s.q.TopItemsToday(ctx, sqlc.TopItemsTodayParams{AdDatetime: since, Limit: 5})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.DashboardResponse{
		TodaySalesPaisa: sales.Total, TodayInvoiceCount: sales.InvoiceCount,
		OpenOrders: openOrders, OpenTables: openTables, VoidCount: voids, DiscountCount: discounts,
	}
	for _, t := range top {
		out.TopItems = append(out.TopItems, &restaurantv1.TopItem{Name: t.Name, Quantity: t.Quantity, AmountPaisa: t.AmountPaisa})
	}
	return out, nil
}

func (s *Service) SalesBook(ctx context.Context, fromUnix, toUnix int64) (*restaurantv1.SalesBookResponse, error) {
	rows, err := s.salesBookRows(ctx, fromUnix, toUnix)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.SalesBookResponse{}
	for _, r := range rows {
		out.Rows = append(out.Rows, toSalesBookRow(r))
	}
	return out, nil
}

func (s *Service) ExportSalesBookCsv(ctx context.Context, fromUnix, toUnix int64) (*restaurantv1.CsvExport, error) {
	rows, err := s.salesBookRows(ctx, fromUnix, toUnix)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Fiscal Year", "Bill No", "Mode", "BS Date", "Buyer", "PAN", "Subtotal", "Discount", "Taxable", "VAT", "Total", "Status"})
	for _, r := range rows {
		_ = w.Write([]string{
			r.FiscalYear, strconv.Itoa(int(r.Number)), r.Mode, r.BsDate, r.BuyerName, r.BuyerPan,
			rupees(r.SubtotalPaisa), rupees(r.DiscountPaisa), rupees(r.TaxablePaisa), rupees(r.VatPaisa), rupees(r.TotalPaisa), r.Status,
		})
	}
	w.Flush()
	return &restaurantv1.CsvExport{Data: buf.Bytes(), Filename: "sales-book.csv"}, nil
}

func (s *Service) salesBookRows(ctx context.Context, fromUnix, toUnix int64) ([]sqlc.Invoice, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.SalesBook(ctx, sqlc.SalesBookParams{AdDatetime: tsOf(fromUnix), AdDatetime_2: tsOf(toUnix)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return rows, nil
}

func (s *Service) DailySales(ctx context.Context, fromUnix, toUnix int64) (*restaurantv1.DailySalesResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.DailySales(ctx, sqlc.DailySalesParams{AdDatetime: tsOf(fromUnix), AdDatetime_2: tsOf(toUnix)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.DailySalesResponse{}
	for _, r := range rows {
		out.Rows = append(out.Rows, &restaurantv1.DailySalesRow{
			BsDate: r.BsDate, InvoiceCount: r.InvoiceCount, TotalPaisa: r.TotalPaisa, VatPaisa: r.VatPaisa, TaxablePaisa: r.TaxablePaisa,
		})
	}
	return out, nil
}

func (s *Service) CancelledInvoices(ctx context.Context, fromUnix, toUnix int64) (*restaurantv1.CancelledInvoicesResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.CancelledInvoices(ctx, sqlc.CancelledInvoicesParams{CancelledAt: tsPg(fromUnix), CancelledAt_2: tsPg(toUnix)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.CancelledInvoicesResponse{}
	for _, r := range rows {
		row := &restaurantv1.CancelledInvoiceRow{
			Number: r.Number, FiscalYear: r.FiscalYear, BsDate: r.BsDate, TotalPaisa: r.TotalPaisa,
			Reason: r.CancelReason, CancelledByUserId: pgUUID(r.CancelledByUserID),
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
	rows, err := s.q.QueryAuditLog(ctx, sqlc.QueryAuditLogParams{
		FromTs: tsOf(req.GetFromUnix()), ToTs: tsOf(req.GetToUnix()), Action: req.GetAction(), RowLimit: limit,
	})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.AuditLogResponse{}
	for _, r := range rows {
		out.Entries = append(out.Entries, &restaurantv1.AuditEntry{
			Id: r.ID, ActorUserId: pgUUID(r.ActorUserID), Action: r.Action, Entity: r.Entity,
			EntityId: r.EntityID.String, Reason: r.Reason.String, ApproverUserId: pgUUID(r.ApproverUserID), CreatedAtUnix: r.CreatedAt.Unix(),
		})
	}
	return out, nil
}

// --- helpers ---

func tsOf(unix int64) time.Time { return time.Unix(unix, 0).UTC() }

func tsPg(unix int64) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Unix(unix, 0).UTC(), Valid: true}
}

func rupees(paisa int64) string { return fmt.Sprintf("%d.%02d", paisa/100, abs(paisa%100)) }

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func pgUUID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	var b [16]byte = u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func toSalesBookRow(r sqlc.Invoice) *restaurantv1.SalesBookRow {
	return &restaurantv1.SalesBookRow{
		FiscalYear: r.FiscalYear, Number: r.Number, Mode: r.Mode, BsDate: r.BsDate,
		BuyerName: r.BuyerName, BuyerPan: r.BuyerPan, SubtotalPaisa: r.SubtotalPaisa, DiscountPaisa: r.DiscountPaisa,
		TaxablePaisa: r.TaxablePaisa, VatPaisa: r.VatPaisa, TotalPaisa: r.TotalPaisa, Status: r.Status,
	}
}
