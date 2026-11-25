package loyalty

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	auth *auth.Service
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, authSvc *auth.Service, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, auth: authSvc, log: log}
}

func (s *Service) emit(ids ...uuid.UUID) {
	if s.hub == nil {
		return
	}
	for _, id := range ids {
		if id != uuid.Nil {
			s.hub.Broadcast(realtime.Event{Type: EventCustomerUpdated, ID: id.String()})
		}
	}
}

func requireBiller(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
}

func isOwner(a *auth.Actor) bool { return a.User.Role == auth.RoleOwner }

func isManager(a *auth.Actor) bool {
	return a.User.Role == auth.RoleOwner || a.User.Role == auth.RoleManager
}

// tx runs fn in a transaction after the pending-stamp sync (the reads count
// fully repaid bills' stamps), returning the members whose card the sync changed.
func (s *Service) tx(ctx context.Context, fn func(q *sqlc.Queries) error) ([]uuid.UUID, error) {
	var synced []uuid.UUID
	err := dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var e error
		if synced, e = SyncPending(ctx, q); e != nil {
			return e
		}
		return fn(q)
	})
	return synced, err
}

var mobileRe = regexp.MustCompile(`^9[78]\d{8}$`)

// FindMember: exact phone match only (no browsable list at the till).
func (s *Service) FindMember(ctx context.Context, req *restaurantv1.FindMemberRequest) (*restaurantv1.FindMemberResponse, error) {
	if _, err := requireBiller(ctx); err != nil {
		return nil, err
	}
	out := &restaurantv1.FindMemberResponse{}
	phone, ok := auth.NormalisePhone(req.GetPhone())
	if !ok || phone == "" {
		return out, nil
	}
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		c, e := q.LoyaltyCustomerByPhone(ctx, phone)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if !c.LoyaltyMember {
			out.CustomerName = c.Name
			return nil
		}
		st, e := settings(ctx, q)
		if e != nil {
			return e
		}
		k, e := loadCard(ctx, q, c, st, time.Now())
		if e != nil {
			return e
		}
		out.Member = k.summary()
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(synced...)
	return out, nil
}

// JoinLoyalty enrolls a guest with their consent. An existing customer with the
// phone (e.g. a credit account) is enrolled; otherwise a loyalty-only customer
// (no credit account) is created.
func (s *Service) JoinLoyalty(ctx context.Context, req *restaurantv1.JoinLoyaltyRequest) (*restaurantv1.MemberSummary, error) {
	actor, err := requireBiller(ctx)
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.MemberSummary
	var id uuid.UUID
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		st, e := settings(ctx, q)
		if e != nil {
			return e
		}
		if !st.Enabled {
			return errOff()
		}
		phone, ok := auth.NormalisePhone(req.GetPhone())
		if !ok || !mobileRe.MatchString(phone) {
			return connecterr.Invalid("phone", "a Nepali mobile number is required")
		}
		if !req.GetConsent() {
			return connecterr.Invalid("consent", "the guest must agree to join")
		}
		var birth pgtype.Int2
		if st.BirthdayRewardPaisa > 0 && req.GetBirthMonthBs() != 0 {
			if m := req.GetBirthMonthBs(); m < 1 || m > 12 {
				return connecterr.Invalid("birth_month_bs", "a month from 1 to 12")
			}
			birth = pgtype.Int2{Int16: int16(req.GetBirthMonthBs()), Valid: true}
		}
		token, e := newToken()
		if e != nil {
			return connecterr.Internal(e)
		}
		var c sqlc.Customer
		existing, e := q.LoyaltyCustomerByPhone(ctx, phone)
		switch {
		case e == nil && existing.LoyaltyMember:
			return connecterr.Duplicate("phone", "this phone is already a member")
		case e == nil:
			c, e = q.LoyaltyEnroll(ctx, sqlc.LoyaltyEnrollParams{ID: existing.ID, LoyaltyJoinedByUserID: dbx.UUID(actor.User.ID), BirthMonthBs: birth, CardToken: dbx.Text(token)})
		case errors.Is(e, pgx.ErrNoRows):
			name := strings.TrimSpace(req.GetName())
			if name == "" || utf8.RuneCountInString(name) > 80 {
				return connecterr.Invalid("name", "a name of up to 80 characters is required")
			}
			c, e = q.LoyaltyCreateMember(ctx, sqlc.LoyaltyCreateMemberParams{Name: name, Phone: phone, CreatedByUserID: dbx.UUID(actor.User.ID), BirthMonthBs: birth, CardToken: dbx.Text(token)})
		}
		if e != nil {
			var pgErr *pgconn.PgError
			if errors.As(e, &pgErr) && pgErr.Code == "23505" {
				return connecterr.Duplicate("phone", "this phone is already taken")
			}
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionJoined, Entity: "customer", EntityID: c.ID.String(),
			After: audit.JSON(map[string]any{"subject": subjectOf(c), "detail": "consent given"})}); e != nil {
			return e
		}
		k, e := readCard(ctx, q, c, st, time.Now())
		if e != nil {
			return e
		}
		out, id = k.summary(), c.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(id)
	return out, nil
}

