package analytics

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// Daily summaries (approach B): one small record per past business day, rebuilt
// from raw bills, orders, items and shifts. Disposable: deleting one only means it
// is rebuilt. Today is always built live and never stored.
//
// Freshness ("dirty" days): triggers in migration 014 mark a past day dirty in the
// same transaction as any write that touches it (bill cancelled, late void, order
// closed, shift closed, QR confirmed...). Every analytics read first rebuilds the
// dirty or missing past days in its range (recompute-on-read), so no background job
// is needed and a read never returns a stale past day.

type countValue struct {
	Count int64 `json:"count"`
	Value int64 `json:"value"`
}

func (c *countValue) add(o countValue) { c.Count += o.Count; c.Value += o.Value }

type itemDay struct {
	MenuItemID string `json:"menuItemId"`
	NameEn     string `json:"nameEn"`
	NameNe     string `json:"nameNe"`
	CategoryID string `json:"categoryId"`
	Quantity   int64  `json:"quantity"`
	Revenue    int64  `json:"revenue"`
	// Cost of the CostQty units whose lines carried SALE stock movements.
	Cost      int64 `json:"cost"`
	CostQty   int64 `json:"costQty"`
	VoidedQty int64 `json:"voidedQty"`
}

type hourDay struct {
	Hour   int   `json:"hour"`
	Sales  int64 `json:"sales"`
	Bills  int64 `json:"bills"`
	Orders int64 `json:"orders"`
	Covers int64 `json:"covers"`
}

type staffDay struct {
	UserID         string `json:"userId"`
	OrdersAsWaiter int64  `json:"ordersAsWaiter"`
	SalesAsWaiter  int64  `json:"salesAsWaiter"`
	Covers         int64  `json:"covers"`
	ItemsAdded     int64  `json:"itemsAdded"`
	ItemsVoided    int64  `json:"itemsVoided"`
	VoidValue      int64  `json:"voidValue"`
	BillsIssued    int64  `json:"billsIssued"`
	DiscountGiven  int64  `json:"discountGiven"`
	SalesBilled    int64  `json:"salesBilled"`
	Cancellations  int64  `json:"cancellations"`
	Approvals      int64  `json:"approvals"`
}

type tableDay struct {
	TableID string `json:"tableId"`
	Orders  int64  `json:"orders"`
	Covers  int64  `json:"covers"`
}

// dwellRow: a closed dine-in order, minutes from created to its last bill, and the
// Kathmandu hour it was opened.
type dwellRow struct {
	TableID string `json:"tableId"`
	Minutes int64  `json:"minutes"`
	Hour    int    `json:"hour"`
}

type daySummary struct {
	Date            string                `json:"date"`
	Sales           int64                 `json:"sales"`
	Bills           int64                 `json:"bills"`
	Covers          int64                 `json:"covers"`
	DineInOrders    int64                 `json:"dineInOrders"`
	TakeawayOrders  int64                 `json:"takeawayOrders"`
	DineInSales     int64                 `json:"dineInSales"`
	TakeawaySales   int64                 `json:"takeawaySales"`
	Discounts       int64                 `json:"discounts"`
	ServiceCharge   int64                 `json:"serviceCharge"`
	Vat             int64                 `json:"vat"`
	ByMethod        map[string]int64      `json:"byMethod"` // money taken; ON_ACCOUNT apart
	OnAccount       int64                 `json:"onAccount"`
	VoidsBefore     countValue            `json:"voidsBefore"`
	VoidsAfter      countValue            `json:"voidsAfter"`
	Cancelled       countValue            `json:"cancelled"`
	CashDifference  int64                 `json:"cashDifference"`
	DiscountReasons map[string]countValue `json:"discountReasons"`
	Items           []itemDay             `json:"items"`
	Hours           []hourDay             `json:"hours"`
	Staff           []staffDay            `json:"staff"`
	Tables          []tableDay            `json:"tables"`
	Dwell           []dwellRow            `json:"dwell"`
}

