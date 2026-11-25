package alerts

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/reports"
	"github.com/oneaccord/restro/api/internal/shift"
)

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, log: log}
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

func (s *Service) internal(what string, err error) error {
	s.log.Error("alerts: "+what, zap.Error(err))
	return connecterr.Internal(err)
}

// reader is who may read alerts: the owner (hideFor nil) or a manager recipient
// (hideFor = them: never alerts they are involved in).
type reader struct {
	user    *sqlc.User
	hideFor pgtype.UUID
	cfg     Config
}

func (r reader) owner() bool { return r.user.Role == auth.RoleOwner }

func (r reader) canSee(a sqlc.Alert) bool {
	return r.owner() || !slices.Contains(a.InvolvedIds, r.user.ID)
}

func (s *Service) requireReader(ctx context.Context) (reader, error) {
	actor, err := auth.RequireActor(ctx)
	if err != nil {
		return reader{}, err
	}
	cfg, err := loadConfig(ctx, s.q)
	if err != nil {
		return reader{}, s.internal("config", err)
	}
	r := reader{user: actor.User, cfg: cfg}
	switch {
	case actor.User.Role == auth.RoleOwner:
		return r, nil
	case actor.User.Role == auth.RoleManager && cfg.isRecipient(actor.User.ID):
		r.hideFor = dbx.UUID(actor.User.ID)
		return r, nil
	}
	return reader{}, connecterr.NotAllowed("only the owner and the managers they choose see alerts")
}

// --- alerts ---

var ruleEnum = func() map[string]restaurantv1.AlertRule {
	m := map[string]restaurantv1.AlertRule{}
	for _, meta := range ruleMetas {
		m[meta.rule] = restaurantv1.AlertRule(restaurantv1.AlertRule_value["ALERT_RULE_"+meta.rule])
	}
	return m
}()

func ruleName(r restaurantv1.AlertRule) string {
	return strings.TrimPrefix(r.String(), "ALERT_RULE_")
}

func severityEnum(s string) restaurantv1.AlertSeverity {
	if s == SeverityHigh {
		return restaurantv1.AlertSeverity_ALERT_SEVERITY_HIGH
	}
	return restaurantv1.AlertSeverity_ALERT_SEVERITY_NORMAL
}

func severityName(s restaurantv1.AlertSeverity) string {
	switch s {
	case restaurantv1.AlertSeverity_ALERT_SEVERITY_HIGH:
		return SeverityHigh
	case restaurantv1.AlertSeverity_ALERT_SEVERITY_NORMAL:
		return SeverityNormal
	}
	return ""
}

func statusEnum(s string) restaurantv1.AlertStatus {
	return restaurantv1.AlertStatus(restaurantv1.AlertStatus_value["ALERT_STATUS_"+s])
}

func statusName(s restaurantv1.AlertStatus) string {
	if s == restaurantv1.AlertStatus_ALERT_STATUS_UNSPECIFIED {
		return ""
	}
	return strings.TrimPrefix(s.String(), "ALERT_STATUS_")
}

func idStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// toProtos converts alerts, filling names from one users lookup.
func (s *Service) toProtos(ctx context.Context, rows []sqlc.Alert) ([]*restaurantv1.Alert, error) {
	var ids []uuid.UUID
	for _, a := range rows {
		ids = append(ids, pgID(a.SubjectUserID), pgID(a.ResolvedByUserID))
		ids = append(ids, a.ApproverIds...)
	}
	users, err := s.q.AlertUserNames(ctx, uniqueIDs(ids))
	if err != nil {
		return nil, s.internal("user names", err)
	}
	names := map[uuid.UUID]string{}
	for _, u := range users {
		names[u.ID] = u.Name
	}
	out := make([]*restaurantv1.Alert, 0, len(rows))
	for _, a := range rows {
		p := &restaurantv1.Alert{
			Id: a.ID.String(), Rule: ruleEnum[a.Rule], Severity: severityEnum(a.Severity), Status: statusEnum(a.Status),
			SubjectUserId: idStr(a.SubjectUserID), SubjectName: names[pgID(a.SubjectUserID)], Subject: a.Subject,
			Count: a.Count, BusinessDate: a.BusinessDate.Time.Format(time.DateOnly),
			FirstAtUnix: a.FirstAt.Unix(), LastAtUnix: a.LastAt.Unix(), ShiftId: idStr(a.ShiftID), InvoiceId: idStr(a.InvoiceID),
			AuditEntryIds: a.AuditIds, ResolvedById: idStr(a.ResolvedByUserID), ResolvedByName: names[pgID(a.ResolvedByUserID)],
			Note: a.Note, Version: a.Version, NumParams: map[string]int64{}, TextParams: map[string]string{},
		}
		if a.ResolvedAt.Valid {
			p.ResolvedAtUnix = a.ResolvedAt.Time.Unix()
		}
		for _, id := range a.ApproverIds {
			p.ApproverIds = append(p.ApproverIds, id.String())
			p.ApproverNames = append(p.ApproverNames, names[id])
		}
		_ = jsonUnmarshal(a.NumParams, &p.NumParams)
		_ = jsonUnmarshal(a.TextParams, &p.TextParams)
		out = append(out, p)
	}
	return out, nil
}