// expireAll expires every member's card that had no visit for expiry_months.
func expireAll(ctx context.Context, q *sqlc.Queries, st sqlc.LoyaltySetting, now time.Time) ([]sqlc.ListLoyaltyMembersRow, error) {
	rows, err := q.ListLoyaltyMembers(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	expired := false
	for _, r := range rows {
		if r.LastVisitAt.Unix() <= 0 || r.Stamps == 0 || !r.LastVisitAt.AddDate(0, int(st.ExpiryMonths), 0).Before(now) {
			continue
		}
		if _, err := insertEntry(ctx, q, entry{customer: r.ID, kind: KindExpire, stamps: -r.Stamps, status: StatusDone, at: now}); err != nil {
			return nil, err
		}
		expired = true
	}
	if !expired {
		return rows, nil
	}
	if rows, err = q.ListLoyaltyMembers(ctx); err != nil {
		return nil, connecterr.Internal(err)
	}
	return rows, nil
}

func phoneFor(actor *auth.Actor, phone string) string {
	if isOwner(actor) {
		return phone
	}
	return MaskPhone(phone)
}

func (s *Service) ListMembers(ctx context.Context, _ *restaurantv1.ListMembersRequest) (*restaurantv1.ListMembersResponse, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.ListMembersResponse{Members: []*restaurantv1.MemberRow{}}
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		st, e := settings(ctx, q)
		if e != nil {
			return e
		}
		rows, e := expireAll(ctx, q, st, time.Now())
		if e != nil {
			return e
		}
		for _, r := range rows {
			row := &restaurantv1.MemberRow{
				CustomerId: r.ID.String(), Name: r.Name, Phone: phoneFor(actor, r.Phone), Stamps: r.Stamps,
				RewardsReady: max(0, r.Stamps/st.StampsPerReward), JoinedAtUnix: r.CreatedAt.Unix(),
			}
			if r.LoyaltyJoinedAt.Valid {
				row.JoinedAtUnix = r.LoyaltyJoinedAt.Time.Unix()
			}
			if r.LastVisitAt.Unix() > 0 {
				row.LastVisitAtUnix = r.LastVisitAt.Unix()
			}
			out.Members = append(out.Members, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(synced...)
	return out, nil
}

// memberCard is the full card (summary, details, ledger newest first).
func memberCard(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, c sqlc.Customer) (*restaurantv1.MemberCard, error) {
	st, err := settings(ctx, q)
	if err != nil {
		return nil, err
	}
	k, err := loadCard(ctx, q, c, st, time.Now())
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.MemberCard{
		Summary: k.summary(), Name: c.Name, Phone: phoneFor(actor, c.Phone), JoinedAtUnix: unixOf(c.LoyaltyJoinedAt),
		CardToken: c.CardToken.String, Entries: []*restaurantv1.LoyaltyEntry{},
	}
	if out.JoinedAtUnix == 0 {
		out.JoinedAtUnix = c.CreatedAt.Unix()
	}
	if c.BirthMonthBs.Valid {
		out.BirthMonthBs = int32(c.BirthMonthBs.Int16)
	}
	if c.LoyaltyJoinedByUserID.Valid {
		if u, err := q.GetUserByID(ctx, c.LoyaltyJoinedByUserID.Bytes); err == nil {
			out.JoinedByName = u.Name
		}
	}
	entries, err := q.ListLoyaltyEntries(ctx, c.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, toEntry(e))
	}
	return out, nil
}

func (s *Service) GetMember(ctx context.Context, req *restaurantv1.GetMemberRequest) (*restaurantv1.MemberCard, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("customer_id", req.GetCustomerId())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.MemberCard
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		c, e := member(ctx, q, id, false)
		if e != nil {
			return e
		}
		out, e = memberCard(ctx, q, actor, c)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(synced...)
	return out, nil
}

// AdjustStamps adds or removes stamps with a reason: owner/manager, or a cashier
// with a manager's approval. A manager's own adjustment is self-approved (the
// SELF_APPROVED alert reports it); the owner's is not checked.
func (s *Service) AdjustStamps(ctx context.Context, req *restaurantv1.AdjustStampsRequest) (*restaurantv1.MemberCard, error) {
	actor, err := requireBiller(ctx)
	if err != nil {
		return nil, err
	}
	if !isManager(actor) && req.GetApproval() == nil {
		return nil, connecterr.NotAllowed("a manager approves stamp adjustments")
	}
	id, err := parseID("customer_id", req.GetCustomerId())
	if err != nil {
		return nil, err
	}
	n := req.GetStamps()
	if n == 0 || n > 100 || n < -100 {
		return nil, connecterr.Invalid("stamps", "add or remove 1 to 100 stamps")
	}
	reason := strings.TrimSpace(req.GetReason())
	if reason == "" {
		return nil, connecterr.Invalid("reason", "a reason is required")
	}
	var approver uuid.UUID
	switch {
	case req.GetApproval() != nil:
		mgr, err := s.auth.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: req.GetApproval().GetManagerUserId(), Pin: req.GetApproval().GetPin()})
		if err != nil {
			return nil, err
		}
		approver = mgr.ID
	case actor.User.Role == auth.RoleManager:
		approver = actor.User.ID
	}
	var out *restaurantv1.MemberCard
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		c, e := member(ctx, q, id, true)
		if e != nil {
			return e
		}
		if _, e := insertEntry(ctx, q, entry{customer: c.ID, kind: KindAdjust, stamps: n, status: StatusDone, by: actor.User.ID,
			approvedBy: approver, reason: reason, at: time.Now()}); e != nil {
			return e
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionAdjusted, Entity: "customer", EntityID: c.ID.String(),
			ApproverID: approver, Reason: reason,
			After: audit.JSON(map[string]any{"subject": subjectOf(c), "stamps": n, "reason": reason, "detail": fmt.Sprintf("%+d · %s", n, reason)})}); e != nil {
			return e
		}
		out, e = memberCard(ctx, q, actor, c)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(append(synced, id)...)
	return out, nil
}

