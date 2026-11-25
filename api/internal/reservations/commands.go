package reservations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/proto"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/idem"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// eventCustomerUpdated mirrors credit.EventCustomerUpdated (a booking may create a customer).
const eventCustomerUpdated = "customer.updated"

func (s *Service) CreateReservation(ctx context.Context, req *restaurantv1.CreateReservationRequest) (*restaurantv1.Reservation, error) {
	actor, err := requireBooker(ctx)
	if err != nil {
		return nil, err
	}
	requestID, err := parseID("request_id", req.GetRequestId())
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.Reservation{}
	createdCustomer := false
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.LockReservations(ctx); e != nil {
			return connecterr.Internal(e)
		}
		st, e := requireOn(ctx, q)
		if e != nil {
			return e
		}
		if done, e := q.GetReservationByRequestID(ctx, requestID); e == nil { // a retry: the original booking
			out, e = s.load(ctx, q, done.ID)
			return e
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Internal(e)
		}
		ps, sa, src, notes := req.GetPartySize(), req.GetStartsAtUnix(), req.GetSource(), req.GetNotes()
		tables, area, whole := req.GetTableIds(), req.GetAreaId(), req.GetWholeArea()
		f, e := s.clean(ctx, q, st, patch{partySize: &ps, startsAt: &sa, durationMin: req.DurationMin, areaID: &area,
			tableIDs: &tables, wholeArea: &whole, source: &src, notes: &notes}, nil)
		if e != nil {
			return e
		}
		var customer sqlc.Customer
		switch {
		case req.GetCustomerId() != "":
			id, e := parseID("customer_id", req.GetCustomerId())
			if e != nil {
				return e
			}
			customer, e = q.LockCustomer(ctx, id)
			if errors.Is(e, pgx.ErrNoRows) {
				return connecterr.Invalid("customer_id", "no such customer")
			}
			if e != nil {
				return connecterr.Internal(e)
			}
		case req.GetNewCustomer() == nil:
			return connecterr.Invalid("customer_id", "a customer or a new customer is required")
		}
		if e := checkClash(ctx, q, uuid.Nil, f.startsAt, f.durationMin, f.tableIDs); e != nil {
			return e
		}
		if req.GetCustomerId() == "" {
			customer, e = newCustomer(ctx, q, actor.User.ID, req.GetNewCustomer())
			if e != nil {
				return e
			}
			createdCustomer = true
		}
		r, e := q.InsertReservation(ctx, sqlc.InsertReservationParams{
			RequestID: requestID, CustomerID: customer.ID, PartySize: f.partySize, StartsAt: f.startsAt, DurationMin: f.durationMin,
			AreaID: areaParam(f.areaID), TableIds: f.tableIDs, WholeArea: f.wholeArea, Source: f.source, Notes: f.notes, UserID: actor.User.ID,
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		detail := fmt.Sprintf("%s · %d · %s · %s", when(r.StartsAt), r.PartySize, tableNames(ctx, q, r.TableIds), r.Source)
		if e := writeAudit(ctx, q, actor.User.ID, ActionCreate, r.ID, detail, ""); e != nil {
			return e
		}
		out, e = s.load(ctx, q, r.ID)
		return e
	})
	if err != nil {
		return nil, err
	}
	if createdCustomer {
		s.emit(realtime.Event{Type: eventCustomerUpdated, ID: out.CustomerId})
	}
	s.emit(realtime.Event{Type: EventUpdated, ID: out.Id})
	return out, nil
}

// change runs one write on one booking: role, switch, row lock, fn, then the
// booking as it is now and `reservation.updated`.
func (s *Service) change(ctx context.Context, rawID string, fn func(q *sqlc.Queries, r sqlc.Reservation, actor *auth.Actor) error) (*restaurantv1.Reservation, error) {
	actor, err := requireBooker(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", rawID)
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.Reservation
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.LockReservations(ctx); e != nil {
			return connecterr.Internal(e)
		}
		if _, e := requireOn(ctx, q); e != nil {
			return e
		}
		r, e := lockReservation(ctx, q, id)
		if e != nil {
			return e
		}
		if e := fn(q, r, actor); e != nil {
			return e
		}
		out, e = s.load(ctx, q, id)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventUpdated, ID: out.Id})
	return out, nil
}

func requireBooked(r sqlc.Reservation) error {
	if r.Status != statusBooked {
		return invalidStatus()
	}
	return nil
}

