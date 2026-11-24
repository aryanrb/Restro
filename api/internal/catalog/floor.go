package catalog

import (
	"context"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

func (s *Service) GetFloor(ctx context.Context) (*restaurantv1.Floor, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	areas, err := s.q.ListAreas(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	tables, err := s.q.ListTables(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.Floor{
		Areas:  make([]*restaurantv1.Area, 0, len(areas)),
		Tables: make([]*restaurantv1.Table, 0, len(tables)),
	}
	for _, a := range areas {
		out.Areas = append(out.Areas, toArea(a))
	}
	for _, t := range tables {
		out.Tables = append(out.Tables, toTable(t))
	}
	return out, nil
}

func (s *Service) SaveArea(ctx context.Context, id string, name *restaurantv1.LocalText) (*restaurantv1.Area, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	var saved sqlc.Area
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var e error
		if id == "" {
			sort, _ := q.NextAreaSort(ctx)
			saved, e = q.CreateArea(ctx, sqlc.CreateAreaParams{NameEn: name.GetEn(), NameNe: name.GetNe(), Sort: sort})
		} else {
			aid, perr := parseID("id", id)
			if perr != nil {
				return perr
			}
			saved, e = q.UpdateArea(ctx, sqlc.UpdateAreaParams{ID: aid, NameEn: name.GetEn(), NameNe: name.GetNe()})
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "floor.area.save", Entity: "area", EntityID: saved.ID.String(), After: audit.JSON(saved)})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "table.updated", ID: saved.ID.String()})
	return toArea(saved), nil
}

func (s *Service) DeleteArea(ctx context.Context, id string) error {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return err
	}
	aid, err := parseID("id", id)
	if err != nil {
		return err
	}
	n, err := s.q.CountTablesInArea(ctx, aid)
	if err != nil {
		return connecterr.Internal(err)
	}
	if n > 0 {
		return connecterr.InUse("remove the area's tables first")
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.DeleteArea(ctx, aid); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "floor.area.delete", Entity: "area", EntityID: id})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "table.updated", ID: id})
	return nil
}

func (s *Service) MoveArea(ctx context.Context, id string, move restaurantv1.Move) error {
	if _, err := auth.RequireManager(ctx); err != nil {
		return err
	}
	aid, err := parseID("id", id)
	if err != nil {
		return err
	}
	areas, err := s.q.ListAreas(ctx)
	if err != nil {
		return connecterr.Internal(err)
	}
	items := make([]sortable, len(areas))
	for i, a := range areas {
		items[i] = sortable{ID: a.ID, Sort: a.Sort}
	}
	a, b, ok := neighborSwap(items, aid, moveUp(move))
	if !ok {
		return nil
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.UpdateAreaSort(ctx, sqlc.UpdateAreaSortParams{ID: a.ID, Sort: a.Sort}); e != nil {
			return connecterr.Internal(e)
		}
		return q.UpdateAreaSort(ctx, sqlc.UpdateAreaSortParams{ID: b.ID, Sort: b.Sort})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "table.updated", ID: id})
	return nil
}

func (s *Service) SaveTable(ctx context.Context, id, areaID, name string, seats int32, active bool) (*restaurantv1.Table, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	aid, err := parseID("area_id", areaID)
	if err != nil {
		return nil, err
	}
	var saved sqlc.DiningTable
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var e error
		if id == "" {
			sort, _ := q.NextTableSort(ctx)
			saved, e = q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: aid, Name: name, Seats: seats, Active: active, Sort: sort})
		} else {
			tid, perr := parseID("id", id)
			if perr != nil {
				return perr
			}
			saved, e = q.UpdateTable(ctx, sqlc.UpdateTableParams{ID: tid, AreaID: aid, Name: name, Seats: seats, Active: active})
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "floor.table.save", Entity: "table", EntityID: saved.ID.String(), After: audit.JSON(saved)})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "table.updated", ID: saved.ID.String()})
	return toTable(saved), nil
}

func (s *Service) MoveTable(ctx context.Context, id string, move restaurantv1.Move) error {
	if _, err := auth.RequireManager(ctx); err != nil {
		return err
	}
	tid, err := parseID("id", id)
	if err != nil {
		return err
	}
	tables, err := s.q.ListTables(ctx)
	if err != nil {
		return connecterr.Internal(err)
	}
	items := make([]sortable, len(tables))
	for i, t := range tables {
		items[i] = sortable{ID: t.ID, Sort: t.Sort}
	}
	a, b, ok := neighborSwap(items, tid, moveUp(move))
	if !ok {
		return nil
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.UpdateTableSort(ctx, sqlc.UpdateTableSortParams{ID: a.ID, Sort: a.Sort}); e != nil {
			return connecterr.Internal(e)
		}
		return q.UpdateTableSort(ctx, sqlc.UpdateTableSortParams{ID: b.ID, Sort: b.Sort})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "table.updated", ID: id})
	return nil
}

// --- Stations ---

func (s *Service) ListStations(ctx context.Context) ([]*restaurantv1.Station, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListStations(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.Station, 0, len(rows))
	for _, st := range rows {
		out = append(out, toStation(st))
	}
	return out, nil
}

func (s *Service) SaveStation(ctx context.Context, id string, name *restaurantv1.LocalText) (*restaurantv1.Station, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	var saved sqlc.Station
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var e error
		if id == "" {
			sort, _ := q.NextStationSort(ctx)
			saved, e = q.CreateStation(ctx, sqlc.CreateStationParams{NameEn: name.GetEn(), NameNe: name.GetNe(), Sort: sort})
		} else {
			sid, perr := parseID("id", id)
			if perr != nil {
				return perr
			}
			saved, e = q.UpdateStation(ctx, sqlc.UpdateStationParams{ID: sid, NameEn: name.GetEn(), NameNe: name.GetNe()})
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "station.save", Entity: "station", EntityID: saved.ID.String(), After: audit.JSON(saved)})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "station.updated", ID: saved.ID.String()})
	return toStation(saved), nil
}

func (s *Service) DeleteStation(ctx context.Context, id string) error {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return err
	}
	sid, err := parseID("id", id)
	if err != nil {
		return err
	}
	used, err := s.q.CountItemsUsingStation(ctx, sid)
	if err != nil {
		return connecterr.Internal(err)
	}
	if used > 0 {
		return connecterr.InUse("menu items use this station")
	}
	count, err := s.q.CountStations(ctx)
	if err != nil {
		return connecterr.Internal(err)
	}
	if count <= 1 {
		return connecterr.InUse("at least one station is required")
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.DeleteStation(ctx, sid); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "station.delete", Entity: "station", EntityID: id})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "station.updated", ID: id})
	return nil
}
