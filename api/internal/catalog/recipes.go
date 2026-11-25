package catalog

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
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Recipes (spec: inventory-wastage-design): per 1 unit sold, quantities in
// thousandths of each stock item's base unit. A variant's own recipe replaces the
// item recipe; modifier recipes are added. Changes apply from now on only (past
// stock movements never change) and are audited as RECIPE_CHANGED.

type recipeLine struct {
	itemID uuid.UUID
	qty    int64
}

// checkRecipe validates lines against the stock items (INVALID(field) for an
// unknown item or a quantity <= 0) and merges repeats of one item.
func checkRecipe(ctx context.Context, q *sqlc.Queries, field string, in []*restaurantv1.RecipeLine) ([]recipeLine, map[uuid.UUID]sqlc.InventoryItem, error) {
	var out []recipeLine
	at := map[uuid.UUID]int{}
	ids := make([]uuid.UUID, 0, len(in))
	for _, l := range in {
		id, err := uuid.Parse(l.GetInventoryItemId())
		if err != nil {
			return nil, nil, connecterr.Invalid(field, "unknown stock item")
		}
		if l.GetQuantityMilli() <= 0 {
			return nil, nil, connecterr.Invalid(field, "each recipe line needs a quantity")
		}
		if i, ok := at[id]; ok {
			out[i].qty += l.GetQuantityMilli()
			continue
		}
		at[id] = len(out)
		out = append(out, recipeLine{id, l.GetQuantityMilli()})
		ids = append(ids, id)
	}
	items := map[uuid.UUID]sqlc.InventoryItem{}
	if len(ids) == 0 {
		return out, items, nil
	}
	found, err := q.ListInventoryItemsByIDs(ctx, ids)
	if err != nil {
		return nil, nil, connecterr.Internal(err)
	}
	for _, it := range found {
		items[it.ID] = it
	}
	if len(items) != len(ids) {
		return nil, nil, connecterr.Invalid(field, "unknown stock item")
	}
	return out, items, nil
}

func recipeText(lines []recipeLine, items map[uuid.UUID]sqlc.InventoryItem) string {
	if len(lines) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		it := items[l.itemID]
		q := fmt.Sprintf("%d", l.qty/1000)
		if r := l.qty % 1000; r != 0 {
			q = strings.TrimRight(fmt.Sprintf("%s.%03d", q, r), "0")
		}
		parts = append(parts, strings.TrimSpace(it.NameEn+" "+q+" "+it.Unit))
	}
	return strings.Join(parts, ", ")
}

type variantRecipeIn struct {
	id        uuid.UUID
	name      string
	useItem   bool
	lines     []recipeLine
	lineItems map[uuid.UUID]sqlc.InventoryItem
}

// SaveRecipe (owner/manager) saves a dish's recipe and, for the variants named, an
// own recipe (or back to the item recipe). Bumps the item version (STALE_VERSION
// on a wrong base_version).
func (s *Service) SaveRecipe(ctx context.Context, req *restaurantv1.SaveRecipeRequest) (*restaurantv1.MenuItem, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	itemID, err := parseID("menu_item_id", req.GetMenuItemId())
	if err != nil {
		return nil, err
	}
	var result *restaurantv1.MenuItem
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		item, e := q.GetItem(ctx, itemID)
		if e != nil {
			return notFound(e, "item")
		}
		lines, items, e := checkRecipe(ctx, q, "recipe", req.GetRecipe())
		if e != nil {
			return e
		}
		variants, e := q.ListVariantsForItem(ctx, itemID)
		if e != nil {
			return connecterr.Internal(e)
		}
		var vin []variantRecipeIn
		for _, vr := range req.GetVariantRecipes() {
			v := variantRecipeIn{useItem: vr.GetUseItemRecipe()}
			for _, x := range variants {
				if x.ID.String() == vr.GetVariantId() {
					v.id, v.name = x.ID, x.NameEn
				}
			}
			if v.id == uuid.Nil {
				return connecterr.Invalid("variant_recipes", "variant does not belong to this item")
			}
			if !v.useItem {
				if v.lines, v.lineItems, e = checkRecipe(ctx, q, "variant_recipes", vr.GetLines()); e != nil {
					return e
				}
			}
			vin = append(vin, v)
		}
		if _, e := q.BumpMenuItemVersion(ctx, sqlc.BumpMenuItemVersionParams{ID: itemID, Version: req.GetBaseVersion()}); errors.Is(e, pgx.ErrNoRows) {
			return connecterr.StaleVersion("this item was changed by someone else; reload")
		} else if e != nil {
			return connecterr.Internal(e)
		}
		if e := q.DeleteMenuItemRecipe(ctx, itemID); e != nil {
			return connecterr.Internal(e)
		}
		for i, l := range lines {
			if e := q.InsertMenuItemRecipeLine(ctx, sqlc.InsertMenuItemRecipeLineParams{MenuItemID: itemID, InventoryItemID: l.itemID, QuantityMilli: l.qty, Sort: int32(i)}); e != nil {
				return connecterr.Internal(e)
			}
		}
		detail := []string{recipeText(lines, items)}
		for _, v := range vin {
			if v.useItem {
				if e := q.DeleteVariantRecipe(ctx, v.id); e != nil {
					return connecterr.Internal(e)
				}
				continue
			}
			if e := q.CreateVariantRecipe(ctx, v.id); e != nil {
				return connecterr.Internal(e)
			}
			if e := q.DeleteVariantRecipeLines(ctx, v.id); e != nil {
				return connecterr.Internal(e)
			}
			for i, l := range v.lines {
				if e := q.InsertVariantRecipeLine(ctx, sqlc.InsertVariantRecipeLineParams{VariantID: v.id, InventoryItemID: l.itemID, QuantityMilli: l.qty, Sort: int32(i)}); e != nil {
					return connecterr.Internal(e)
				}
			}
			detail = append(detail, v.name+": "+recipeText(v.lines, v.lineItems))
		}
		assembled, e := s.assembleItem(ctx, q, itemID)
		if e != nil {
			return e
		}
		result = assembled
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "RECIPE_CHANGED", Entity: "menu_item", EntityID: itemID.String(),
			Reason: item.NameEn + " · " + strings.Join(detail, "; "), After: audit.JSON(result.GetRecipe()),
		})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return result, nil
}

