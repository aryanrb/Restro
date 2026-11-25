// Package analytics serves trends over days, BS months and fiscal years (owner and
// managers), the festival / closed-day calendar, and analytics settings. Past days
// come from disposable daily summaries (see summary.go); today is always live.
// The rules mirror the web's fake backend (web/src/lib/api/mock/mock-analytics-api.ts).
package analytics

import (
	"context"
	"math"
	"slices"
	"sort"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// MinStaffDays: people with fewer days in the range are "too little data".
const MinStaffDays = 5

// LunchEndsHour: orders opened before 16:00 count as lunch.
const LunchEndsHour = 16

// RangeTooLarge: more than 3 fiscal years in one call.
func RangeTooLarge() *connecterr.Error {
	return connecterr.New(connect.CodeInvalidArgument, restaurantv1.ErrorReason_ERROR_REASON_RANGE_TOO_LARGE, "at most 3 fiscal years per request")
}

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	log  *zap.Logger
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), log: log, now: time.Now}
}

// readCtx is everything one read needs besides the summaries.
type readCtx struct {
	actor    *auth.Actor
	now      time.Time
	today    time.Time
	first    time.Time // zero when there is no data at all
	events   []sqlc.CalendarEvent
	closed   map[string]bool
	settings sqlc.GetAnalyticsSettingsRow
}

func isOwner(a *auth.Actor) bool { return a.User.Role == auth.RoleOwner }

func (c *readCtx) canSeeStaff() bool {
	return isOwner(c.actor) || (c.actor.User.Role == auth.RoleManager && slices.Contains(c.settings.ManagersSeeStaff, c.actor.User.ID))
}

func (s *Service) readCtx(ctx context.Context) (*readCtx, error) {
	a, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	c := &readCtx{actor: a, now: now, today: ktmDay(now), closed: map[string]bool{}}
	firstUnix, err := s.q.AnalyticsFirstActivityUnix(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	if firstUnix > 0 {
		c.first = ktmDay(time.Unix(firstUnix, 0))
	}
	if c.events, err = s.q.ListCalendarEventsIn(ctx, sqlc.ListCalendarEventsInParams{
		FromDay: pgDate(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)), ToDay: pgDate(time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)),
	}); err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, e := range c.events {
		if e.Kind == "CLOSED" {
			for _, d := range daysIn(e.FromDate.Time, e.ToDate.Time) {
				c.closed[dayStr(d)] = true
			}
		}
	}
	if c.settings, err = s.q.GetAnalyticsSettings(ctx); err != nil {
		return nil, connecterr.Internal(err)
	}
	return c, nil
}

// openDays: up to today, from the first date with data, not marked closed.
func (c *readCtx) openDays(r dayRange) []time.Time {
	if c.first.IsZero() {
		return nil
	}
	lo, hi := maxDay(r.from, c.first), minDay(r.to, c.today)
	var out []time.Time
	for _, d := range daysIn(lo, hi) {
		if !c.closed[dayStr(d)] {
			out = append(out, d)
		}
	}
	return out
}

func (c *readCtx) upTo(r dayRange) int64 {
	if !r.from.After(c.today) && !r.to.Before(c.today) {
		return c.now.Unix()
	}
	return 0
}

func ratio(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return int64(math.Round(float64(a) / float64(b)))
}

// pct1 is a/b as a percentage with one decimal.
func pct1(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*1000) / 10
}

func pick(sums map[string]*daySummary, r dayRange) []*daySummary {
	var out []*daySummary
	for _, d := range daysIn(r.from, r.to) {
		if s, ok := sums[dayStr(d)]; ok {
			out = append(out, s)
		}
	}
	return out
}

func (c *readCtx) headline(r dayRange, list []*daySummary) *restaurantv1.AnalyticsHeadline {
	h := &restaurantv1.AnalyticsHeadline{}
	var sales, dineInSales, takeawaySales int64
	var bills, covers, dineIn, takeaway int64
	for _, d := range list {
		sales += d.Sales
		bills += d.Bills
		covers += d.Covers
		dineInSales += d.DineInSales
		takeawaySales += d.TakeawaySales
		dineIn += d.DineInOrders
		takeaway += d.TakeawayOrders
	}
	open := int64(len(c.openDays(r)))
	var closed int32
	if last := minDay(r.to, c.today); !r.from.After(last) {
		for _, d := range daysIn(r.from, last) {
			if c.closed[dayStr(d)] {
				closed++
			}
		}
	}
	h.SalesPaisa, h.Bills, h.Covers = sales, int32(bills), int32(covers)
	h.AverageBillPaisa = ratio(sales, bills)
	h.PerCoverPaisa = ratio(dineInSales, covers)
	h.DineInSalesPaisa, h.TakeawaySalesPaisa = dineInSales, takeawaySales
	h.DineInOrders, h.TakeawayOrders = int32(dineIn), int32(takeaway)
	h.OpenDays, h.ClosedDays = int32(open), closed
	h.DailyAveragePaisa = ratio(sales, open)
	return h
}

