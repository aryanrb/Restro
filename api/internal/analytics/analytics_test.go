package analytics

import (
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/reports"
)

// Integration tests mirroring web/src/lib/api/mock/mock-analytics-api.test.ts.

func dayTrend(t *testing.T, f *fixture, from, to time.Time) *restaurantv1.TrendResponse {
	t.Helper()
	tr, err := f.svc.Trend(f.owner, &restaurantv1.GetTrendRequest{Range: f.rng(from, to), Grain: restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_DAY})
	if err != nil {
		t.Fatalf("trend: %v", err)
	}
	return tr
}

func TestAccessOwnerAndManagerOnly(t *testing.T) {
	f := newFixture(t)
	f.scenario()
	two := f.rng(f.yesterday, f.today)
	for _, staff := range []struct {
		name string
		ctx  func() error
	}{
		{"cashier meta", func() error { _, err := f.svc.Meta(f.hari); return err }},
		{"waiter meta", func() error { _, err := f.svc.Meta(f.gita); return err }},
		{"cashier trend", func() error { _, err := f.svc.Trend(f.hari, &restaurantv1.GetTrendRequest{Range: two}); return err }},
		{"waiter calendar", func() error {
			_, err := f.svc.ListCalendarEvents(f.gita, &restaurantv1.ListCalendarEventsRequest{Range: two})
			return err
		}},
	} {
		if got := reasonOf(staff.ctx()); got != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
			t.Fatalf("%s: reason %v, want NOT_ALLOWED", staff.name, got)
		}
	}
	m, err := f.svc.Meta(f.sita)
	if err != nil {
		t.Fatal(err)
	}
	if m.CanSeeStaff || m.IsOwner || !m.Modules.Credit || !m.Modules.Inventory || m.Modules.Loyalty || m.Modules.Reservations {
		t.Fatalf("manager meta = %+v", m)
	}
	if _, err := f.svc.Staff(f.sita, &restaurantv1.GetStaffAnalyticsRequest{Range: two}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
		t.Fatalf("manager staff: %v", err)
	}
	if _, err := f.svc.SaveSettings(f.sita, &restaurantv1.AnalyticsSettings{ManagersSeeStaff: []string{f.sitaID.String()}}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
		t.Fatalf("manager save settings: %v", err)
	}
	if _, err := f.svc.Rebuild(f.sita, &restaurantv1.RebuildAnalyticsRequest{Range: two}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
		t.Fatalf("manager rebuild: %v", err)
	}
	if m, _ := f.svc.Meta(f.owner); !m.CanSeeStaff || !m.IsOwner {
		t.Fatalf("owner meta = %+v", m)
	}
	// The owner opts Sita in; a cashier's id is dropped.
	saved, err := f.svc.SaveSettings(f.owner, &restaurantv1.AnalyticsSettings{ManagersSeeStaff: []string{f.sitaID.String(), f.hariID.String()}, HighlightRules: true})
	if err != nil || !slices.Equal(saved.ManagersSeeStaff, []string{f.sitaID.String()}) {
		t.Fatalf("save settings = %v, %v", saved, err)
	}
	if m, _ := f.svc.Meta(f.sita); !m.CanSeeStaff {
		t.Fatal("opted-in manager can't see staff")
	}
	st, err := f.svc.Staff(f.sita, &restaurantv1.GetStaffAnalyticsRequest{Range: two})
	if err != nil || len(st.Rows) == 0 {
		t.Fatalf("opted-in manager staff = %v, %v", st, err)
	}
}

func TestAtMostThreeFiscalYearsPerCall(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Trend(f.owner, &restaurantv1.GetTrendRequest{Range: &restaurantv1.AnalyticsRange{From: "2020-01-01", To: dayStr(f.today)}, Grain: restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_MONTH_BS})
	if reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_RANGE_TOO_LARGE {
		t.Fatalf("6 years: %v", err)
	}
	_, err = f.svc.Hours(f.owner, &restaurantv1.GetHoursRequest{Range: f.rng(f.today, f.yesterday)})
	if reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_INVALID {
		t.Fatalf("from after to: %v", err)
	}
}

