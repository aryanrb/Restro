-- name: TodaySalesTotal :one
SELECT COALESCE(sum(total_paisa), 0)::bigint AS total, count(*)::int AS invoice_count
FROM invoices WHERE ad_datetime >= $1 AND status = 'ISSUED';

-- name: CountOpenOrders :one
SELECT count(*)::int FROM orders WHERE status = 'OPEN';

-- name: CountOpenTables :one
SELECT count(DISTINCT table_id)::int FROM orders WHERE status = 'OPEN' AND table_id IS NOT NULL;

-- name: CountTodayVoids :one
SELECT count(*)::int FROM audit_log WHERE action = 'order.void' AND created_at >= $1;

-- name: CountTodayDiscounts :one
SELECT count(*)::int FROM invoices WHERE discount_paisa > 0 AND ad_datetime >= $1 AND status = 'ISSUED';

-- name: TopItemsToday :many
SELECT l.name_en AS name, sum(l.quantity)::int AS quantity, sum(l.line_total_paisa)::bigint AS amount_paisa
FROM invoice_lines l JOIN invoices i ON i.id = l.invoice_id
WHERE i.ad_datetime >= $1 AND i.status = 'ISSUED'
GROUP BY l.name_en ORDER BY quantity DESC LIMIT $2;

-- name: SalesBook :many
SELECT * FROM invoices WHERE ad_datetime BETWEEN $1 AND $2 ORDER BY fiscal_year, mode, number;

-- name: DailySales :many
SELECT bs_date,
       count(*)::int AS invoice_count,
       COALESCE(sum(total_paisa), 0)::bigint AS total_paisa,
       COALESCE(sum(vat_paisa), 0)::bigint AS vat_paisa,
       COALESCE(sum(taxable_paisa), 0)::bigint AS taxable_paisa
FROM invoices WHERE ad_datetime BETWEEN $1 AND $2 AND status = 'ISSUED'
GROUP BY bs_date ORDER BY bs_date;

-- name: CancelledInvoices :many
SELECT * FROM invoices WHERE status = 'CANCELLED' AND cancelled_at BETWEEN $1 AND $2 ORDER BY cancelled_at;

-- name: QueryAuditLog :many
SELECT * FROM audit_log
WHERE created_at BETWEEN @from_ts AND @to_ts AND (@action::text = '' OR action = @action::text)
ORDER BY created_at DESC
LIMIT @row_limit;
