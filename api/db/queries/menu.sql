-- Categories ---------------------------------------------------------------

-- name: ListCategories :many
SELECT * FROM menu_categories ORDER BY sort, name_en;

-- name: NextCategorySort :one
SELECT COALESCE(MAX(sort), 0) + 1 FROM menu_categories;

-- name: CreateCategory :one
INSERT INTO menu_categories (name_en, name_ne, active, sort) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: UpdateCategory :one
UPDATE menu_categories SET name_en = $2, name_ne = $3, active = $4 WHERE id = $1 RETURNING *;

-- name: UpdateCategorySort :exec
UPDATE menu_categories SET sort = $2 WHERE id = $1;

-- name: DeleteCategory :exec
DELETE FROM menu_categories WHERE id = $1;

-- name: CountItemsInCategory :one
SELECT count(*) FROM menu_items WHERE category_id = $1;

-- Items --------------------------------------------------------------------

-- name: ListItems :many
SELECT * FROM menu_items ORDER BY sort, name_en;

-- name: GetItem :one
SELECT * FROM menu_items WHERE id = $1;

-- name: GetCategory :one
SELECT * FROM menu_categories WHERE id = $1;

-- name: ListItemsInCategory :many
SELECT * FROM menu_items WHERE category_id = $1 ORDER BY sort, name_en;

-- name: NextItemSort :one
SELECT COALESCE(MAX(sort), 0) + 1 FROM menu_items WHERE category_id = $1;

-- name: CreateItem :one
INSERT INTO menu_items (category_id, name_en, name_ne, price_paisa, color, station_id, active, sort)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateItem :one
UPDATE menu_items SET
    version = version + 1, category_id = $3, name_en = $4, name_ne = $5,
    price_paisa = $6, color = $7, station_id = $8, active = $9, sort = $10, updated_at = now()
WHERE id = $1 AND version = $2
RETURNING *;

-- name: UpdateItemSort :exec
UPDATE menu_items SET sort = $2 WHERE id = $1;

-- name: SetItemImage :one
UPDATE menu_items SET version = version + 1, image_url = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: Set86 :exec
UPDATE menu_items SET is_86d = @is_86d, updated_at = now() WHERE id = ANY(@ids::uuid[]);

-- Variants -----------------------------------------------------------------

-- name: ListVariants :many
SELECT * FROM item_variants ORDER BY item_id, sort;

-- name: GetVariant :one
SELECT * FROM item_variants WHERE id = $1;

-- name: GetModifierByID :one
SELECT * FROM modifiers WHERE id = $1;

-- name: ListVariantsForItem :many
SELECT * FROM item_variants WHERE item_id = $1 ORDER BY sort;

-- name: ListModifierGroupsForItem :many
SELECT g.* FROM modifier_groups g
JOIN item_modifier_groups l ON l.group_id = g.id
WHERE l.item_id = $1
ORDER BY g.name_en;

-- name: DeleteItemVariants :exec
DELETE FROM item_variants WHERE item_id = $1;

-- name: UpdateVariant :one
UPDATE item_variants SET name_en = $3, name_ne = $4, price_paisa = $5, sort = $6
WHERE id = $1 AND item_id = $2
RETURNING *;

-- name: DeleteItemVariantsExcept :exec
-- Removes the item's variants that are not in keep (upsert-by-id saves).
DELETE FROM item_variants WHERE item_id = @item_id AND NOT (id = ANY(@keep::uuid[]));

-- name: CreateVariant :one
INSERT INTO item_variants (item_id, name_en, name_ne, price_paisa, sort) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- Modifier groups ----------------------------------------------------------

-- name: ListModifierGroups :many
SELECT * FROM modifier_groups ORDER BY name_en;

-- name: GetModifierGroup :one
SELECT * FROM modifier_groups WHERE id = $1;

-- name: ListModifiers :many
SELECT * FROM modifiers ORDER BY group_id, sort;

-- name: ListItemModifierGroups :many
SELECT * FROM item_modifier_groups;

-- name: CreateModifierGroup :one
INSERT INTO modifier_groups (name_en, name_ne, min_select, max_select) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: UpdateModifierGroup :one
UPDATE modifier_groups SET version = version + 1, name_en = $3, name_ne = $4, min_select = $5, max_select = $6
WHERE id = $1 AND version = $2
RETURNING *;

-- name: DeleteModifierGroup :exec
DELETE FROM modifier_groups WHERE id = $1;

-- name: DeleteGroupModifiers :exec
DELETE FROM modifiers WHERE group_id = $1;

-- name: UpdateModifier :one
UPDATE modifiers SET name_en = $3, name_ne = $4, price_delta_paisa = $5, sort = $6
WHERE id = $1 AND group_id = $2
RETURNING *;

-- name: DeleteGroupModifiersExcept :exec
-- Removes the group's modifiers that are not in keep (upsert-by-id saves).
DELETE FROM modifiers WHERE group_id = @group_id AND NOT (id = ANY(@keep::uuid[]));

-- name: CreateModifier :one
INSERT INTO modifiers (group_id, name_en, name_ne, price_delta_paisa, sort) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: CountItemsUsingModifierGroup :one
SELECT count(*) FROM item_modifier_groups WHERE group_id = $1;

-- name: DeleteItemModifierLinks :exec
DELETE FROM item_modifier_groups WHERE item_id = $1;

-- name: CreateItemModifierLink :exec
INSERT INTO item_modifier_groups (item_id, group_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;
