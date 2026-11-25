-- Live view (owner/manager, read-only). Open = OPEN or BILLED (not closed yet).

-- name: LiveOpenOrders :many
SELECT o.id, o.type, o.table_id, o.guest_count, o.created_at, o.label, o.status, o.bill_requested_at,
       COALESCE(t.name, '')::text AS table_name,
       COALESCE(u.name, '')::text AS waiter_name
FROM orders o
LEFT JOIN dining_tables t ON t.id = o.table_id
LEFT JOIN users u ON u.id = o.waiter_user_id
WHERE o.status IN ('OPEN', 'BILLED')
ORDER BY o.created_at, o.id;

-- name: LiveOrderByID :one
SELECT o.id, o.type, o.table_id, o.guest_count, o.created_at, o.label, o.status, o.bill_requested_at,
       COALESCE(t.name, '')::text AS table_name,
       COALESCE(u.name, '')::text AS waiter_name
FROM orders o
LEFT JOIN dining_tables t ON t.id = o.table_id
LEFT JOIN users u ON u.id = o.waiter_user_id
WHERE o.id = $1;

-- Sent lines (not DRAFT) of the given orders; billed = on an ISSUED bill.
-- name: LiveItemsOfOrders :many
SELECT i.id, i.order_id, i.name_en, i.name_ne, i.variant_name_en, i.variant_name_ne, i.has_variant,
       i.quantity, i.unit_price_paisa, i.station_id, i.status, i.sent_at, i.note, i.void_reason,
       COALESCE(vu.name, '')::text AS voided_by_name,
       COALESCE(au.name, '')::text AS void_approved_by_name,
       (inv.id IS NOT NULL)::boolean AS billed,
       COALESCE(inv.mode, '')::text AS bill_mode,
       COALESCE(inv.fiscal_year, '')::text AS bill_fiscal_year,
       COALESCE(inv.number, 0)::int AS bill_number
FROM order_items i
LEFT JOIN users vu ON vu.id = i.voided_by_user_id
LEFT JOIN users au ON au.id = i.void_approved_by_user_id
LEFT JOIN invoices inv ON inv.id = i.invoice_id AND inv.status = 'ISSUED'
WHERE i.order_id = ANY(@order_ids::uuid[]) AND i.status <> 'DRAFT'
ORDER BY i.sent_at, i.created_at, i.id;

-- name: Live86Items :many
SELECT name_en, name_ne FROM menu_items WHERE is_86d AND active ORDER BY sort, name_en;

-- Guests of dine-in orders opened since $1 (not cancelled).
-- name: LiveCoversSince :one
SELECT COALESCE(sum(guest_count), 0)::int FROM orders
WHERE type = 'DINE_IN' AND status <> 'CANCELLED' AND created_at >= $1;

-- Today's exceptions: voids, bill cancellations, reprints, 86s, shift closes, and
-- issued bills that carry a discount. Newest first, at most 50.
-- name: LiveExceptions :many
SELECT a.id, a.actor_user_id, a.action, a.entity, a.entity_id, a.after, a.reason,
       a.approver_user_id, a.device_label, a.created_at,
       COALESCE(actor.name, '')::text AS actor_name,
       COALESCE(approver.name, '')::text AS approver_name,
       COALESCE(inv.discount_paisa, 0)::bigint AS discount_paisa,
       COALESCE(inv.discount_reason, '')::text AS discount_reason,
       COALESCE(
         CASE a.entity
           WHEN 'invoice' THEN CASE WHEN inv.mode = 'ESTIMATE' THEN 'E-' ELSE '' END
                               || split_part(inv.fiscal_year, '.', 1) || '/' || right(split_part(inv.fiscal_year, '.', 2), 2)
                               || '-' || inv.number
           WHEN 'order_item' THEN (SELECT oi.name_en || CASE WHEN oi.variant_name_en <> '' THEN ' (' || oi.variant_name_en || ')' ELSE '' END
                                   FROM order_items oi WHERE oi.id::text = a.entity_id)
           WHEN 'menu_item' THEN (SELECT m.name_en FROM menu_items m WHERE m.id::text = a.entity_id)
           WHEN 'shift' THEN (SELECT u.name FROM shifts s JOIN users u ON u.id = s.cashier_user_id WHERE s.id::text = a.entity_id)
         END, '')::text AS subject
FROM audit_log a
LEFT JOIN invoices inv ON a.entity = 'invoice' AND inv.id::text = a.entity_id
LEFT JOIN users actor ON actor.id = a.actor_user_id
LEFT JOIN users approver ON approver.id = a.approver_user_id
WHERE a.created_at >= @since
  AND (a.action IN ('order.void', 'invoice.cancel', 'invoice.reprint', 'menu.item.86', 'shift.close')
       OR (a.action = 'invoice.issue' AND inv.discount_paisa > 0))
ORDER BY a.created_at DESC, a.id DESC
LIMIT 50;