func TestDailySummarySalesEqualTheDailySalesReport(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	tr := dayTrend(t, f, f.yesterday, f.today)
	daily, err := reports.NewService(f.pool, zap.NewNop()).DailySales(f.owner, startNPT(f.yesterday).Unix(), startNPT(addDays(f.today, 1)).Unix()-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily.Rows) != 2 || len(tr.Periods) != 2 {
		t.Fatalf("rows %d, periods %d", len(daily.Rows), len(tr.Periods))
	}
	for i, row := range daily.Rows {
		p := tr.Periods[i]
		if p.Key != row.AdDate || p.Headline.SalesPaisa != row.TotalPaisa || p.Headline.Bills != row.InvoiceCount {
			t.Fatalf("period %s %d/%d vs daily %s %d/%d", p.Key, p.Headline.SalesPaisa, p.Headline.Bills, row.AdDate, row.TotalPaisa, row.InvoiceCount)
		}
	}
	if tr.Periods[1].Headline.SalesPaisa != s.aTotal+s.bTotal {
		t.Fatalf("today sales %d", tr.Periods[1].Headline.SalesPaisa)
	}
	tot := tr.Total
	if tot.SalesPaisa != s.aTotal+s.bTotal+s.cTotal || tot.Bills != 3 || tot.Covers != 5 || tot.DineInOrders != 2 || tot.TakeawayOrders != 1 || tot.OpenDays != 2 {
		t.Fatalf("total = %+v", tot)
	}
	if tot.DineInSalesPaisa != s.aTotal+s.cTotal || tot.PerCoverPaisa != ratio(s.aTotal+s.cTotal, 5) {
		t.Fatalf("dine-in %d, per cover %d", tot.DineInSalesPaisa, tot.PerCoverPaisa)
	}
	if tr.UpToUnix == 0 {
		t.Fatal("today is live: up_to must be set")
	}
	// Yesterday is now stored; today never is.
	var stored []string
	rows, _ := f.pool.Query(f.ctx, `SELECT to_char(day, 'YYYY-MM-DD') FROM analytics_days ORDER BY day`)
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		stored = append(stored, d)
	}
	if !slices.Contains(stored, dayStr(f.yesterday)) || slices.Contains(stored, dayStr(f.today)) {
		t.Fatalf("stored days %v", stored)
	}
}

func TestBillCancelledAfterItsDayMarksTheDayDirtyAndItIsRebuilt(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	if got := dayTrend(t, f, f.yesterday, f.today).Periods[0].Headline.SalesPaisa; got != s.cTotal {
		t.Fatalf("yesterday before = %d", got)
	}
	var dirty int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM analytics_dirty_days`).Scan(&dirty)
	if dirty != 0 {
		t.Fatalf("dirty after the read = %d", dirty)
	}
	f.cancel(s.c, f.hariID, f.sitaID, time.Now())
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM analytics_dirty_days WHERE day = $1`, pgDate(f.yesterday)).Scan(&dirty)
	if dirty != 1 {
		t.Fatal("cancelling yesterday's bill must mark yesterday dirty in the same transaction")
	}
	after := dayTrend(t, f, f.yesterday, f.today)
	if after.Periods[0].Headline.SalesPaisa != 0 {
		t.Fatalf("yesterday after cancel = %d", after.Periods[0].Headline.SalesPaisa)
	}
	// The cancellation itself counts on the day it happened (today).
	m, err := f.svc.Money(f.owner, &restaurantv1.GetMoneyAnalyticsRequest{Range: f.rng(f.today, f.today)})
	if err != nil || m.Total.Cancelled.Count != 1 || m.Total.Cancelled.ValuePaisa != s.cTotal {
		t.Fatalf("money today = %+v, %v", m.GetTotal().GetCancelled(), err)
	}
}

func TestRebuildIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.scenario()
	first := dayTrend(t, f, f.yesterday, f.today)
	two := f.rng(f.yesterday, f.today)
	for range 2 {
		r, err := f.svc.Rebuild(f.owner, &restaurantv1.RebuildAnalyticsRequest{Range: two})
		if err != nil || r.Days != 1 { // yesterday; today is always live
			t.Fatalf("rebuild = %v, %v", r, err)
		}
	}
	again := dayTrend(t, f, f.yesterday, f.today)
	for i := range first.Periods {
		if first.Periods[i].Headline.SalesPaisa != again.Periods[i].Headline.SalesPaisa {
			t.Fatalf("period %d changed on rebuild", i)
		}
	}
	if m, _ := f.svc.Meta(f.owner); m.LastBuildAtUnix == 0 || m.FirstDate != dayStr(f.yesterday) {
		t.Fatalf("meta = %+v", m)
	}
}

