package catalog

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

const maxImageBytes = 1 << 20 // 1 MB

func (s *Service) GetMenu(ctx context.Context) (*restaurantv1.Menu, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	cats, err := s.q.ListCategories(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	items, err := s.q.ListItems(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	variants, err := s.q.ListVariants(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	groups, err := s.q.ListModifierGroups(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	mods, err := s.q.ListModifiers(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	links, err := s.q.ListItemModifierGroups(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}

	variantsByItem := map[uuid.UUID][]sqlc.ItemVariant{}
	for _, v := range variants {
		variantsByItem[v.ItemID] = append(variantsByItem[v.ItemID], v)
	}
	groupIDsByItem := map[uuid.UUID][]string{}
	for _, l := range links {
		groupIDsByItem[l.ItemID] = append(groupIDsByItem[l.ItemID], l.GroupID.String())
	}
	modsByGroup := map[uuid.UUID][]sqlc.Modifier{}
	for _, m := range mods {
		modsByGroup[m.GroupID] = append(modsByGroup[m.GroupID], m)
	}

	out := &restaurantv1.Menu{}
	for _, c := range cats {
		out.Categories = append(out.Categories, toCategory(c))
	}
	for _, it := range items {
		out.Items = append(out.Items, toMenuItem(it, variantsByItem[it.ID], groupIDsByItem[it.ID]))
	}
	for _, g := range groups {
		out.ModifierGroups = append(out.ModifierGroups, toModifierGroup(g, modsByGroup[g.ID]))
	}
	return out, nil
}

// --- categories ---

func (s *Service) SaveCategory(ctx context.Context, id string, name *restaurantv1.LocalText, active bool) (*restaurantv1.Category, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	var saved sqlc.MenuCategory
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var e error
		if id == "" {
			sort, _ := q.NextCategorySort(ctx)
			saved, e = q.CreateCategory(ctx, sqlc.CreateCategoryParams{NameEn: name.GetEn(), NameNe: name.GetNe(), Active: active, Sort: sort})
		} else {
			cid, perr := parseID("id", id)
			if perr != nil {
				return perr
			}
			saved, e = q.UpdateCategory(ctx, sqlc.UpdateCategoryParams{ID: cid, NameEn: name.GetEn(), NameNe: name.GetNe(), Active: active})
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "menu.category.save", Entity: "menu_category", EntityID: saved.ID.String(), After: audit.JSON(saved)})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return toCategory(saved), nil
}

func (s *Service) DeleteCategory(ctx context.Context, id string) error {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return err
	}
	cid, err := parseID("id", id)
	if err != nil {
		return err
	}
	n, err := s.q.CountItemsInCategory(ctx, cid)
	if err != nil {
		return connecterr.Internal(err)
	}
	if n > 0 {
		return connecterr.InUse("move or delete the category's items first")
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.DeleteCategory(ctx, cid); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "menu.category.delete", Entity: "menu_category", EntityID: id})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return nil
}

func (s *Service) MoveCategory(ctx context.Context, id string, move restaurantv1.Move) error {
	if _, err := auth.RequireManager(ctx); err != nil {
		return err
	}
	cid, err := parseID("id", id)
	if err != nil {
		return err
	}
	cats, err := s.q.ListCategories(ctx)
	if err != nil {
		return connecterr.Internal(err)
	}
	items := make([]sortable, len(cats))
	for i, c := range cats {
		items[i] = sortable{ID: c.ID, Sort: c.Sort}
	}
	a, b, ok := neighborSwap(items, cid, moveUp(move))
	if !ok {
		return nil
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.UpdateCategorySort(ctx, sqlc.UpdateCategorySortParams{ID: a.ID, Sort: a.Sort}); e != nil {
			return connecterr.Internal(e)
		}
		return q.UpdateCategorySort(ctx, sqlc.UpdateCategorySortParams{ID: b.ID, Sort: b.Sort})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return nil
}

// --- items ---

func (s *Service) SaveItem(ctx context.Context, in *restaurantv1.MenuItemInput, version *int64) (*restaurantv1.MenuItem, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	if verr := validateItem(in); verr != nil {
		return nil, verr
	}
	catID, err := parseID("category_id", in.GetCategoryId())
	if err != nil {
		return nil, err
	}
	stationID, err := parseID("station_id", in.GetStationId())
	if err != nil {
		return nil, err
	}

	var result *restaurantv1.MenuItem
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var itemID uuid.UUID
		if in.GetId() == "" {
			sort, _ := q.NextItemSort(ctx, catID)
			created, e := q.CreateItem(ctx, sqlc.CreateItemParams{
				CategoryID: catID, NameEn: in.GetName().GetEn(), NameNe: in.GetName().GetNe(),
				PricePaisa: in.GetPricePaisa(), Color: in.GetColor(), StationID: stationID, Active: in.GetActive(), Sort: sort,
			})
			if e != nil {
				return connecterr.Internal(e)
			}
			itemID = created.ID
		} else {
			id, perr := parseID("id", in.GetId())
			if perr != nil {
				return perr
			}
			if version == nil {
				return connecterr.Invalid("version", "version is required when editing")
			}
			_, e := q.UpdateItem(ctx, sqlc.UpdateItemParams{
				ID: id, Version: *version, CategoryID: catID, NameEn: in.GetName().GetEn(), NameNe: in.GetName().GetNe(),
				PricePaisa: in.GetPricePaisa(), Color: in.GetColor(), StationID: stationID, Active: in.GetActive(),
			})
			if errors.Is(e, pgx.ErrNoRows) {
				return connecterr.StaleVersion("this item was changed by someone else; reload")
			}
			if e != nil {
				return connecterr.Internal(e)
			}
			itemID = id
		}

		// Replace variants and modifier links (append-only is only for ORDER items).
		if e := q.DeleteItemVariants(ctx, itemID); e != nil {
			return connecterr.Internal(e)
		}
		for i, v := range in.GetVariants() {
			if _, e := q.CreateVariant(ctx, sqlc.CreateVariantParams{
				ItemID: itemID, NameEn: v.GetName().GetEn(), NameNe: v.GetName().GetNe(), PricePaisa: v.GetPricePaisa(), Sort: int32(i),
			}); e != nil {
				return connecterr.Internal(e)
			}
		}
		if e := q.DeleteItemModifierLinks(ctx, itemID); e != nil {
			return connecterr.Internal(e)
		}
		for _, gid := range in.GetModifierGroupIds() {
			g, perr := parseID("modifier_group_ids", gid)
			if perr != nil {
				return perr
			}
			if e := q.CreateItemModifierLink(ctx, sqlc.CreateItemModifierLinkParams{ItemID: itemID, GroupID: g}); e != nil {
				return connecterr.Internal(e)
			}
		}

		assembled, e := s.assembleItem(ctx, q, itemID)
		if e != nil {
			return e
		}
		result = assembled
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "menu.item.save", Entity: "menu_item", EntityID: itemID.String(), After: audit.JSON(result)})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return result, nil
}

func (s *Service) assembleItem(ctx context.Context, q *sqlc.Queries, itemID uuid.UUID) (*restaurantv1.MenuItem, error) {
	it, err := q.GetItem(ctx, itemID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	allV, err := q.ListVariants(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	var vs []sqlc.ItemVariant
	for _, v := range allV {
		if v.ItemID == itemID {
			vs = append(vs, v)
		}
	}
	links, err := q.ListItemModifierGroups(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	var gids []string
	for _, l := range links {
		if l.ItemID == itemID {
			gids = append(gids, l.GroupID.String())
		}
	}
	return toMenuItem(it, vs, gids), nil
}

func (s *Service) MoveItem(ctx context.Context, id string, move restaurantv1.Move) error {
	if _, err := auth.RequireManager(ctx); err != nil {
		return err
	}
	iid, err := parseID("id", id)
	if err != nil {
		return err
	}
	items, err := s.q.ListItems(ctx)
	if err != nil {
		return connecterr.Internal(err)
	}
	ss := make([]sortable, len(items))
	for i, it := range items {
		ss[i] = sortable{ID: it.ID, Sort: it.Sort}
	}
	a, b, ok := neighborSwap(ss, iid, moveUp(move))
	if !ok {
		return nil
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.UpdateItemSort(ctx, sqlc.UpdateItemSortParams{ID: a.ID, Sort: a.Sort}); e != nil {
			return connecterr.Internal(e)
		}
		return q.UpdateItemSort(ctx, sqlc.UpdateItemSortParams{ID: b.ID, Sort: b.Sort})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return nil
}

func (s *Service) SetItemImage(ctx context.Context, id string, image []byte, contentType string) (*restaurantv1.MenuItem, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	iid, err := parseID("id", id)
	if err != nil {
		return nil, err
	}
	url := ""
	if len(image) > 0 {
		if len(image) > maxImageBytes {
			return nil, connecterr.Invalid("image", "image must be 1 MB or smaller")
		}
		switch contentType {
		case "image/jpeg", "image/png", "image/webp":
		default:
			return nil, connecterr.Invalid("image", "image must be JPEG, PNG or WebP")
		}
		url = fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(image))
	}
	var result *restaurantv1.MenuItem
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if _, e := q.SetItemImage(ctx, sqlc.SetItemImageParams{ID: iid, ImageUrl: url}); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return connecterr.NotFound("item not found")
			}
			return connecterr.Internal(e)
		}
		assembled, e := s.assembleItem(ctx, q, iid)
		if e != nil {
			return e
		}
		result = assembled
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "menu.item.image", Entity: "menu_item", EntityID: id})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return result, nil
}

