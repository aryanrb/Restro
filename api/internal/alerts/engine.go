// Package alerts turns audit events into suspicious-activity alerts (spec:
// suspicious-activity-alerts-design). The rule registry runs inside the
// transaction that writes each audit_log row (audit/hook), so an event and its
// alert commit together; a failing rule is rolled back to a savepoint, logged
// and skipped — alerts never fail the sale, void or close.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/audit/hook"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Realtime events.
const (
	EventAlertCreated = "alert.created"
	EventAlertUpdated = "alert.updated"
)

// Install registers the rules as the audit hook. restore unregisters them.
func Install(log *zap.Logger) (restore func()) {
	if log == nil {
		log = zap.NewNop()
	}
	e := &engine{log: log}
	return hook.Set(e.onAudit)
}

type engine struct{ log *zap.Logger }

// triggered reports whether any rule (or the day summary) runs on this action.
func triggered(action string) bool {
	if action == "shift.close" {
		return true
	}
	for _, d := range registry {
		if slices.Contains(d.triggers, action) {
			return true
		}
	}
	return false
}

func (e *engine) onAudit(ctx context.Context, q *sqlc.Queries, row sqlc.AuditLog) {
	if !triggered(row.Action) {
		return
	}
	g := guard{q: q}
	var cfg Config
	if err := g.run(ctx, func() (err error) { cfg, err = loadConfig(ctx, q); return }); err != nil {
		e.log.Error("alert config", zap.Error(err))
		return
	}
	at := row.CreatedAt
	day := businessDate(at, cfg.OpeningHours)
	var after map[string]any
	if len(row.After) > 0 {
		_ = json.Unmarshal(row.After, &after)
	}
	for _, d := range registry {
		if !slices.Contains(d.triggers, row.Action) {
			continue
		}
		setting := cfg.rule(d.rule)
		if setting == nil || !setting.Enabled {
			continue
		}
		c := &ruleCtx{ctx: ctx, q: q, row: row, after: after, t: setting.Thresholds, cfg: cfg, day: day, at: at}
		err := g.run(ctx, func() error {
			firings, err := d.eval(c)
			if err != nil {
				return err
			}
			for _, f := range firings {
				if err := record(c, d.rule, setting.Severity, f); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			e.log.Error("alert rule failed", zap.String("rule", d.rule), zap.String("action", row.Action), zap.Error(err))
		}
	}
	if row.Action == "shift.close" {
		if err := g.run(ctx, func() error { return queueSummary(ctx, q, cfg, day, at) }); err != nil {
			e.log.Error("day summary push", zap.Error(err))
		}
	}
}

// guard runs fn inside a savepoint so a failing rule rolls back only its own
// writes and leaves the caller's transaction usable. Without a transaction (an
// audit written on the pool) statements autocommit and no savepoint is needed.
type guard struct {
	q    *sqlc.Queries
	noTx bool
}

func (g *guard) run(ctx context.Context, fn func() error) error {
	if !g.noTx {
		if err := g.q.AlertSavepoint(ctx); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "25P01" { // no_active_sql_transaction
				g.noTx = true
			} else {
				return err
			}
		}
	}
	err := fn()
	if g.noTx {
		return err
	}
	if err != nil {
		if rerr := g.q.AlertRollbackSavepoint(ctx); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return g.q.AlertReleaseSavepoint(ctx)
}

// firing is one rule hit. It is grouped with the open alert of the same rule +
// subject + group key.
type firing struct {
	subject   uuid.UUID   // the person the alert is about; Nil = none
	approvers []uuid.UUID // managers who approved
	involved  []uuid.UUID // others it is about (hidden from them as a manager recipient)
	text      string      // bill number, item, staff name...
	num       map[string]int64
	str       map[string]string
	amount    *int64 // for the push (paisa); never a name
	shiftID   uuid.UUID
	invoiceID uuid.UUID
	groupKey  string // default: the subject's open shift, else the business day
	dedupKey  string // at most one alert ever per rule + dedupKey
}

func amountOf(v int64) *int64 { return &v }

func optUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil} }

func dateOf(day string) pgtype.Date { return pgtype.Date{Time: midnightUTC(day), Valid: true} }

func midnightUTC(day string) time.Time {
	t, _ := time.Parse(time.DateOnly, day)
	return t
}

func record(c *ruleCtx, rule, severity string, f firing) error {
	ctx, q := c.ctx, c.q
	if f.dedupKey != "" {
		exists, err := q.AlertDedupExists(ctx, sqlc.AlertDedupExistsParams{Rule: rule, DedupKey: f.dedupKey})
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
	}
	if f.groupKey == "" {
		f.groupKey = "day:" + c.day
		if f.subject != uuid.Nil {
			sh, err := q.AlertOpenShiftOf(ctx, f.subject)
			if err == nil {
				f.groupKey = "shift:" + sh.ID.String()
			}
		}
	}
	subjectKey := "-"
	if f.subject != uuid.Nil {
		subjectKey = f.subject.String()
	}
	approvers := uniqueIDs(f.approvers)
	involved := uniqueIDs(append(append([]uuid.UUID{f.subject}, approvers...), f.involved...))
	num, _ := json.Marshal(orEmpty(f.num))
	str, _ := json.Marshal(orEmptyS(f.str))
	p := sqlc.UpsertAlertParams{
		Rule: rule, Severity: severity, SubjectUserID: optUUID(f.subject), ApproverIds: approvers, InvolvedIds: involved,
		Subject: f.text, NumParams: num, TextParams: str, BusinessDate: dateOf(c.day), At: c.at,
		ShiftID: optUUID(f.shiftID), InvoiceID: optUUID(f.invoiceID), AuditID: c.row.ID,
		GroupKey: subjectKey + "|" + f.groupKey,
	}
	if f.dedupKey != "" {
		p.DedupKey = pgtype.Text{String: f.dedupKey, Valid: true}
	}
	a, err := q.UpsertAlert(ctx, p)
	if err != nil {
		return err
	}
	if a.Count > 1 {
		queueEvent(ctx, realtime.Event{Type: EventAlertUpdated, ID: a.ID.String(), Version: a.Version})
		return nil
	}
	queueEvent(ctx, realtime.Event{Type: EventAlertCreated, ID: a.ID.String(), Version: a.Version})
	if severity == SeverityHigh {
		return queueAlertPush(ctx, q, c.cfg, a, f.amount)
	}
	return nil
}

