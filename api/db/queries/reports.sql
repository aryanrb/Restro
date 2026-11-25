-- name: TodaySalesTotal :one
SELECT COALESCE(sum(total_paisa), 0)::bigint AS total, count(*)::int AS invoice_count
FROM invoices WHERE ad_datetime >= $1 AND status = 'ISSUED';

-- name: CountOpenOrders :one
SELECT count(*)::int FROM orders WHERE status = 'OPEN';

-- name: CountOpenTables :one
SELECT count(DISTINCT table_id)::int FROM orders WHERE status = 'OPEN' AND table_id IS NOT NULL;

-- name: CountTodayDiscounts :one
SELECT count(*)::int FROM invoices WHERE discount_paisa > 0 AND ad_datetime >= $1 AND status = 'ISSUED';

-- name: TopItemsToday :many
SELECT l.name_en AS name, sum(l.quantity)::int AS quantity, sum(l.line_total_paisa)::bigint AS amount_paisa
FROM invoice_lines l JOIN invoices i ON i.id = l.invoice_id
WHERE i.ad_datetime >= $1 AND i.status = 'ISSUED'
GROUP BY l.name_en ORDER BY quantity DESC LIMIT $2;

-- name: QueryAuditLog :many
SELECT * FROM audit_log
WHERE created_at BETWEEN @from_ts AND @to_ts AND (@action::text = '' OR action = @action::text)
ORDER BY created_at DESC
LIMIT @row_limit;

-- name: TodaySalesByMethod :many
SELECT p.method, COALESCE(sum(p.amount_paisa), 0)::bigint AS total
FROM payments p JOIN invoices i ON i.id = p.invoice_id
WHERE i.ad_datetime >= $1 AND i.status = 'ISSUED' AND p.status = 'CONFIRMED'
GROUP BY p.method ORDER BY p.method;

-- name: TodayDiscountTotal :one
SELECT COALESCE(sum(discount_paisa), 0)::bigint FROM invoices WHERE ad_datetime >= $1 AND status = 'ISSUED';

-- Items voided since $1 and their value (the order line's unit price already
-- includes its modifiers).
-- name: TodayVoids :one
SELECT count(*)::int AS void_count,
       COALESCE(sum(oi.unit_price_paisa * oi.quantity), 0)::bigint AS void_value
FROM audit_log a
LEFT JOIN order_items oi ON a.entity = 'order_item' AND oi.id::text = a.entity_id
WHERE a.action = 'order.void' AND a.created_at >= $1;

-- name: TodayCancelled :one
SELECT count(*)::int AS cancelled_count, COALESCE(sum(total_paisa), 0)::bigint AS cancelled_value
FROM invoices WHERE status = 'CANCELLED' AND cancelled_at >= $1;

-- Daily totals by Kathmandu AD day. Money columns count issued bills only;
-- cancelled_count is bills issued that day and since cancelled.
-- name: DailySalesDetail :many
SELECT to_char(ad_datetime AT TIME ZONE 'Asia/Kathmandu', 'YYYY-MM-DD')::text AS ad_date,
       min(bs_date)::text AS bs_date,
       (count(*) FILTER (WHERE status = 'ISSUED'))::int AS invoice_count,
       (COALESCE(sum(total_paisa) FILTER (WHERE status = 'ISSUED'), 0))::bigint AS total_paisa,
       (COALESCE(sum(vat_paisa) FILTER (WHERE status = 'ISSUED'), 0))::bigint AS vat_paisa,
       (COALESCE(sum(taxable_paisa) FILTER (WHERE status = 'ISSUED'), 0))::bigint AS taxable_paisa,
       (COALESCE(sum(discount_paisa) FILTER (WHERE status = 'ISSUED'), 0))::bigint AS discount_paisa,
       (COALESCE(sum(service_charge_paisa) FILTER (WHERE status = 'ISSUED'), 0))::bigint AS service_charge_paisa,
       (count(*) FILTER (WHERE status = 'CANCELLED'))::int AS cancelled_count
FROM invoices
WHERE ad_datetime BETWEEN @from_ts AND @to_ts
GROUP BY 1 ORDER BY 1;

-- name: DailySalesByMethod :many
SELECT to_char(i.ad_datetime AT TIME ZONE 'Asia/Kathmandu', 'YYYY-MM-DD')::text AS ad_date,
       p.method, COALESCE(sum(p.amount_paisa), 0)::bigint AS total
FROM payments p JOIN invoices i ON i.id = p.invoice_id
WHERE i.ad_datetime BETWEEN @from_ts AND @to_ts AND i.status = 'ISSUED' AND p.status = 'CONFIRMED'
GROUP BY 1, 2 ORDER BY 1, 2;

-- Sales book rows with the issuer's name and the bill's last print (time + who;
-- 0 / '' when never printed). Tax invoices only unless include_estimates.
-- name: SalesBookDetail :many
SELECT i.id, i.fiscal_year, i.number, i.mode, i.bs_date, i.ad_datetime, i.buyer_name, i.buyer_pan,
       i.subtotal_paisa, i.discount_paisa, i.taxable_paisa, i.vat_paisa, i.total_paisa, i.status, i.print_count,
       COALESCE(u.name, '')::text AS entered_by_name,
       COALESCE(floor(extract(epoch FROM lp.printed_at)), 0)::bigint AS last_printed_at_unix,
       COALESCE(lp.printed_by_name, '')::text AS last_printed_by_name