func TestClosedDaysAreGapsAndLeftOutOfAverages(t *testing.T) {
	f := newFixture(t)
	f.scenario()
	if _, err := f.svc.SaveCalendarEvent(f.owner, &restaurantv1.SaveCalendarEventRequest{
		Name: local("Closed for repairs", "मर्मतका लागि बन्द"), From: dayStr(f.weekAgo), To: dayStr(f.weekAgo), Kind: restaurantv1.CalendarKind_CALENDAR_KIND_CLOSED,
	}); err != nil {
		t.Fatal(err)
	}
	tr := dayTrend(t, f, f.weekAgo, f.today)
	if !tr.Periods[0].Closed || tr.Periods[0].Key != dayStr(f.weekAgo) {
		t.Fatalf("first period = %+v", tr.Periods[0])
	}
	if tr.Total.ClosedDays != 1 || tr.Total.OpenDays != 2 { // data starts yesterday; the closed day isn't counted
		t.Fatalf("total = %+v", tr.Total)
	}
	if tr.Total.DailyAveragePaisa != ratio(tr.Total.SalesPaisa, 2) {
		t.Fatalf("daily average %d", tr.Total.DailyAveragePaisa)
	}
}

func TestBSMonthAndFiscalYearGrainsWithComparisons(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	bs, _ := adToBs(f.today)
	from, _ := bsToAd(bs.Year, bs.Month, 1)
	n, _ := bsMonthDays(bs.Year, bs.Month)
	month := dayRange{from, addDays(from, n-1)}
	m, err := f.svc.Trend(f.owner, &restaurantv1.GetTrendRequest{Range: toRange(month), Grain: restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_MONTH_BS, Compare: restaurantv1.AnalyticsCompare_ANALYTICS_COMPARE_LAST_YEAR})
	if err != nil {
		t.Fatal(err)
	}
	ly, _ := lastYearRange(month)
	if len(m.Periods) != 1 || m.Periods[0].Key != fmt.Sprintf("%d-%02d", bs.Year, bs.Month-1) {
		t.Fatalf("month periods = %+v", m.Periods)
	}
	if m.ComparedRange.From != dayStr(ly.from) || m.ComparedRange.To != dayStr(ly.to) || m.ComparedTotal.SalesPaisa != 0 {
		t.Fatalf("compared = %+v / %+v", m.ComparedRange, m.ComparedTotal)
	}
	fyStart, _ := fiscalYearOf(f.today)
	fyFrom, _ := bsToAd(fyStart, 4, 1)
	fyNext, _ := bsToAd(fyStart+1, 4, 1)
	fy := dayRange{fyFrom, addDays(fyNext, -1)}
	fr, err := f.svc.Trend(f.owner, &restaurantv1.GetTrendRequest{Range: toRange(fy), Grain: restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_FY, Compare: restaurantv1.AnalyticsCompare_ANALYTICS_COMPARE_PREVIOUS})
	if err != nil {
		t.Fatal(err)
	}
	prev := previousRange(fy)
	if fr.Periods[0].Key != fmt.Sprint(fyStart) || fr.ComparedRange.From != dayStr(prev.from) {
		t.Fatalf("fy = %s, compared %+v", fr.Periods[0].Key, fr.ComparedRange)
	}
	w, err := f.svc.Trend(f.owner, &restaurantv1.GetTrendRequest{Range: f.rng(f.weekAgo, f.today), Grain: restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_WEEK})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := parseDay(w.Periods[1].From)
	if len(w.Periods) != 2 || weekday(second) != 0 { // 8 days always touch two Sunday–Saturday weeks
		t.Fatalf("weeks = %+v", w.Periods)
	}
	custom, err := f.svc.Trend(f.owner, &restaurantv1.GetTrendRequest{Range: f.rng(f.today, f.today), Compare: restaurantv1.AnalyticsCompare_ANALYTICS_COMPARE_PREVIOUS, CompareRange: f.rng(f.yesterday, f.yesterday)})
	if err != nil || custom.Compared[0].Period.Headline.SalesPaisa != s.cTotal {
		t.Fatalf("custom compare = %+v, %v", custom.GetCompared(), err)
	}
}