// LeaveLoyalty: the stamps are removed, the link stops working, and the phone is
// erased unless the customer still has a credit account.
func (s *Service) LeaveLoyalty(ctx context.Context, req *restaurantv1.LeaveLoyaltyRequest) (*restaurantv1.LeaveLoyaltyResponse, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("customer_id", req.GetCustomerId())
	if err != nil {
		return nil, err
	}
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		c, e := member(ctx, q, id, true)
		if e != nil {
			return e
		}
		st, e := settings(ctx, q)
		if e != nil {
			return e
		}
		k, e := loadCard(ctx, q, c, st, time.Now())
		if e != nil {
			return e
		}
		if k.stats.Stamps != 0 {
			if _, e := insertEntry(ctx, q, entry{customer: c.ID, kind: KindAdjust, stamps: -k.stats.Stamps, status: StatusDone, by: actor.User.ID,
				reason: "Left the card", at: time.Now()}); e != nil {
				return e
			}
		}
		if e := q.LoyaltyLeave(ctx, c.ID); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionLeft, Entity: "customer", EntityID: c.ID.String(),
			After: audit.JSON(map[string]any{"subject": subjectOf(c)})})
	})
	if err != nil {
		return nil, err
	}
	s.emit(append(synced, id)...)
	return &restaurantv1.LeaveLoyaltyResponse{}, nil
}

