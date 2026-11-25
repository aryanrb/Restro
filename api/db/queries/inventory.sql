-- Inventory items ------------------------------------------------------------

-- name: CreateInventoryItem :one
INSERT INTO inventory_items (name_en, name_ne, unit, low_stock_milli, active, unit_cost_e4, packs, sort)
VALUES ($1, $2, $3, $4, $5, $6, $7,
    (SELECT COALESCE(MAX(sort), 0) + 1 FROM inventory_items))
RETURNING *;

-- name: UpdateInventoryItem :one
UPDATE inventory_items SET
    version = version + 1, name_en = $3, name_ne = $4, unit = $5, low_stock_milli = $6,
    active = $7, unit_cost_e4 = $8, packs = $9, updated_at = now()
WHERE id = $1 AND version = $2
RETURNING *;

-- name: SetInventoryTracked :one
UPDATE inventory_items SET tracked = $2, version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: GetInventoryItem :one
SELECT * FROM inventory_items WHERE id = $1;

-- name: GetInventoryItemForUpdate :one
SELECT * FROM inventory_items WHERE id = $1 FOR UPDATE;

-- name: ListInventoryItems :many
SELECT * FROM inventory_items
WHERE active OR @include_inactive::boolean
ORDER BY sort, name_en;

-- name: ListInventoryItemsByIDs :many
SELECT * FROM inventory_items WHERE id = ANY(@ids::uuid[]) ORDER BY sort, name_en;

-- name: InventoryItemHasMovements :one
SELECT EXISTS (SELECT 1 FROM stock_movements WHERE inventory_item_id = $1);

-- name: InventoryItemIDsWithMovements :many
SELECT DISTINCT inventory_item_id FROM stock_movements;

-- Adds delta to the on-hand cache (and locks the row) and returns the item after.
-- name: AddOnHand :one
UPDATE inventory_items SET on_hand_milli = on_hand_milli + @delta
WHERE id = @id
RETURNING *;

-- Stock ledger ---------------------------------------------------------------

-- name: InsertStockMovement :one
INSERT INTO stock_movements (inventory_item_id, quantity_milli, type, ref_id, reason, by_user_id, at, unit_cost_e4)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: MaxMovementSeq :one
SELECT COALESCE(MAX(seq), 0)::bigint FROM stock_movements;

-- name: ListMovements :many
SELECT m.*, COALESCE(u.name, '')::text AS by_name
FROM stock_movements m
LEFT JOIN users u ON u.id = m.by_user_id
WHERE m.inventory_item_id = @inventory_item_id
  AND (sqlc.narg(from_ts)::timestamptz IS NULL OR m.at >= sqlc.narg(from_ts))
  AND (sqlc.narg(to_ts)::timestamptz IS NULL OR m.at <= sqlc.narg(to_ts))
  AND (@type::text = '' OR m.type = @type)
ORDER BY m.at DESC, m.seq DESC;

-- Movements of one item up to (and including) a ledger point: at < point, or the
-- same at with seq <= point seq. Oldest first.
-- name: ListItemMovementsUntil :many
SELECT * FROM stock_movements
WHERE inventory_item_id = @inventory_item_id
  AND (at < @at OR (at = @at AND seq <= @seq))
ORDER BY at, seq;

-- name: OnHandAt :one
SELECT COALESCE(SUM(quantity_milli), 0)::bigint FROM stock_movements
WHERE inventory_item_id = @inventory_item_id
  AND (at < @at OR (at = @at AND seq <= @seq));

-- name: ListMovementsByRef :many
SELECT * FROM stock_movements WHERE ref_id = $1 AND type = $2 ORDER BY seq;

-- name: ListMovementsByRefs :many
SELECT * FROM stock_movements WHERE ref_id = ANY(@refs::uuid[]) AND type = @type ORDER BY seq;

-- Recipes --------------------------------------------------------------------

-- name: ListMenuItemRecipeLines :many
SELECT * FROM menu_item_recipe_lines WHERE menu_item_id = $1 ORDER BY sort;

