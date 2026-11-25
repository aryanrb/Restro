-- Idempotency ---------------------------------------------------------------

-- name: GetProcessedCommand :one
SELECT response FROM processed_commands WHERE command_id = $1;

-- name: InsertProcessedCommand :one
INSERT INTO processed_commands (command_id, user_id, response)
VALUES ($1, $2, $3)
ON CONFLICT (command_id) DO NOTHING
RETURNING command_id;

-- Orders --------------------------------------------------------------------

-- name: CreateOrder :one
INSERT INTO orders (client_id, type, table_id, waiter_user_id, guest_count)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (client_id) DO NOTHING
RETURNING *;

-- name: GetOrderByClientID :one
SELECT * FROM orders WHERE client_id = $1;

-- name: GetOrderByID :one
SELECT * FROM orders WHERE id = $1;

-- name: ListOpenOrders :many
SELECT * FROM orders WHERE status IN ('OPEN', 'BILLED') ORDER BY created_at;

-- name: BumpOrderVersion :one
UPDATE orders SET version = version + 1, updated_at = now() WHERE id = $1 RETURNING version;

-- name: CheckBumpOrderVersion :one
UPDATE orders SET version = version + 1, updated_at = now()
WHERE id = $1 AND version = $2
RETURNING version;

-- name: SetOrderStatus :exec
UPDATE orders SET status = $2, updated_at = now() WHERE id = $1;

-- Locks the order row so the close-rule evaluation that follows sees every
-- change committed by concurrent order/kitchen/billing transactions.
-- name: LockOrderForUpdate :one
SELECT * FROM orders WHERE id = $1 FOR UPDATE;

-- Close rule inputs: live (non-voided) lines, and how many of them are both on an
-- ISSUED invoice and handed over (SERVED, or READY for a takeaway).
-- name: GetOrderCloseState :one
SELECT
    count(i.id) FILTER (WHERE i.status <> 'VOIDED')::bigint AS live,
    count(i.id) FILTER (
        WHERE i.status <> 'VOIDED'
          AND inv.status = 'ISSUED'
          AND (i.status = 'SERVED' OR (o.type = 'TAKEAWAY' AND i.status = 'READY'))
    )::bigint AS done
FROM orders o
LEFT JOIN order_items i ON i.order_id = o.id
LEFT JOIN invoices inv ON inv.id = i.invoice_id
WHERE o.id = $1;

-- name: CloseOrder :execrows
UPDATE orders SET status = 'CLOSED', version = version + 1, updated_at = now()
WHERE id = $1 AND status IN ('OPEN', 'BILLED');

-- name: SetOrderTable :exec
UPDATE orders SET table_id = $2, updated_at = now() WHERE id = $1;

-- Order items ---------------------------------------------------------------

-- Per-line idempotency: a line whose client_id already exists inserts nothing
-- (no row returned); the caller decides whether that is a replay or a clash.
-- name: CreateOrderItem :one
INSERT INTO order_items (
    order_id, client_id, menu_item_id, name_en, name_ne, variant_name_en, variant_name_ne,
    has_variant, unit_price_paisa, quantity, note, station_id, status
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, @status)
ON CONFLICT (client_id) DO NOTHING
RETURNING *;

-- name: GetOrderItemByClientID :one
SELECT * FROM order_items WHERE client_id = $1;

-- name: ListOrderItems :many
SELECT * FROM order_items WHERE order_id = $1 ORDER BY created_at;

-- name: GetOrderItem :one
SELECT * FROM order_items WHERE id = $1;

-- name: SetOrderItemStatus :exec
UPDATE order_items SET status = @status
WHERE order_id = @order_id AND id = ANY(@ids::uuid[]) AND status = ANY(@from_status::text[]);

-- name: VoidOrderItem :exec
UPDATE order_items
SET status = 'VOIDED', void_reason = $2, voided_by_user_id = $3, void_approved_by_user_id = $4
WHERE id = $1;

-- name: MoveOrderItemsToOrder :exec
UPDATE order_items SET order_id = $2
WHERE order_id = $1 AND status NOT IN ('VOIDED');

-- name: CreateOrderItemModifier :exec
INSERT INTO order_item_modifiers (order_item_id, name_en, name_ne, price_delta_paisa)
VALUES ($1, $2, $3, $4);

-- name: ListOrderItemModifiersForOrder :many
SELECT m.* FROM order_item_modifiers m
JOIN order_items i ON i.id = m.order_item_id
WHERE i.order_id = $1;