func TestBusyHoursAveragePerWeekdayOccurrence(t *testing.T) {
	f := newFixture(t)
	// The takeaway order and its bill happen on the same weekday last week.
	s := f.scenarioWith(f.weekAgo)
	h, err := f.svc.Hours(f.owner, &restaurantv1.GetHoursRequest{Range: f.rng(f.weekAgo, f.today), Metric: restaurantv1.HoursMetric_HOURS_METRIC_SALES})
	if err != nil {
		t.Fatal(err)
	}
	wd := weekday(f.today)
	row := h.Rows[wd]
	if row.Occurrences != 2 {
		t.Fatalf("occurrences = %d", row.Occurrences)
	}
	var total int64
	for _, v := range row.Total {
		total += v
	}
	if total != s.aTotal+s.bTotal {
		t.Fatalf("total %d", total)
	}
	if ha, hb := ktmHour(s.aAt), ktmHour(s.bAt); ha == hb && row.Average[ha] != ratio(s.aTotal+s.bTotal, 2) {
		t.Fatalf("average %d", row.Average[ha])
	}
	if h.FirstHour == nil {
		t.Fatal("hours with activity expected")
	}
	cv, err := f.svc.Hours(f.owner, &restaurantv1.GetHoursRequest{Range: f.rng(f.yesterday, f.today), Metric: restaurantv1.HoursMetric_HOURS_METRIC_COVERS})
	if err != nil {
		t.Fatal(err)
	}
	var covers int64
	for _, r := range cv.Rows {
		for _, v := range r.Total {
			covers += v
		}
	}
	if covers != 5 {
		t.Fatalf("covers %d", covers)
	}
}

func TestMenuQuantityRevenueShareVoidsCompareAndNeverOrdered(t *testing.T) {
	f := newFixture(t)
	f.scenario()
	m, err := f.svc.Menu(f.owner, &restaurantv1.GetMenuAnalyticsRequest{Range: f.rng(f.today, f.today), Compare: restaurantv1.AnalyticsCompare_ANALYTICS_COMPARE_PREVIOUS})
	if err != nil {
		t.Fatal(err)
	}
	row := func(rows []*restaurantv1.MenuRow, id uuid.UUID) *restaurantv1.MenuRow {
		for _, r := range rows {
			if r.MenuItemId == id.String() {
				return r
			}
		}
		t.Fatalf("no row for %s", id)
		return nil
	}
	veg := row(m.Rows, f.veg)
	if veg.Quantity != 1 || veg.RevenuePaisa != 18_000 || veg.GetComparedQuantity() != 1 || veg.GetComparedRevenuePaisa() != 18_000 || veg.ChangeBp == nil || *veg.ChangeBp != 0 || veg.CostPaisa != nil || veg.MenuClass != restaurantv1.MenuClass_MENU_CLASS_UNSPECIFIED {
		t.Fatalf("veg = %+v", veg)
	}
	if l := row(m.Rows, f.lassi); l.Quantity != 2 || l.RevenuePaisa != 24_000 {
		t.Fatalf("lassi = %+v", l)
	}
	var share int32
	for _, r := range m.Rows {
		share += r.ShareBp
	}
	if share < 9_999 || m.Costs {
		t.Fatalf("share %d, costs %v", share, m.Costs)
	}
	if !slices.ContainsFunc(m.NeverOrdered, func(i *restaurantv1.NeverOrderedItem) bool { return i.MenuItemId == f.coke.String() }) {
		t.Fatal("coke was never ordered")
	}
	y, err := f.svc.Menu(f.owner, &restaurantv1.GetMenuAnalyticsRequest{Range: f.rng(f.yesterday, f.yesterday)})
	if err != nil {
		t.Fatal(err)
	}
	if th := row(y.Rows, f.thuk); th.Quantity != 0 || th.VoidedQty != 1 {
		t.Fatalf("thukpa = %+v", th)
	}
	d, err := f.svc.Menu(f.owner, &restaurantv1.GetMenuAnalyticsRequest{Range: f.rng(f.yesterday, f.today), CategoryId: f.drinks.String()})
	if err != nil || len(d.Rows) != 1 || d.Rows[0].MenuItemId != f.lassi.String() {
		t.Fatalf("drinks = %+v, %v", d.GetRows(), err)
	}
}