-- name: ListAllMenuItemRecipeLines :many
SELECT * FROM menu_item_recipe_lines ORDER BY menu_item_id, sort;

-- name: DeleteMenuItemRecipe :exec
DELETE FROM menu_item_recipe_lines WHERE menu_item_id = $1;

-- name: InsertMenuItemRecipeLine :exec
INSERT INTO menu_item_recipe_lines (menu_item_id, inventory_item_id, quantity_milli, sort) VALUES ($1, $2, $3, $4);

-- name: ListVariantRecipeIDs :many
SELECT variant_id FROM variant_recipes;

-- name: VariantHasOwnRecipe :one
SELECT EXISTS (SELECT 1 FROM variant_recipes WHERE variant_id = $1);

-- name: ListVariantRecipeLines :many
SELECT * FROM variant_recipe_lines WHERE variant_id = $1 ORDER BY sort;

-- name: ListAllVariantRecipeLines :many
SELECT * FROM variant_recipe_lines ORDER BY variant_id, sort;

-- name: DeleteVariantRecipe :exec
DELETE FROM variant_recipes WHERE variant_id = $1;

-- name: CreateVariantRecipe :exec
INSERT INTO variant_recipes (variant_id) VALUES ($1) ON CONFLICT DO NOTHING;

-- name: DeleteVariantRecipeLines :exec
DELETE FROM variant_recipe_lines WHERE variant_id = $1;

-- name: InsertVariantRecipeLine :exec
INSERT INTO variant_recipe_lines (variant_id, inventory_item_id, quantity_milli, sort) VALUES ($1, $2, $3, $4);

-- name: ListModifierRecipeLinesFor :many
SELECT * FROM modifier_recipe_lines WHERE modifier_id = ANY(@ids::uuid[]) ORDER BY modifier_id, sort;

-- name: ListAllModifierRecipeLines :many
SELECT * FROM modifier_recipe_lines ORDER BY modifier_id, sort;

-- name: DeleteModifierRecipe :exec
DELETE FROM modifier_recipe_lines WHERE modifier_id = $1;

-- name: InsertModifierRecipeLine :exec
INSERT INTO modifier_recipe_lines (modifier_id, inventory_item_id, quantity_milli, sort) VALUES ($1, $2, $3, $4);

-- name: BumpMenuItemVersion :one
UPDATE menu_items SET version = version + 1, updated_at = now()
WHERE id = $1 AND version = $2
RETURNING version;

-- Order lines: what a line was ordered as, for expanding its recipe at send.
-- name: SetOrderItemStockKeys :exec
UPDATE order_items SET variant_id = $2, modifier_ids = @modifier_ids::uuid[] WHERE id = $1;

-- Lines voided after the kitchen started (a manager approved the void), sent in a range.
-- name: ListVoidedAfterCooking :many
SELECT oi.*, COALESCE(u.name, '')::text AS approver_name
FROM order_items oi
LEFT JOIN users u ON u.id = oi.void_approved_by_user_id
WHERE oi.status = 'VOIDED' AND oi.void_approved_by_user_id IS NOT NULL
  AND oi.sent_at >= @from_ts AND oi.sent_at <= @to_ts
ORDER BY oi.sent_at;

-- Order lines voided after cooking with a SALE movement in (since, until].
-- name: ListVoidedAfterCookingSales :many
SELECT m.* FROM stock_movements m
JOIN order_items oi ON oi.id = m.ref_id
WHERE m.type = 'SALE' AND oi.status = 'VOIDED' AND oi.void_approved_by_user_id IS NOT NULL
  AND (sqlc.narg(since)::timestamptz IS NULL OR m.at > sqlc.narg(since))
  AND m.at <= @until
ORDER BY m.seq;

-- Waste ----------------------------------------------------------------------

-- name: GetWasteByRequestID :one
SELECT * FROM waste_entries WHERE request_id = $1;