func parseDate(field, v string) (pgtype.Date, error) {
	if v == "" {
		return pgtype.Date{}, nil
	}
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return pgtype.Date{}, connecterr.Invalid(field, "date must be YYYY-MM-DD")
	}
	return pgtype.Date{Time: t, Valid: true}, nil
}

func parseOptID(field, v string) (pgtype.UUID, error) {
	if v == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return pgtype.UUID{}, connecterr.Invalid(field, "invalid id")
	}
	return dbx.UUID(id), nil
}

func (s *Service) ListAlerts(ctx context.Context, req *restaurantv1.ListAlertsRequest) (*restaurantv1.ListAlertsResponse, error) {
	r, err := s.requireReader(ctx)
	if err != nil {
		return nil, err
	}
	p := sqlc.ListAlertsParams{HideFor: r.hideFor, Status: statusName(req.GetStatus())}
	if req.GetRule() != restaurantv1.AlertRule_ALERT_RULE_UNSPECIFIED {
		p.Rule = ruleName(req.GetRule())
	}
	if p.SubjectUserID, err = parseOptID("user_id", req.GetUserId()); err != nil {
		return nil, err
	}
	if p.FromDate, err = parseDate("from_date", req.GetFromDate()); err != nil {
		return nil, err
	}
	if p.ToDate, err = parseDate("to_date", req.GetToDate()); err != nil {
		return nil, err
	}
	rows, err := s.q.ListAlerts(ctx, p)
	if err != nil {
		return nil, s.internal("list", err)
	}
	alerts, err := s.toProtos(ctx, rows)
	if err != nil {
		return nil, err
	}
	return &restaurantv1.ListAlertsResponse{Alerts: alerts}, nil
}

func (s *Service) GetAlert(ctx context.Context, req *restaurantv1.GetAlertRequest) (*restaurantv1.GetAlertResponse, error) {
	r, err := s.requireReader(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, connecterr.Invalid("id", "invalid id")
	}
	a, err := s.q.GetAlert(ctx, id)
	if noRows(err) {
		return nil, connecterr.NotFound("alert not found")
	}
	if err != nil {
		return nil, s.internal("get", err)
	}
	if !r.canSee(a) {
		return nil, connecterr.NotAllowed("this alert is about you")
	}
	ps, err := s.toProtos(ctx, []sqlc.Alert{a})
	if err != nil {
		return nil, err
	}
	rows, err := s.q.AlertAuditEntries(ctx, a.AuditIds)
	if err != nil {
		return nil, s.internal("audit entries", err)
	}
	out := &restaurantv1.GetAlertResponse{Alert: ps[0]}
	for _, e := range rows {
		out.AuditEntries = append(out.AuditEntries, &restaurantv1.AuditEntry{
			Id: e.ID, ActorUserId: idStr(e.ActorUserID), Action: e.Action, Entity: e.Entity, EntityId: e.EntityID.String,
			Reason: e.Reason.String, ApproverUserId: idStr(e.ApproverUserID), CreatedAtUnix: e.CreatedAt.Unix(),
			ActorName: e.ActorName, ApproverName: e.ApproverName, Subject: e.Subject, DeviceLabel: e.DeviceLabel.String,
			Detail: reports.AuditDetail(e.Reason.String, e.After),
		})
	}
	return out, nil
}