func grainOf(g restaurantv1.AnalyticsGrain) grain {
	switch g {
	case restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_WEEK:
		return grainWeek
	case restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_MONTH_BS:
		return grainMonthBS
	case restaurantv1.AnalyticsGrain_ANALYTICS_GRAIN_FY:
		return grainFY
	default:
		return grainDay
	}
}

func rangeOf(r *restaurantv1.AnalyticsRange) (dayRange, error) {
	return parseRange(r.GetFrom(), r.GetTo())
}

func toRange(r dayRange) *restaurantv1.AnalyticsRange {
	return &restaurantv1.AnalyticsRange{From: dayStr(r.from), To: dayStr(r.to)}
}

// comparedRange: compare_range overrides compare; nil without a comparison.
func comparedRange(r dayRange, compare restaurantv1.AnalyticsCompare, override *restaurantv1.AnalyticsRange) (*dayRange, error) {
	if override != nil && (override.GetFrom() != "" || override.GetTo() != "") {
		cr, err := rangeOf(override)
		if err != nil {
			return nil, err
		}
		return &cr, nil
	}
	switch compare {
	case restaurantv1.AnalyticsCompare_ANALYTICS_COMPARE_PREVIOUS:
		p := previousRange(r)
		return &p, nil
	case restaurantv1.AnalyticsCompare_ANALYTICS_COMPARE_LAST_YEAR:
		ly, err := lastYearRange(r)
		if err != nil {
			return nil, err
		}
		return &ly, nil
	}
	return nil, nil
}

func eventOverlaps(e sqlc.CalendarEvent, r dayRange) bool {
	return r.overlaps(e.FromDate.Time, e.ToDate.Time)
}

// ---------- Meta ----------

