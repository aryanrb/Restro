// Package live serves the owner's read-only live view: every table's state, open
// orders, the kitchen per station, the till and today's exceptions (owner and
// managers only). It writes nothing. The rules mirror the web's fake backend
// (web/src/lib/api/mock/mock-live-api.ts) and the KDS ticket grouping and
// thresholds (web/src/lib/kitchen/tickets.ts).
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/alerts"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/shift"
)

var nepal = time.FixedZone("NPT", 5*3600+45*60)

type Service struct {
	q      *sqlc.Queries
	shifts *shift.Service
	log    *zap.Logger
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, log *zap.Logger) *Service {
	return &Service{q: sqlc.New(pool), shifts: shift.NewService(pool, nil, log), log: log, now: time.Now}
}

func startOfDayNPT(t time.Time) time.Time {
	n := t.In(nepal)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, nepal)
}

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

// minutesSince: whole minutes, never negative.
func minutesSince(t, now time.Time) int32 {
	d := now.Sub(t)
	if d < 0 {
		return 0
	}
	return int32(d / time.Minute)
}

var moneyMethods = []struct {
	name string
	enum restaurantv1.PaymentMethod
}{
	{"CASH", restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH},
	{"QR_FONEPAY", restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY},
	{"QR_OTHER", restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_OTHER},
	{"CARD", restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD},
}

func methodTotals(by map[restaurantv1.PaymentMethod]int64) []*restaurantv1.MethodTotal {
	out := make([]*restaurantv1.MethodTotal, 0, len(moneyMethods))
	for _, m := range moneyMethods {
		out = append(out, &restaurantv1.MethodTotal{Method: m.enum, TotalPaisa: by[m.enum]})
	}
	return out
}

func orderTypeEnum(t string) restaurantv1.OrderType {
	if t == "TAKEAWAY" {
		return restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY
	}
	return restaurantv1.OrderType_ORDER_TYPE_DINE_IN
}

func itemStatusEnum(s string) restaurantv1.OrderItemStatus {
	switch s {
	case "DRAFT":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_DRAFT
	case "SENT":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SENT
	case "PREPARING":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_PREPARING
	case "READY":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_READY
	case "SERVED":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SERVED
	case "VOIDED":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_VOIDED
	}
	return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_UNSPECIFIED
}

type orderRow = sqlc.LiveOpenOrdersRow
type itemRow = sqlc.LiveItemsOfOrdersRow

func placeOf(o orderRow) string {
	if o.TableID.Valid && o.TableName != "" {
		return o.TableName
	}
	return o.Label
}

func waiting(status string) bool { return status == "SENT" || status == "PREPARING" }

// toLiveOrder: one open order as the live view shows it (snapshot and order card).
func toLiveOrder(o orderRow, items []itemRow, now time.Time, red int32) *restaurantv1.LiveOrder {
	lo := &restaurantv1.LiveOrder{
		Id: o.ID.String(), Type: orderTypeEnum(o.Type), Place: placeOf(o), WaiterName: o.WaiterName,
		GuestCount: o.GuestCount, CreatedAtUnix: o.CreatedAt.Unix(), ItemCounts: &restaurantv1.LiveItemCounts{},
	}
	if o.TableID.Valid {
		lo.TableId = uuid.UUID(o.TableID.Bytes).String()
	}
	states := make([]lineState, 0, len(items))
	var lastSent, oldestWaiting time.Time
	for _, i := range items {
		states = append(states, lineState{id: i.ID.String(), status: i.Status, billed: i.Billed})
		switch i.Status {
		case "SENT":
			lo.ItemCounts.Sent++
		case "PREPARING":
			lo.ItemCounts.Preparing++
		case "READY":
			lo.ItemCounts.Ready++
		case "SERVED":
			lo.ItemCounts.Served++
		case "VOIDED":
			lo.ItemCounts.Voided++
		}
		if i.SentAt.Valid && i.SentAt.Time.After(lastSent) {
			lastSent = i.SentAt.Time
		}
		if i.Status == "VOIDED" {
			continue
		}
		value := i.UnitPricePaisa * int64(i.Quantity)
		lo.RunningTotalPaisa += value
		if i.Billed {
			lo.BilledTotalPaisa += value
		}
		if waiting(i.Status) && i.SentAt.Valid && (oldestWaiting.IsZero() || i.SentAt.Time.Before(oldestWaiting)) {
			oldestWaiting = i.SentAt.Time
		}
	}
	if !lastSent.IsZero() {
		lo.LastSentAtUnix = lastSent.Unix()
	}
	if !oldestWaiting.IsZero() {
		m := minutesSince(oldestWaiting, now)
		lo.OldestWaitingMinutes = &m
		lo.Late = m >= red
	}
	// A guest's Ask for bill (guest service) sets bill_requested_at until Done or a bill.
	if o.BillRequestedAt.Valid {
		lo.BillRequestedAtUnix = o.BillRequestedAt.Time.Unix()
	}
	lo.State = orderState(states, o.BillRequestedAt.Valid)
	return lo
}

