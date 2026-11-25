package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

func countStatusEnum(s string) restaurantv1.CountStatus {
	switch s {
	case "OPEN":
		return restaurantv1.CountStatus_COUNT_STATUS_OPEN
	case "SUBMITTED":
		return restaurantv1.CountStatus_COUNT_STATUS_SUBMITTED
	case "POSTED":
		return restaurantv1.CountStatus_COUNT_STATUS_POSTED
	case "DISCARDED":
		return restaurantv1.CountStatus_COUNT_STATUS_DISCARDED
	}
	return restaurantv1.CountStatus_COUNT_STATUS_UNSPECIFIED
}

func optInt(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func int8Of(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: true} }

// countView renders a count; cashier/kitchen never see expected, variance or value (blind).
func countView(c sqlc.StockCount, lines []sqlc.StockCountLine, names map[uuid.UUID]string, admin bool) *restaurantv1.StockCount {
	out := &restaurantv1.StockCount{
		Id: c.ID.String(), Number: c.Number, Status: countStatusEnum(c.Status), ListId: pgID(c.ListID), Label: c.Label,
		StartedByUserId: c.StartedByUserID.String(), StartedByName: names[c.StartedByUserID], StartedAtUnix: c.StartedAt.Unix(),
		SubmittedByUserId: pgID(c.SubmittedByUserID), SubmittedAtUnix: unix(c.SubmittedAt),
		PostedByUserId: pgID(c.PostedByUserID), PostedAtUnix: unix(c.PostedAt),
		DiscardedReason: c.DiscardedReason, DiscardedAtUnix: unix(c.DiscardedAt),
		Lines: make([]*restaurantv1.CountLine, 0, len(lines)),
	}
	if admin {
		out.TotalValuePaisa = optInt(c.TotalValuePaisa)
	}
	for _, l := range lines {
		cl := &restaurantv1.CountLine{
			InventoryItemId: l.InventoryItemID.String(), CountedMilli: optInt(l.CountedMilli), CountedAtUnix: unix(l.CountedAt),
			CountedByUserId: pgID(l.CountedByUserID), Recount: l.Recount,
		}
		if admin {
			cl.ExpectedMilli, cl.VarianceMilli, cl.ValuePaisa = optInt(l.ExpectedMilli), optInt(l.VarianceMilli), optInt(l.ValuePaisa)
		}
		out.Lines = append(out.Lines, cl)
	}
	return out
}

func (s *Service) loadCount(ctx context.Context, q *sqlc.Queries, c sqlc.StockCount, admin bool) (*restaurantv1.StockCount, error) {
	lines, err := q.ListCountLines(ctx, c.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	names, err := userNames(ctx, q, c.StartedByUserID)
	if err != nil {
		return nil, err
	}
	return countView(c, lines, names, admin), nil
}

// getCountForUpdate locks a count row (NOT_FOUND when unknown).
func getCountForUpdate(ctx context.Context, q *sqlc.Queries, id string) (sqlc.StockCount, error) {
	cid, err := parseID("count_id", id)
	if err != nil {
		return sqlc.StockCount{}, err
	}
	c, err := q.GetStockCountForUpdate(ctx, cid)
	if err != nil {
		return sqlc.StockCount{}, notFound(err, "count")
	}
	return c, nil
}

// StartCount starts a blind count of all tracked items, a saved list, or chosen items.
func (s *Service) StartCount(ctx context.Context, req *restaurantv1.StartCountRequest) (*restaurantv1.StockCount, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	var items []sqlc.InventoryItem
	var label string
	var listID pgtype.UUID
	switch {
	case len(req.GetInventoryItemIds()) > 0:
		var names []string
		for _, raw := range req.GetInventoryItemIds() {
			id, e := parseID("inventory_item_ids", raw)
			if e != nil {
				return nil, e
			}
			it, e := s.q.GetInventoryItem(ctx, id)
			if e != nil {
				return nil, notFound(e, "stock item")
			}
			if !live(it) {
				return nil, itemUntracked(it.NameEn + " is not tracked")
			}
			if !slices.ContainsFunc(items, func(x sqlc.InventoryItem) bool { return x.ID == id }) {
				items = append(items, it)
				names = append(names, it.NameEn)
			}
		}
		label = strings.Join(names, ", ")
	case req.GetListId() != "":
		id, e := parseID("list_id", req.GetListId())
		if e != nil {
			return nil, e
		}
		list, e := s.q.GetCountList(ctx, id)
		if e != nil {
			return nil, notFound(e, "count list")
		}
		all, e := s.q.ListInventoryItemsByIDs(ctx, list.InventoryItemIds)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		for _, it := range all {
			if live(it) {
				items = append(items, it)
			}
		}
		label, listID = list.Name, dbx.UUID(list.ID)
	default:
		all, e := s.q.ListInventoryItems(ctx, false)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		for _, it := range all {
			if live(it) {
				items = append(items, it)
			}
		}
	}
	if len(items) == 0 {
		return nil, connecterr.Invalid("lines", "there are no tracked items to count")
	}
	slices.SortStableFunc(items, func(a, b sqlc.InventoryItem) int { return int(a.Sort) - int(b.Sort) })

	var out *restaurantv1.StockCount
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		c, e := q.CreateStockCount(ctx, sqlc.CreateStockCountParams{ListID: listID, Label: label, StartedByUserID: actor.User.ID, StartedAt: now()})
		if e != nil {
			return connecterr.Internal(e)
		}
		for i, it := range items {
			if e := q.InsertCountLine(ctx, sqlc.InsertCountLineParams{CountID: c.ID, InventoryItemID: it.ID, Sort: int32(i)}); e != nil {
				return connecterr.Internal(e)
			}
		}
		out, e = s.loadCount(ctx, q, c, isAdmin(actor))
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "count.updated", ID: out.Id})
	return out, nil
}

