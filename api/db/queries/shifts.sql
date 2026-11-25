-- name: OpenShift :one
INSERT INTO shifts (cashier_user_id, opening_float_paisa, note) VALUES ($1, $2, $3) RETURNING *;

-- name: GetOpenShiftForUser :one
SELECT * FROM shifts WHERE cashier_user_id = $1 AND status = 'OPEN';

-- name: GetShift :one
SELECT * FROM shifts WHERE id = $1;

-- name: ListOpenShifts :many
SELECT * FROM shifts WHERE status = 'OPEN' ORDER BY opened_at;

-- The opening note is kept; the close-out note goes in closing_note.
-- name: CloseShift :one
UPDATE shifts SET status = 'CLOSED', counted_cash_paisa = $2, closing_note = $3, closed_at = now()
WHERE id = $1 AND status = 'OPEN'
RETURNING *;

-- Confirmed takings by method. A payment on a bill cancelled while this shift was
-- still open does not count; one cancelled after it closed still counts here (it
-- was in the drawer) and is counted back as an adjustment in the canceller's shift.
-- name: ShiftPaymentTotals :many
SELECT p.method, COALESCE(sum(p.amount_paisa), 0)::bigint AS total
FROM payments p
JOIN invoices i ON i.id = p.invoice_id
JOIN shifts s ON s.id = p.shift_id
WHERE p.shift_id = $1 AND p.status = 'CONFIRMED'
  AND NOT (i.status = 'CANCELLED' AND (s.closed_at IS NULL OR i.cancelled_at IS NULL OR i.cancelled_at <= s.closed_at))
GROUP BY p.method;

-- Every payment taken in the shift (all methods), oldest first.
-- name: ListShiftPayments :many
SELECT p.id, p.invoice_id, p.method, p.amount_paisa, p.reference, p.status, p.created_at,
       i.number AS invoice_number, i.fiscal_year, i.mode, i.status AS invoice_status
FROM payments p
JOIN invoices i ON i.id = p.invoice_id
WHERE p.shift_id = $1
ORDER BY p.created_at, p.id;

-- name: ListShiftAdjustments :many
SELECT a.id, a.shift_id, a.original_shift_id, a.invoice_id, a.payment_id, a.method, a.amount_paisa, a.created_at,
       i.number AS invoice_number, i.fiscal_year, i.mode
FROM shift_adjustments a
JOIN invoices i ON i.id = a.invoice_id
WHERE a.shift_id = $1
ORDER BY a.created_at, a.id;

-- Records the adjustments owed by cancelled bills whose payments were taken in a
-- shift that had already closed when the bill was cancelled. Each lands in the
-- canceller's shift that was open at cancelled_at. Idempotent (one row per
-- payment); invoice_id NULL = every cancelled bill (catch-up).
-- name: RecordCancelAdjustments :execrows
INSERT INTO shift_adjustments (shift_id, original_shift_id, invoice_id, payment_id, method, amount_paisa, created_by_user_id, created_at)
SELECT DISTINCT ON (p.id) cs.id, p.shift_id, i.id, p.id, p.method, p.amount_paisa, i.cancelled_by_user_id, i.cancelled_at
FROM invoices i
JOIN payments p ON p.invoice_id = i.id AND p.status = 'CONFIRMED'
JOIN shifts os ON os.id = p.shift_id
JOIN shifts cs ON cs.cashier_user_id = i.cancelled_by_user_id
     AND cs.opened_at <= i.cancelled_at AND (cs.closed_at IS NULL OR cs.closed_at >= i.cancelled_at)
WHERE i.status = 'CANCELLED' AND i.cancelled_at IS NOT NULL
  AND os.closed_at IS NOT NULL AND os.closed_at < i.cancelled_at
  AND (sqlc.narg('invoice_id')::uuid IS NULL OR i.id = sqlc.narg('invoice_id')::uuid)
  AND NOT EXISTS (SELECT 1 FROM shift_adjustments a WHERE a.payment_id = p.id)
ORDER BY p.id, cs.opened_at DESC
ON CONFLICT (payment_id) DO NOTHING;

-- Payments of a cancelled bill that need an adjustment but have no canceller
-- shift to land in (the canceller had no shift open at the time).
-- name: CountUnplacedCancelAdjustments :one
SELECT count(*)::int
FROM invoices i
JOIN payments p ON p.invoice_id = i.id AND p.status = 'CONFIRMED'
JOIN shifts os ON os.id = p.shift_id
WHERE i.id = $1 AND i.status = 'CANCELLED'
  AND os.closed_at IS NOT NULL AND os.closed_at < i.cancelled_at
  AND NOT EXISTS (SELECT 1 FROM shift_adjustments a WHERE a.payment_id = p.id);

-- Recent shifts, newest first; cashier_user_id NULL = everyone's.
-- name: ListShifts :many
SELECT * FROM shifts
WHERE (sqlc.narg('cashier_user_id')::uuid IS NULL OR cashier_user_id = sqlc.narg('cashier_user_id')::uuid)
ORDER BY opened_at DESC, id
LIMIT @row_limit;

-- Kept for the deprecated ShiftReport.qr_payments field.
-- name: ListShiftQrPayments :many
SELECT p.*, i.number AS invoice_number
FROM payments p
JOIN invoices i ON i.id = p.invoice_id
WHERE p.shift_id = $1 AND p.method IN ('QR_FONEPAY', 'QR_OTHER')
ORDER BY p.created_at;

-- Reads the caller's open shift inside a billing transaction and holds a share
-- lock on it, so a concurrent CloseShift waits until the payments recorded
-- against it have committed (and are counted in its report).
-- name: LockOpenShiftForUser :one
SELECT * FROM shifts WHERE cashier_user_id = $1 AND status = 'OPEN' FOR SHARE;