func (s *Service) ResolveAlerts(ctx context.Context, req *restaurantv1.ResolveAlertsRequest) (*restaurantv1.ResolveAlertsResponse, error) {
	r, err := s.requireReader(ctx)
	if err != nil {
		return nil, err
	}
	status := statusName(req.GetStatus())
	if status != "OK" && status != "PROBLEM" {
		return nil, connecterr.Invalid("status", "status must be OK or PROBLEM")
	}
	note := strings.TrimSpace(req.GetNote())
	if status == "PROBLEM" && note == "" {
		return nil, connecterr.Invalid("note", "say what the problem was")
	}
	if len(req.GetIds()) == 0 {
		return nil, connecterr.Invalid("ids", "choose at least one alert")
	}
	var ids []uuid.UUID
	for _, v := range req.GetIds() {
		id, perr := uuid.Parse(v)
		if perr != nil {
			return nil, connecterr.Invalid("ids", "invalid id")
		}
		ids = append(ids, id)
	}
	ids = uniqueIDs(ids)
	var done []sqlc.Alert
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		locked, e := q.LockAlerts(ctx, ids)
		if e != nil {
			return connecterr.Internal(e)
		}
		if len(locked) != len(ids) {
			return connecterr.NotFound("alert not found")
		}
		byID := map[uuid.UUID]sqlc.Alert{}
		for _, a := range locked {
			// A resolution is changed by the owner only.
			if !r.canSee(a) || (a.Status != "OPEN" && !r.owner()) {
				return connecterr.NotAllowed("you can't resolve this alert")
			}
			byID[a.ID] = a
		}
		for _, id := range ids { // in the order asked
			a := byID[id]
			updated, e := q.ResolveAlert(ctx, sqlc.ResolveAlertParams{Status: status, Note: note, ResolvedBy: dbx.UUID(r.user.ID), ID: id})
			if e != nil {
				return connecterr.Internal(e)
			}
			if e := audit.Write(ctx, q, audit.Entry{
				ActorID: r.user.ID, Action: "alert.resolve", Entity: "alert", EntityID: id.String(), Reason: note,
				Before: audit.JSON(map[string]string{"status": a.Status}),
				After:  audit.JSON(map[string]string{"status": status, "rule": a.Rule, "detail": a.Rule + " · " + a.Subject}),
			}); e != nil {
				return e
			}
			done = append(done, updated)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, a := range done {
		s.emit(realtime.Event{Type: EventAlertUpdated, ID: a.ID.String(), Version: a.Version})
	}
	alerts, err := s.toProtos(ctx, done)
	if err != nil {
		return nil, err
	}
	return &restaurantv1.ResolveAlertsResponse{Alerts: alerts}, nil
}

func (s *Service) GetAlertCounts(ctx context.Context) (*restaurantv1.GetAlertCountsResponse, error) {
	r, err := s.requireReader(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.q.AlertCounts(ctx, r.hideFor)
	if err != nil {
		return nil, s.internal("counts", err)
	}
	return &restaurantv1.GetAlertCountsResponse{High: c.High, Open: c.Open}, nil
}

// --- config ---

func toConfigProto(c Config) *restaurantv1.AlertConfig {
	out := &restaurantv1.AlertConfig{AfterHoursGraceMinutes: int32(c.Grace), DailyPushCap: int32(c.PushCap)}
	for _, r := range c.Rules {
		out.Rules = append(out.Rules, &restaurantv1.AlertRuleSetting{
			Rule: ruleEnum[r.Rule], Enabled: r.Enabled, Severity: severityEnum(r.Severity), Thresholds: r.Thresholds,
		})
	}
	if c.OpeningHours != nil {
		out.OpeningHours = &restaurantv1.WeekHours{}
		for _, h := range c.OpeningHours {
			d := &restaurantv1.DayHours{}
			if h != nil {
				d.Open, d.Close = h.Open, h.Close
			}
			out.OpeningHours.Days = append(out.OpeningHours.Days, d)
		}
	}
	if c.Quiet != nil {
		out.QuietHours = &restaurantv1.QuietHours{From: c.Quiet.From, To: c.Quiet.To}
	}
	for _, id := range c.Recipients {
		out.RecipientIds = append(out.RecipientIds, id.String())
	}
	return out
}

func fromConfigProto(p *restaurantv1.AlertConfig) (Config, error) {
	c := Config{Grace: int(p.GetAfterHoursGraceMinutes()), PushCap: int(p.GetDailyPushCap())}
	for _, r := range p.GetRules() {
		th := map[string]int64{}
		for k, v := range r.GetThresholds() {
			th[k] = v
		}
		c.Rules = append(c.Rules, RuleSetting{Rule: ruleName(r.GetRule()), Enabled: r.GetEnabled(), Severity: severityName(r.GetSeverity()), Thresholds: th})
	}
	if p.GetOpeningHours() != nil {
		c.OpeningHours = []*DayHours{}
		for _, d := range p.GetOpeningHours().GetDays() {
			if d.GetOpen() == "" && d.GetClose() == "" {
				c.OpeningHours = append(c.OpeningHours, nil)
			} else {
				c.OpeningHours = append(c.OpeningHours, &DayHours{Open: d.GetOpen(), Close: d.GetClose()})
			}
		}
	}
	if q := p.GetQuietHours(); q != nil {
		c.Quiet = &QuietHours{From: q.GetFrom(), To: q.GetTo()}
	}
	for _, v := range p.GetRecipientIds() {
		id, err := uuid.Parse(v)
		if err != nil {
			return Config{}, connecterr.Invalid("recipient_ids", "invalid id")
		}
		c.Recipients = append(c.Recipients, id)
	}
	return c, nil
}

func (s *Service) GetAlertConfig(ctx context.Context) (*restaurantv1.AlertConfig, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	cfg, err := loadConfig(ctx, s.q)
	if err != nil {
		return nil, s.internal("config", err)
	}
	return toConfigProto(cfg), nil
}

func (s *Service) UpdateAlertConfig(ctx context.Context, req *restaurantv1.UpdateAlertConfigRequest) (*restaurantv1.AlertConfig, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner)
	if err != nil {
		return nil, err
	}
	next, err := fromConfigProto(req.GetConfig())
	if err != nil {
		return nil, err
	}
	staff, err := s.q.ListStaff(ctx)
	if err != nil {
		return nil, s.internal("staff", err)
	}
	var managers []uuid.UUID
	for _, u := range staff {
		if u.Active && u.Role == auth.RoleManager {
			managers = append(managers, u.ID)
		}
	}
	if field := validate(next, managers); field != "" {
		return nil, connecterr.Invalid(field, "invalid alert setting")
	}
	var saved Config
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		before, e := loadConfig(ctx, q)
		if e != nil {
			return connecterr.Internal(e)
		}
		row, e := q.UpdateAlertConfig(ctx, saveParams(next, before))
		if e != nil {
			return connecterr.Internal(e)
		}
		saved = configFromRow(row)
		rules, hours, notify := changes(before, saved)
		if len(rules) > 0 || hours {
			parts := rules
			if hours {
				parts = append(parts, "opening hours")
			}
			if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "alert.rules_change", Entity: "settings", EntityID: "alerts",
				After: audit.JSON(map[string]string{"detail": strings.Join(parts, ", ")})}); e != nil {
				return e
			}
		}
		if notify {
			if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "alert.notifications_change", Entity: "settings", EntityID: "alerts",
				After: audit.JSON(map[string]string{"detail": fmt.Sprintf("cap %d · recipients %d", saved.PushCap, len(saved.Recipients))})}); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "settings.updated", ID: "alerts"})
	return toConfigProto(saved), nil
}