FROM invoices i
LEFT JOIN users u ON u.id = i.created_by_user_id
LEFT JOIN LATERAL (
    SELECT p.printed_at, pu.name AS printed_by_name
    FROM invoice_prints p JOIN users pu ON pu.id = p.printed_by_user_id
    WHERE p.invoice_id = i.id
    ORDER BY p.printed_at DESC, p.id DESC
    LIMIT 1
) lp ON true
WHERE i.ad_datetime BETWEEN @from_ts AND @to_ts
  AND (@include_estimates::boolean OR i.mode = 'TAX_INVOICE')
ORDER BY i.fiscal_year, i.mode, i.number;

-- name: CancelledInvoicesDetail :many
SELECT i.id, i.fiscal_year, i.number, i.mode, i.bs_date, i.total_paisa, i.cancel_reason, i.cancelled_at,
       i.created_by_user_id, i.cancelled_by_user_id, i.cancel_approved_by_user_id,
       COALESCE(iu.name, '')::text AS issued_by_name,
       COALESCE(cu.name, '')::text AS cancelled_by_name,
       COALESCE(au.name, '')::text AS approved_by_name
FROM invoices i
LEFT JOIN users iu ON iu.id = i.created_by_user_id
LEFT JOIN users cu ON cu.id = i.cancelled_by_user_id
LEFT JOIN users au ON au.id = i.cancel_approved_by_user_id
WHERE i.status = 'CANCELLED' AND i.cancelled_at BETWEEN @from_ts AND @to_ts
ORDER BY i.cancelled_at;

-- Audit rows with actor/approver names and a readable subject for the entity.
-- name: QueryAuditLogDetail :many
SELECT a.id, a.actor_user_id, a.action, a.entity, a.entity_id, a.before, a.after, a.reason,
       a.approver_user_id, a.device_label, a.created_at,
       COALESCE(actor.name, '')::text AS actor_name,
       COALESCE(approver.name, '')::text AS approver_name,
       COALESCE(
         CASE a.entity
           WHEN 'invoice' THEN (SELECT CASE WHEN i.mode = 'ESTIMATE' THEN 'E-' ELSE '' END
                                       || split_part(i.fiscal_year, '.', 1) || '/' || right(split_part(i.fiscal_year, '.', 2), 2)
                                       || '-' || i.number
                                FROM invoices i WHERE i.id::text = a.entity_id)
           WHEN 'order_item' THEN (SELECT oi.name_en || CASE WHEN oi.variant_name_en <> '' THEN ' (' || oi.variant_name_en || ')' ELSE '' END
                                   FROM order_items oi WHERE oi.id::text = a.entity_id)
           WHEN 'order' THEN (SELECT COALESCE(t.name, 'Takeaway') FROM orders o LEFT JOIN dining_tables t ON t.id = o.table_id
                              WHERE o.id::text = a.entity_id)
           WHEN 'user' THEN (SELECT u.name FROM users u WHERE u.id::text = a.entity_id)
           WHEN 'shift' THEN (SELECT u.name FROM shifts s JOIN users u ON u.id = s.cashier_user_id WHERE s.id::text = a.entity_id)
           WHEN 'menu_item' THEN (SELECT m.name_en FROM menu_items m WHERE m.id::text = a.entity_id)
           WHEN 'menu_category' THEN (SELECT c.name_en FROM menu_categories c WHERE c.id::text = a.entity_id)
           WHEN 'modifier_group' THEN (SELECT g.name_en FROM modifier_groups g WHERE g.id::text = a.entity_id)
           WHEN 'table' THEN (SELECT t.name FROM dining_tables t WHERE t.id::text = a.entity_id)
           WHEN 'area' THEN (SELECT ar.name_en FROM areas ar WHERE ar.id::text = a.entity_id)
           WHEN 'station' THEN (SELECT st.name_en FROM stations st WHERE st.id::text = a.entity_id)
           WHEN 'device' THEN (SELECT d.label FROM devices d WHERE d.id::text = a.entity_id)
           WHEN 'customer' THEN (SELECT cu.name FROM customers cu WHERE cu.id::text = a.entity_id)
           WHEN 'repayment' THEN (SELECT 'R-' || r.number FROM repayments r WHERE r.id::text = a.entity_id)
           WHEN 'settings' THEN 'Settings'
         END, '')::text AS subject
FROM audit_log a
LEFT JOIN users actor ON actor.id = a.actor_user_id
LEFT JOIN users approver ON approver.id = a.approver_user_id
WHERE a.created_at BETWEEN @from_ts AND @to_ts
  AND (@action::text = '' OR a.action = @action::text)
  AND (sqlc.narg('actor_id')::uuid IS NULL OR a.actor_user_id = sqlc.narg('actor_id')::uuid)
ORDER BY a.created_at DESC, a.id DESC
LIMIT @row_limit;