func (s *Service) Meta(ctx context.Context) (*restaurantv1.AnalyticsMeta, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	last, err := s.q.AnalyticsLastBuildUnix(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	m := &restaurantv1.AnalyticsMeta{
		// Built backend features. Loyalty and reservations don't exist yet; alerts
		// are not wired into analytics yet.
		Modules:         &restaurantv1.AnalyticsModules{Inventory: true, Credit: true},
		LastBuildAtUnix: last,
		CanSeeStaff:     c.canSeeStaff(),
		IsOwner:         isOwner(c.actor),
	}
	if !c.first.IsZero() {
		m.FirstDate = dayStr(c.first)
	}
	return m, nil
}

// ---------- Trend ----------

func (s *Service) trendPeriods(ctx context.Context, c *readCtx, r dayRange, g grain) ([]*restaurantv1.TrendPeriod, error) {
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	periods, err := periodsOf(r, g)
	if err != nil {
		return nil, err
	}
	out := make([]*restaurantv1.TrendPeriod, 0, len(periods))
	for _, p := range periods {
		pr := dayRange{p.from, p.to}
		tp := &restaurantv1.TrendPeriod{Key: p.key, From: dayStr(p.from), To: dayStr(p.to), Headline: c.headline(pr, pick(sums, pr)), EventIds: []string{}}
		for _, e := range c.events {
			if eventOverlaps(e, pr) {
				tp.EventIds = append(tp.EventIds, e.ID.String())
			}
		}
		tp.Closed = true
		for _, d := range daysIn(p.from, p.to) {
			if !c.closed[dayStr(d)] {
				tp.Closed = false
				break
			}
		}
		out = append(out, tp)
	}
	return out, nil
}

func (s *Service) Trend(ctx context.Context, req *restaurantv1.GetTrendRequest) (*restaurantv1.TrendResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	cmp, err := comparedRange(r, req.GetCompare(), req.GetCompareRange())
	if err != nil {
		return nil, err
	}
	g := grainOf(req.GetGrain())
	periods, err := s.trendPeriods(ctx, c, r, g)
	if err != nil {
		return nil, err
	}
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.TrendResponse{Range: toRange(r), Periods: periods, Total: c.headline(r, pick(sums, r)), UpToUnix: c.upTo(r)}
	var cmpPeriods []*restaurantv1.TrendPeriod
	if cmp != nil {
		out.ComparedRange = toRange(*cmp)
		if cmpPeriods, err = s.trendPeriods(ctx, c, *cmp, g); err != nil {
			return nil, err
		}
		cs, err := s.summaries(ctx, c, *cmp)
		if err != nil {
			return nil, err
		}
		out.ComparedTotal = c.headline(*cmp, pick(cs, *cmp))
	}
	for i := range periods {
		cp := &restaurantv1.ComparedTrendPeriod{}
		if i < len(cmpPeriods) {
			cp.Period = cmpPeriods[i]
		}
		out.Compared = append(out.Compared, cp)
	}
	for _, e := range c.events {
		if eventOverlaps(e, r) || (cmp != nil && eventOverlaps(e, *cmp)) {
			out.Events = append(out.Events, toEvent(e))
		}
	}
	return out, nil
}

// ---------- Busy hours ----------

type hoursGrid struct {
	total, average [7][24]int64
	occurrences    [7]int64
}

func (s *Service) hoursGrid(ctx context.Context, c *readCtx, r dayRange, metric restaurantv1.HoursMetric) (*hoursGrid, error) {
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	g := &hoursGrid{}
	for _, d := range c.openDays(r) {
		wd := weekday(d)
		g.occurrences[wd]++
		sum, ok := sums[dayStr(d)]
		if !ok {
			continue
		}
		for _, h := range sum.Hours {
			switch metric {
			case restaurantv1.HoursMetric_HOURS_METRIC_ORDERS:
				g.total[wd][h.Hour] += h.Orders
			case restaurantv1.HoursMetric_HOURS_METRIC_COVERS:
				g.total[wd][h.Hour] += h.Covers
			default:
				g.total[wd][h.Hour] += h.Sales
			}
		}
	}
	for wd := range 7 {
		for h := range 24 {
			g.average[wd][h] = ratio(g.total[wd][h], g.occurrences[wd])
		}
	}
	return g, nil
}

func (s *Service) Hours(ctx context.Context, req *restaurantv1.GetHoursRequest) (*restaurantv1.HoursResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	metric := req.GetMetric()
	if metric == restaurantv1.HoursMetric_HOURS_METRIC_UNSPECIFIED {
		metric = restaurantv1.HoursMetric_HOURS_METRIC_SALES
	}
	g, err := s.hoursGrid(ctx, c, r, metric)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.HoursResponse{Metric: metric, UpToUnix: c.upTo(r)}
	first, last := -1, -1
	for wd := range 7 {
		out.Rows = append(out.Rows, &restaurantv1.HoursRow{Weekday: int32(wd), Average: g.average[wd][:], Total: g.total[wd][:], Occurrences: int32(g.occurrences[wd])})
	}
	for h := range 24 {
		for wd := range 7 {
			if g.total[wd][h] > 0 {
				if first < 0 {
					first = h
				}
				last = h
				break
			}
		}
	}
	if first >= 0 {
		f, l := int32(first), int32(last)
		out.FirstHour, out.LastHour = &f, &l
	}
	return out, nil
}

// ---------- Menu ----------

type itemAgg struct {
	nameEn, nameNe, categoryID           string
	quantity, revenue, voided, cost, cqt int64
}

func itemTotals(list []*daySummary) map[string]*itemAgg {
	out := map[string]*itemAgg{}
	for _, d := range list {
		for _, i := range d.Items {
			a, ok := out[i.MenuItemID]
			if !ok {
				a = &itemAgg{nameEn: i.NameEn, nameNe: i.NameNe, categoryID: i.CategoryID}
				out[i.MenuItemID] = a
			}
			a.quantity += i.Quantity
			a.revenue += i.Revenue
			a.voided += i.VoidedQty
			a.cost += i.Cost
			a.cqt += i.CostQty
		}
	}
	return out
}

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

func (s *Service) menu(ctx context.Context, c *readCtx, r dayRange, cmp *dayRange, categoryID string) (*restaurantv1.MenuAnalyticsResponse, error) {
	inCat := func(id string) bool { return categoryID == "" || id == categoryID }
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	now := itemTotals(pick(sums, r))
	var before map[string]*itemAgg
	if cmp != nil {
		cs, err := s.summaries(ctx, c, *cmp)
		if err != nil {
			return nil, err
		}
		before = itemTotals(pick(cs, *cmp))
	}
	var totalRevenue int64
	for _, a := range now {
		if inCat(a.categoryID) {
			totalRevenue += a.revenue
		}
	}
	out := &restaurantv1.MenuAnalyticsResponse{UpToUnix: c.upTo(r)}
	for id, a := range now {
		if !inCat(a.categoryID) {
			continue
		}
		row := &restaurantv1.MenuRow{
			MenuItemId: id, Name: local(a.nameEn, a.nameNe), CategoryId: a.categoryID,
			Quantity: int32(a.quantity), RevenuePaisa: a.revenue, VoidedQty: int32(a.voided),
		}
		if totalRevenue > 0 {
			row.ShareBp = int32(math.Round(float64(a.revenue) / float64(totalRevenue) * 10_000))
		}
		if before != nil {
			var bq, br int64
			if b, ok := before[id]; ok {
				bq, br = b.quantity, b.revenue
			}
			q32 := int32(bq)
			row.ComparedQuantity, row.ComparedRevenuePaisa = &q32, &br
			if br > 0 {
				ch := int32(math.Round(float64(a.revenue-br) / float64(br) * 10_000))
				row.ChangeBp = &ch
			}
		}
		// Actual cost from the stock movements of the lines sold; scaled up to the
		// whole quantity when only some lines carried a cost (recipe added mid-range).
		if a.cqt > 0 && a.quantity > 0 {
			cost := int64(math.Round(float64(a.cost) * float64(a.quantity) / float64(a.cqt)))
			margin := a.revenue - cost
			row.CostPaisa, row.MarginPaisa = &cost, &margin
			out.Costs = true
		}
		out.Rows = append(out.Rows, row)
	}
	classify(out.Rows)
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if a.RevenuePaisa != b.RevenuePaisa {
			return a.RevenuePaisa > b.RevenuePaisa
		}
		if a.Quantity != b.Quantity {
			return a.Quantity > b.Quantity
		}
		return a.MenuItemId < b.MenuItemId
	})
	sold := map[string]bool{}
	for _, row := range out.Rows {
		if row.Quantity > 0 {
			sold[row.MenuItemId] = true
		}
	}
	items, err := s.q.AnalyticsMenuItems(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, m := range items {
		if m.Active && inCat(m.CategoryID.String()) && !sold[m.ID.String()] {
			out.NeverOrdered = append(out.NeverOrdered, &restaurantv1.NeverOrderedItem{MenuItemId: m.ID.String(), Name: local(m.NameEn, m.NameNe), CategoryId: m.CategoryID.String()})
		}
	}
	return out, nil
}