func orEmpty(m map[string]int64) map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return m
}

func orEmptyS(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// queueAlertPush: only High alerts interrupt — not in quiet hours, at most
// PushCap a business day, then one "more waiting".
func queueAlertPush(ctx context.Context, q *sqlc.Queries, cfg Config, a sqlc.Alert, amount *int64) error {
	if inQuietHours(a.FirstAt, cfg.Quiet) {
		return nil
	}
	sent, err := q.CountDayAlertPushes(ctx, sqlc.CountDayAlertPushesParams{BusinessDate: a.BusinessDate, Kind: "ALERT"})
	if err != nil {
		return err
	}
	p := sqlc.InsertAlertPushParams{At: a.FirstAt, BusinessDate: a.BusinessDate, Kind: "MORE", ExcludeIds: []uuid.UUID{}}
	if int(sent) < cfg.PushCap {
		p.Kind, p.Rule, p.AlertID, p.ExcludeIds = "ALERT", a.Rule, optUUID(a.ID), a.InvolvedIds
		if amount != nil {
			p.AmountPaisa = pgtype.Int8{Int64: *amount, Valid: true}
		}
	}
	return q.InsertAlertPush(ctx, p)
}

// queueSummary sends the day-summary push when the last open shift closes, once a
// business day, and only after closing time when opening hours are set (the
// summary timer also sends it at closing + 30 min).
func queueSummary(ctx context.Context, q *sqlc.Queries, cfg Config, day string, at time.Time) error {
	open, err := q.AlertCountOpenShifts(ctx)
	if err != nil || open > 0 {
		return err
	}
	if closeAt := businessDayClose(day, cfg.OpeningHours); closeAt != nil && at.Before(*closeAt) {
		return nil
	}
	return insertSummaryPush(ctx, q, cfg, day, at)
}

func insertSummaryPush(ctx context.Context, q *sqlc.Queries, cfg Config, day string, at time.Time) error {
	exists, err := q.AlertPushExists(ctx, sqlc.AlertPushExistsParams{BusinessDate: dateOf(day), Kind: "SUMMARY"})
	if err != nil || exists {
		return err
	}
	from, to := businessDayRange(day, cfg.OpeningHours)
	sales, err := q.AlertDaySales(ctx, sqlc.AlertDaySalesParams{FromTs: from, ToTs: to})
	if err != nil {
		return err
	}
	byRule, err := q.DayAlertsByRule(ctx, sqlc.DayAlertsByRuleParams{BusinessDate: dateOf(day)})
	if err != nil {
		return err
	}
	var openAlerts int32
	for _, r := range byRule {
		openAlerts += r.Open
	}
	return q.InsertAlertPush(ctx, sqlc.InsertAlertPushParams{
		At: at, BusinessDate: dateOf(day), Kind: "SUMMARY", AmountPaisa: pgtype.Int8{Int64: sales.Sales, Valid: true},
		Count: pgtype.Int4{Int32: openAlerts, Valid: true}, ExcludeIds: []uuid.UUID{},
	})
}

// --- realtime: alert events are broadcast once the request finished ---

type eventsKey struct{}

type collector struct {
	mu  sync.Mutex
	evs []realtime.Event
}

func queueEvent(ctx context.Context, ev realtime.Event) {
	c, _ := ctx.Value(eventsKey{}).(*collector)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, e := range c.evs {
		if e.Type == ev.Type && e.ID == ev.ID {
			c.evs[i] = ev
			return
		}
	}
	c.evs = append(c.evs, ev)
}

// WithEvents gives ctx an alert-event collector; take returns what was queued.
func WithEvents(ctx context.Context) (context.Context, func() []realtime.Event) {
	c := &collector{}
	return context.WithValue(ctx, eventsKey{}, c), func() []realtime.Event {
		c.mu.Lock()
		defer c.mu.Unlock()
		out := c.evs
		c.evs = nil
		return out
	}
}

// EventsInterceptor broadcasts the alert.created / alert.updated events queued
// while handling a request once the handler returned. On success its transaction
// committed; on failure they are sent too, because some audited writes commit on
// their own (a PIN lockout) — an event for a rolled-back alert only makes clients
// refetch.
func EventsInterceptor(hub *realtime.Hub) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, take := WithEvents(ctx)
			resp, err := next(ctx, req)
			if hub != nil {
				for _, ev := range take() {
					hub.Broadcast(ev)
				}
			}
			return resp, err
		}
	}
}

func fmtHHMM(at time.Time) string {
	m := ktmClock(at).minutes
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}