// --- notifications ---

func (s *Service) ListNotifications(ctx context.Context) (*restaurantv1.ListNotificationsResponse, error) {
	actor, err := auth.RequireActor(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.requireReader(ctx)
	if err != nil {
		// Every manager's app polls this; one who isn't a recipient simply has nothing.
		if actor.User.Role == auth.RoleManager {
			return &restaurantv1.ListNotificationsResponse{}, nil
		}
		return nil, err
	}
	rows, err := s.q.ListAlertPushes(ctx, r.hideFor)
	if err != nil {
		return nil, s.internal("pushes", err)
	}
	out := &restaurantv1.ListNotificationsResponse{}
	for _, p := range rows {
		pp := &restaurantv1.AlertPush{
			Id: p.ID.String(), AtUnix: p.At.Unix(), Kind: restaurantv1.AlertPushKind(restaurantv1.AlertPushKind_value["ALERT_PUSH_KIND_"+p.Kind]),
			Rule: ruleEnum[p.Rule], AlertId: idStr(p.AlertID), BusinessDate: p.BusinessDate.Time.Format(time.DateOnly),
		}
		if p.AmountPaisa.Valid {
			pp.AmountPaisa = &p.AmountPaisa.Int64
		}
		if p.Count.Valid {
			pp.Count = &p.Count.Int32
		}
		out.Pushes = append(out.Pushes, pp)
	}
	return out, nil
}

// --- day summary ---

func (s *Service) GetDaySummary(ctx context.Context, req *restaurantv1.GetDaySummaryRequest) (*restaurantv1.DaySummary, error) {
	r, err := s.requireReader(ctx)
	if err != nil {
		return nil, err
	}
	date, err := parseDate("date", req.GetDate())
	if err != nil || !date.Valid {
		return nil, connecterr.Invalid("date", "date must be YYYY-MM-DD")
	}
	day := req.GetDate()
	from, to := businessDayRange(day, r.cfg.OpeningHours)
	out := &restaurantv1.DaySummary{BusinessDate: day, GeneratedAtUnix: time.Now().Unix()}
	sales, err := s.q.AlertDaySales(ctx, sqlc.AlertDaySalesParams{FromTs: from, ToTs: to})
	if err != nil {
		return nil, s.internal("sales", err)
	}
	out.SalesPaisa, out.Bills, out.DiscountsPaisa = sales.Sales, sales.Bills, sales.Discounts
	methods, err := s.q.AlertDayByMethod(ctx, sqlc.AlertDayByMethodParams{FromTs: from, ToTs: to})
	if err != nil {
		return nil, s.internal("by method", err)
	}
	for _, m := range methods {
		out.ByMethod = append(out.ByMethod, &restaurantv1.MethodTotal{
			Method: restaurantv1.PaymentMethod(restaurantv1.PaymentMethod_value["PAYMENT_METHOD_"+m.Method]), TotalPaisa: m.Total,
		})
	}
	voids, err := s.q.AlertDayVoids(ctx, sqlc.AlertDayVoidsParams{FromTs: ts(from), ToTs: ts(to)})
	if err != nil {
		return nil, s.internal("voids", err)
	}
	out.VoidCount, out.VoidValuePaisa = voids.Count, voids.Value
	cancelled, err := s.q.AlertDayCancelled(ctx, sqlc.AlertDayCancelledParams{FromTs: ts(from), ToTs: ts(to)})
	if err != nil {
		return nil, s.internal("cancelled", err)
	}
	out.CancelledCount, out.CancelledValuePaisa = cancelled.Count, cancelled.Value
	shifts, err := s.q.AlertDayShifts(ctx, sqlc.AlertDayShiftsParams{FromTs: from, ToTs: to})
	if err != nil {
		return nil, s.internal("shifts", err)
	}
	for _, sh := range shifts {
		row := sqlc.Shift{ID: sh.ID, CashierUserID: sh.CashierUserID, OpenedAt: sh.OpenedAt, ClosedAt: sh.ClosedAt,
			OpeningFloatPaisa: sh.OpeningFloatPaisa, CountedCashPaisa: sh.CountedCashPaisa, Status: sh.Status}
		expected, err := shift.ExpectedCash(ctx, s.q, row)
		if err != nil {
			return nil, s.internal("expected cash", err)
		}
		ds := &restaurantv1.DaySummaryShift{Id: sh.ID.String(), UserName: sh.UserName, OpenedAtUnix: sh.OpenedAt.Unix(), ExpectedCashPaisa: expected}
		if sh.ClosedAt.Valid {
			ds.ClosedAtUnix = sh.ClosedAt.Time.Unix()
		}
		if sh.Status == "CLOSED" && sh.CountedCashPaisa.Valid {
			counted, diff := sh.CountedCashPaisa.Int64, sh.CountedCashPaisa.Int64-expected
			ds.CountedCashPaisa, ds.DifferencePaisa = &counted, &diff
		}
		out.Shifts = append(out.Shifts, ds)
	}
	served, err := s.q.AlertServedUnbilled(ctx)
	if err != nil {
		return nil, s.internal("served not billed", err)
	}
	out.ServedNotBilled = int32(len(served))
	byRule, err := s.q.DayAlertsByRule(ctx, sqlc.DayAlertsByRuleParams{BusinessDate: date, HideFor: r.hideFor})
	if err != nil {
		return nil, s.internal("alerts by rule", err)
	}
	for _, b := range byRule {
		sev := SeverityNormal
		if b.AnyHigh {
			sev = SeverityHigh
		}
		out.Alerts = append(out.Alerts, &restaurantv1.DaySummaryRuleTotal{Rule: ruleEnum[b.Rule], Severity: severityEnum(sev), Total: b.Total, Open: b.Open})
		out.OpenAlerts += b.Open
		out.HighAlerts += b.OpenHigh
	}
	// High first, then the most frequent.
	slices.SortStableFunc(out.Alerts, func(a, b *restaurantv1.DaySummaryRuleTotal) int {
		if a.Severity != b.Severity {
			return int(a.Severity) - int(b.Severity)
		}
		return int(b.Total) - int(a.Total)
	})
	return out, nil
}