// classify places costed items within their category (Kasavana–Smith): popular
// when their share of the category's quantity is at least 70% of an equal share;
// high margin when their margin per unit is at least the category's weighted
// average. Items without a cost are not classified.
func classify(rows []*restaurantv1.MenuRow) {
	type cat struct{ n, qty, margin int64 }
	cats := map[string]*cat{}
	for _, r := range rows {
		if r.CostPaisa == nil || r.Quantity <= 0 {
			continue
		}
		c, ok := cats[r.CategoryId]
		if !ok {
			c = &cat{}
			cats[r.CategoryId] = c
		}
		c.n++
		c.qty += int64(r.Quantity)
		c.margin += *r.MarginPaisa
	}
	for _, r := range rows {
		c, ok := cats[r.CategoryId]
		if r.CostPaisa == nil || r.Quantity <= 0 || !ok || c.qty == 0 {
			continue
		}
		popular := float64(r.Quantity)/float64(c.qty) >= 0.7/float64(c.n)
		highMargin := float64(*r.MarginPaisa)/float64(r.Quantity) >= float64(c.margin)/float64(c.qty)
		switch {
		case popular && highMargin:
			r.MenuClass = restaurantv1.MenuClass_MENU_CLASS_STAR
		case popular:
			r.MenuClass = restaurantv1.MenuClass_MENU_CLASS_PLOWHORSE
		case highMargin:
			r.MenuClass = restaurantv1.MenuClass_MENU_CLASS_PUZZLE
		default:
			r.MenuClass = restaurantv1.MenuClass_MENU_CLASS_DOG
		}
	}
}

func (s *Service) Menu(ctx context.Context, req *restaurantv1.GetMenuAnalyticsRequest) (*restaurantv1.MenuAnalyticsResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	cmp, err := comparedRange(r, req.GetCompare(), req.GetCompareRange())
	if err != nil {
		return nil, err
	}
	return s.menu(ctx, c, r, cmp, req.GetCategoryId())
}

// ---------- Tables ----------