// Menu engineering from the SALE stock movements' unit cost snapshots.
func TestMenuEngineeringFromStockMovementCosts(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	flour := f.id(`INSERT INTO inventory_items (name_en, unit, tracked, unit_cost_e4) VALUES ('Flour', 'g', true, 1000000) RETURNING id`)
	// Veg momo today: 100 g at Rs 1/g = Rs 100 cost; chowmein: 20 g = Rs 20.
	var veg, chow uuid.UUID
	_ = f.pool.QueryRow(f.ctx, `SELECT id FROM order_items WHERE order_id = $1 AND menu_item_id = $2`, s.o1, f.veg).Scan(&veg)
	_ = f.pool.QueryRow(f.ctx, `SELECT id FROM order_items WHERE order_id = $1 AND menu_item_id = $2`, s.o1, f.chow).Scan(&chow)
	for _, mv := range []struct {
		ref uuid.UUID
		qty int64
	}{{veg, -100_000}, {chow, -20_000}} {
		f.exec(`INSERT INTO stock_movements (inventory_item_id, quantity_milli, type, ref_id, at, unit_cost_e4)
			VALUES ($1, $2, 'SALE', $3, now(), 1000000)`, flour, mv.qty, mv.ref)
	}
	m, err := f.svc.Menu(f.owner, &restaurantv1.GetMenuAnalyticsRequest{Range: f.rng(f.today, f.today), CategoryId: f.food.String()})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Costs {
		t.Fatal("costs expected")
	}
	byID := map[string]*restaurantv1.MenuRow{}
	for _, r := range m.Rows {
		byID[r.MenuItemId] = r
	}
	v, c := byID[f.veg.String()], byID[f.chow.String()]
	if v.GetCostPaisa() != 10_000 || v.GetMarginPaisa() != 8_000 || c.GetCostPaisa() != 2_000 || c.GetMarginPaisa() != 13_000 {
		t.Fatalf("veg %+v, chow %+v", v, c)
	}
	// Both sold once (popular); chowmein's margin is above the category average.
	if v.MenuClass != restaurantv1.MenuClass_MENU_CLASS_PLOWHORSE || c.MenuClass != restaurantv1.MenuClass_MENU_CLASS_STAR {
		t.Fatalf("classes veg %v, chow %v", v.MenuClass, c.MenuClass)
	}
}

func TestTablesDwellToTheLastBillTurnoverAndTakeawayShare(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	tb, err := f.svc.Tables(f.owner, &restaurantv1.GetTablesAnalyticsRequest{Range: f.rng(f.yesterday, f.today)})
	if err != nil {
		t.Fatal(err)
	}
	var t1 *restaurantv1.TableUse
	for _, x := range tb.Tables {
		if x.TableId == f.t1.String() {
			t1 = x
		}
	}
	if t1 == nil || t1.Orders != 1 || t1.Turnover != 0.5 {
		t.Fatalf("t1 = %+v", t1)
	}
	if med := int64(t1.Dwell.GetMedian()); med < s.dwellA-1 || med > s.dwellA+1 {
		t.Fatalf("t1 dwell %d, want ~%d", med, s.dwellA)
	}
	if tb.DineInOrders != 2 || tb.TakeawayOrders != 1 || tb.TakeawayShareBp != 3333 || tb.Overall.Count != 2 || tb.OpenDays != 2 {
		t.Fatalf("tables = %+v", tb)
	}
	if len(tb.ByMeal) != 2 || len(tb.ByWeekday) != 7 || len(tb.Areas) != 1 || tb.Areas[0].Orders != 2 {
		t.Fatalf("breakdowns = %+v / %+v", tb.ByMeal, tb.Areas)
	}
}

