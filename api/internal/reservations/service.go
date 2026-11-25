// Package reservations owns table bookings: staff-entered bookings by a credit
// customer (created inline at limit 0), optional tables with no overlapping
// BOOKED windows per table, seating into a DINE_IN order, cancel / no-show /
// restore, "keep waiting", message marks, settings, the day report and the
// dashboard figures. "Late" and "released" are derived (Hold), never stored.
// Mirrors the web fake (web/src/lib/api/mock/mock-reservations-api.ts).
package reservations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
)

// Audit actions (the web maps them to RESERVATION_CREATED, RESERVATION_CHANGED,
// RESERVATION_CANCELLED, RESERVATION_NO_SHOW, RESERVATION_SEATED, SETTINGS_CHANGED).
const (
	ActionCreate   = "reservation.create"
	ActionChange   = "reservation.change"
	ActionCancel   = "reservation.cancel"
	ActionNoShow   = "reservation.no_show"
	ActionSeat     = "reservation.seat"
	ActionSettings = "reservation.settings"
)

// EventUpdated is the realtime poke for a booking (id = reservation id, or "settings").
const EventUpdated = "reservation.updated"

const (
	statusBooked    = "BOOKED"
	statusSeated    = "SEATED"
	statusCancelled = "CANCELLED"
	statusNoShow    = "NO_SHOW"
)

var nepal = time.FixedZone("NPT", 5*3600+45*60)

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	log  *zap.Logger
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, log: log, now: time.Now}
}

func (s *Service) emit(evs ...realtime.Event) {
	if s.hub == nil {
		return
	}
	for _, ev := range evs {
		s.hub.Broadcast(ev)
	}
}

// --- errors ---

func failed(reason restaurantv1.ErrorReason, field, msg string) *connecterr.Error {
	e := connecterr.New(connect.CodeFailedPrecondition, reason, msg)
	e.Field = field
	return e
}

// tableAlreadyBooked carries the clashing booking's id as its message (the web shows it).
func tableAlreadyBooked(clash uuid.UUID) *connecterr.Error {
	return failed(restaurantv1.ErrorReason_ERROR_REASON_TABLE_ALREADY_BOOKED, "table_ids", clash.String())
}

func invalidStatus() *connecterr.Error {
	return failed(restaurantv1.ErrorReason_ERROR_REASON_INVALID_STATUS, "", "not possible from this booking's status")
}

func tooEarly() *connecterr.Error {
	return failed(restaurantv1.ErrorReason_ERROR_REASON_TOO_EARLY, "", "a no-show can only be marked after the booking time")
}

func reservationsOff() *connecterr.Error {
	return failed(restaurantv1.ErrorReason_ERROR_REASON_RESERVATIONS_OFF, "", "reservations are switched off")
}

// tableOccupied: the web's TABLE_OCCUPIED, sent as IN_USE (no reason slot left) with field "table_ids".
func tableOccupied() *connecterr.Error {
	e := connecterr.InUse("a chosen table has an open order")
	e.Field = "table_ids"
	return e
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

// requireBooker admits everyone who answers the phone: all roles except kitchen.
func requireBooker(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier, auth.RoleWaiter)
}

func requireOn(ctx context.Context, q *sqlc.Queries) (sqlc.ReservationSetting, error) {
	st, err := q.GetReservationSettings(ctx)
	if err != nil {
		return st, connecterr.Internal(err)
	}
	if !st.Enabled {
		return st, reservationsOff()
	}
	return st, nil
}

// --- derived hold state ---

// Hold states of a BOOKED booking (web: reservations/rules.ts holdState).
// UPCOMING: too early to hold the table. HOLD: Reserved (from hold-before until
// the grace ends). LATE: past the grace, still held. RELEASED: past release-after
// (+ keep waiting). "" unless BOOKED.
const (
	HoldUpcoming = "UPCOMING"
	HoldHold     = "HOLD"
	HoldLate     = "LATE"
	HoldReleased = "RELEASED"
)

func Hold(status string, startsAt time.Time, holdExtendedMin int32, st sqlc.ReservationSetting, now time.Time) string {
	if status != statusBooked {
		return ""
	}
	late := now.Sub(startsAt).Minutes()
	switch {
	case late < -float64(st.HoldBeforeMin):
		return HoldUpcoming
	case late < float64(st.GraceMin):
		return HoldHold
	case late < float64(st.ReleaseAfterMin+holdExtendedMin):
		return HoldLate
	}
	return HoldReleased
}

