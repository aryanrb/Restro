package reservations

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// parseRange turns Kathmandu dates from–to (inclusive) into [start, end) instants.
func parseRange(from, to string, maxDays int) (time.Time, time.Time, error) {
	a, err := time.ParseInLocation("2006-01-02", from, nepal)
	if err != nil {
		return a, a, connecterr.Invalid("from", "a date YYYY-MM-DD")
	}
	b, err := time.ParseInLocation("2006-01-02", to, nepal)
	if err != nil {
		return a, b, connecterr.Invalid("to", "a date YYYY-MM-DD")
	}
	if b.Before(a) || b.Sub(a) > time.Duration(maxDays)*24*time.Hour {
		return a, b, connecterr.Invalid("to", "to must be on or after from, within range")
	}
	return a, b.AddDate(0, 0, 1), nil
}

func (s *Service) ListReservations(ctx context.Context, req *restaurantv1.ListReservationsRequest) (*restaurantv1.ListReservationsResponse, error) {
	if _, err := requireBooker(ctx); err != nil {
		return nil, err
	}
	from, to, err := parseRange(req.GetFrom(), req.GetTo(), 400)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListReservationsBetween(ctx, sqlc.ListReservationsBetweenParams{FromAt: from, ToAt: to})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListReservationsResponse{Reservations: []*restaurantv1.Reservation{}}
	for _, r := range rows {
		p := toProto(r.Reservation, r.CustomerName, r.CustomerPhone)
		if req.GetStatus() == restaurantv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED || p.Status == req.GetStatus() {
			out.Reservations = append(out.Reservations, p)
		}
	}
	return out, nil
}

func (s *Service) GetReservation(ctx context.Context, req *restaurantv1.GetReservationRequest) (*restaurantv1.Reservation, error) {
	if _, err := requireBooker(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.q, id)
}