func TestStaffRatesNextToTheAverageByName(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	st, err := f.svc.Staff(f.owner, &restaurantv1.GetStaffAnalyticsRequest{Range: f.rng(f.yesterday, f.today)})
	if err != nil {
		t.Fatal(err)
	}
	if st.MinDays != 5 {
		t.Fatalf("min days %d", st.MinDays)
	}
	names := []string{}
	for _, r := range st.Rows {
		names = append(names, r.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("not by name: %v", names)
	}
	var gita, hari *restaurantv1.StaffRate
	for _, r := range st.Rows {
		switch r.UserId {
		case f.gitaID.String():
			gita = r
		case f.hariID.String():
			hari = r
		case f.ownerID.String():
			t.Fatal("the owner did nothing in the range")
		}
	}
	if gita == nil || gita.DaysWorked != 2 || gita.OrdersAsWaiter != 3 || gita.Covers != 5 || gita.ItemsAdded != 6 || gita.ItemsVoided != 1 || !gita.TooLittleData {
		t.Fatalf("gita = %+v", gita)
	}
	if gita.VoidsPer_100 != 16.7 || gita.Role != restaurantv1.Role_ROLE_WAITER {
		t.Fatalf("gita voids/100 %v", gita.VoidsPer_100)
	}
	if hari == nil || hari.SalesBilledPaisa != s.aTotal+s.bTotal+s.cTotal || hari.DiscountGivenPaisa != s.bDiscount || hari.DiscountPct != pct1(s.bDiscount, s.aTotal+s.bTotal+s.cTotal) {
		t.Fatalf("hari = %+v", hari)
	}
	if st.Average.VoidsPer_100 != 16.7 {
		t.Fatalf("average = %+v", st.Average)
	}
}

func TestMoneyPaymentMixDiscountReasonsVoidsAndOnAccount(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	cust := f.id(`INSERT INTO customers (name, phone, credit_limit_paisa) VALUES ('Ram Shrestha', '9841234567', 1000000) RETURNING id`)
	o := f.order(order{typ: "TAKEAWAY", label: "#2", waiter: f.gitaID, created: f.at(f.today, 0.4), status: "CLOSED"})
	it := f.item(item{order: o, menu: f.coke, name: "Coke", price: 8_000, station: f.bar, status: "SERVED", sent: f.at(f.today, 0.4), addedBy: f.gitaID})
	_, credit := f.bill(bill{order: o, at: f.at(f.today, 0.5), items: []uuid.UUID{it}, by: f.hariID, customer: cust})

	m, err := f.svc.Money(f.owner, &restaurantv1.GetMoneyAnalyticsRequest{Range: f.rng(f.yesterday, f.today), Grain: restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_DAY})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Periods) != 2 {
		t.Fatalf("periods %d", len(m.Periods))
	}
	by := map[restaurantv1.PaymentMethod]int64{}
	for _, x := range m.Total.ByMethod {
		by[x.Method] = x.TotalPaisa
	}
	if len(m.Total.ByMethod) != 4 || by[restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY] != s.bTotal || by[restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH] != s.aTotal+s.cTotal {
		t.Fatalf("by method %v", m.Total.ByMethod)
	}
	if m.Total.GetOnAccountPaisa() != credit || m.Total.SalesPaisa != s.aTotal+s.bTotal+s.cTotal+credit {
		t.Fatalf("on account %d, sales %d", m.Total.GetOnAccountPaisa(), m.Total.SalesPaisa)
	}
	if vb := m.Total.VoidsBefore; vb.Count != 1 || vb.ValuePaisa != 22_000 {
		t.Fatalf("voids before %+v", vb)
	}
	if len(m.DiscountReasons) != 1 || m.DiscountReasons[0].Reason != "Regular" || m.DiscountReasons[0].Count != 1 || m.DiscountReasons[0].ValuePaisa != s.bDiscount {
		t.Fatalf("reasons %+v", m.DiscountReasons)
	}
}

// A closed shift's counted − expected cash lands on the day it closed, and a
// change after the fact (a late adjustment) marks that day dirty.
func TestMoneyCashDifferenceOfShiftsClosedInThePeriod(t *testing.T) {
	f := newFixture(t)
	s := f.scenario()
	closedAt := f.at(f.yesterday, 0.9)
	// Takings in the shift: a + b (today) and c are on f.shift; close a separate one.
	sh := f.id(`INSERT INTO shifts (cashier_user_id, opened_at, opening_float_paisa) VALUES ($1, $2, 500000) RETURNING id`, f.sitaID, f.at(f.yesterday, 0.1))
	f.exec(`UPDATE payments SET shift_id = $1 WHERE invoice_id = $2`, sh, s.c)
	f.exec(`UPDATE shifts SET status = 'CLOSED', closed_at = $2, counted_cash_paisa = $3 WHERE id = $1`, sh, closedAt, 500_000+s.cTotal-1_000)
	m, err := f.svc.Money(f.owner, &restaurantv1.GetMoneyAnalyticsRequest{Range: f.rng(f.yesterday, f.yesterday)})
	if err != nil || m.Total.CashDifferencePaisa != -1_000 {
		t.Fatalf("cash difference = %d, %v", m.GetTotal().GetCashDifferencePaisa(), err)
	}
}

