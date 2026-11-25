-- Idempotency ---------------------------------------------------------------

-- name: GetProcessedCommand :one
SELECT response FROM processed_commands WHERE command_id = $1;

-- name: InsertProcessedCommand :one
INSERT INTO processed_commands (command_id, user_id, response)
VALUES ($1, $2, $3)
ON CONFLICT (command_id) DO NOTHING
RETURNING command_id;

-- Orders --------------------------------------------------------------------

-- label is free text (e.g. a takeaway customer name); "" when none.
-- name: CreateOrder :one
INSERT INTO orders (client_id, type, table_id, waiter_user_id, guest_count, label)
VALUES ($1, $2, $3, $4, $5, @label)
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

-- Moves an order to a table (DINE_IN) or off the floor (NULL table -> TAKEAWAY).
-- name: SetOrderTable :exec
UPDATE orders
SET table_id = @table_id,
    type = CASE WHEN @table_id::uuid IS NULL THEN 'TAKEAWAY' ELSE 'DINE_IN' END,
    updated_at = now()
WHERE id = @id;

-- name: SetOrderGuestCount :exec
UPDATE orders SET guest_count = $2, updated_at = now() WHERE id = $1;

-- name: AddOrderGuestCount :exec
UPDATE orders SET guest_count = guest_count + @add, updated_at = now() WHERE id = @id;

-- name: GetUserName :one
SELECT name FROM users WHERE id = $1;

-- Order items ---------------------------------------------------------------

-- Per-line idempotency: a line whose client_id already exists inserts nothing
-- (no row returned); the caller decides whether that is a replay or a clash.
-- sent_at is stamped when the line is created already SENT (one-step add+send).
-- name: CreateOrderItem :one
INSERT INTO order_items (
    order_id, client_id, menu_item_id, name_en, name_ne, variant_name_en, variant_name_ne,
    has_variant, unit_price_paisa, quantity, note, station_id, status, added_by_user_id, sent_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, @status, @added_by_user_id,
    CASE WHEN @status::text = 'DRAFT' THEN NULL ELSE now() END)
ON CONFLICT (client_id) DO NOTHING
RETURNING *;

-- name: GetOrderItemByClientID :one
SELECT * FROM order_items WHERE client_id = $1;

-- name: ListOrderItems :many
SELECT * FROM order_items WHERE order_id = $1 ORDER BY created_at;

-- name: GetOrderItem :one
SELECT * FROM order_items WHERE id = $1;

-- Moving a line to SENT stamps sent_at (once).
-- name: SetOrderItemStatus :exec
UPDATE order_items
SET status = @status,
    sent_at = CASE WHEN @status::text = 'SENT' THEN COALESCE(sent_at, now()) ELSE sent_at END
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

-- name: SetOrderReservation :exec
UPDATE orders SET reservation_id = @reservation_id WHERE id = @id;

-- Item-level commands accept a line's server id or its client_id: maps the client
-- ids among @ids to server ids (ids that are no line's client_id are not returned).
-- name: OrderItemIDsByClientIDs :many
SELECT id, client_id FROM order_items WHERE client_id = ANY(@ids::uuid[]);