// NewCardLink issues a new status-page token; the old link stops working.
func (s *Service) NewCardLink(ctx context.Context, req *restaurantv1.NewCardLinkRequest) (*restaurantv1.MemberCard, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("customer_id", req.GetCustomerId())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.MemberCard
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		c, e := member(ctx, q, id, true)
		if e != nil {
			return e
		}
		token, e := newToken()
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := q.LoyaltySetCardToken(ctx, sqlc.LoyaltySetCardTokenParams{ID: c.ID, CardToken: dbx.Text(token)}); e != nil {
			return connecterr.Internal(e)
		}
		c.CardToken = dbx.Text(token)
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionLinkReset, Entity: "customer", EntityID: c.ID.String(),
			After: audit.JSON(map[string]any{"subject": subjectOf(c)})}); e != nil {
			return e
		}
		out, e = memberCard(ctx, q, actor, c)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(append(synced, id)...)
	return out, nil
}

func (s *Service) GetLoyaltySettings(ctx context.Context, _ *restaurantv1.GetLoyaltySettingsRequest) (*restaurantv1.LoyaltySettings, error) {
	if _, err := requireBiller(ctx); err != nil {
		return nil, err
	}
	st, err := settings(ctx, s.q)
	if err != nil {
		return nil, err
	}
	return toSettings(st), nil
}

