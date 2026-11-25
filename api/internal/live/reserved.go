package live

import (
	"context"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/reservations"
)

// reservedTables: the booking holding each table right now (HOLD or LATE), by
// table id, earliest booking first. Empty when reservations are off. The caller
// shows it only on tables with no open order (the floor's "reserved" tile).
func (s *Service) reservedTables(ctx context.Context, now time.Time) (map[uuid.UUID]*restaurantv1.LiveReservation, error) {
	out := map[uuid.UUID]*restaurantv1.LiveReservation{}
	st, err := s.q.GetReservationSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	if !st.Enabled {
		return out, nil
	}
	rows, err := s.q.ActiveBookedReservations(ctx, sqlc.ActiveBookedReservationsParams{
		Since: now.Add(-48 * time.Hour), Until: now.Add(time.Duration(st.HoldBeforeMin+1) * time.Minute),
	})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, r := range rows {
		hold := reservations.Hold("BOOKED", r.StartsAt, r.HoldExtendedMin, st, now)
		if hold != reservations.HoldHold && hold != reservations.HoldLate {
			continue
		}
		for _, id := range r.TableIds {
			if _, ok := out[id]; !ok {
				out[id] = &restaurantv1.LiveReservation{Id: r.ID.String(), Name: r.CustomerName, PartySize: r.PartySize,
					StartsAtUnix: r.StartsAt.Unix(), Late: hold == reservations.HoldLate}
			}
		}
	}
	return out, nil
}