func (s *Service) UpdateReservation(ctx context.Context, req *restaurantv1.UpdateReservationRequest) (*restaurantv1.Reservation, error) {
	return s.change(ctx, req.GetId(), func(q *sqlc.Queries, r sqlc.Reservation, actor *auth.Actor) error {
		if e := requireBooked(r); e != nil {
			return e
		}
		if r.Version != req.GetBaseVersion() {
			return connecterr.StaleVersion("this booking was changed; reload and retry")
		}
		st, e := q.GetReservationSettings(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		p := patch{partySize: req.PartySize, startsAt: req.StartsAtUnix, durationMin: req.DurationMin, areaID: req.AreaId,
			wholeArea: req.WholeArea, source: req.Source, notes: req.Notes}
		if req.GetSetTableIds() {
			t := req.GetTableIds()
			p.tableIDs = &t
		}
		before := fieldsOf(r)
		f, e := s.clean(ctx, q, st, p, &before)
		if e != nil {
			return e
		}
		if e := checkClash(ctx, q, r.ID, f.startsAt, f.durationMin, f.tableIDs); e != nil {
			return e
		}
		detail := describeChange(ctx, q, before, f)
		if detail == "" {
			return nil
		}
		if e := q.UpdateReservationFields(ctx, sqlc.UpdateReservationFieldsParams{
			ID: r.ID, PartySize: f.partySize, StartsAt: f.startsAt, DurationMin: f.durationMin, AreaID: areaParam(f.areaID),
			TableIds: f.tableIDs, WholeArea: f.wholeArea, Source: f.source, Notes: f.notes, UserID: actor.User.ID,
		}); e != nil {
			return connecterr.Internal(e)
		}
		return writeAudit(ctx, q, actor.User.ID, ActionChange, r.ID, detail, "")
	})
}

func (s *Service) CancelReservation(ctx context.Context, req *restaurantv1.CancelReservationRequest) (*restaurantv1.Reservation, error) {
	return s.change(ctx, req.GetId(), func(q *sqlc.Queries, r sqlc.Reservation, actor *auth.Actor) error {
		if e := requireBooked(r); e != nil {
			return e
		}
		reason := strings.TrimSpace(req.GetReason())
		if reason == "" {
			return connecterr.Invalid("reason", "a reason is required")
		}
		if e := q.SetReservationCancelled(ctx, sqlc.SetReservationCancelledParams{ID: r.ID, Reason: dbx.Text(reason), UserID: dbx.UUID(actor.User.ID)}); e != nil {
			return connecterr.Internal(e)
		}
		return writeAudit(ctx, q, actor.User.ID, ActionCancel, r.ID, reason, reason)
	})
}

func (s *Service) MarkNoShow(ctx context.Context, req *restaurantv1.MarkNoShowRequest) (*restaurantv1.Reservation, error) {
	return s.change(ctx, req.GetId(), func(q *sqlc.Queries, r sqlc.Reservation, actor *auth.Actor) error {
		if e := requireBooked(r); e != nil {
			return e
		}
		if s.now().Before(r.StartsAt) {
			return tooEarly()
		}
		if e := q.SetReservationNoShow(ctx, sqlc.SetReservationNoShowParams{ID: r.ID, UserID: dbx.UUID(actor.User.ID)}); e != nil {
			return connecterr.Internal(e)
		}
		return writeAudit(ctx, q, actor.User.ID, ActionNoShow, r.ID, when(r.StartsAt), "")
	})
}

// RestoreReservation: cancelled / no-show back to booked, only on the Kathmandu day it happened.
func (s *Service) RestoreReservation(ctx context.Context, req *restaurantv1.RestoreReservationRequest) (*restaurantv1.Reservation, error) {
	return s.change(ctx, req.GetId(), func(q *sqlc.Queries, r sqlc.Reservation, actor *auth.Actor) error {
		var at pgtype.Timestamptz
		switch r.Status {
		case statusCancelled:
			at = r.CancelledAt
		case statusNoShow:
			at = r.NoShowAt
		}
		if !at.Valid || ktmDate(at.Time) != ktmDate(s.now()) {
			return invalidStatus()
		}
		if e := checkClash(ctx, q, r.ID, r.StartsAt, r.DurationMin, r.TableIds); e != nil {
			return e
		}
		if e := q.RestoreReservation(ctx, sqlc.RestoreReservationParams{ID: r.ID, UserID: actor.User.ID}); e != nil {
			return connecterr.Internal(e)
		}
		return writeAudit(ctx, q, actor.User.ID, ActionChange, r.ID, "restored (was "+r.Status+")", "")
	})
}

// ExtendHold is "Keep waiting": minutes added to the release time.
func (s *Service) ExtendHold(ctx context.Context, req *restaurantv1.ExtendHoldRequest) (*restaurantv1.Reservation, error) {
	return s.change(ctx, req.GetId(), func(q *sqlc.Queries, r sqlc.Reservation, actor *auth.Actor) error {
		if e := requireBooked(r); e != nil {
			return e
		}
		m := req.GetMinutes()
		if m < 1 || m > 240 {
			return connecterr.Invalid("minutes", "1 to 240 minutes")
		}
		if e := q.ExtendReservationHold(ctx, sqlc.ExtendReservationHoldParams{ID: r.ID, Minutes: m, UserID: actor.User.ID}); e != nil {
			return connecterr.Internal(e)
		}
		return writeAudit(ctx, q, actor.User.ID, ActionChange, r.ID, fmt.Sprintf("keep waiting +%d min", m), "")
	})
}

// MarkMessaged records that "Send confirmation / reminder" was tapped. Not an edit:
// no version bump and no audit, so an open edit dialog isn't made stale.
func (s *Service) MarkMessaged(ctx context.Context, req *restaurantv1.MarkMessagedRequest) (*restaurantv1.Reservation, error) {
	return s.change(ctx, req.GetId(), func(q *sqlc.Queries, r sqlc.Reservation, _ *auth.Actor) error {
		var e error
		switch req.GetKind() {
		case restaurantv1.ReservationMessageKind_RESERVATION_MESSAGE_KIND_CONFIRMATION:
			e = q.MarkReservationConfirmed(ctx, r.ID)
		case restaurantv1.ReservationMessageKind_RESERVATION_MESSAGE_KIND_REMINDER:
			e = q.MarkReservationReminded(ctx, r.ID)
		default:
			return connecterr.Invalid("kind", "CONFIRMATION or REMINDER")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return nil
	})
}

// SeatReservation creates the DINE_IN order on the first table with the guest
// count and reservation_id (or links an open order on one of the tables), and
// marks the booking seated, in one transaction. Idempotent on command_id.
func (s *Service) SeatReservation(ctx context.Context, req *restaurantv1.SeatReservationRequest) (*restaurantv1.Order, error) {
	actor, err := requireBooker(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.Order{}
	err = idem.Run(ctx, s.pool, s.q, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		if e := q.LockReservations(ctx); e != nil {
			return connecterr.Internal(e)
		}
		if _, e := requireOn(ctx, q); e != nil {
			return e
		}
		r, e := lockReservation(ctx, q, id)
		if e != nil {
			return e
		}
		if e := requireBooked(r); e != nil {
			return e
		}
		guests := req.GetGuestCount()
		if guests < 1 || guests > 200 {
			return connecterr.Invalid("guest_count", "1 to 200 guests")
		}
		var tableIDs []uuid.UUID
		for _, raw := range req.GetTableIds() {
			tid, e := parseID("table_ids", raw)
			if e != nil {
				return e
			}
			if !slices.Contains(tableIDs, tid) {
				tableIDs = append(tableIDs, tid)
			}
		}
		tables, e := q.ListTables(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		if len(tableIDs) == 0 {
			return connecterr.Invalid("table_ids", "choose at least one table")
		}
		for _, tid := range tableIDs {
			if !slices.ContainsFunc(tables, func(t sqlc.DiningTable) bool { return t.ID == tid && t.Active }) {
				return connecterr.Invalid("table_ids", "no such active table")
			}
		}

		var orderID uuid.UUID
		if req.GetExistingOrderId() != "" {
			oid, e := parseID("existing_order_id", req.GetExistingOrderId())
			if e != nil {
				return e
			}
			o, e := q.GetOrderByID(ctx, oid)
			if errors.Is(e, pgx.ErrNoRows) || (e == nil && (o.Status != "OPEN" || !o.TableID.Valid || !slices.Contains(tableIDs, uuid.UUID(o.TableID.Bytes)))) {
				return connecterr.Invalid("existing_order_id", "not an open order on these tables")
			}
			if e != nil {
				return connecterr.Internal(e)
			}
			orderID = o.ID
			if !o.ReservationID.Valid || uuid.UUID(o.ReservationID.Bytes) != r.ID {
				if e := q.SetOrderReservation(ctx, sqlc.SetOrderReservationParams{ID: o.ID, ReservationID: dbx.UUID(r.ID)}); e != nil {
					return connecterr.Internal(e)
				}
				if _, e := q.BumpOrderVersion(ctx, o.ID); e != nil {
					return connecterr.Internal(e)
				}
			}
		} else {
			busy, e := q.OpenOrdersOnTables(ctx, tableIDs)
			if e != nil {
				return connecterr.Internal(e)
			}
			if len(busy) > 0 {
				return tableOccupied()
			}
			o, e := q.CreateOrder(ctx, sqlc.CreateOrderParams{
				ClientID: uuid.New(), Type: "DINE_IN", TableID: dbx.UUID(tableIDs[0]), WaiterUserID: actor.User.ID, GuestCount: guests,
			})
			if e != nil {
				return connecterr.Internal(e)
			}
			orderID = o.ID
			if e := q.SetOrderReservation(ctx, sqlc.SetOrderReservationParams{ID: o.ID, ReservationID: dbx.UUID(r.ID)}); e != nil {
				return connecterr.Internal(e)
			}
		}
		if e := q.SetReservationSeated(ctx, sqlc.SetReservationSeatedParams{
			ID: r.ID, TableIds: tableIDs, OrderID: dbx.UUID(orderID), UserID: dbx.UUID(actor.User.ID),
		}); e != nil {
			return connecterr.Internal(e)
		}
		detail := fmt.Sprintf("%s · %d · order %s", tableNames(ctx, q, tableIDs), guests, orderID)
		if e := writeAudit(ctx, q, actor.User.ID, ActionSeat, r.ID, detail, ""); e != nil {
			return e
		}
		ord, e := orders.AssembleOrder(ctx, q, orderID)
		if e != nil {
			return e
		}
		proto.Reset(out)
		proto.Merge(out, ord)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(
		realtime.Event{Type: "order.updated", ID: out.Id, Version: out.Version},
		realtime.Event{Type: EventUpdated, ID: id.String()},
	)
	return out, nil
}