func (s *Service) Set86(ctx context.Context, ids []string, is86d bool) error {
	actor, err := auth.RequireActor(ctx) // any staff can 86 an item
	if err != nil {
		return err
	}
	uids := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		u, perr := parseID("ids", id)
		if perr != nil {
			return perr
		}
		uids = append(uids, u)
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.Set86(ctx, sqlc.Set86Params{Is86d: is86d, Ids: uids}); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "menu.item.86", Entity: "menu_item",
			After: audit.JSON(map[string]any{"ids": ids, "is_86d": is86d}),
		})
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		s.emit(realtime.Event{Type: "item.86d", ID: id})
	}
	return nil
}

// --- modifier groups ---

func (s *Service) SaveModifierGroup(ctx context.Context, in *restaurantv1.ModifierGroupInput, version *int64) (*restaurantv1.ModifierGroup, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	if verr := validateModifierGroup(in); verr != nil {
		return nil, verr
	}
	var result *restaurantv1.ModifierGroup
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var groupID uuid.UUID
		if in.GetId() == "" {
			g, e := q.CreateModifierGroup(ctx, sqlc.CreateModifierGroupParams{
				NameEn: in.GetName().GetEn(), NameNe: in.GetName().GetNe(), MinSelect: in.GetMin(), MaxSelect: in.GetMax(),
			})
			if e != nil {
				return connecterr.Internal(e)
			}
			groupID = g.ID
		} else {
			id, perr := parseID("id", in.GetId())
			if perr != nil {
				return perr
			}
			if version == nil {
				return connecterr.Invalid("version", "version is required when editing")
			}
			_, e := q.UpdateModifierGroup(ctx, sqlc.UpdateModifierGroupParams{
				ID: id, Version: *version, NameEn: in.GetName().GetEn(), NameNe: in.GetName().GetNe(), MinSelect: in.GetMin(), MaxSelect: in.GetMax(),
			})
			if errors.Is(e, pgx.ErrNoRows) {
				return connecterr.StaleVersion("this modifier group was changed by someone else; reload")
			}
			if e != nil {
				return connecterr.Internal(e)
			}
			groupID = id
		}
		if e := q.DeleteGroupModifiers(ctx, groupID); e != nil {
			return connecterr.Internal(e)
		}
		for i, m := range in.GetModifiers() {
			if _, e := q.CreateModifier(ctx, sqlc.CreateModifierParams{
				GroupID: groupID, NameEn: m.GetName().GetEn(), NameNe: m.GetName().GetNe(), PriceDeltaPaisa: m.GetPriceDeltaPaisa(), Sort: int32(i),
			}); e != nil {
				return connecterr.Internal(e)
			}
		}
		g, e := q.GetModifierGroup(ctx, groupID)
		if e != nil {
			return connecterr.Internal(e)
		}
		mods, e := q.ListModifiers(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		var gm []sqlc.Modifier
		for _, m := range mods {
			if m.GroupID == groupID {
				gm = append(gm, m)
			}
		}
		result = toModifierGroup(g, gm)
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "menu.modifier_group.save", Entity: "modifier_group", EntityID: groupID.String(), After: audit.JSON(result)})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return result, nil
}