-- name: InsertWasteEntry :one
INSERT INTO waste_entries (
    request_id, inventory_item_id, menu_item_id, variant_id, modifier_ids, name_en, name_ne, unit,
    quantity_milli, reason, note, by_user_id, approved_by_user_id, at, value_paisa
) VALUES ($1, $2, $3, $4, @modifier_ids::uuid[], $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (request_id) DO NOTHING
RETURNING *;

-- name: ListWasteInRange :many
SELECT w.*, COALESCE(u.name, '')::text AS by_name, COALESCE(a.name, '')::text AS approved_by_name
FROM waste_entries w
JOIN users u ON u.id = w.by_user_id
LEFT JOIN users a ON a.id = w.approved_by_user_id
WHERE w.at >= @from_ts AND w.at <= @to_ts
ORDER BY w.at DESC, w.id DESC;

-- Stock counts ---------------------------------------------------------------

-- name: CreateCountList :one
INSERT INTO count_lists (name, inventory_item_ids) VALUES ($1, @inventory_item_ids::uuid[]) RETURNING *;

-- name: UpdateCountList :one
UPDATE count_lists SET name = $2, inventory_item_ids = @inventory_item_ids::uuid[] WHERE id = $1 RETURNING *;

-- name: GetCountList :one
SELECT * FROM count_lists WHERE id = $1;

-- name: ListCountLists :many
SELECT * FROM count_lists ORDER BY created_at, name;

-- name: CreateStockCount :one
INSERT INTO stock_counts (list_id, label, started_by_user_id, started_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: InsertCountLine :exec
INSERT INTO stock_count_lines (count_id, inventory_item_id, sort) VALUES ($1, $2, $3);

-- name: GetStockCount :one
SELECT * FROM stock_counts WHERE id = $1;

-- name: GetStockCountForUpdate :one
SELECT * FROM stock_counts WHERE id = $1 FOR UPDATE;

-- name: ListStockCounts :many
SELECT * FROM stock_counts ORDER BY number DESC;

-- name: ListCountLines :many
SELECT * FROM stock_count_lines WHERE count_id = $1 ORDER BY sort, inventory_item_id;

-- name: ListAllCountLines :many
SELECT * FROM stock_count_lines ORDER BY count_id, sort, inventory_item_id;

-- name: SetCountLine :execrows
UPDATE stock_count_lines
SET counted_milli = $3, counted_at = $4, counted_seq = $5, counted_by_user_id = $6
WHERE count_id = $1 AND inventory_item_id = $2;

-- name: ClearCountLine :execrows
UPDATE stock_count_lines
SET counted_milli = NULL, counted_at = NULL, counted_seq = NULL, counted_by_user_id = NULL,
    recount = recount OR @recount::boolean
WHERE count_id = $1 AND inventory_item_id = $2;

-- name: SetCountLineResult :exec
UPDATE stock_count_lines SET expected_milli = $3, variance_milli = $4, value_paisa = $5
WHERE count_id = $1 AND inventory_item_id = $2;

-- name: SubmitStockCount :exec
UPDATE stock_counts SET status = 'SUBMITTED', submitted_by_user_id = $2, submitted_at = $3 WHERE id = $1;

-- name: ReopenStockCount :exec
UPDATE stock_counts SET status = 'OPEN', submitted_by_user_id = NULL, submitted_at = NULL WHERE id = $1;

-- name: PostStockCount :exec
UPDATE stock_counts SET status = 'POSTED', posted_by_user_id = $2, posted_at = $3, total_value_paisa = $4 WHERE id = $1;

-- name: DiscardStockCount :exec
UPDATE stock_counts SET status = 'DISCARDED', discarded_reason = $2, discarded_at = $3 WHERE id = $1;

-- Counted lines of POSTED counts for one item (for the variance period), any order.
-- name: ListPostedCountLinesForItem :many
SELECT l.* FROM stock_count_lines l
JOIN stock_counts c ON c.id = l.count_id
WHERE c.status = 'POSTED' AND l.inventory_item_id = $1
  AND l.counted_milli IS NOT NULL AND l.counted_at IS NOT NULL;

-- name: ListUserNames :many
SELECT id, name FROM users WHERE id = ANY(@ids::uuid[]);