// --- converters ---

func unixOf(t pgtype.Timestamptz) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.Unix()
}

func idOf(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func ids(in []uuid.UUID) []string {
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = id.String()
	}
	return out
}

func sourceEnum(s string) restaurantv1.ReservationSource {
	if v, ok := restaurantv1.ReservationSource_value["RESERVATION_SOURCE_"+s]; ok {
		return restaurantv1.ReservationSource(v)
	}
	return restaurantv1.ReservationSource_RESERVATION_SOURCE_UNSPECIFIED
}

func sourceStr(s restaurantv1.ReservationSource) (string, bool) {
	if s == restaurantv1.ReservationSource_RESERVATION_SOURCE_UNSPECIFIED {
		return "PHONE", true
	}
	name, ok := restaurantv1.ReservationSource_name[int32(s)]
	return strings.TrimPrefix(name, "RESERVATION_SOURCE_"), ok
}

func statusEnum(s string) restaurantv1.ReservationStatus {
	if v, ok := restaurantv1.ReservationStatus_value["RESERVATION_STATUS_"+s]; ok {
		return restaurantv1.ReservationStatus(v)
	}
	return restaurantv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED
}

func toProto(r sqlc.Reservation, name, phone string) *restaurantv1.Reservation {
	out := &restaurantv1.Reservation{
		Id: r.ID.String(), CustomerId: r.CustomerID.String(), CustomerName: name, CustomerPhone: phone,
		PartySize: r.PartySize, StartsAtUnix: r.StartsAt.Unix(), DurationMin: r.DurationMin, AreaId: idOf(r.AreaID),
		TableIds: ids(r.TableIds), WholeArea: r.WholeArea, Status: statusEnum(r.Status), Source: sourceEnum(r.Source),
		Notes: r.Notes, OrderId: idOf(r.OrderID), SeatedAtUnix: unixOf(r.SeatedAt), SeatedById: idOf(r.SeatedByUserID),
		CancelReason: r.CancelReason.String, CancelledAtUnix: unixOf(r.CancelledAt), CancelledById: idOf(r.CancelledByUserID),
		NoShowAtUnix: unixOf(r.NoShowAt), NoShowById: idOf(r.NoShowByUserID), ConfirmedAtUnix: unixOf(r.ConfirmedAt),
		RemindedAtUnix: unixOf(r.RemindedAt), HoldExtendedMin: r.HoldExtendedMin, CreatedById: r.CreatedByUserID.String(),
		CreatedAtUnix: r.CreatedAt.Unix(), UpdatedById: r.UpdatedByUserID.String(), UpdatedAtUnix: r.UpdatedAt.Unix(), Version: r.Version,
	}
	return out
}