// SaveCountLines records counted quantities (OPEN only); each counted line gets its
// own timestamp, so expected is what was on hand at that moment. Absent clears.
func (s *Service) SaveCountLines(ctx context.Context, req *restaurantv1.SaveCountLinesRequest) (*restaurantv1.StockCount, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	return s.countTx(ctx, actor, req.GetCountId(), func(q *sqlc.Queries, c sqlc.StockCount) error {
		if c.Status != "OPEN" {
			return connecterr.Invalid("status", "this count is no longer open")
		}
		type change struct {
			id      uuid.UUID
			counted *int64
		}
		var changes []change
		for _, l := range req.GetLines() {
			id, e := uuid.Parse(l.GetInventoryItemId())
			if e != nil {
				return connecterr.Invalid("lines", "item is not on this count")
			}
			if l.CountedMilli != nil && l.GetCountedMilli() < 0 {
				return connecterr.Invalid("lines", "a count can't be negative")
			}
			changes = append(changes, change{id, l.CountedMilli})
		}
		at := now()
		seq, e := q.MaxMovementSeq(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		for _, ch := range changes {
			var n int64
			if ch.counted == nil {
				n, e = q.ClearCountLine(ctx, sqlc.ClearCountLineParams{CountID: c.ID, InventoryItemID: ch.id})
			} else {
				n, e = q.SetCountLine(ctx, sqlc.SetCountLineParams{
					CountID: c.ID, InventoryItemID: ch.id, CountedMilli: int8Of(*ch.counted), CountedAt: ts(at), CountedSeq: int8Of(seq), CountedByUserID: dbx.UUID(actor.User.ID),
				})
			}
			if e != nil {
				return connecterr.Internal(e)
			}
			if n == 0 {
				return connecterr.Invalid("lines", "item is not on this count")
			}
		}
		return nil
	})
}

// countTx runs fn on a locked count and returns the count as the actor may see it.
func (s *Service) countTx(ctx context.Context, actor *auth.Actor, id string, fn func(q *sqlc.Queries, c sqlc.StockCount) error) (*restaurantv1.StockCount, error) {
	var out *restaurantv1.StockCount
	err := dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		c, e := getCountForUpdate(ctx, q, id)
		if e != nil {
			return e
		}
		if e := fn(q, c); e != nil {
			return e
		}
		if c, e = q.GetStockCount(ctx, c.ID); e != nil {
			return connecterr.Internal(e)
		}
		out, e = s.loadCount(ctx, q, c, isAdmin(actor))
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "count.updated", ID: out.Id})
	return out, nil
}