func (s *Service) ListCustomerReservations(ctx context.Context, req *restaurantv1.ListCustomerReservationsRequest) (*restaurantv1.ListReservationsResponse, error) {
	if _, err := requireBooker(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("customer_id", req.GetCustomerId())
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListCustomerReservations(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListReservationsResponse{Reservations: []*restaurantv1.Reservation{}}
	for _, r := range rows {
		out.Reservations = append(out.Reservations, toProto(r.Reservation, r.CustomerName, r.CustomerPhone))
	}
	return out, nil
}

// SuggestTables: up to three table sets for a party that are not booked in the
// window. Single tables that fit (preferred area first, smallest first), or, when
// none fits, the biggest free tables of one area put together until the party fits.
func (s *Service) SuggestTables(ctx context.Context, req *restaurantv1.SuggestTablesRequest) (*restaurantv1.SuggestTablesResponse, error) {
	if _, err := requireBooker(ctx); err != nil {
		return nil, err
	}
	var exclude, area uuid.UUID
	var err error
	if req.GetExcludeId() != "" {
		if exclude, err = parseID("exclude_id", req.GetExcludeId()); err != nil {
			return nil, err
		}
	}
	if req.GetAreaId() != "" {
		if area, err = parseID("area_id", req.GetAreaId()); err != nil {
			return nil, err
		}
	}
	if req.GetDurationMin() < 1 || req.GetPartySize() < 1 {
		return nil, connecterr.Invalid("party_size", "party size and duration are required")
	}
	start := time.Unix(req.GetStartsAtUnix(), 0)
	busy, err := s.q.BookedTablesInWindow(ctx, sqlc.BookedTablesInWindowParams{
		ExcludeID: exclude, StartsAt: start, EndsAt: start.Add(time.Duration(req.GetDurationMin()) * time.Minute),
	})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	tables, err := s.q.ListTables(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.SuggestTablesResponse{Sets: []*restaurantv1.TableSet{}}
	for _, set := range suggest(tables, req.GetPartySize(), area, busy) {
		out.Sets = append(out.Sets, &restaurantv1.TableSet{TableIds: ids(set)})
	}
	return out, nil
}

func suggest(all []sqlc.DiningTable, party int32, area uuid.UUID, busy []uuid.UUID) [][]uuid.UUID {
	var free []sqlc.DiningTable
	for _, t := range all {
		if t.Active && !contains(busy, t.ID) {
			free = append(free, t)
		}
	}
	pref := func(t sqlc.DiningTable) int {
		if area != uuid.Nil && t.AreaID != area {
			return 1
		}
		return 0
	}
	var singles []sqlc.DiningTable
	for _, t := range free {
		if t.Seats >= party {
			singles = append(singles, t)
		}
	}
	sort.SliceStable(singles, func(i, j int) bool {
		a, b := singles[i], singles[j]
		if pref(a) != pref(b) {
			return pref(a) < pref(b)
		}
		if a.Seats != b.Seats {
			return a.Seats < b.Seats
		}
		return a.Sort < b.Sort
	})
	var sets [][]uuid.UUID
	for i := 0; i < len(singles) && i < 3; i++ {
		sets = append(sets, []uuid.UUID{singles[i].ID})
	}
	if len(sets) > 0 {
		return sets
	}
	var areas []uuid.UUID
	for _, t := range free {
		if !contains(areas, t.AreaID) {
			areas = append(areas, t.AreaID)
		}
	}
	sort.SliceStable(areas, func(i, j int) bool { return areas[i] == area && areas[j] != area })
	for _, a := range areas {
		var mine []sqlc.DiningTable
		for _, t := range free {
			if t.AreaID == a {
				mine = append(mine, t)
			}
		}
		sort.SliceStable(mine, func(i, j int) bool { return mine[i].Seats > mine[j].Seats })
		var pick []uuid.UUID
		var seats int32
		for _, t := range mine {
			if seats >= party {
				break
			}
			pick = append(pick, t.ID)
			seats += t.Seats
		}
		if seats >= party {
			sets = append(sets, pick)
		}
		if len(sets) == 3 {
			break
		}
	}
	return sets
}

func contains(list []uuid.UUID, id uuid.UUID) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

var countryCodeRe = regexp.MustCompile(`^(\+|00)977`)

// matches: name contains the query (any case), or the phone contains its digits
// (3 or more, without a +977 / 00977 prefix) — as the credit customer search.
func matches(name, phone, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if strings.Contains(strings.ToLower(name), q) {
		return true
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, q)
	if countryCodeRe.MatchString(q) {
		digits = strings.TrimPrefix(strings.TrimPrefix(digits, "00"), "977")
	}
	return len(digits) >= 3 && strings.Contains(phone, digits)
}

// FindBookingCustomers is the phone-first lookup for booking staff (any role but
// kitchen): no credit figures, booking history derived from bookings.
func (s *Service) FindBookingCustomers(ctx context.Context, req *restaurantv1.FindBookingCustomersRequest) (*restaurantv1.FindBookingCustomersResponse, error) {
	if _, err := requireBooker(ctx); err != nil {
		return nil, err
	}
	out := &restaurantv1.FindBookingCustomersResponse{Customers: []*restaurantv1.BookingCustomer{}}
	if strings.TrimSpace(req.GetQuery()) == "" {
		return out, nil
	}
	all, err := s.q.ListBookingCustomers(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	var hits []uuid.UUID
	for _, c := range all {
		if len(hits) == 10 {
			break
		}
		if matches(c.Name, c.Phone, req.GetQuery()) {
			hits = append(hits, c.ID)
			out.Customers = append(out.Customers, &restaurantv1.BookingCustomer{Id: c.ID.String(), Name: c.Name, Phone: c.Phone, Notes: c.Notes})
		}
	}
	if len(hits) == 0 {
		return out, nil
	}
	stats, err := s.q.CustomerBookingStats(ctx, hits)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, st := range stats {
		for _, c := range out.Customers {
			if c.Id == st.CustomerID.String() {
				c.BookingCount, c.NoShowCount = st.BookingCount, st.NoShowCount
				if st.LastVisitAt.Unix() > 0 {
					c.LastVisitAtUnix = st.LastVisitAt.Unix()
				}
			}
		}
	}
	return out, nil
}

// --- settings ---

func settingsProto(st sqlc.ReservationSetting) *restaurantv1.ReservationSettings {
	return &restaurantv1.ReservationSettings{
		Enabled: st.Enabled, DurationMin: st.DurationMin, LargePartySize: st.LargePartySize, LargeDurationMin: st.LargeDurationMin,
		HoldBeforeMin: st.HoldBeforeMin, GraceMin: st.GraceMin, ReleaseAfterMin: st.ReleaseAfterMin,
	}
}

func (s *Service) GetReservationSettings(ctx context.Context, _ *restaurantv1.GetReservationSettingsRequest) (*restaurantv1.ReservationSettings, error) {
	if _, err := requireBooker(ctx); err != nil {
		return nil, err
	}
	st, err := s.q.GetReservationSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return settingsProto(st), nil
}

func (s *Service) SaveReservationSettings(ctx context.Context, req *restaurantv1.ReservationSettings) (*restaurantv1.ReservationSettings, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range []struct {
		field    string
		v        int32
		min, max int32
	}{
		{"duration_min", req.GetDurationMin(), 15, 720}, {"large_party_size", req.GetLargePartySize(), 2, 200},
		{"large_duration_min", req.GetLargeDurationMin(), 15, 720}, {"hold_before_min", req.GetHoldBeforeMin(), 0, 600},
		{"grace_min", req.GetGraceMin(), 0, 240}, {"release_after_min", req.GetReleaseAfterMin(), 0, 480},
	} {
		if c.v < c.min || c.v > c.max {
			return nil, connecterr.Invalid(c.field, "out of range")
		}
	}
	if req.GetReleaseAfterMin() < req.GetGraceMin() {
		return nil, connecterr.Invalid("release_after_min", "release after must be at least the grace")
	}
	var out *restaurantv1.ReservationSettings
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		before, e := q.GetReservationSettings(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		st, e := q.SaveReservationSettings(ctx, sqlc.SaveReservationSettingsParams{
			Enabled: req.GetEnabled(), DurationMin: req.GetDurationMin(), LargePartySize: req.GetLargePartySize(),
			LargeDurationMin: req.GetLargeDurationMin(), HoldBeforeMin: req.GetHoldBeforeMin(), GraceMin: req.GetGraceMin(),
			ReleaseAfterMin: req.GetReleaseAfterMin(),
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		out = settingsProto(st)
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionSettings, Entity: "settings", EntityID: "reservations",
			Before: audit.JSON(settingsProto(before)), After: audit.JSON(out)})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventUpdated, ID: "settings"})
	return out, nil
}

// --- report ---

type dayRow = restaurantv1.ReservationReportRow

func emptyRow(date string) *dayRow {
	row := &dayRow{Date: date, BySource: &restaurantv1.ReservationSourceCounts{}}
	if d, err := time.ParseInLocation("2006-01-02", date, nepal); err == nil {
		if bs, err := bsdate.FromAD(d.Add(12 * time.Hour)); err == nil {
			row.DateBs = bs.String()
		}
	}
	return row
}

// rows: per Kathmandu day of starts_at. Covers leave out cancelled bookings and no-shows.
func (s *Service) rows(ctx context.Context, from, to time.Time) ([]*dayRow, error) {
	list, err := s.q.ListReservationsBetween(ctx, sqlc.ListReservationsBetweenParams{FromAt: from, ToAt: to})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	var out []*dayRow
	byDate := map[string]*dayRow{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		row := emptyRow(ktmDate(d))
		byDate[row.Date] = row
		out = append(out, row)
	}
	for _, v := range list {
		r := v.Reservation
		row := byDate[ktmDate(r.StartsAt)]
		if row == nil {
			continue
		}
		row.Bookings++
		switch r.Status {
		case statusBooked:
			row.Open++
			row.Covers += r.PartySize
		case statusSeated:
			row.Seated++
			row.Covers += r.PartySize
		case statusCancelled:
			row.Cancelled++
		case statusNoShow:
			row.NoShows++
		}
		switch r.Source {
		case "PHONE":
			row.BySource.Phone++
		case "IN_PERSON":
			row.BySource.InPerson++
		case "MESSAGE":
			row.BySource.Message++
		default:
			row.BySource.Other++
		}
	}
	return out, nil
}

func (s *Service) ReservationReport(ctx context.Context, req *restaurantv1.ReservationReportRequest) (*restaurantv1.ReservationReportResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	from, to, err := parseRange(req.GetFrom(), req.GetTo(), 400)
	if err != nil {
		return nil, err
	}
	rows, err := s.rows(ctx, from, to)
	if err != nil {
		return nil, err
	}
	return &restaurantv1.ReservationReportResponse{Rows: rows}, nil
}

// GetReservationsToday is the owner/manager dashboard tile.
func (s *Service) GetReservationsToday(ctx context.Context, _ *restaurantv1.ReservationsTodayRequest) (*restaurantv1.ReservationsToday, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	today := ktmDate(s.now())
	from, to, err := parseRange(today, today, 1)
	if err != nil {
		return nil, err
	}
	rows, err := s.rows(ctx, from, to)
	if err != nil {
		return nil, err
	}
	r := rows[0]
	return &restaurantv1.ReservationsToday{Bookings: r.Bookings - r.Cancelled, Covers: r.Covers, NoShows: r.NoShows}, nil
}
