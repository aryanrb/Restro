-- name: ListStationOpenItems :many
SELECT
    i.*,
    o.client_id AS order_client_id,
    o.type      AS order_type,
    COALESCE(t.name, '') AS table_name
FROM order_items i
JOIN orders o ON o.id = i.order_id
LEFT JOIN dining_tables t ON t.id = o.table_id
WHERE i.station_id = $1 AND i.status IN ('SENT', 'PREPARING')
ORDER BY i.created_at;

-- name: ListModifiersByItemIDs :many
SELECT * FROM order_item_modifiers WHERE order_item_id = ANY(@item_ids::uuid[]);

-- name: ListOrderItemsByIDs :many
SELECT * FROM order_items WHERE id = ANY(@ids::uuid[]);

-- name: KdsSetItemsStatus :exec
UPDATE order_items SET status = @status
WHERE id = ANY(@ids::uuid[]) AND status = ANY(@from_status::text[]);

-- name: BumpStationTicket :many
UPDATE order_items SET status = 'READY'
WHERE order_id = @order_id AND station_id = @station_id AND status IN ('SENT', 'PREPARING')
RETURNING id;