func (s *Service) SubmitCount(ctx context.Context, id string) (*restaurantv1.StockCount, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	return s.countTx(ctx, actor, id, func(q *sqlc.Queries, c sqlc.StockCount) error {
		if c.Status != "OPEN" {
			return connecterr.Invalid("status", "this count is no longer open")
		}
		lines, e := q.ListCountLines(ctx, c.ID)
		if e != nil {
			return connecterr.Internal(e)
		}
		for _, l := range lines {
			if !l.CountedMilli.Valid {
				return connecterr.Invalid("lines", "count every line before submitting")
			}
		}
		if e := q.SubmitStockCount(ctx, sqlc.SubmitStockCountParams{ID: c.ID, SubmittedByUserID: dbx.UUID(actor.User.ID), SubmittedAt: ts(now())}); e != nil {
			return connecterr.Internal(e)
		}
		return nil
	})
}

// RecountLines (owner/manager) sends a submitted count back to OPEN with the chosen
// lines cleared and marked for a recount.
func (s *Service) RecountLines(ctx context.Context, req *restaurantv1.RecountLinesRequest) (*restaurantv1.StockCount, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	return s.countTx(ctx, actor, req.GetCountId(), func(q *sqlc.Queries, c sqlc.StockCount) error {
		if c.Status != "SUBMITTED" {
			return connecterr.Invalid("status", "only a submitted count can be sent back for a recount")
		}
		n := int64(0)
		for _, raw := range req.GetInventoryItemIds() {
			id, e := uuid.Parse(raw)
			if e != nil {
				continue
			}
			k, e := q.ClearCountLine(ctx, sqlc.ClearCountLineParams{CountID: c.ID, InventoryItemID: id, Recount: true})
			if e != nil {
				return connecterr.Internal(e)
			}
			n += k
		}
		if n == 0 {
			return connecterr.Invalid("inventory_item_ids", "choose lines on this count")
		}
		if e := q.ReopenStockCount(ctx, c.ID); e != nil {
			return connecterr.Internal(e)
		}
		return nil
	})
}

// PostCount (owner/manager) computes expected (on hand at each line's counted
// moment), variance and value, and writes one COUNT_ADJUST per line at its counted
// time, all in one transaction.
func (s *Service) PostCount(ctx context.Context, id string) (*restaurantv1.StockCount, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	var total int64
	return s.countTx(ctx, actor, id, func(q *sqlc.Queries, c sqlc.StockCount) error {
		if c.Status == "POSTED" {
			return countAlreadyPosted()
		}
		if c.Status != "SUBMITTED" {
			return connecterr.Invalid("status", "submit the count before posting it")
		}
		lines, e := q.ListCountLines(ctx, c.ID)
		if e != nil {
			return connecterr.Internal(e)
		}
		// expected for every line first, then the adjustments
		var moves []move
		for _, l := range lines {
			if !l.CountedMilli.Valid || !l.CountedAt.Valid {
				return connecterr.Invalid("lines", "count every line before posting")
			}
			it, e := q.GetInventoryItem(ctx, l.InventoryItemID)
			if e != nil {
				return connecterr.Internal(e)
			}
			expected, e := q.OnHandAt(ctx, sqlc.OnHandAtParams{InventoryItemID: it.ID, At: l.CountedAt.Time, Seq: l.CountedSeq.Int64})
			if e != nil {
				return connecterr.Internal(e)
			}
			variance := l.CountedMilli.Int64 - expected
			value := valuePaisa(term{variance, it.UnitCostE4})
			total += value
			if e := q.SetCountLineResult(ctx, sqlc.SetCountLineResultParams{
				CountID: c.ID, InventoryItemID: it.ID, ExpectedMilli: int8Of(expected), VarianceMilli: int8Of(variance), ValuePaisa: int8Of(value),
			}); e != nil {
				return connecterr.Internal(e)
			}
			moves = append(moves, move{itemID: it.ID, qtyMilli: variance, typ: TypeCountAdjust, ref: c.ID, by: actor.User.ID, at: l.CountedAt.Time, costE4: it.UnitCostE4})
		}
		if e := record(ctx, q, moves); e != nil {
			return e
		}
		if e := q.PostStockCount(ctx, sqlc.PostStockCountParams{ID: c.ID, PostedByUserID: dbx.UUID(actor.User.ID), PostedAt: ts(now()), TotalValuePaisa: int8Of(total)}); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "STOCK_COUNT_POSTED", Entity: "stock_count", EntityID: c.ID.String(),
			Reason: fmt.Sprintf("Count #%d · total variance %d paisa", c.Number, total), After: audit.JSON(map[string]any{"total": total, "number": c.Number}),
		})
	})
}

