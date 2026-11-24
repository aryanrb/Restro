-- name: OpenShift :one
INSERT INTO shifts (cashier_user_id, opening_float_paisa, note) VALUES ($1, $2, $3) RETURNING *;

-- name: GetOpenShiftForUser :one
SELECT * FROM shifts WHERE cashier_user_id = $1 AND status = 'OPEN';

-- name: GetShift :one
SELECT * FROM shifts WHERE id = $1;

-- name: ListOpenShifts :many
SELECT * FROM shifts WHERE status = 'OPEN' ORDER BY opened_at;

-- name: CloseShift :one
UPDATE shifts SET status = 'CLOSED', counted_cash_paisa = $2, note = $3, closed_at = now()
WHERE id = $1 AND status = 'OPEN'
RETURNING *;

-- name: ShiftPaymentTotals :many
SELECT method, COALESCE(sum(amount_paisa), 0)::bigint AS total
FROM payments WHERE shift_id = $1 AND status = 'CONFIRMED'
GROUP BY method;

-- name: ListShiftQrPayments :many
SELECT p.*, i.number AS invoice_number
FROM payments p
JOIN invoices i ON i.id = p.invoice_id
WHERE p.shift_id = $1 AND p.method IN ('QR_FONEPAY', 'QR_OTHER')
ORDER BY p.created_at;