// dwellStats: nearest-rank percentiles of whole minutes.
func dwellStats(values []int64) *restaurantv1.DwellStats {
	if len(values) == 0 {
		return &restaurantv1.DwellStats{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	rank := func(q float64) *int32 {
		i := int(math.Ceil(q*float64(len(sorted)))) - 1
		i = max(0, min(len(sorted)-1, i))
		v := int32(sorted[i])
		return &v
	}
	return &restaurantv1.DwellStats{Count: int32(len(sorted)), Median: rank(0.5), P90: rank(0.9)}
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

func (s *Service) Tables(ctx context.Context, req *restaurantv1.GetTablesAnalyticsRequest) (*restaurantv1.TablesAnalyticsResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	list := pick(sums, r)
	openDays := int64(len(c.openDays(r)))
	type dw struct {
		tableID       string
		minutes       int64
		hour, weekday int
	}
	var dwell []dw
	use := map[string]*tableDay{}
	out := &restaurantv1.TablesAnalyticsResponse{OpenDays: int32(openDays), UpToUnix: c.upTo(r)}
	for _, d := range list {
		day, _ := parseDay(d.Date)
		for _, x := range d.Dwell {
			dwell = append(dwell, dw{x.TableID, x.Minutes, x.Hour, weekday(day)})
		}
		for _, t := range d.Tables {
			u, ok := use[t.TableID]
			if !ok {
				u = &tableDay{}
				use[t.TableID] = u
			}
			u.Orders += t.Orders
			u.Covers += t.Covers
		}
		out.DineInOrders += int32(d.DineInOrders)
		out.TakeawayOrders += int32(d.TakeawayOrders)
	}
	minutesWhere := func(keep func(dw) bool) []int64 {
		var v []int64
		for _, x := range dwell {
			if keep(x) {
				v = append(v, x.minutes)
			}
		}
		return v
	}
	tables, err := s.q.ListTables(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	areaOf := map[string]string{}
	areaOrders := map[string]int32{}
	for _, t := range tables {
		id := t.ID.String()
		areaOf[id] = t.AreaID.String()
		u := use[id]
		if u == nil {
			u = &tableDay{}
		}
		row := &restaurantv1.TableUse{
			TableId: id, Name: t.Name, AreaId: t.AreaID.String(), Seats: t.Seats, Orders: int32(u.Orders), Covers: int32(u.Covers),
			Dwell: dwellStats(minutesWhere(func(x dw) bool { return x.tableID == id })),
		}
		if openDays > 0 {
			row.Turnover = round1(float64(u.Orders) / float64(openDays))
			if t.Seats > 0 {
				row.CoversPerSeat = round1(float64(u.Covers) / float64(int64(t.Seats)*openDays))
			}
		}
		areaOrders[row.AreaId] += row.Orders
		out.Tables = append(out.Tables, row)
	}
	areas, err := s.q.ListAreas(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, a := range areas {
		id := a.ID.String()
		out.Areas = append(out.Areas, &restaurantv1.AreaUse{
			AreaId: id, Name: local(a.NameEn, a.NameNe), Orders: areaOrders[id],
			Dwell: dwellStats(minutesWhere(func(x dw) bool { return areaOf[x.tableID] == id })),
		})
	}
	for wd := range 7 {
		out.ByWeekday = append(out.ByWeekday, &restaurantv1.WeekdayDwell{Weekday: int32(wd), Dwell: dwellStats(minutesWhere(func(x dw) bool { return x.weekday == wd }))})
	}
	out.ByMeal = []*restaurantv1.MealDwell{
		{Meal: restaurantv1.Meal_MEAL_LUNCH, Dwell: dwellStats(minutesWhere(func(x dw) bool { return x.hour < LunchEndsHour }))},
		{Meal: restaurantv1.Meal_MEAL_DINNER, Dwell: dwellStats(minutesWhere(func(x dw) bool { return x.hour >= LunchEndsHour }))},
	}
	out.Overall = dwellStats(minutesWhere(func(dw) bool { return true }))
	if n := int64(out.DineInOrders + out.TakeawayOrders); n > 0 {
		out.TakeawayShareBp = int32(math.Round(float64(out.TakeawayOrders) / float64(n) * 10_000))
	}
	return out, nil
}

// ---------- Staff ----------

func (s *Service) Staff(ctx context.Context, req *restaurantv1.GetStaffAnalyticsRequest) (*restaurantv1.StaffAnalyticsResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	if !c.canSeeStaff() {
		return nil, connecterr.NotAllowed("the staff section is the owner's")
	}
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	type agg struct {
		staffDay
		days, waiterDays int64
	}
	byUser := map[string]*agg{}
	for _, d := range pick(sums, r) {
		for _, p := range d.Staff {
			a, ok := byUser[p.UserID]
			if !ok {
				a = &agg{staffDay: staffDay{UserID: p.UserID}}
				byUser[p.UserID] = a
			}
			// A day worked = at least one order, item or bill by that person.
			if p.OrdersAsWaiter > 0 || p.BillsIssued > 0 || p.ItemsAdded > 0 {
				a.days++
			}
			if p.OrdersAsWaiter > 0 {
				a.waiterDays++
			}
			a.OrdersAsWaiter += p.OrdersAsWaiter
			a.SalesAsWaiter += p.SalesAsWaiter
			a.Covers += p.Covers
			a.ItemsAdded += p.ItemsAdded
			a.ItemsVoided += p.ItemsVoided
			a.SalesBilled += p.SalesBilled
			a.DiscountGiven += p.DiscountGiven
			a.Cancellations += p.Cancellations
			a.Approvals += p.Approvals
		}
	}
	users, err := s.q.AnalyticsUsers(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	userOf := map[string]sqlc.AnalyticsUsersRow{}
	for _, u := range users {
		userOf[u.ID.String()] = u
	}
	out := &restaurantv1.StaffAnalyticsResponse{MinDays: MinStaffDays, UpToUnix: c.upTo(r)}
	var tot agg
	for _, a := range byUser {
		if a.days == 0 && a.Approvals == 0 && a.Cancellations == 0 {
			continue
		}
		u := userOf[a.UserID]
		name := u.Name
		if name == "" {
			name = "—"
		}
		out.Rows = append(out.Rows, &restaurantv1.StaffRate{
			UserId: a.UserID, Name: name, Role: roleEnum(u.Role), DaysWorked: int32(a.days),
			OrdersAsWaiter: int32(a.OrdersAsWaiter), SalesAsWaiterPaisa: a.SalesAsWaiter, Covers: int32(a.Covers),
			SalesPerDayPaisa: ratio(a.SalesAsWaiter, a.waiterDays), PerCoverPaisa: ratio(a.SalesAsWaiter, a.Covers),
			ItemsAdded: int32(a.ItemsAdded), ItemsVoided: int32(a.ItemsVoided), VoidsPer_100: pct1(a.ItemsVoided, a.ItemsAdded),
			SalesBilledPaisa: a.SalesBilled, DiscountGivenPaisa: a.DiscountGiven, DiscountPct: pct1(a.DiscountGiven, a.SalesBilled),
			Cancellations: int32(a.Cancellations), Approvals: int32(a.Approvals), TooLittleData: a.days < MinStaffDays,
		})
		tot.SalesAsWaiter += a.SalesAsWaiter
		tot.waiterDays += a.waiterDays
		tot.Covers += a.Covers
		tot.ItemsVoided += a.ItemsVoided
		tot.ItemsAdded += a.ItemsAdded
		tot.DiscountGiven += a.DiscountGiven
		tot.SalesBilled += a.SalesBilled
	}
	sort.SliceStable(out.Rows, func(i, j int) bool {
		if out.Rows[i].Name != out.Rows[j].Name {
			return out.Rows[i].Name < out.Rows[j].Name
		}
		return out.Rows[i].UserId < out.Rows[j].UserId
	})
	out.Average = &restaurantv1.StaffAverage{
		SalesPerDayPaisa: ratio(tot.SalesAsWaiter, tot.waiterDays), PerCoverPaisa: ratio(tot.SalesAsWaiter, tot.Covers),
		VoidsPer_100: pct1(tot.ItemsVoided, tot.ItemsAdded), DiscountPct: pct1(tot.DiscountGiven, tot.SalesBilled),
	}
	return out, nil
}

func roleEnum(r string) restaurantv1.Role {
	switch r {
	case auth.RoleOwner:
		return restaurantv1.Role_ROLE_OWNER
	case auth.RoleManager:
		return restaurantv1.Role_ROLE_MANAGER
	case auth.RoleCashier:
		return restaurantv1.Role_ROLE_CASHIER
	case auth.RoleWaiter:
		return restaurantv1.Role_ROLE_WAITER
	case auth.RoleKitchen:
		return restaurantv1.Role_ROLE_KITCHEN
	}
	return restaurantv1.Role_ROLE_UNSPECIFIED
}

// ---------- Money ----------

type moneyAgg struct {
	sales, onAccount, discounts, cashDifference int64
	byMethod                                    map[string]int64
	voidsBefore, voidsAfter, cancelled          countValue
}

func moneyOf(list []*daySummary) *moneyAgg {
	m := &moneyAgg{byMethod: zeroMethods()}
	for _, d := range list {
		for _, k := range moneyMethods {
			m.byMethod[k] += d.ByMethod[k]
		}
		m.sales += d.Sales
		m.onAccount += d.OnAccount
		m.discounts += d.Discounts
		m.cashDifference += d.CashDifference
		m.voidsBefore.add(d.VoidsBefore)
		m.voidsAfter.add(d.VoidsAfter)
		m.cancelled.add(d.Cancelled)
	}
	return m
}

var methodEnum = map[string]restaurantv1.PaymentMethod{
	"CASH": restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH, "QR_FONEPAY": restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY,
	"QR_OTHER": restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_OTHER, "CARD": restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD,
}

func methodTotals(m map[string]int64) []*restaurantv1.MethodTotal {
	out := make([]*restaurantv1.MethodTotal, 0, len(moneyMethods))
	for _, k := range moneyMethods {
		out = append(out, &restaurantv1.MethodTotal{Method: methodEnum[k], TotalPaisa: m[k]})
	}
	return out
}

func cvProto(c countValue) *restaurantv1.AnalyticsCountValue {
	return &restaurantv1.AnalyticsCountValue{Count: int32(c.Count), ValuePaisa: c.Value}
}

func (m *moneyAgg) proto(key string, r dayRange) *restaurantv1.MoneyPeriod {
	onAccount := m.onAccount
	return &restaurantv1.MoneyPeriod{
		Key: key, From: dayStr(r.from), To: dayStr(r.to), SalesPaisa: m.sales, ByMethod: methodTotals(m.byMethod),
		OnAccountPaisa: &onAccount, DiscountsPaisa: m.discounts, DiscountPct: pct1(m.discounts, m.sales),
		VoidsBefore: cvProto(m.voidsBefore), VoidsAfter: cvProto(m.voidsAfter), Cancelled: cvProto(m.cancelled),
		CashDifferencePaisa: m.cashDifference,
	}
}

func (s *Service) Money(ctx context.Context, req *restaurantv1.GetMoneyAnalyticsRequest) (*restaurantv1.MoneyAnalyticsResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	periods, err := periodsOf(r, grainOf(req.GetGrain()))
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.MoneyAnalyticsResponse{UpToUnix: c.upTo(r)}
	for _, p := range periods {
		pr := dayRange{p.from, p.to}
		out.Periods = append(out.Periods, moneyOf(pick(sums, pr)).proto(p.key, pr))
	}
	all := pick(sums, r)
	out.Total = moneyOf(all).proto("total", r)
	reasons := map[string]countValue{}
	for _, d := range all {
		for k, v := range d.DiscountReasons {
			cv := reasons[k]
			cv.add(v)
			reasons[k] = cv
		}
	}
	for k, v := range reasons {
		out.DiscountReasons = append(out.DiscountReasons, &restaurantv1.DiscountReasonTotal{Reason: k, Count: int32(v.Count), ValuePaisa: v.Value})
	}
	sort.Slice(out.DiscountReasons, func(i, j int) bool {
		a, b := out.DiscountReasons[i], out.DiscountReasons[j]
		if a.ValuePaisa != b.ValuePaisa {
			return a.ValuePaisa > b.ValuePaisa
		}
		return a.Reason < b.Reason
	})
	return out, nil
}

// ---------- Highlights ----------

// Highlights: up to 5 sentences picked by fixed rules and thresholds.
func (s *Service) Highlights(ctx context.Context, req *restaurantv1.GetHighlightsRequest) (*restaurantv1.HighlightsResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	cmp, err := comparedRange(r, req.GetCompare(), req.GetCompareRange())
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.HighlightsResponse{}
	if !c.settings.HighlightRules {
		return out, nil
	}
	add := func(h *restaurantv1.AnalyticsHighlight) { out.Highlights = append(out.Highlights, h) }

	// 1. Busiest slot by average sales.
	g, err := s.hoursGrid(ctx, c, r, restaurantv1.HoursMetric_HOURS_METRIC_SALES)
	if err != nil {
		return nil, err
	}
	var best *restaurantv1.AnalyticsHighlight
	for wd := range 7 {
		for h := range 24 {
			if v := g.average[wd][h]; v > 0 && (best == nil || v > best.ValuePaisa) {
				best = &restaurantv1.AnalyticsHighlight{Kind: restaurantv1.HighlightKind_HIGHLIGHT_KIND_BUSIEST_SLOT, Weekday: int32(wd), Hour: int32(h), ValuePaisa: v}
			}
		}
	}
	if best != nil {
		add(best)
	}
	// 2. Items up or down more than 25% with at least 20 sold.
	if cmp != nil {
		m, err := s.menu(ctx, c, r, cmp, "")
		if err != nil {
			return nil, err
		}
		var moved []*restaurantv1.MenuRow
		for _, row := range m.Rows {
			if row.ChangeBp != nil && abs32(*row.ChangeBp) > 2500 && max(row.Quantity, row.GetComparedQuantity()) >= 20 {
				moved = append(moved, row)
			}
		}
		sort.SliceStable(moved, func(i, j int) bool { return abs32(*moved[i].ChangeBp) > abs32(*moved[j].ChangeBp) })
		for _, row := range moved[:min(2, len(moved))] {
			kind := restaurantv1.HighlightKind_HIGHLIGHT_KIND_ITEM_DOWN
			if *row.ChangeBp > 0 {
				kind = restaurantv1.HighlightKind_HIGHLIGHT_KIND_ITEM_UP
			}
			add(&restaurantv1.AnalyticsHighlight{Kind: kind, Name: row.Name, ChangePct: int32(math.Round(float64(abs32(*row.ChangeBp)) / 100)), Quantity: row.Quantity})
		}
	}
	// 3. Discounts / voids above 1.5× their 3-month average.
	base := dayRange{addDays(r.from, -90), addDays(r.from, -1)}
	sums, err := s.summaries(ctx, c, r)
	if err != nil {
		return nil, err
	}
	baseSums, err := s.summaries(ctx, c, base)
	if err != nil {
		return nil, err
	}
	nowM, baseM := moneyOf(pick(sums, r)), moneyOf(pick(baseSums, base))
	if bp, np := pct1(baseM.discounts, baseM.sales), pct1(nowM.discounts, nowM.sales); bp > 0 && np >= 1.5*bp {
		add(&restaurantv1.AnalyticsHighlight{Kind: restaurantv1.HighlightKind_HIGHLIGHT_KIND_DISCOUNTS_HIGH, Pct: np, AveragePct: bp})
	}
	voidPct := func(m *moneyAgg) float64 { return pct1(m.voidsBefore.Value+m.voidsAfter.Value, m.sales) }
	if bp, np := voidPct(baseM), voidPct(nowM); bp > 0 && np >= 1.5*bp {
		add(&restaurantv1.AnalyticsHighlight{Kind: restaurantv1.HighlightKind_HIGHLIGHT_KIND_VOIDS_HIGH, Pct: np, AveragePct: bp})
	}
	if cmp != nil {
		// 4. Payment mix shift above 10 points.
		cs, err := s.summaries(ctx, c, *cmp)
		if err != nil {
			return nil, err
		}
		cmpM := moneyOf(pick(cs, *cmp))
		if nowM.sales > 0 && cmpM.sales > 0 {
			var top *restaurantv1.AnalyticsHighlight
			for _, k := range moneyMethods {
				points := int32(math.Round((float64(nowM.byMethod[k])/float64(nowM.sales) - float64(cmpM.byMethod[k])/float64(cmpM.sales)) * 100))
				if top == nil || abs32(points) > abs32(top.Points) {
					top = &restaurantv1.AnalyticsHighlight{Kind: restaurantv1.HighlightKind_HIGHLIGHT_KIND_PAYMENT_SHIFT, Method: methodEnum[k], Points: points}
				}
			}
			if abs32(top.Points) > 10 {
				add(top)
			}
		}
		// 5. A festival in only one of the two periods.
		festivals := func(x dayRange) []sqlc.CalendarEvent {
			var out []sqlc.CalendarEvent
			for _, e := range c.events {
				if e.Kind == "FESTIVAL" && eventOverlaps(e, x) {
					out = append(out, e)
				}
			}
			return out
		}
		same := func(a, b sqlc.CalendarEvent) bool {
			return (a.Key.Valid && a.Key.String != "" && a.Key.String == b.Key.String) || a.NameEn == b.NameEn
		}
		here, there := festivals(r), festivals(*cmp)
		oneSide := func(list, other []sqlc.CalendarEvent, inCompared bool) {
			for _, e := range list {
				if !slices.ContainsFunc(other, func(x sqlc.CalendarEvent) bool { return same(x, e) }) {
					add(&restaurantv1.AnalyticsHighlight{Kind: restaurantv1.HighlightKind_HIGHLIGHT_KIND_FESTIVAL_ONE_SIDE, Name: local(e.NameEn, e.NameNe), InCompared: inCompared})
				}
			}
		}
		oneSide(here, there, false)
		oneSide(there, here, true)
	}
	if len(out.Highlights) > 5 {
		out.Highlights = out.Highlights[:5]
	}
	return out, nil
}

func abs32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}

// ---------- Customers ----------

// maskPhone keeps the first 2 and last 3 digits ("98XXXXX567"), as the web does.
func maskPhone(p string) string {
	if len(p) < 5 {
		return p
	}
	x := make([]byte, len(p)-5)
	for i := range x {
		x[i] = 'X'
	}
	return p[:2] + string(x) + p[len(p)-3:]
}

// Customers: identified customers only (credit accounts on the bill), read live
// from the raw bills (small; no summary needed).
func (s *Service) Customers(ctx context.Context, req *restaurantv1.GetCustomersAnalyticsRequest) (*restaurantv1.CustomersAnalyticsResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	bills, err := s.q.AnalyticsIdentifiedBills(ctx, sqlc.AnalyticsIdentifiedBillsParams{FromTs: startNPT(r.from), ToTs: startNPT(addDays(r.to, 1))})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	type cust struct {
		name, phone string
		days        map[string]bool
		spend       int64
	}
	byCustomer := map[uuid.UUID]*cust{}
	out := &restaurantv1.CustomersAnalyticsResponse{}
	for _, b := range bills {
		out.SalesPaisa += b.TotalPaisa
		if !b.CustomerID.Valid {
			continue
		}
		out.IdentifiedSalesPaisa += b.TotalPaisa
		id := uuid.UUID(b.CustomerID.Bytes)
		cu, ok := byCustomer[id]
		if !ok {
			cu = &cust{name: b.CustomerName, phone: b.CustomerPhone, days: map[string]bool{}}
			byCustomer[id] = cu
		}
		cu.days[dayStr(ktmDay(b.AdDatetime))] = true
		cu.spend += b.TotalPaisa
	}
	if out.SalesPaisa > 0 {
		out.IdentifiedPct = int32(math.Round(float64(out.IdentifiedSalesPaisa) * 100 / float64(out.SalesPaisa)))
	}
	out.Customers = int32(len(byCustomer))
	var gaps []int
	repeaters := 0
	for _, cu := range byCustomer {
		days := make([]string, 0, len(cu.days))
		for d := range cu.days {
			days = append(days, d)
		}
		sort.Strings(days)
		if len(days) >= 2 {
			repeaters++
		}
		for k := 1; k < len(days); k++ {
			a, _ := parseDay(days[k-1])
			b, _ := parseDay(days[k])
			gaps = append(gaps, countDays(a, b)-1)
		}
	}
	if len(byCustomer) > 0 {
		out.RepeatRate = int32(math.Round(float64(repeaters) * 100 / float64(len(byCustomer))))
	}
	if len(gaps) > 0 {
		sum := 0
		for _, g := range gaps {
			sum += g
		}
		avg := math.Round(float64(sum)/float64(len(gaps))*10) / 10
		out.AvgDaysBetween = &avg
	}
	if isOwner(c.actor) {
		top := &restaurantv1.TopCustomers{}
		for _, cu := range byCustomer {
			top.Rows = append(top.Rows, &restaurantv1.TopCustomer{Name: cu.name, Phone: maskPhone(cu.phone), Visits: int32(len(cu.days)), SpendPaisa: cu.spend})
		}
		sort.Slice(top.Rows, func(i, j int) bool {
			if top.Rows[i].SpendPaisa != top.Rows[j].SpendPaisa {
				return top.Rows[i].SpendPaisa > top.Rows[j].SpendPaisa
			}
			return top.Rows[i].Name < top.Rows[j].Name
		})
		top.Rows = top.Rows[:min(10, len(top.Rows))]
		out.Top = top
	}
	return out, nil
}

// ---------- Rebuild ----------

// Rebuild (owner) throws away and rebuilds the stored summaries of past days in
// the range (from the first day with data); today is always live.
func (s *Service) Rebuild(ctx context.Context, req *restaurantv1.RebuildAnalyticsRequest) (*restaurantv1.RebuildAnalyticsResponse, error) {
	c, err := s.readCtx(ctx)
	if err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	if !isOwner(c.actor) {
		return nil, connecterr.NotAllowed("owner only")
	}
	out := &restaurantv1.RebuildAnalyticsResponse{}
	if c.first.IsZero() {
		return out, nil
	}
	lo, hi := maxDay(r.from, c.first), minDay(r.to, s.lastStorable(c))
	if lo.After(hi) {
		return out, nil
	}
	if err := s.q.DeleteAnalyticsDaysIn(ctx, sqlc.DeleteAnalyticsDaysInParams{FromDay: pgDate(lo), ToDay: pgDate(hi)}); err != nil {
		return nil, connecterr.Internal(err)
	}
	days := daysIn(lo, hi)
	if err := s.rebuildDays(ctx, days, nil); err != nil {
		return nil, err
	}
	out.Days = int32(len(days))
	return out, nil
}