func (s *Service) DiscardCount(ctx context.Context, id, reason string) (*restaurantv1.StockCount, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	return s.countTx(ctx, actor, id, func(q *sqlc.Queries, c sqlc.StockCount) error {
		if reason == "" {
			return connecterr.Invalid("reason", "say why the count is discarded")
		}
		if c.Status == "POSTED" {
			return countAlreadyPosted()
		}
		if c.Status == "DISCARDED" {
			return connecterr.Invalid("status", "this count is already discarded")
		}
		if e := q.DiscardStockCount(ctx, sqlc.DiscardStockCountParams{ID: c.ID, DiscardedReason: reason, DiscardedAt: ts(now())}); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "STOCK_COUNT_DISCARDED", Entity: "stock_count", EntityID: c.ID.String(),
			Reason: fmt.Sprintf("Count #%d · %s", c.Number, reason),
		})
	})
}

func (s *Service) GetCount(ctx context.Context, id string) (*restaurantv1.StockCount, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	cid, err := parseID("count_id", id)
	if err != nil {
		return nil, err
	}
	c, err := s.q.GetStockCount(ctx, cid)
	if err != nil {
		return nil, notFound(err, "count")
	}
	return s.loadCount(ctx, s.q, c, isAdmin(actor))
}

// ListCounts is newest first.
func (s *Service) ListCounts(ctx context.Context) ([]*restaurantv1.StockCount, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.q.ListStockCounts(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	lines, err := s.q.ListAllCountLines(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	byCount := map[uuid.UUID][]sqlc.StockCountLine{}
	for _, l := range lines {
		byCount[l.CountID] = append(byCount[l.CountID], l)
	}
	var ids []uuid.UUID
	for _, c := range counts {
		ids = append(ids, c.StartedByUserID)
	}
	names, err := userNames(ctx, s.q, ids...)
	if err != nil {
		return nil, err
	}
	out := make([]*restaurantv1.StockCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, countView(c, byCount[c.ID], names, isAdmin(actor)))
	}
	return out, nil
}

func countListView(l sqlc.CountList) *restaurantv1.CountList {
	out := &restaurantv1.CountList{Id: l.ID.String(), Name: l.Name, InventoryItemIds: make([]string, 0, len(l.InventoryItemIds))}
	for _, id := range l.InventoryItemIds {
		out.InventoryItemIds = append(out.InventoryItemIds, id.String())
	}
	return out
}

// SaveCountList (owner/manager) saves a named list of items to count together.
func (s *Service) SaveCountList(ctx context.Context, req *restaurantv1.SaveCountListRequest) (*restaurantv1.CountList, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, connecterr.Invalid("name", "a list needs a name")
	}
	var ids []uuid.UUID
	for _, raw := range req.GetInventoryItemIds() {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, connecterr.Invalid("inventory_item_ids", "unknown stock item")
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, connecterr.Invalid("inventory_item_ids", "choose at least one item")
	}
	found, err := s.q.ListInventoryItemsByIDs(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	if len(found) != len(ids) {
		return nil, connecterr.Invalid("inventory_item_ids", "unknown stock item")
	}
	var saved sqlc.CountList
	if req.GetId() == "" {
		saved, err = s.q.CreateCountList(ctx, sqlc.CreateCountListParams{Name: name, InventoryItemIds: ids})
	} else {
		id, perr := parseID("id", req.GetId())
		if perr != nil {
			return nil, perr
		}
		saved, err = s.q.UpdateCountList(ctx, sqlc.UpdateCountListParams{ID: id, Name: name, InventoryItemIds: ids})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("count list not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	s.emit(realtime.Event{Type: "count.updated", ID: "lists"})
	return countListView(saved), nil
}

func (s *Service) ListCountLists(ctx context.Context) ([]*restaurantv1.CountList, error) {
	if _, err := requireStock(ctx); err != nil {
		return nil, err
	}
	lists, err := s.q.ListCountLists(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.CountList, 0, len(lists))
	for _, l := range lists {
		out = append(out, countListView(l))
	}
	return out, nil
}