func (s *Service) load(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (*restaurantv1.Reservation, error) {
	v, err := q.GetReservationView(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		e := connecterr.NotFound("booking not found")
		e.Field = "id"
		return nil, e
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return toProto(v.Reservation, v.CustomerName, v.CustomerPhone), nil
}

func lockReservation(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (sqlc.Reservation, error) {
	r, err := q.LockReservation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		e := connecterr.NotFound("booking not found")
		e.Field = "id"
		return r, e
	}
	if err != nil {
		return r, connecterr.Internal(err)
	}
	return r, nil
}

// --- fields ---

type fields struct {
	partySize   int32
	startsAt    time.Time
	durationMin int32
	areaID      uuid.UUID // Nil = any
	tableIDs    []uuid.UUID
	wholeArea   bool
	source      string
	notes       string
}

func fieldsOf(r sqlc.Reservation) fields {
	f := fields{partySize: r.PartySize, startsAt: r.StartsAt, durationMin: r.DurationMin, tableIDs: r.TableIds,
		wholeArea: r.WholeArea, source: r.Source, notes: r.Notes}
	if r.AreaID.Valid {
		f.areaID = r.AreaID.Bytes
	}
	return f
}

// patch holds the requested changes (nil = keep).
type patch struct {
	partySize   *int32
	startsAt    *int64
	durationMin *int32
	areaID      *string
	tableIDs    *[]string
	wholeArea   *bool
	source      *restaurantv1.ReservationSource
	notes       *string
}

// clean validates the booking fields as the server rules say; prev is the stored
// booking when editing (nil on create).
func (s *Service) clean(ctx context.Context, q *sqlc.Queries, st sqlc.ReservationSetting, p patch, prev *fields) (fields, error) {
	var f fields
	if prev != nil {
		f = *prev
	}
	if p.partySize != nil {
		f.partySize = *p.partySize
	}
	if f.partySize < 1 || f.partySize > 200 {
		return f, connecterr.Invalid("party_size", "1 to 200 people")
	}
	if p.startsAt != nil {
		f.startsAt = time.Unix(*p.startsAt, 0).UTC()
	}
	if f.startsAt.IsZero() || f.startsAt.Unix() <= 0 {
		return f, connecterr.Invalid("starts_at", "a booking time is required")
	}
	if prev == nil || !f.startsAt.Equal(prev.startsAt) {
		now := s.now()
		if f.startsAt.Before(now.Add(-15*time.Minute)) || f.startsAt.After(now.AddDate(0, 0, 365)) {
			return f, connecterr.Invalid("starts_at", "not more than 15 minutes ago, at most a year ahead")
		}
	}
	switch {
	case p.durationMin != nil:
		f.durationMin = *p.durationMin
	case prev == nil:
		f.durationMin = st.DurationMin
		if f.partySize >= st.LargePartySize {
			f.durationMin = st.LargeDurationMin
		}
	}
	if f.durationMin < 15 || f.durationMin > 720 {
		return f, connecterr.Invalid("duration_min", "15 to 720 minutes")
	}
	if p.areaID != nil {
		f.areaID = uuid.Nil
		if *p.areaID != "" {
			id, err := parseID("area_id", *p.areaID)
			if err != nil {
				return f, err
			}
			f.areaID = id
		}
	}
	if f.areaID != uuid.Nil {
		if _, err := q.GetArea(ctx, f.areaID); errors.Is(err, pgx.ErrNoRows) {
			return f, connecterr.Invalid("area_id", "no such area")
		} else if err != nil {
			return f, connecterr.Internal(err)
		}
	}
	if p.wholeArea != nil {
		f.wholeArea = *p.wholeArea
	}
	if p.tableIDs != nil {
		f.tableIDs = nil
		for _, raw := range *p.tableIDs {
			id, err := parseID("table_ids", raw)
			if err != nil {
				return f, err
			}
			if !slices.Contains(f.tableIDs, id) {
				f.tableIDs = append(f.tableIDs, id)
			}
		}
	}
	tables, err := q.ListTables(ctx)
	if err != nil {
		return f, connecterr.Internal(err)
	}
	if f.wholeArea {
		if f.areaID == uuid.Nil {
			return f, connecterr.Invalid("area_id", "a whole-area booking needs an area")
		}
		f.tableIDs = nil
		for _, t := range tables {
			if t.AreaID == f.areaID && t.Active {
				f.tableIDs = append(f.tableIDs, t.ID)
			}
		}
	}
	for _, id := range f.tableIDs {
		if !slices.ContainsFunc(tables, func(t sqlc.DiningTable) bool { return t.ID == id && t.Active }) {
			return f, connecterr.Invalid("table_ids", "no such active table")
		}
	}
	if f.tableIDs == nil {
		f.tableIDs = []uuid.UUID{}
	}
	if p.source != nil || prev == nil {
		var src restaurantv1.ReservationSource
		if p.source != nil {
			src = *p.source
		}
		name, ok := sourceStr(src)
		if !ok {
			return f, connecterr.Invalid("source", "unknown source")
		}
		f.source = name
	}
	if p.notes != nil {
		f.notes = strings.TrimSpace(*p.notes)
	}
	if utf8.RuneCountInString(f.notes) > 500 {
		return f, connecterr.Invalid("notes", "notes up to 500 characters")
	}
	return f, nil
}

// checkClash: no table may be in two BOOKED bookings whose windows overlap.
func checkClash(ctx context.Context, q *sqlc.Queries, self uuid.UUID, startsAt time.Time, durationMin int32, tables []uuid.UUID) error {
	if len(tables) == 0 {
		return nil
	}
	clash, err := q.FindReservationClash(ctx, sqlc.FindReservationClashParams{
		ExcludeID: self, TableIds: tables, StartsAt: startsAt, EndsAt: startsAt.Add(time.Duration(durationMin) * time.Minute),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return connecterr.Internal(err)
	}
	return tableAlreadyBooked(clash)
}

func areaParam(id uuid.UUID) pgtype.UUID { return dbx.UUIDOpt(id) }

func ktmDate(t time.Time) string { return t.In(nepal).Format("2006-01-02") }
func ktmTime(t time.Time) string { return t.In(nepal).Format("15:04") }
func when(t time.Time) string    { return ktmDate(t) + " " + ktmTime(t) }

func tableNames(ctx context.Context, q *sqlc.Queries, tableIDs []uuid.UUID) string {
	if len(tableIDs) == 0 {
		return "—"
	}
	tables, err := q.ListTables(ctx)
	if err != nil {
		return fmt.Sprint(len(tableIDs), " tables")
	}
	names := make([]string, 0, len(tableIDs))
	for _, id := range tableIDs {
		name := id.String()
		for _, t := range tables {
			if t.ID == id {
				name = t.Name
			}
		}
		names = append(names, name)
	}
	return strings.Join(names, "+")
}

func describeChange(ctx context.Context, q *sqlc.Queries, before, after fields) string {
	var parts []string
	if !before.startsAt.Equal(after.startsAt) {
		if ktmDate(before.startsAt) == ktmDate(after.startsAt) {
			parts = append(parts, fmt.Sprintf("time %s → %s", ktmTime(before.startsAt), ktmTime(after.startsAt)))
		} else {
			parts = append(parts, fmt.Sprintf("time %s → %s", when(before.startsAt), when(after.startsAt)))
		}
	}
	if before.partySize != after.partySize {
		parts = append(parts, fmt.Sprintf("people %d → %d", before.partySize, after.partySize))
	}
	if before.durationMin != after.durationMin {
		parts = append(parts, fmt.Sprintf("duration %d → %d min", before.durationMin, after.durationMin))
	}
	if !slices.Equal(before.tableIDs, after.tableIDs) {
		parts = append(parts, fmt.Sprintf("tables %s → %s", tableNames(ctx, q, before.tableIDs), tableNames(ctx, q, after.tableIDs)))
	}
	if before.areaID != after.areaID {
		parts = append(parts, "area")
	}
	if before.notes != after.notes {
		parts = append(parts, "notes")
	}
	if before.source != after.source {
		parts = append(parts, fmt.Sprintf("source %s → %s", before.source, after.source))
	}
	if before.wholeArea != after.wholeArea {
		parts = append(parts, "whole area")
	}
	return strings.Join(parts, " · ")
}

func writeAudit(ctx context.Context, q *sqlc.Queries, actor uuid.UUID, action string, id uuid.UUID, detail string, reason string) error {
	return audit.Write(ctx, q, audit.Entry{ActorID: actor, Action: action, Entity: "reservation", EntityID: id.String(),
		After: audit.JSON(map[string]any{"detail": detail}), Reason: reason})
}

// --- customers ---

// newCustomer creates a caller not on file yet, in the same write, at credit limit 0.
func newCustomer(ctx context.Context, q *sqlc.Queries, actor uuid.UUID, in *restaurantv1.NewBookingCustomer) (sqlc.Customer, error) {
	name := strings.TrimSpace(in.GetName())
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return sqlc.Customer{}, connecterr.Invalid("name", "a name of up to 80 characters is required")
	}
	phone, ok := auth.NormalisePhone(in.GetPhone())
	if !ok {
		return sqlc.Customer{}, connecterr.Invalid("phone", "not a Nepali phone number")
	}
	if phone != "" {
		if _, err := q.CustomerIDByPhone(ctx, phone); err == nil {
			return sqlc.Customer{}, connecterr.Duplicate("phone", "another customer has this phone")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return sqlc.Customer{}, connecterr.Internal(err)
		}
	}
	c, err := q.CreateCustomer(ctx, sqlc.CreateCustomerParams{Name: name, Phone: phone, CreatedByUserID: dbx.UUID(actor)})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return c, connecterr.Duplicate("phone", "another customer has this phone")
		}
		return c, connecterr.Internal(err)
	}
	if err := audit.Write(ctx, q, audit.Entry{ActorID: actor, Action: "customer.change", Entity: "customer", EntityID: c.ID.String(),
		After: audit.JSON(map[string]any{"detail": "created (booking)"})}); err != nil {
		return c, err
	}
	return c, nil
}
