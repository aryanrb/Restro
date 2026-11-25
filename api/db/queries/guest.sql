-- Guest table ordering (internal/guest).

-- name: GuestTableByCode :one
SELECT * FROM dining_tables WHERE guest_code = $1 AND active;

-- Locks the table row: serializes a table's guest submits (pending limit) and
-- accepts (one open order per table).
-- name: LockGuestTable :one
SELECT * FROM dining_tables WHERE id = $1 FOR UPDATE;

-- name: GetGuestTable :one
SELECT * FROM dining_tables WHERE id = $1;

-- name: ListGuestTables :many
SELECT t.* FROM dining_tables t
JOIN areas a ON a.id = t.area_id
ORDER BY a.sort, t.sort, t.name;

-- name: SetTableGuestOrdering :one
UPDATE dining_tables SET guest_ordering = @guest_ordering WHERE id = @id RETURNING *;

-- name: SetTableGuestCode :one
UPDATE dining_tables SET guest_code = @guest_code WHERE id = @id RETURNING *;

-- name: SetSettingsGuestOrdering :one
UPDATE settings SET guest_ordering = @guest_ordering, version = version + 1, updated_at = now()
WHERE id = 1 RETURNING *;

-- name: CreateGuestRequest :one
INSERT INTO guest_requests (table_id, kind, request_id, lines)
VALUES (@table_id, @kind, @request_id, @lines)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetGuestOrderByRequestID :one
SELECT * FROM guest_requests WHERE table_id = @table_id AND kind = 'ORDER' AND request_id = @request_id;

-- name: GetOpenGuestAlert :one
SELECT * FROM guest_requests WHERE table_id = @table_id AND kind = @kind AND status = 'PENDING';

-- name: CountPendingGuestOrders :one
SELECT count(*) FROM guest_requests WHERE table_id = $1 AND kind = 'ORDER' AND status = 'PENDING';

-- name: LockGuestRequest :one
SELECT * FROM guest_requests WHERE id = $1 FOR UPDATE;

-- name: HandleGuestRequest :one
UPDATE guest_requests
SET status = @status, handled_by_user_id = @handled_by_user_id, handled_at = now(),
    reject_reason = sqlc.narg(reject_reason), order_id = sqlc.narg(order_id), lines = @lines
WHERE id = @id AND status = 'PENDING'
RETURNING *;

-- Pending requests plus those handled since @since, oldest first, with names.
-- name: ListGuestRequests :many
SELECT r.*, t.name AS table_name, COALESCE(u.name, '')::text AS handled_by_name
FROM guest_requests r
JOIN dining_tables t ON t.id = r.table_id
LEFT JOIN users u ON u.id = r.handled_by_user_id
WHERE r.status = 'PENDING' OR r.handled_at > @since
ORDER BY r.created_at, r.id;

-- One table's requests: pending, or handled since @since; oldest first.
-- name: ListTableGuestRequests :many
SELECT * FROM guest_requests
WHERE table_id = @table_id AND (status = 'PENDING' OR handled_at >= @since)
ORDER BY created_at, id;

-- The table's OPEN orders, oldest first.
-- name: OpenOrdersOfTable :many
SELECT * FROM orders WHERE table_id = $1 AND status = 'OPEN' ORDER BY created_at, id;

-- name: TableOfOrder :one
SELECT table_id FROM orders WHERE id = $1;

-- Not a version bump: a guest's request must never make a waiter's command stale.
-- name: MarkBillRequested :many
UPDATE orders SET bill_requested_at = now()
WHERE table_id = $1 AND status = 'OPEN' AND bill_requested_at IS NULL
RETURNING id;

-- name: ClearTableBillRequested :many
UPDATE orders SET bill_requested_at = NULL
WHERE table_id = $1 AND status = 'OPEN' AND bill_requested_at IS NOT NULL
RETURNING id;

-- A bill was issued on the order: Ask for bill is answered.
-- name: ClearOrderBillRequested :exec
UPDATE orders SET bill_requested_at = NULL WHERE id = $1 AND bill_requested_at IS NOT NULL;

-- Marks the order's table's open Ask for bill alerts done (a bill was issued on it).
-- name: DoneOrderTableBillAlerts :many
UPDATE guest_requests SET status = 'DONE', handled_by_user_id = sqlc.narg(handled_by_user_id), handled_at = now()
WHERE table_id = (SELECT o.table_id FROM orders o WHERE o.id = @order_id) AND kind = 'BILL' AND status = 'PENDING'
RETURNING id, table_id;

-- name: MarkOrderItemsGuest :exec
UPDATE order_items SET source = 'GUEST' WHERE client_id = ANY(@client_ids::uuid[]);
