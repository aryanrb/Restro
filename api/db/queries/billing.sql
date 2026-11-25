-- Gap-free number allocation: the ON CONFLICT ... DO UPDATE locks the counter row.
-- name: AllocateInvoiceNumber :one
INSERT INTO invoice_counters (fiscal_year, mode, next_number) VALUES ($1, $2, 2)
ON CONFLICT (fiscal_year, mode) DO UPDATE SET next_number = invoice_counters.next_number + 1
RETURNING next_number - 1 AS number;

-- name: CreateInvoice :one
INSERT INTO invoices (
    fiscal_year, number, mode, bs_date, bs_year, bs_month, bs_day, order_id,
    buyer_name, buyer_pan, subtotal_paisa, discount_paisa, service_charge_paisa,
    taxable_paisa, vat_paisa, rounding_paisa, total_paisa, discount_reason,
    discount_approved_by_user_id, created_by_user_id,
    display_number, seller_name_en, seller_name_ne, seller_address, seller_pan_number,
    seller_vat_rate_bp, seller_vat_registered, seller_logo_url, issued_by_name, place, order_client_id,
    loyalty_customer_id, reward_paisa, loyalty_snapshot
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
    $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34
) RETURNING *;

-- name: CreateInvoiceLine :one
INSERT INTO invoice_lines (invoice_id, order_item_id, name_en, name_ne, variant_name, quantity, unit_price_paisa, modifiers_json, line_total_paisa, order_item_client_id, variant_name_ne)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetInvoice :one
SELECT * FROM invoices WHERE id = $1;

-- name: GetInvoiceLines :many
SELECT * FROM invoice_lines WHERE invoice_id = $1 ORDER BY id;

-- name: CancelInvoice :one
UPDATE invoices
SET status = 'CANCELLED', cancel_reason = $2, cancelled_by_user_id = $3, cancel_approved_by_user_id = $4, cancelled_at = now()
WHERE id = $1 AND status = 'ISSUED'
RETURNING *;

-- name: IncrementPrintCount :one
UPDATE invoices SET print_count = print_count + 1 WHERE id = $1 RETURNING print_count;

-- name: CreateInvoicePrint :exec
INSERT INTO invoice_prints (invoice_id, printed_by_user_id) VALUES ($1, $2);

-- Oldest first, with the printer's current name.
-- name: ListInvoicePrints :many
SELECT p.printed_at, p.printed_by_user_id, u.name AS printed_by_name
FROM invoice_prints p JOIN users u ON u.id = p.printed_by_user_id
WHERE p.invoice_id = $1
ORDER BY p.printed_at, p.id;

-- The invoice's place for a dine-in order.
-- name: GetDiningTableName :one
SELECT name FROM dining_tables WHERE id = $1;

-- Locks the order row for the billing transaction so concurrent bills (different
-- command ids) of the same order serialize; the loser then sees the new status /
-- unbilled set. Order mutations (version bumps) also wait on this lock.
-- name: LockOrderByClientIDForBilling :one
SELECT * FROM orders WHERE client_id = $1 FOR UPDATE;

-- name: ListUnbilledOrderItems :many
SELECT * FROM order_items WHERE order_id = $1 AND invoice_id IS NULL AND status <> 'VOIDED' ORDER BY created_at;

-- Only claims items that are still unbilled; the caller checks the row count so
-- an item billed by a concurrent command is never re-assigned.
-- name: SetOrderItemsInvoice :execrows
UPDATE order_items SET invoice_id = @invoice_id WHERE id = ANY(@ids::uuid[]) AND invoice_id IS NULL;

-- name: UnlinkInvoiceItems :many
UPDATE order_items SET invoice_id = NULL WHERE invoice_id = $1 RETURNING order_id;

-- name: CreateCbmsOutbox :exec
INSERT INTO cbms_outbox (invoice_id, kind, payload, status) VALUES ($1, $2, $3, $4);

-- name: CreatePayment :one
INSERT INTO payments (invoice_id, method, amount_paisa, reference, confirmed_by_user_id, shift_id, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListPaymentsForInvoice :many
SELECT * FROM payments WHERE invoice_id = $1 ORDER BY created_at;

-- name: ListInvoicesForOrder :many
SELECT * FROM invoices WHERE order_id = $1 ORDER BY ad_datetime DESC, number DESC;

-- name: ListInvoicesByBsDate :many
SELECT * FROM invoices WHERE bs_date = $1 ORDER BY ad_datetime DESC, number DESC;

-- Half-open AD range [from, to).
-- name: ListInvoicesByAdRange :many
SELECT * FROM invoices
WHERE ad_datetime >= @from_time AND ad_datetime < @to_time
ORDER BY ad_datetime DESC, number DESC;