// SaveModifierRecipe (owner/manager) saves the lines a modifier adds to a dish.
func (s *Service) SaveModifierRecipe(ctx context.Context, req *restaurantv1.SaveModifierRecipeRequest) error {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return err
	}
	modID, err := parseID("modifier_id", req.GetModifierId())
	if err != nil {
		return err
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		mod, e := q.GetModifierByID(ctx, modID)
		if e != nil {
			return notFound(e, "modifier")
		}
		lines, items, e := checkRecipe(ctx, q, "recipe", req.GetRecipe())
		if e != nil {
			return e
		}
		if e := q.DeleteModifierRecipe(ctx, modID); e != nil {
			return connecterr.Internal(e)
		}
		for i, l := range lines {
			if e := q.InsertModifierRecipeLine(ctx, sqlc.InsertModifierRecipeLineParams{ModifierID: modID, InventoryItemID: l.itemID, QuantityMilli: l.qty, Sort: int32(i)}); e != nil {
				return connecterr.Internal(e)
			}
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "RECIPE_CHANGED", Entity: "modifier", EntityID: modID.String(),
			Reason: mod.NameEn + " · + " + recipeText(lines, items),
		})
	})
	if err != nil {
		return err
	}
	s.emit(realtime.Event{Type: "menu.updated"})
	return nil
}

// menuRecipes is every recipe, keyed by owner, as proto lines.
type menuRecipes struct {
	item, variant, modifier map[uuid.UUID][]*restaurantv1.RecipeLine
	own                     map[uuid.UUID]bool
}

func loadMenuRecipes(ctx context.Context, q *sqlc.Queries) (menuRecipes, error) {
	r := menuRecipes{
		item: map[uuid.UUID][]*restaurantv1.RecipeLine{}, variant: map[uuid.UUID][]*restaurantv1.RecipeLine{},
		modifier: map[uuid.UUID][]*restaurantv1.RecipeLine{}, own: map[uuid.UUID]bool{},
	}
	pl := func(id uuid.UUID, qty int64) *restaurantv1.RecipeLine {
		return &restaurantv1.RecipeLine{InventoryItemId: id.String(), QuantityMilli: qty}
	}
	il, err := q.ListAllMenuItemRecipeLines(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, l := range il {
		r.item[l.MenuItemID] = append(r.item[l.MenuItemID], pl(l.InventoryItemID, l.QuantityMilli))
	}
	own, err := q.ListVariantRecipeIDs(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, id := range own {
		r.own[id] = true
	}
	vl, err := q.ListAllVariantRecipeLines(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, l := range vl {
		r.variant[l.VariantID] = append(r.variant[l.VariantID], pl(l.InventoryItemID, l.QuantityMilli))
	}
	ml, err := q.ListAllModifierRecipeLines(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, l := range ml {
		r.modifier[l.ModifierID] = append(r.modifier[l.ModifierID], pl(l.InventoryItemID, l.QuantityMilli))
	}
	return r, nil
}

// attachRecipes fills the recipe fields of menu items, their variants and modifiers.
func attachRecipes(ctx context.Context, q *sqlc.Queries, items []*restaurantv1.MenuItem, groups []*restaurantv1.ModifierGroup) error {
	r, err := loadMenuRecipes(ctx, q)
	if err != nil {
		return err
	}
	for _, it := range items {
		it.Recipe = r.item[uuid.MustParse(it.Id)]
		for _, v := range it.Variants {
			vid := uuid.MustParse(v.Id)
			v.HasOwnRecipe = r.own[vid]
			v.Recipe = r.variant[vid]
		}
	}
	for _, g := range groups {
		for _, m := range g.Modifiers {
			m.Recipe = r.modifier[uuid.MustParse(m.Id)]
		}
	}
	return nil
}