// ticket: one KDS ticket (order + station + send time), as the KDS groups them.
type ticket struct {
	orderID, place string
	sentAt         time.Time
	items          []itemRow
}

// stations: the KDS view per station. Only active tickets count (something still
// SENT or PREPARING); an all-void ticket matters to the cook, not to the owner.
func stations(list []sqlc.Station, orders map[uuid.UUID]orderRow, items []itemRow, now time.Time, yellow, red int32) []*restaurantv1.LiveStation {
	byStation := map[uuid.UUID][]*ticket{}
	index := map[string]*ticket{}
	counts := map[uuid.UUID]*restaurantv1.LiveStation{}
	for _, st := range list {
		counts[st.ID] = &restaurantv1.LiveStation{StationId: st.ID.String(), Name: local(st.NameEn, st.NameNe), LateItems: []*restaurantv1.LiveLateItem{}}
	}
	for _, i := range items {
		ls := counts[i.StationID]
		if ls != nil {
			switch i.Status {
			case "SENT":
				ls.ItemsSent++
			case "PREPARING":
				ls.ItemsPreparing++
			case "READY":
				ls.ItemsReady++
			}
		}
		if !i.SentAt.Valid {
			continue
		}
		key := fmt.Sprintf("%s|%s|%d", i.OrderID, i.StationID, i.SentAt.Time.UnixMicro())
		t, ok := index[key]
		if !ok {
			t = &ticket{orderID: i.OrderID.String(), place: placeOf(orders[i.OrderID]), sentAt: i.SentAt.Time}
			index[key] = t
			byStation[i.StationID] = append(byStation[i.StationID], t)
		}
		t.items = append(t.items, i)
	}
	out := make([]*restaurantv1.LiveStation, 0, len(list))
	for _, st := range list {
		ls := counts[st.ID]
		tickets := byStation[st.ID]
		sort.SliceStable(tickets, func(a, b int) bool { return tickets[a].sentAt.Before(tickets[b].sentAt) })
		for _, t := range tickets {
			active := false
			for _, i := range t.items {
				if waiting(i.Status) {
					active = true
				}
			}
			if !active {
				continue
			}
			ls.Tickets++
			age := now.Sub(t.sentAt)
			if age < 0 {
				age = 0
			}
			minutes := int32(age / time.Minute)
			if ls.OldestMinutes == nil || minutes > *ls.OldestMinutes {
				m := minutes
				ls.OldestMinutes = &m
			}
			switch {
			case age >= time.Duration(red)*time.Minute:
				ls.Late++
				for _, i := range t.items {
					if waiting(i.Status) {
						ls.LateItems = append(ls.LateItems, &restaurantv1.LiveLateItem{OrderId: t.orderID, Place: t.place, Name: local(i.NameEn, i.NameNe), Quantity: i.Quantity, Minutes: minutes})
					}
				}
			case age >= time.Duration(yellow)*time.Minute:
				ls.Slow++
			}
		}
		out = append(out, ls)
	}
	return out
}