func TestHighlightsByFixedRules(t *testing.T) {
	f := newFixture(t)
	f.scenario()
	if _, err := f.svc.SaveCalendarEvent(f.owner, &restaurantv1.SaveCalendarEventRequest{
		Name: local("Local jatra", "स्थानीय जात्रा"), From: dayStr(f.today), To: dayStr(f.today), Kind: restaurantv1.CalendarKind_CALENDAR_KIND_FESTIVAL,
	}); err != nil {
		t.Fatal(err)
	}
	req := &restaurantv1.GetHighlightsRequest{Range: f.rng(f.today, f.today), Compare: restaurantv1.AnalyticsCompare_ANALYTICS_COMPARE_PREVIOUS}
	hs, err := f.svc.Highlights(f.owner, req)
	if err != nil {
		t.Fatal(err)
	}
	busiest, jatra := false, false
	for _, h := range hs.Highlights {
		if h.Kind == restaurantv1.HighlightKind_HIGHLIGHT_KIND_BUSIEST_SLOT {
			busiest = true
		}
		if h.Kind == restaurantv1.HighlightKind_HIGHLIGHT_KIND_FESTIVAL_ONE_SIDE && h.Name.En == "Local jatra" && !h.InCompared {
			jatra = true
		}
	}
	if !busiest || !jatra || len(hs.Highlights) > 5 {
		t.Fatalf("highlights = %+v", hs.Highlights)
	}
	if _, err := f.svc.SaveSettings(f.owner, &restaurantv1.AnalyticsSettings{HighlightRules: false}); err != nil {
		t.Fatal(err)
	}
	if hs, _ := f.svc.Highlights(f.owner, req); len(hs.Highlights) != 0 {
		t.Fatalf("switched off: %+v", hs.Highlights)
	}
}