// UpdateLoyaltySettings (owner): changes apply from now on.
func (s *Service) UpdateLoyaltySettings(ctx context.Context, req *restaurantv1.UpdateLoyaltySettingsRequest) (*restaurantv1.LoyaltySettings, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner)
	if err != nil {
		return nil, err
	}
	in := req.GetSettings()
	check := func(field string, v, lo, hi int64) error {
		if v < lo || v > hi {
			return connecterr.Invalid(field, fmt.Sprintf("must be from %d to %d", lo, hi))
		}
		return nil
	}
	for _, c := range []struct {
		field     string
		v, lo, hi int64
	}{
		{"stamps_per_reward", int64(in.GetStampsPerReward()), 1, 100},
		{"min_spend_paisa", in.GetMinSpendPaisa(), 0, 10_000_000},
		{"reward_value_paisa", in.GetRewardValuePaisa(), 100, 10_000_000},
		{"expiry_months", int64(in.GetExpiryMonths()), 1, 120},
		{"birthday_reward_paisa", in.GetBirthdayRewardPaisa(), 0, 10_000_000},
	} {
		if err := check(c.field, c.v, c.lo, c.hi); err != nil {
			return nil, err
		}
	}
	var out *restaurantv1.LoyaltySettings
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		before, e := settings(ctx, q)
		if e != nil {
			return e
		}
		st, e := q.UpdateLoyaltySettings(ctx, sqlc.UpdateLoyaltySettingsParams{
			Enabled: in.GetEnabled(), StampsPerReward: in.GetStampsPerReward(), MinSpendPaisa: in.GetMinSpendPaisa(),
			RewardValuePaisa: in.GetRewardValuePaisa(), ExpiryMonths: in.GetExpiryMonths(), CreditBillsEarn: in.GetCreditBillsEarn(),
			BirthdayRewardPaisa: in.GetBirthdayRewardPaisa(),
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		out = toSettings(st)
		onOff := "off"
		if st.Enabled {
			onOff = "on"
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionSettingsChanged, Entity: "settings", EntityID: "loyalty",
			Before: audit.JSON(toSettings(before)), After: audit.JSON(map[string]any{
				"settings": out,
				"detail": fmt.Sprintf("%s · %d stamps → Rs %d · min Rs %d · %d months", onOff, st.StampsPerReward,
					st.RewardValuePaisa/100, st.MinSpendPaisa/100, st.ExpiryMonths),
			})})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetLoyaltyReport (owner/manager): members, stamps earned and rewards redeemed
// in the range (bills still issued), what the cards are worth now, and
// redemptions by cashier.
func (s *Service) GetLoyaltyReport(ctx context.Context, req *restaurantv1.LoyaltyReportRequest) (*restaurantv1.LoyaltyReport, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	from, err := time.ParseInLocation(time.DateOnly, req.GetFrom(), nepal)
	if err != nil {
		return nil, connecterr.Invalid("from", "expected a date as YYYY-MM-DD")
	}
	to, err := time.ParseInLocation(time.DateOnly, req.GetTo(), nepal)
	if err != nil {
		return nil, connecterr.Invalid("to", "expected a date as YYYY-MM-DD")
	}
	if to.Before(from) {
		return nil, connecterr.Invalid("to", "the end date is before the start date")
	}
	end := to.AddDate(0, 0, 1)
	out := &restaurantv1.LoyaltyReport{From: req.GetFrom(), To: req.GetTo(), ByCashier: []*restaurantv1.LoyaltyCashierRow{}}
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		st, e := settings(ctx, q)
		if e != nil {
			return e
		}
		members, e := expireAll(ctx, q, st, time.Now())
		if e != nil {
			return e
		}
		activeSince := end.Add(-time.Second).Add(-90 * 24 * time.Hour)
		out.MembersTotal = int32(len(members))
		for _, m := range members {
			if !m.LastVisitAt.Before(activeSince) {
				out.MembersActive_90++
			}
			out.OutstandingPaisa += outstanding(m.Stamps, st)
		}
		if out.MembersNew, e = q.LoyaltyNewMembers(ctx, sqlc.LoyaltyNewMembersParams{FromTs: pgtype.Timestamptz{Time: from, Valid: true}, ToTs: pgtype.Timestamptz{Time: end, Valid: true}}); e != nil {
			return connecterr.Internal(e)
		}
		entries, e := q.LoyaltyReportEntries(ctx, sqlc.LoyaltyReportEntriesParams{FromTs: from, ToTs: end})
		if e != nil {
			return connecterr.Internal(e)
		}
		byCashier := map[uuid.UUID]*restaurantv1.LoyaltyCashierRow{}
		for _, en := range entries {
			if en.InvoiceStatus.String != "ISSUED" {
				continue
			}
			switch en.Kind {
			case KindEarn:
				out.StampsEarned++
			case KindRedeem, KindBirthday:
				v := en.RewardValuePaisa.Int64
				out.RedemptionsCount++
				out.RedemptionsValuePaisa += v
				who := uuid.UUID(en.IssuedBy.Bytes)
				row := byCashier[who]
				if row == nil {
					row = &restaurantv1.LoyaltyCashierRow{UserId: uuidStr(en.IssuedBy), Name: en.IssuedByName.String}
					byCashier[who] = row
				}
				row.Count++
				row.ValuePaisa += v
			}
		}
		for _, r := range byCashier {
			out.ByCashier = append(out.ByCashier, r)
		}
		sort.Slice(out.ByCashier, func(i, j int) bool {
			a, b := out.ByCashier[i], out.ByCashier[j]
			if a.ValuePaisa != b.ValuePaisa {
				return a.ValuePaisa > b.ValuePaisa
			}
			return a.Name < b.Name
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(synced...)
	return out, nil
}

// GetCardStatus is public (no login): first name, stamps, rewards ready and the
// expiry. No phone, no bill history. An unknown or replaced token is NOT_FOUND "token".
func (s *Service) GetCardStatus(ctx context.Context, req *restaurantv1.GetCardStatusRequest) (*restaurantv1.CardStatus, error) {
	token := strings.TrimSpace(req.GetToken())
	if token == "" {
		return nil, errLinkInvalid()
	}
	var out *restaurantv1.CardStatus
	synced, err := s.tx(ctx, func(q *sqlc.Queries) error {
		c, e := q.LoyaltyCustomerByToken(ctx, dbx.Text(token))
		if errors.Is(e, pgx.ErrNoRows) {
			return errLinkInvalid()
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		st, e := settings(ctx, q)
		if e != nil {
			return e
		}
		k, e := loadCard(ctx, q, c, st, time.Now())
		if e != nil {
			return e
		}
		sum := k.summary()
		out = &restaurantv1.CardStatus{FirstName: sum.FirstName, Stamps: sum.Stamps, PerReward: sum.PerReward, RewardsReady: sum.RewardsReady,
			RewardValuePaisa: sum.RewardValuePaisa, ValidUntilBs: sum.ValidUntilBs}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(synced...)
	return out, nil
}