// rawData is everything a build reads, for a span of days.
type rawData struct {
	invoices  []sqlc.AnalyticsInvoicesRow
	payments  []sqlc.AnalyticsPaymentsRow
	lines     []sqlc.AnalyticsLinesRow
	orders    []sqlc.AnalyticsOrdersRow
	items     []sqlc.AnalyticsSentItemsRow
	cancelled []sqlc.AnalyticsCancelledInvoicesRow
	shifts    []sqlc.AnalyticsClosedShiftsRow
}

func tsPg(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// readRaw loads the raw rows of business days from..to in one read-only,
// repeatable-read transaction with a statement timeout, so the build sees one
// consistent snapshot and can never hold up ordering.
func (s *Service) readRaw(ctx context.Context, from, to time.Time) (*rawData, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 20000"); err != nil {
		return nil, connecterr.Internal(err)
	}
	q := s.q.WithTx(tx)
	lo, hi := startNPT(from), startNPT(addDays(to, 1))
	raw := &rawData{}
	if raw.invoices, err = q.AnalyticsInvoices(ctx, sqlc.AnalyticsInvoicesParams{FromTs: lo, ToTs: hi}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if raw.payments, err = q.AnalyticsPayments(ctx, sqlc.AnalyticsPaymentsParams{FromTs: lo, ToTs: hi}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if raw.lines, err = q.AnalyticsLines(ctx, sqlc.AnalyticsLinesParams{FromTs: lo, ToTs: hi}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if raw.orders, err = q.AnalyticsOrders(ctx, sqlc.AnalyticsOrdersParams{FromTs: lo, ToTs: hi}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if raw.items, err = q.AnalyticsSentItems(ctx, sqlc.AnalyticsSentItemsParams{FromTs: tsPg(lo), ToTs: tsPg(hi)}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if raw.cancelled, err = q.AnalyticsCancelledInvoices(ctx, sqlc.AnalyticsCancelledInvoicesParams{FromTs: tsPg(lo), ToTs: tsPg(hi)}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if raw.shifts, err = q.AnalyticsClosedShifts(ctx, sqlc.AnalyticsClosedShiftsParams{FromTs: tsPg(lo), ToTs: tsPg(hi)}); err != nil {
		return nil, connecterr.Internal(err)
	}
	return raw, nil
}

// dayBuilder accumulates one day's summary.
type dayBuilder struct {
	sum    *daySummary
	hours  map[int]*hourDay
	staff  map[string]*staffDay
	items  map[string]*itemDay
	tables map[string]*tableDay
}

func newDayBuilder(d time.Time) *dayBuilder {
	return &dayBuilder{
		sum:    &daySummary{Date: dayStr(d), ByMethod: zeroMethods(), DiscountReasons: map[string]countValue{}},
		hours:  map[int]*hourDay{},
		staff:  map[string]*staffDay{},
		items:  map[string]*itemDay{},
		tables: map[string]*tableDay{},
	}
}

var moneyMethods = []string{"CASH", "QR_FONEPAY", "QR_OTHER", "CARD"}

func zeroMethods() map[string]int64 {
	m := map[string]int64{}
	for _, k := range moneyMethods {
		m[k] = 0
	}
	return m
}

func (b *dayBuilder) hour(h int) *hourDay {
	r, ok := b.hours[h]
	if !ok {
		r = &hourDay{Hour: h}
		b.hours[h] = r
	}
	return r
}

// person is nil for no user (the row is then not attributed to anyone).
func (b *dayBuilder) person(id string) *staffDay {
	if id == "" {
		return &staffDay{}
	}
	r, ok := b.staff[id]
	if !ok {
		r = &staffDay{UserID: id}
		b.staff[id] = r
	}
	return r
}

func (b *dayBuilder) item(menuItemID, nameEn, nameNe, categoryID string) *itemDay {
	r, ok := b.items[menuItemID]
	if !ok {
		r = &itemDay{MenuItemID: menuItemID, NameEn: nameEn, NameNe: nameNe, CategoryID: categoryID}
		b.items[menuItemID] = r
	}
	return r
}

func (b *dayBuilder) finish() *daySummary {
	s := b.sum
	s.TakeawaySales = s.Sales - s.DineInSales
	s.Items = make([]itemDay, 0, len(b.items))
	for _, it := range b.items {
		s.Items = append(s.Items, *it)
	}
	sort.Slice(s.Items, func(i, j int) bool { return s.Items[i].MenuItemID < s.Items[j].MenuItemID })
	s.Hours = make([]hourDay, 0, len(b.hours))
	for _, h := range b.hours {
		s.Hours = append(s.Hours, *h)
	}
	sort.Slice(s.Hours, func(i, j int) bool { return s.Hours[i].Hour < s.Hours[j].Hour })
	s.Staff = make([]staffDay, 0, len(b.staff))
	for _, p := range b.staff {
		s.Staff = append(s.Staff, *p)
	}
	sort.Slice(s.Staff, func(i, j int) bool { return s.Staff[i].UserID < s.Staff[j].UserID })
	s.Tables = make([]tableDay, 0, len(b.tables))
	for _, t := range b.tables {
		s.Tables = append(s.Tables, *t)
	}
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].TableID < s.Tables[j].TableID })
	if s.Dwell == nil {
		s.Dwell = []dwellRow{}
	}
	return s
}

// buildDays turns raw rows into one summary per day from..to (empty days
// included). Idempotent: the same raw data gives the same summaries.
func buildDays(raw *rawData, from, to time.Time) map[string]*daySummary {
	builders := map[string]*dayBuilder{}
	for _, d := range daysIn(from, to) {
		builders[dayStr(d)] = newDayBuilder(d)
	}
	on := func(t time.Time) *dayBuilder { return builders[dayStr(ktmDay(t))] }

	// Bills by the day they were issued.
	type invInfo struct {
		b      *dayBuilder
		dineIn bool
	}
	invs := map[uuid.UUID]invInfo{}
	for _, inv := range raw.invoices {
		b := on(inv.AdDatetime)
		if b == nil {
			continue
		}
		s := b.sum
		s.Sales += inv.TotalPaisa
		s.Bills++
		s.Discounts += inv.DiscountPaisa
		s.ServiceCharge += inv.ServiceChargePaisa
		s.Vat += inv.VatPaisa
		dineIn := inv.OrderType == "DINE_IN"
		if dineIn {
			s.DineInSales += inv.TotalPaisa
		}
		invs[inv.ID] = invInfo{b, dineIn}
		h := b.hour(ktmHour(inv.AdDatetime))
		h.Sales += inv.TotalPaisa
		h.Bills++
		if w := uuidStr(inv.WaiterUserID); w != "" {
			b.person(w).SalesAsWaiter += inv.TotalPaisa
		}
		cashier := b.person(uuidStr(inv.CreatedByUserID))
		cashier.BillsIssued++
		cashier.SalesBilled += inv.TotalPaisa
		cashier.DiscountGiven += inv.DiscountPaisa
		if inv.DiscountPaisa > 0 {
			reason := strings.TrimSpace(inv.DiscountReason)
			if reason == "" {
				reason = "—"
			}
			cv := s.DiscountReasons[reason]
			cv.add(countValue{1, inv.DiscountPaisa})
			s.DiscountReasons[reason] = cv
		}
	}
	for _, p := range raw.payments {
		info, ok := invs[p.InvoiceID]
		if !ok {
			continue
		}
		if p.Method == "ON_ACCOUNT" {
			info.b.sum.OnAccount += p.AmountPaisa
		} else {
			info.b.sum.ByMethod[p.Method] += p.AmountPaisa
		}
	}
	for _, l := range raw.lines {
		b := on(l.AdDatetime)
		if b == nil {
			continue
		}
		key := l.MenuItemID
		if key == "" {
			key = "line:" + l.NameEn
		}
		it := b.item(key, l.NameEn, l.NameNe, l.CategoryID)
		it.Quantity += int64(l.Quantity)
		it.Revenue += l.LineTotalPaisa
		if l.HasCost {
			it.Cost += l.CostPaisa
			it.CostQty += int64(l.Quantity)
		}
	}

	// Orders, covers and table use by the day the order was opened (when guests
	// arrive). Cancelled (merged-away) orders are not read.
	for _, o := range raw.orders {
		b := on(o.CreatedAt)
		if b == nil {
			continue
		}
		h := b.hour(ktmHour(o.CreatedAt))
		h.Orders++
		waiter := b.person(o.WaiterUserID.String())
		waiter.OrdersAsWaiter++
		if o.Type != "DINE_IN" {
			b.sum.TakeawayOrders++
			continue
		}
		b.sum.DineInOrders++
		b.sum.Covers += int64(o.GuestCount)
		h.Covers += int64(o.GuestCount)
		waiter.Covers += int64(o.GuestCount)
		if !o.TableID.Valid {
			continue
		}
		tid := uuidStr(o.TableID)
		t, ok := b.tables[tid]
		if !ok {
			t = &tableDay{TableID: tid}
			b.tables[tid] = t
		}
		t.Orders++
		t.Covers += int64(o.GuestCount)
		// Dwell ends at the order's last bill (there is no "table cleared" event).
		if o.Status == "CLOSED" && o.LastBillUnix > 0 {
			minutes := (o.LastBillUnix - o.CreatedAt.Unix() + 30) / 60
			b.sum.Dwell = append(b.sum.Dwell, dwellRow{TableID: tid, Minutes: max(0, minutes), Hour: ktmHour(o.CreatedAt)})
		}
	}

	// Items sent that day; a void belongs to the send (it has no time of its own).
	for _, it := range raw.items {
		if !it.SentAt.Valid {
			continue
		}
		b := on(it.SentAt.Time)
		if b == nil {
			continue
		}
		b.person(uuidStr(it.AddedByUserID)).ItemsAdded += int64(it.Quantity)
		if it.Status != "VOIDED" {
			continue
		}
		value := it.UnitPricePaisa * int64(it.Quantity)
		if it.VoidApprovedByUserID.Valid {
			b.sum.VoidsAfter.add(countValue{1, value})
			b.person(uuidStr(it.VoidApprovedByUserID)).Approvals++
		} else {
			b.sum.VoidsBefore.add(countValue{1, value})
		}
		b.item(it.MenuItemID.String(), it.NameEn, it.NameNe, it.CategoryID).VoidedQty += int64(it.Quantity)
		if it.VoidedByUserID.Valid {
			p := b.person(uuidStr(it.VoidedByUserID))
			p.ItemsVoided += int64(it.Quantity)
			p.VoidValue += value
		}
	}

	for _, c := range raw.cancelled {
		if !c.CancelledAt.Valid {
			continue
		}
		b := on(c.CancelledAt.Time)
		if b == nil {
			continue
		}
		b.sum.Cancelled.add(countValue{1, c.TotalPaisa})
		if c.CancelledByUserID.Valid {
			b.person(uuidStr(c.CancelledByUserID)).Cancellations++
		}
		if c.CancelApprovedByUserID.Valid {
			b.person(uuidStr(c.CancelApprovedByUserID)).Approvals++
		}
	}

	for _, sh := range raw.shifts {
		if !sh.ClosedAt.Valid {
			continue
		}
		if b := on(sh.ClosedAt.Time); b != nil {
			b.sum.CashDifference += sh.DifferencePaisa
		}
	}

	out := make(map[string]*daySummary, len(builders))
	for k, b := range builders {
		out[k] = b.finish()
	}
	return out
}

// graceAfterMidnight: yesterday is still computed live (not stored) this long
// after midnight, so a transaction that started before midnight has committed
// before its day is first stored.
const graceAfterMidnight = 5 * time.Minute

// summaries returns one summary per day in r, clipped to [first data, today]: past
// days from the store (rebuilt first when dirty or missing), today (and
// yesterday, just after midnight) live.
func (s *Service) summaries(ctx context.Context, c *readCtx, r dayRange) (map[string]*daySummary, error) {
	out := map[string]*daySummary{}
	if c.first.IsZero() {
		return out, nil
	}
	lo, hi := maxDay(r.from, c.first), minDay(r.to, c.today)
	if lo.After(hi) {
		return out, nil
	}
	storeHi := s.lastStorable(c)
	if pastHi := minDay(hi, storeHi); !lo.After(pastHi) {
		if err := s.storedSummaries(ctx, lo, pastHi, out); err != nil {
			return nil, err
		}
	}
	if liveLo := maxDay(lo, addDays(storeHi, 1)); !liveLo.After(hi) {
		raw, err := s.readRaw(ctx, liveLo, hi)
		if err != nil {
			return nil, err
		}
		for k, v := range buildDays(raw, liveLo, hi) {
			out[k] = v
		}
	}
	return out, nil
}

// lastStorable is the last day that may be stored: yesterday, or the day before
// within the grace period after midnight.
func (s *Service) lastStorable(c *readCtx) time.Time {
	if c.now.Before(startNPT(c.today).Add(graceAfterMidnight)) {
		return addDays(c.today, -2)
	}
	return addDays(c.today, -1)
}

func pgDate(d time.Time) pgtype.Date { return pgtype.Date{Time: d, Valid: true} }

func (s *Service) storedSummaries(ctx context.Context, lo, hi time.Time, out map[string]*daySummary) error {
	stored, err := s.q.AnalyticsDaysIn(ctx, sqlc.AnalyticsDaysInParams{FromDay: pgDate(lo), ToDay: pgDate(hi)})
	if err != nil {
		return connecterr.Internal(err)
	}
	dirty, err := s.q.AnalyticsDirtyDaysIn(ctx, sqlc.AnalyticsDirtyDaysInParams{FromDay: pgDate(lo), ToDay: pgDate(hi)})
	if err != nil {
		return connecterr.Internal(err)
	}
	isDirty := map[string]bool{}
	for _, d := range dirty {
		isDirty[dayStr(d.Time)] = true
	}
	have := map[string]bool{}
	for _, row := range stored {
		k := dayStr(row.Day.Time)
		if isDirty[k] {
			continue
		}
		var sum daySummary
		if err := json.Unmarshal(row.Summary, &sum); err != nil {
			continue // unreadable: rebuilt below
		}
		out[k] = &sum
		have[k] = true
	}
	var need []time.Time
	for _, d := range daysIn(lo, hi) {
		if !have[dayStr(d)] {
			need = append(need, d)
		}
	}
	if len(need) == 0 {
		return nil
	}
	return s.rebuildDays(ctx, need, out)
}

// rebuildDays builds and stores the given past days (sorted), one raw read per
// run of consecutive days. Their dirty marks are cleared before the raw data is
// read: a write committing after that marks the day again, so it is never lost.
func (s *Service) rebuildDays(ctx context.Context, days []time.Time, out map[string]*daySummary) error {
	marks := make([]pgtype.Date, 0, len(days))
	for _, d := range days {
		marks = append(marks, pgDate(d))
	}
	if err := s.q.ClearAnalyticsDirtyDays(ctx, marks); err != nil {
		return connecterr.Internal(err)
	}
	for i := 0; i < len(days); {
		j := i
		for j+1 < len(days) && addDays(days[j], 1).Equal(days[j+1]) {
			j++
		}
		raw, err := s.readRaw(ctx, days[i], days[j])
		if err != nil {
			return err
		}
		built := buildDays(raw, days[i], days[j])
		for _, d := range days[i : j+1] {
			sum := built[dayStr(d)]
			b, err := json.Marshal(sum)
			if err != nil {
				return connecterr.Internal(err)
			}
			if err := s.q.UpsertAnalyticsDay(ctx, sqlc.UpsertAnalyticsDayParams{Day: pgDate(d), Summary: b}); err != nil {
				return connecterr.Internal(err)
			}
			if out != nil {
				out[dayStr(d)] = sum
			}
		}
		i = j + 1
	}
	return nil
}