func TestCalendarShipsFestivalsOwnerEditsEveryChangeAudited(t *testing.T) {
	f := newFixture(t)
	fy := &restaurantv1.AnalyticsRange{From: "2026-07-17", To: "2027-07-16"}
	list, err := f.svc.ListCalendarEvents(f.owner, &restaurantv1.ListCalendarEventsRequest{Range: fy})
	if err != nil {
		t.Fatal(err)
	}
	var dashain []*restaurantv1.CalendarEvent
	var newYear *restaurantv1.CalendarEvent
	for _, e := range list.Events {
		if e.Key == "dashain" {
			dashain = append(dashain, e)
		}
		if e.Key == "bs-new-year" {
			newYear = e
		}
	}
	if len(dashain) != 1 || !dashain[0].BuiltIn || !dashain[0].Approximate || dashain[0].Kind != restaurantv1.CalendarKind_CALENDAR_KIND_FESTIVAL {
		t.Fatalf("dashain = %+v", dashain)
	}
	if newYear == nil || newYear.From != "2027-04-14" || newYear.Approximate {
		t.Fatalf("new year = %+v", newYear)
	}
	ml, err := f.svc.ListCalendarEvents(f.sita, &restaurantv1.ListCalendarEventsRequest{Range: fy})
	if err != nil || len(ml.Events) != len(list.Events) {
		t.Fatalf("manager list = %d, %v", len(ml.GetEvents()), err)
	}
	today := dayStr(f.today)
	if _, err := f.svc.SaveCalendarEvent(f.sita, &restaurantv1.SaveCalendarEventRequest{Name: local("X", ""), From: today, To: today, Kind: restaurantv1.CalendarKind_CALENDAR_KIND_OTHER}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
		t.Fatalf("manager save: %v", err)
	}
	d := dashain[0]
	moved, err := f.svc.SaveCalendarEvent(f.owner, &restaurantv1.SaveCalendarEventRequest{Id: d.Id, Name: d.Name, From: "2026-10-12", To: d.To, Kind: d.Kind})
	if err != nil || moved.Id != d.Id || moved.From != "2026-10-12" || moved.Approximate || !moved.BuiltIn || moved.Key != "dashain" {
		t.Fatalf("moved = %+v, %v", moved, err)
	}
	if _, err := f.svc.SaveCalendarEvent(f.owner, &restaurantv1.SaveCalendarEventRequest{Name: local("", ""), From: today, To: today, Kind: restaurantv1.CalendarKind_CALENDAR_KIND_OTHER}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_INVALID {
		t.Fatalf("blank name: %v", err)
	}
	if _, err := f.svc.SaveCalendarEvent(f.owner, &restaurantv1.SaveCalendarEventRequest{Name: local("Bandh", "बन्द"), From: today, To: dayStr(f.yesterday), Kind: restaurantv1.CalendarKind_CALENDAR_KIND_CLOSED}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_INVALID {
		t.Fatalf("to before from: %v", err)
	}
	if _, err := f.svc.SaveCalendarEvent(f.owner, &restaurantv1.SaveCalendarEventRequest{Id: uuid.NewString(), Name: local("Gone", ""), From: today, To: today, Kind: restaurantv1.CalendarKind_CALENDAR_KIND_OTHER}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND {
		t.Fatalf("unknown id: %v", err)
	}
	bandh, err := f.svc.SaveCalendarEvent(f.owner, &restaurantv1.SaveCalendarEventRequest{Name: local("Bandh", "बन्द"), From: today, To: today, Kind: restaurantv1.CalendarKind_CALENDAR_KIND_CLOSED})
	if err != nil || bandh.BuiltIn || bandh.CreatedByUserId != f.ownerID.String() || bandh.Key != "" {
		t.Fatalf("bandh = %+v, %v", bandh, err)
	}
	if err := f.svc.RemoveCalendarEvent(f.owner, bandh.Id); err != nil {
		t.Fatal(err)
	}
	after, _ := f.svc.ListCalendarEvents(f.owner, &restaurantv1.ListCalendarEventsRequest{Range: f.rng(f.today, f.today)})
	if slices.ContainsFunc(after.Events, func(e *restaurantv1.CalendarEvent) bool { return e.Id == bandh.Id }) {
		t.Fatal("removed event still listed")
	}
	rows, err := f.pool.Query(f.ctx, `SELECT action, after->>'detail' FROM audit_log WHERE entity = 'calendar_event' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for rows.Next() {
		var a, d string
		_ = rows.Scan(&a, &d)
		actions = append(actions, a)
	}
	if !slices.Equal(actions, []string{"calendar.save", "calendar.save", "calendar.remove"}) {
		t.Fatalf("audit = %v", actions)
	}
}

func TestCustomersIdentifiedShareRepeatRateTopForOwnerOnly(t *testing.T) {
	f := newFixture(t)
	ram := f.id(`INSERT INTO customers (name, phone, credit_limit_paisa) VALUES ('Ram Shrestha', '9841234567', 10000000) RETURNING id`)
	mina := f.id(`INSERT INTO customers (name, phone, credit_limit_paisa) VALUES ('Mina', '9851000000', 10000000) RETURNING id`)
	sale := func(d time.Time, customer uuid.UUID) int64 {
		o := f.order(order{typ: "DINE_IN", table: f.t1, waiter: f.gitaID, guests: 2, created: f.at(d, 0.3), status: "CLOSED"})
		it := f.item(item{order: o, menu: f.veg, name: "Veg Momo", price: 55_000, status: "SERVED", sent: f.at(d, 0.3), addedBy: f.gitaID})
		_, total := f.bill(bill{order: o, at: f.at(d, 0.4), items: []uuid.UUID{it}, by: f.hariID, customer: customer})
		return total
	}
	twoAgo := addDays(f.today, -2)
	sale(twoAgo, ram)
	sale(twoAgo, uuid.Nil) // walk-in
	sale(f.yesterday, ram)
	sale(f.yesterday, mina)
	r := f.rng(twoAgo, f.today)
	m, err := f.svc.Customers(f.sita, &restaurantv1.GetCustomersAnalyticsRequest{Range: r})
	if err != nil {
		t.Fatal(err)
	}
	if m.SalesPaisa != 220_000 || m.IdentifiedSalesPaisa != 165_000 || m.IdentifiedPct != 75 || m.Customers != 2 || m.RepeatRate != 50 || m.GetAvgDaysBetween() != 1 || m.Top != nil {
		t.Fatalf("manager customers = %+v", m)
	}
	o, err := f.svc.Customers(f.owner, &restaurantv1.GetCustomersAnalyticsRequest{Range: r})
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Top.Rows) != 2 || o.Top.Rows[0].Name != "Ram Shrestha" || o.Top.Rows[0].Phone != "98XXXXX567" || o.Top.Rows[0].Visits != 2 || o.Top.Rows[0].SpendPaisa != 110_000 ||
		o.Top.Rows[1].Name != "Mina" || o.Top.Rows[1].Phone != "98XXXXX000" {
		t.Fatalf("top = %+v", o.Top.Rows)
	}
	if _, err := f.svc.Customers(f.hari, &restaurantv1.GetCustomersAnalyticsRequest{Range: r}); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
		t.Fatalf("cashier: %v", err)
	}
}
