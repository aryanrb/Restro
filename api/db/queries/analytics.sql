-- Analytics: raw reads for building daily summaries over [from_ts, to_ts), the
-- summary store, the festival calendar and analytics settings.

-- ISSUED bills in the range with their order's type and waiter.
-- name: AnalyticsInvoices :many
SELECT i.id, i.ad_datetime, i.total_paisa, i.discount_paisa, i.service_charge_paisa, i.vat_paisa,
       i.discount_reason, i.created_by_user_id,
       COALESCE(o.type, '')::text AS order_type,
       o.waiter_user_id
FROM invoices i
LEFT JOIN orders o ON o.id = i.order_id
WHERE i.status = 'ISSUED' AND i.ad_datetime >= @from_ts AND i.ad_datetime < @to_ts;

-- Confirmed payments of those bills (the daily sales report's rule).
-- name: AnalyticsPayments :many
SELECT p.invoice_id, p.method, p.amount_paisa
FROM payments p
JOIN invoices i ON i.id = p.invoice_id
WHERE i.status = 'ISSUED' AND p.status = 'CONFIRMED' AND i.ad_datetime >= @from_ts AND i.ad_datetime < @to_ts;

-- Lines of those bills, with the menu item and category they came from and their
-- actual cost from the SALE stock movements of the order item (unit cost
-- snapshot; has_cost = some movement exists).
-- name: AnalyticsLines :many
SELECT i.ad_datetime, l.name_en, l.name_ne, l.quantity, l.line_total_paisa,
       COALESCE(oi.menu_item_id::text, '')::text AS menu_item_id,
       COALESCE(m.category_id::text, '')::text AS category_id,
       (COALESCE(c.n, 0) > 0)::boolean AS has_cost,
       COALESCE(round(c.cost_e7 / 10000000), 0)::bigint AS cost_paisa
FROM invoice_lines l
JOIN invoices i ON i.id = l.invoice_id
LEFT JOIN order_items oi ON oi.id = l.order_item_id
LEFT JOIN menu_items m ON m.id = oi.menu_item_id
LEFT JOIN LATERAL (
    SELECT count(*) AS n, sum(-sm.quantity_milli::numeric * sm.unit_cost_e4) AS cost_e7
    FROM stock_movements sm
    WHERE sm.ref_id = l.order_item_id AND sm.type IN ('SALE', 'SALE_REVERSAL')
) c ON true
WHERE i.status = 'ISSUED' AND i.ad_datetime >= @from_ts AND i.ad_datetime < @to_ts;

-- Orders opened in the range (not cancelled), with their last issued bill.
-- name: AnalyticsOrders :many
SELECT o.id, o.type, o.table_id, o.waiter_user_id, o.guest_count, o.created_at, o.status,
       COALESCE(floor(extract(epoch FROM (SELECT max(b.ad_datetime) FROM invoices b
                                          WHERE b.order_id = o.id AND b.status = 'ISSUED'))), 0)::bigint AS last_bill_unix
FROM orders o
WHERE o.status <> 'CANCELLED' AND o.created_at >= @from_ts AND o.created_at < @to_ts;

-- Lines sent in the range (a void belongs to the day of its send).
-- name: AnalyticsSentItems :many
SELECT i.menu_item_id, i.name_en, i.name_ne, i.quantity, i.unit_price_paisa, i.status, i.sent_at,
       i.added_by_user_id, i.voided_by_user_id, i.void_approved_by_user_id,
       COALESCE(m.category_id::text, '')::text AS category_id
FROM order_items i
LEFT JOIN menu_items m ON m.id = i.menu_item_id
WHERE i.status <> 'DRAFT' AND i.sent_at >= @from_ts AND i.sent_at < @to_ts;

-- name: AnalyticsCancelledInvoices :many
SELECT cancelled_at, total_paisa, cancelled_by_user_id, cancel_approved_by_user_id
FROM invoices
WHERE status = 'CANCELLED' AND cancelled_at >= @from_ts AND cancelled_at < @to_ts;

-- Shifts closed in the range and their counted − expected cash. Expected = float +
-- cash takings − cash adjustments, by the same rules as ShiftPaymentTotals.
-- name: AnalyticsClosedShifts :many
SELECT s.closed_at,
       (s.counted_cash_paisa - s.opening_float_paisa
        - COALESCE((SELECT sum(p.amount_paisa) FROM payments p JOIN invoices i ON i.id = p.invoice_id
                    WHERE p.shift_id = s.id AND p.status = 'CONFIRMED' AND p.method = 'CASH'
                      AND NOT (i.status = 'CANCELLED' AND (i.cancelled_at IS NULL OR i.cancelled_at <= s.closed_at))), 0)
        - COALESCE((SELECT sum(r.amount_paisa) FROM repayments r
                    WHERE r.shift_id = s.id AND r.method = 'CASH'
                      AND NOT (r.voided_at IS NOT NULL AND r.voided_at <= s.closed_at)), 0)
        + COALESCE((SELECT sum(a.amount_paisa) FROM shift_adjustments a
                    WHERE a.shift_id = s.id AND a.method = 'CASH'), 0))::bigint AS difference_paisa
FROM shifts s
WHERE s.status = 'CLOSED' AND s.counted_cash_paisa IS NOT NULL
  AND s.closed_at >= @from_ts AND s.closed_at < @to_ts;

-- ISSUED bills in the range and the credit customer on their on-account payment.
-- name: AnalyticsIdentifiedBills :many
SELECT i.ad_datetime, i.total_paisa, c.id AS customer_id,
       COALESCE(c.name, '')::text AS customer_name,
       COALESCE(c.phone, '')::text AS customer_phone
FROM invoices i
LEFT JOIN LATERAL (
    SELECT p.customer_id FROM payments p
    WHERE p.invoice_id = i.id AND p.method = 'ON_ACCOUNT'
    ORDER BY p.created_at, p.id LIMIT 1
) pc ON true
LEFT JOIN customers c ON c.id = pc.customer_id
WHERE i.status = 'ISSUED' AND i.ad_datetime >= @from_ts AND i.ad_datetime < @to_ts;

-- First instant with any bill or order; 0 when there is none.
-- name: AnalyticsFirstActivityUnix :one
SELECT COALESCE(floor(extract(epoch FROM LEAST((SELECT min(ad_datetime) FROM invoices),
                                              (SELECT min(created_at) FROM orders)))), 0)::bigint;

-- name: AnalyticsUsers :many
SELECT id, name, role FROM users;

-- name: AnalyticsMenuItems :many
SELECT id, name_en, name_ne, category_id, active FROM menu_items ORDER BY sort, name_en;

-- name: AnalyticsHasRecipes :one
SELECT (EXISTS (SELECT 1 FROM menu_item_recipe_lines)
        OR EXISTS (SELECT 1 FROM variant_recipe_lines)
        OR EXISTS (SELECT 1 FROM modifier_recipe_lines))::boolean;

-- Summary store ---------------------------------------------------------------

-- name: AnalyticsDaysIn :many
SELECT day, summary, built_at FROM analytics_days WHERE day BETWEEN @from_day AND @to_day;

-- name: UpsertAnalyticsDay :exec
INSERT INTO analytics_days (day, summary, built_at) VALUES ($1, $2, now())
ON CONFLICT (day) DO UPDATE SET summary = EXCLUDED.summary, built_at = EXCLUDED.built_at;

-- name: DeleteAnalyticsDaysIn :exec
DELETE FROM analytics_days WHERE day BETWEEN @from_day AND @to_day;

-- name: AnalyticsDirtyDaysIn :many
SELECT day FROM analytics_dirty_days WHERE day BETWEEN @from_day AND @to_day;

-- name: ClearAnalyticsDirtyDays :exec
DELETE FROM analytics_dirty_days WHERE day = ANY(@days::date[]);

-- name: AnalyticsLastBuildUnix :one
SELECT COALESCE(floor(extract(epoch FROM (SELECT max(built_at) FROM analytics_days))), 0)::bigint;

-- Calendar --------------------------------------------------------------------

-- name: ListCalendarEventsIn :many
SELECT * FROM calendar_events
WHERE from_date <= @to_day AND to_date >= @from_day
ORDER BY from_date, name_en, id;

-- name: GetCalendarEvent :one
SELECT * FROM calendar_events WHERE id = $1;

-- name: CreateCalendarEvent :one
INSERT INTO calendar_events (name_en, name_ne, from_date, to_date, kind, created_by_user_id, key)
VALUES (@name_en, @name_ne, @from_day, @to_day, @kind, @created_by_user_id, @key)
RETURNING *;

-- Saved by the owner: the date is now theirs, no longer "to check".
-- name: UpdateCalendarEvent :one
UPDATE calendar_events
SET name_en = @name_en, name_ne = @name_ne, from_date = @from_day, to_date = @to_day, kind = @kind,
    key = @key, approximate = false, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteCalendarEvent :execrows
DELETE FROM calendar_events WHERE id = $1;

-- Settings --------------------------------------------------------------------

-- name: GetAnalyticsSettings :one
SELECT managers_see_staff, highlight_rules FROM analytics_settings WHERE id;

-- name: SaveAnalyticsSettings :one
UPDATE analytics_settings SET managers_see_staff = @managers_see_staff, highlight_rules = @highlight_rules, updated_at = now()
WHERE id
RETURNING managers_see_staff, highlight_rules;