func (s *Service) DeleteModifierGroup(ctx context.Context, id string) error {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return err
	}
	gid, err := parseID("id", id)
	if err != nil {
		return err
	}
	n, err := s.q.CountItemsUsingModifierGroup(ctx, gid)
	if err != nil {
		return connecterr.Internal(err)
	}
	if n > 0 {
		return connecterr.InUse("menu items use this modifier group")
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.DeleteModifierGroup(ctx, gid); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "menu.modifier_group.delete", Entity: "modifier_group", EntityID: id})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return nil
}

// --- validation ---

func validateItem(in *restaurantv1.MenuItemInput) *connecterr.Error {
	if in.GetName().GetEn() == "" {
		return connecterr.Invalid("name", "English name is required")
	}
	if len(in.GetVariants()) == 0 {
		if in.GetPricePaisa() <= 0 {
			return connecterr.Invalid("price", "a price is required when there are no variants")
		}
		return nil
	}
	for i, v := range in.GetVariants() {
		if v.GetName().GetEn() == "" {
			return connecterr.Invalid(fmt.Sprintf("variants.%d.name", i), "variant name is required")
		}
		if v.GetPricePaisa() <= 0 {
			return connecterr.Invalid(fmt.Sprintf("variants.%d.price", i), "variant price is required")
		}
	}
	return nil
}

func validateModifierGroup(in *restaurantv1.ModifierGroupInput) *connecterr.Error {
	if in.GetName().GetEn() == "" {
		return connecterr.Invalid("name", "English name is required")
	}
	mods := in.GetModifiers()
	if len(mods) == 0 {
		return connecterr.Invalid("modifiers", "at least one modifier is required")
	}
	for i, m := range mods {
		if m.GetName().GetEn() == "" {
			return connecterr.Invalid(fmt.Sprintf("modifiers.%d.name", i), "modifier name is required")
		}
	}
	min, max := in.GetMin(), in.GetMax()
	if min < 0 || max < 1 || min > max || int(max) > len(mods) {
		return connecterr.Invalid("max", "choose-count range is invalid for the number of modifiers")
	}
	return nil
}
