package analytics

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// Festival / closed-day calendar. Reads: owner and manager. Changes: owner only,
// audit-logged in the same transaction.

// maxEventDays: a single event spans at most 60 days.
const maxEventDays = 60

var kindName = map[restaurantv1.CalendarKind]string{
	restaurantv1.CalendarKind_CALENDAR_KIND_FESTIVAL: "FESTIVAL",
	restaurantv1.CalendarKind_CALENDAR_KIND_CLOSED:   "CLOSED",
	restaurantv1.CalendarKind_CALENDAR_KIND_OTHER:    "OTHER",
}

func kindEnum(k string) restaurantv1.CalendarKind {
	for e, n := range kindName {
		if n == k {
			return e
		}
	}
	return restaurantv1.CalendarKind_CALENDAR_KIND_UNSPECIFIED
}

func toEvent(e sqlc.CalendarEvent) *restaurantv1.CalendarEvent {
	return &restaurantv1.CalendarEvent{
		Id: e.ID.String(), Name: local(e.NameEn, e.NameNe), From: dayStr(e.FromDate.Time), To: dayStr(e.ToDate.Time),
		Kind: kindEnum(e.Kind), BuiltIn: e.BuiltIn, CreatedByUserId: uuidStr(e.CreatedByUserID), Key: e.Key.String, Approximate: e.Approximate,
	}
}

func (s *Service) ListCalendarEvents(ctx context.Context, req *restaurantv1.ListCalendarEventsRequest) (*restaurantv1.ListCalendarEventsResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	r, err := rangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListCalendarEventsIn(ctx, sqlc.ListCalendarEventsInParams{FromDay: pgDate(r.from), ToDay: pgDate(r.to)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListCalendarEventsResponse{}
	for _, e := range rows {
		out.Events = append(out.Events, toEvent(e))
	}
	return out, nil
}

func requireOwner(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner)
}

func (s *Service) SaveCalendarEvent(ctx context.Context, req *restaurantv1.SaveCalendarEventRequest) (*restaurantv1.CalendarEvent, error) {
	a, err := requireOwner(ctx)
	if err != nil {
		return nil, err
	}
	nameEn, nameNe := strings.TrimSpace(req.GetName().GetEn()), strings.TrimSpace(req.GetName().GetNe())
	if nameEn == "" {
		return nil, connecterr.Invalid("name", "a name is required")
	}
	from, ok1 := parseDay(req.GetFrom())
	to, ok2 := parseDay(req.GetTo())
	if !ok1 || !ok2 || from.After(to) || countDays(from, to) > maxEventDays {
		return nil, connecterr.Invalid("to", "dates must be YYYY-MM-DD, from <= to, at most 60 days")
	}
	kind, ok := kindName[req.GetKind()]
	if !ok {
		return nil, connecterr.Invalid("kind", "kind is required")
	}
	var saved sqlc.CalendarEvent
	verb := "added"
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if req.GetId() == "" {
			key := dbx.TextOpt(req.GetKey())
			var e error
			saved, e = q.CreateCalendarEvent(ctx, sqlc.CreateCalendarEventParams{
				NameEn: nameEn, NameNe: nameNe, FromDay: pgDate(from), ToDay: pgDate(to), Kind: kind,
				CreatedByUserID: dbx.UUID(a.User.ID), Key: key,
			})
			if e != nil {
				return connecterr.Internal(e)
			}
		} else {
			id, e := uuid.Parse(req.GetId())
			if e != nil {
				return connecterr.Invalid("id", "invalid id")
			}
			existing, e := q.GetCalendarEvent(ctx, id)
			if errors.Is(e, pgx.ErrNoRows) {
				return connecterr.NotFound("calendar event not found")
			}
			if e != nil {
				return connecterr.Internal(e)
			}
			key := existing.Key
			if req.Key != nil {
				key = dbx.TextOpt(req.GetKey())
			}
			if saved, e = q.UpdateCalendarEvent(ctx, sqlc.UpdateCalendarEventParams{
				ID: id, NameEn: nameEn, NameNe: nameNe, FromDay: pgDate(from), ToDay: pgDate(to), Kind: kind, Key: key,
			}); e != nil {
				return connecterr.Internal(e)
			}
			verb = "edited"
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: a.User.ID, Action: "calendar.save", Entity: "calendar_event", EntityID: saved.ID.String(),
			After: audit.JSON(map[string]any{"name": nameEn, "detail": fmt.Sprintf("%s · %s · %s – %s", verb, kind, dayStr(from), dayStr(to))}),
		})
	})
	if err != nil {
		return nil, err
	}
	return toEvent(saved), nil
}

func (s *Service) RemoveCalendarEvent(ctx context.Context, idStr string) error {
	a, err := requireOwner(ctx)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return connecterr.Invalid("id", "invalid id")
	}
	return dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		e, err := q.GetCalendarEvent(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return connecterr.NotFound("calendar event not found")
		}
		if err != nil {
			return connecterr.Internal(err)
		}
		if _, err := q.DeleteCalendarEvent(ctx, id); err != nil {
			return connecterr.Internal(err)
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: a.User.ID, Action: "calendar.remove", Entity: "calendar_event", EntityID: id.String(),
			Before: audit.JSON(map[string]any{"name": e.NameEn}),
			After:  audit.JSON(map[string]any{"detail": fmt.Sprintf("removed · %s · %s – %s", e.NameEn, dayStr(e.FromDate.Time), dayStr(e.ToDate.Time))}),
		})
	})
}

// ---------- Settings ----------

func settingsProto(managers []uuid.UUID, highlights bool) *restaurantv1.AnalyticsSettings {
	out := &restaurantv1.AnalyticsSettings{HighlightRules: highlights, ManagersSeeStaff: []string{}}
	for _, id := range managers {
		out.ManagersSeeStaff = append(out.ManagersSeeStaff, id.String())
	}
	return out
}

func (s *Service) GetSettings(ctx context.Context) (*restaurantv1.AnalyticsSettings, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	row, err := s.q.GetAnalyticsSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return settingsProto(row.ManagersSeeStaff, row.HighlightRules), nil
}

// SaveSettings (owner): ids that aren't managers are dropped.
func (s *Service) SaveSettings(ctx context.Context, in *restaurantv1.AnalyticsSettings) (*restaurantv1.AnalyticsSettings, error) {
	a, err := requireOwner(ctx)
	if err != nil {
		return nil, err
	}
	users, err := s.q.AnalyticsUsers(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	managers := map[string]uuid.UUID{}
	for _, u := range users {
		if u.Role == auth.RoleManager {
			managers[u.ID.String()] = u.ID
		}
	}
	keep := []uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for _, raw := range in.GetManagersSeeStaff() {
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		if m, ok := managers[id.String()]; ok && !seen[m] {
			seen[m] = true
			keep = append(keep, m)
		}
	}
	var row sqlc.SaveAnalyticsSettingsRow
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var e error
		if row, e = q.SaveAnalyticsSettings(ctx, sqlc.SaveAnalyticsSettingsParams{ManagersSeeStaff: keep, HighlightRules: in.GetHighlightRules()}); e != nil {
			return connecterr.Internal(e)
		}
		onOff := "off"
		if row.HighlightRules {
			onOff = "on"
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: a.User.ID, Action: "settings.update", Entity: "analytics_settings",
			After: audit.JSON(map[string]any{"detail": fmt.Sprintf("analytics: staff section for %d manager(s) · highlights %s", len(keep), onOff)}),
		})
	})
	if err != nil {
		return nil, err
	}
	return settingsProto(row.ManagersSeeStaff, row.HighlightRules), nil
}
