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
-- On-account payments are not money and never count. Credit repayments taken in
-- the shift count by method, by the same rule as bills: one voided while the shift
-- was open doesn't count; one voided after it closed still counts here and is
-- counted back as an adjustment in the voider's shift.
-- name: ShiftPaymentTotals :many
SELECT t.method::text AS method, COALESCE(sum(t.amount), 0)::bigint AS total
FROM (
    SELECT p.method, p.amount_paisa AS amount
    FROM payments p
    JOIN invoices i ON i.id = p.invoice_id
    JOIN shifts s ON s.id = p.shift_id
    WHERE p.shift_id = $1 AND p.status = 'CONFIRMED' AND p.method <> 'ON_ACCOUNT'
      AND NOT (i.status = 'CANCELLED' AND (s.closed_at IS NULL OR i.cancelled_at IS NULL OR i.cancelled_at <= s.closed_at))
    UNION ALL
    SELECT r.method, r.amount_paisa
    FROM repayments r
    JOIN shifts s ON s.id = r.shift_id
    WHERE r.shift_id = $1
      AND NOT (r.voided_at IS NOT NULL AND (s.closed_at IS NULL OR r.voided_at <= s.closed_at))
) t
GROUP BY t.method;

-- Repayments counted in the shift (same rule as ShiftPaymentTotals).
-- name: ShiftRepaymentTotal :one
SELECT COALESCE(sum(r.amount_paisa), 0)::bigint
FROM repayments r
JOIN shifts s ON s.id = r.shift_id
WHERE r.shift_id = $1
  AND NOT (r.voided_at IS NOT NULL AND (s.closed_at IS NULL OR r.voided_at <= s.closed_at));

-- On-account amounts billed in the shift (bills cancelled while it was open excluded).
-- name: ShiftCreditGiven :one
SELECT COALESCE(sum(p.amount_paisa), 0)::bigint
FROM payments p
JOIN invoices i ON i.id = p.invoice_id
JOIN shifts s ON s.id = p.shift_id
WHERE p.shift_id = $1 AND p.method = 'ON_ACCOUNT'
  AND NOT (i.status = 'CANCELLED' AND (s.closed_at IS NULL OR i.cancelled_at IS NULL OR i.cancelled_at <= s.closed_at));

-- Every repayment taken in the shift, oldest first.
-- name: ListShiftRepayments :many
SELECT id, number, method, amount_paisa, reference, created_at, voided_at
FROM repayments WHERE shift_id = $1
ORDER BY created_at, number;

-- Every payment taken in the shift (all methods), oldest first.
-- name: ListShiftPayments :many
SELECT p.id, p.invoice_id, p.method, p.amount_paisa, p.reference, p.status, p.created_at,
       i.number AS invoice_number, i.fiscal_year, i.mode, i.status AS invoice_status
FROM payments p
JOIN invoices i ON i.id = p.invoice_id
WHERE p.shift_id = $1 AND p.method <> 'ON_ACCOUNT'
ORDER BY p.created_at, p.id;

-- A row is either a cancelled bill's payment (invoice fields set) or a voided
-- repayment (repayment_id + repayment_number set).
-- name: ListShiftAdjustments :many
SELECT a.id, a.shift_id, a.original_shift_id, a.invoice_id, a.payment_id, a.repayment_id, a.method, a.amount_paisa, a.created_at,
       COALESCE(i.number, 0)::int AS invoice_number, COALESCE(i.fiscal_year, '')::text AS fiscal_year, COALESCE(i.mode, '')::text AS mode,
       COALESCE(r.number, 0)::int AS repayment_number
FROM shift_adjustments a
LEFT JOIN invoices i ON i.id = a.invoice_id
LEFT JOIN repayments r ON r.id = a.repayment_id
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
JOIN payments p ON p.invoice_id = i.id AND p.status = 'CONFIRMED' AND p.method <> 'ON_ACCOUNT'
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
JOIN payments p ON p.invoice_id = i.id AND p.status = 'CONFIRMED' AND p.method <> 'ON_ACCOUNT'
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

-- Supplier cash out of / back into the shift's drawer (purchases): till payments
-- (+; one voided while its shift was still open never counts), till payments from
-- a closed shift voided in this one (-), and refunds handed back on returns (-).
-- name: ListShiftPaidOuts :many
SELECT t.kind::text AS kind, t.ref_id::uuid AS ref_id, t.ref::text AS ref, t.supplier_name::text AS supplier_name,
       t.amount::bigint AS amount_paisa, COALESCE(u.name, '')::text AS by_name, t.at::timestamptz AS at
FROM (
    SELECT 'SUPPLIER_PAYMENT' AS kind, sp.id AS ref_id, 'SP-' || sp.number AS ref, sp.supplier_name, sp.amount_paisa AS amount,
           sp.paid_by_user_id AS by_id, sp.at
    FROM supplier_payments sp
    WHERE sp.method = 'TILL' AND sp.shift_id = @shift_id::uuid
      AND NOT (sp.voided_at IS NOT NULL AND sp.void_shift_id IS NULL)
    UNION ALL
    SELECT 'SUPPLIER_PAYMENT_VOID', sp.id, 'SP-' || sp.number, sp.supplier_name, -sp.amount_paisa, sp.voided_by_user_id, sp.voided_at
    FROM supplier_payments sp
    WHERE sp.void_shift_id = @shift_id::uuid
    UNION ALL
    SELECT 'SUPPLIER_REFUND', r.id, 'P-' || p.number, p.supplier_name, -r.refund_paisa, r.by_user_id, r.at
    FROM purchase_returns r JOIN purchases p ON p.id = r.purchase_id
    WHERE r.refund_shift_id = @shift_id::uuid AND r.refund_paisa > 0
) t
LEFT JOIN users u ON u.id = t.by_id
ORDER BY t.at, t.ref_id;