func (s *Service) Snapshot(ctx context.Context) (*restaurantv1.LiveSnapshot, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	now := s.now()
	since := startOfDayNPT(now)
	settings, err := s.q.GetSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	yellow, red := settings.KdsYellowMinutes, settings.KdsRedMinutes
	orders, err := s.q.LiveOpenOrders(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	ids := make([]uuid.UUID, 0, len(orders))
	byID := map[uuid.UUID]orderRow{}
	for _, o := range orders {
		ids = append(ids, o.ID)
		byID[o.ID] = o
	}
	items, err := s.q.LiveItemsOfOrders(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	itemsOf := map[uuid.UUID][]itemRow{}
	for _, i := range items {
		itemsOf[i.OrderID] = append(itemsOf[i.OrderID], i)
	}

	snap := &restaurantv1.LiveSnapshot{AtUnix: now.Unix(), YellowMinutes: yellow, RedMinutes: red}
	byTable := map[string][]*restaurantv1.LiveOrder{}
	for _, o := range orders {
		lo := toLiveOrder(o, itemsOf[o.ID], now, red)
		snap.MoneyOnTablesPaisa += lo.RunningTotalPaisa - lo.BilledTotalPaisa
		if o.Type == "TAKEAWAY" || lo.TableId == "" {
			snap.Takeaway = append(snap.Takeaway, lo)
		} else {
			byTable[lo.TableId] = append(byTable[lo.TableId], lo)
		}
	}

	tables, err := s.q.ListTables(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	reserved, err := s.reservedTables(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		if !t.Active {
			continue
		}
		mine := byTable[t.ID.String()]
		lt := &restaurantv1.LiveTable{
			TableId: t.ID.String(), AreaId: t.AreaID.String(), Name: t.Name, Seats: t.Seats, State: tableState(mine), Orders: mine,
		}
		if len(mine) == 0 {
			lt.Reservation = reserved[t.ID]
		}
		snap.Tables = append(snap.Tables, lt)
	}
	areas, err := s.q.ListAreas(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, a := range areas {
		snap.Areas = append(snap.Areas, &restaurantv1.LiveArea{Id: a.ID.String(), Name: local(a.NameEn, a.NameNe)})
	}
	stationList, err := s.q.ListStations(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	snap.Stations = stations(stationList, byID, items, now, yellow, red)
	out86, err := s.q.Live86Items(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, m := range out86 {
		snap.Items_86 = append(snap.Items_86, local(m.NameEn, m.NameNe))
	}

	if snap.Tills, err = s.tills(ctx); err != nil {
		return nil, err
	}
	if snap.Today, err = s.today(ctx, since); err != nil {
		return nil, err
	}
	credit, err := s.q.CreditGivenSince(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	snap.CreditTodayPaisa = &credit
	if snap.Exceptions, err = s.exceptions(ctx, since); err != nil {
		return nil, err
	}
	if err := s.alerts(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// tills: each open shift with its shift-report takings and expected cash.
func (s *Service) tills(ctx context.Context) ([]*restaurantv1.LiveTill, error) {
	open, err := s.q.ListOpenShifts(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	var out []*restaurantv1.LiveTill
	for _, sh := range open {
		report, err := s.shifts.GetShiftReport(ctx, sh.ID.String())
		if err != nil {
			return nil, err
		}
		by := map[restaurantv1.PaymentMethod]int64{}
		for _, t := range report.Totals {
			by[t.Method] += t.TotalPaisa
		}
		out = append(out, &restaurantv1.LiveTill{
			ShiftId: sh.ID.String(), UserName: report.GetShift().GetCashierName(), OpenedAtUnix: sh.OpenedAt.Unix(),
			OpeningFloatPaisa: sh.OpeningFloatPaisa, Takings: methodTotals(by), ExpectedCashPaisa: report.ExpectedCashPaisa,
		})
	}
	return out, nil
}

func paymentMethodEnum(m string) restaurantv1.PaymentMethod {
	for _, x := range moneyMethods {
		if x.name == m {
			return x.enum
		}
	}
	return restaurantv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED
}

// today: ISSUED bills since midnight (the dashboard's definition), money taken by
// method (on-account is credit, reported apart), and today's dine-in covers.
func (s *Service) today(ctx context.Context, since time.Time) (*restaurantv1.LiveToday, error) {
	sales, err := s.q.TodaySalesTotal(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	methods, err := s.q.TodaySalesByMethod(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	by := map[restaurantv1.PaymentMethod]int64{}
	for _, m := range methods {
		if e := paymentMethodEnum(m.Method); e != restaurantv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED {
			by[e] += m.Total
		}
	}
	covers, err := s.q.LiveCoversSince(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return &restaurantv1.LiveToday{SalesPaisa: sales.Total, Bills: sales.InvoiceCount, Covers: covers, ByMethod: methodTotals(by)}, nil
}

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func rupees(paisa int64) string {
	sign := ""
	if paisa < 0 {
		sign, paisa = "-", -paisa
	}
	return fmt.Sprintf("%s%d.%02d", sign, paisa/100, paisa%100)
}

// exceptionDetail: one readable line, e.g. "Rs 150.50 · wrong table".
func exceptionDetail(r sqlc.LiveExceptionsRow) string {
	var parts []string
	if r.Action == "invoice.issue" && r.DiscountPaisa > 0 {
		parts = append(parts, "discount Rs "+rupees(r.DiscountPaisa))
		if reason := strings.TrimSpace(r.DiscountReason); reason != "" {
			parts = append(parts, reason)
		}
	}
	var m map[string]any
	if len(r.After) > 0 && json.Unmarshal(r.After, &m) == nil {
		for _, k := range []struct{ key, label string }{{"amount", ""}, {"total", "total "}, {"counted_cash", "counted "}} {
			if v, ok := m[k.key].(float64); ok && r.Action != "invoice.issue" {
				parts = append(parts, k.label+"Rs "+rupees(int64(v)))
			}
		}
		if v, ok := m["print_count"].(float64); ok {
			parts = append(parts, fmt.Sprintf("print #%d", int64(v)))
		}
		if v, ok := m["detail"].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	if r.Reason.String != "" {
		parts = append(parts, r.Reason.String)
	}
	return strings.Join(parts, " · ")
}

func (s *Service) exceptions(ctx context.Context, since time.Time) ([]*restaurantv1.LiveException, error) {
	rows, err := s.q.LiveExceptions(ctx, since)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.LiveException, 0, len(rows))
	for _, r := range rows {
		out = append(out, &restaurantv1.LiveException{Entry: &restaurantv1.AuditEntry{
			Id: r.ID, ActorUserId: uuidStr(r.ActorUserID), Action: r.Action, Entity: r.Entity, EntityId: r.EntityID.String,
			Reason: r.Reason.String, ApproverUserId: uuidStr(r.ApproverUserID), CreatedAtUnix: r.CreatedAt.Unix(),
			ActorName: r.ActorName, ApproverName: r.ApproverName, Subject: r.Subject, DeviceLabel: r.DeviceLabel.String,
			Detail: exceptionDetail(r),
		}})
	}
	return out, nil
}

// alerts fills today.alerts_open (the bell's open count) and each exception's
// alert_id, for a caller who may read alerts; others keep them absent / "".
func (s *Service) alerts(ctx context.Context, snap *restaurantv1.LiveSnapshot) error {
	ok, hideFor, err := alerts.ReaderScope(ctx, s.q)
	if err != nil || !ok {
		return err
	}
	counts, err := s.q.AlertCounts(ctx, hideFor)
	if err != nil {
		return connecterr.Internal(err)
	}
	snap.Today.AlertsOpen = &counts.Open
	if len(snap.Exceptions) == 0 {
		return nil
	}
	ids := make([]int64, len(snap.Exceptions))
	for i, x := range snap.Exceptions {
		ids[i] = x.Entry.Id
	}
	rows, err := s.q.LiveExceptionAlerts(ctx, sqlc.LiveExceptionAlertsParams{AuditIds: ids, HideFor: hideFor})
	if err != nil {
		return connecterr.Internal(err)
	}
	alertOf := make(map[int64]string, len(rows))
	for _, r := range rows {
		alertOf[r.AuditID] = r.AlertID.String()
	}
	for _, x := range snap.Exceptions {
		x.AlertId = alertOf[x.Entry.Id]
	}
	return nil
}

// Order is the read-only order card of any order (open or not).
func (s *Service) Order(ctx context.Context, orderID string) (*restaurantv1.LiveOrderCard, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(orderID)
	if err != nil {
		return nil, connecterr.Invalid("order_id", "invalid id")
	}
	o, err := s.q.LiveOrderByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("order not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	settings, err := s.q.GetSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	items, err := s.q.LiveItemsOfOrders(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	itemIDs := make([]uuid.UUID, 0, len(items))
	for _, i := range items {
		itemIDs = append(itemIDs, i.ID)
	}
	mods, err := s.q.ListModifiersByItemIDs(ctx, itemIDs)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	modsOf := map[uuid.UUID][]*restaurantv1.LocalText{}
	for _, m := range mods {
		modsOf[m.OrderItemID] = append(modsOf[m.OrderItemID], local(m.NameEn, m.NameNe))
	}
	card := &restaurantv1.LiveOrderCard{Order: toLiveOrder(orderRow(o), items, s.now(), settings.KdsRedMinutes)}
	for _, i := range items {
		line := &restaurantv1.LiveOrderLine{
			Id: i.ID.String(), Name: local(i.NameEn, i.NameNe), Modifiers: modsOf[i.ID], Quantity: i.Quantity,
			UnitPricePaisa: i.UnitPricePaisa, Status: itemStatusEnum(i.Status), Note: i.Note, VoidReason: i.VoidReason,
			VoidedByName: i.VoidedByName, VoidApprovedByName: i.VoidApprovedByName,
		}
		if i.HasVariant {
			line.VariantName = local(i.VariantNameEn, i.VariantNameNe)
		}
		if i.SentAt.Valid {
			line.SentAtUnix = i.SentAt.Time.Unix()
		}
		if i.Billed {
			line.BillNumber = shift.DisplayNumber(i.BillMode, i.BillFiscalYear, i.BillNumber)
		}
		card.Lines = append(card.Lines, line)
	}
	return card, nil
}
