-- Credit customers (udharo): accounts, balances, repayments, statements.

-- name: ListCustomersWithBalance :many
SELECT c.*, b.balance_paisa
FROM customers c JOIN customer_balances b ON b.customer_id = c.id
ORDER BY lower(c.name), c.created_at, c.id;

-- name: GetCustomerWithBalance :one
SELECT c.*, b.balance_paisa
FROM customers c JOIN customer_balances b ON b.customer_id = c.id
WHERE c.id = $1;

-- Locks the account for the billing / repayment transaction, so two tills can't
-- both use the last of a limit (or repay the same balance twice).
-- name: LockCustomer :one
SELECT * FROM customers WHERE id = $1 FOR UPDATE;

-- name: CustomerBalance :one
SELECT balance_paisa FROM customer_balances WHERE customer_id = $1;

-- name: CustomerIDByPhone :one
SELECT id FROM customers WHERE phone = $1 AND phone <> '';

-- name: CreateCustomer :one
INSERT INTO customers (name, phone, notes, pan, address, contact_person, created_by_user_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateCustomer :one
UPDATE customers
SET name = $2, phone = $3, notes = $4, pan = $5, address = $6, contact_person = $7,
    credit_limit_paisa = $8, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetCustomerActive :one
UPDATE customers SET active = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- On-account amounts per issued bill (one row per bill and customer), oldest
-- first. customer_id NULL = every customer.
-- name: ListCreditCharges :many
SELECT p.customer_id::uuid AS customer_id, i.id AS invoice_id, i.ad_datetime, i.bs_date,
       i.fiscal_year, i.mode, i.number, i.display_number,
       sum(p.amount_paisa)::bigint AS amount_paisa
FROM payments p JOIN invoices i ON i.id = p.invoice_id
WHERE p.method = 'ON_ACCOUNT' AND i.status = 'ISSUED'
  AND (sqlc.narg('customer_id')::uuid IS NULL OR p.customer_id = sqlc.narg('customer_id')::uuid)
GROUP BY p.customer_id, i.id
ORDER BY i.ad_datetime, i.number, i.id;

-- Repayments not voided, oldest first. customer_id NULL = every customer.
-- name: ListLiveRepayments :many
SELECT * FROM repayments
WHERE voided_at IS NULL
  AND (sqlc.narg('customer_id')::uuid IS NULL OR customer_id = sqlc.narg('customer_id')::uuid)
ORDER BY created_at, number;

-- Gap-free repayment receipt numbers: the upsert locks the single counter row.
-- name: AllocateRepaymentNumber :one
INSERT INTO repayment_counters (id, next_number) VALUES (true, 2)
ON CONFLICT (id) DO UPDATE SET next_number = repayment_counters.next_number + 1
RETURNING (next_number - 1)::int AS number;

-- name: CreateRepayment :one
INSERT INTO repayments (number, customer_id, amount_paisa, method, reference, note, shift_id, taken_by_user_id, bs_date, balance_after_paisa)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- An on-account tender of a bill: no money, so no provider; confirmed at once.
-- name: CreateOnAccountPayment :one
INSERT INTO payments (invoice_id, method, amount_paisa, reference, confirmed_by_user_id, shift_id, status,
                      customer_id, customer_name, balance_after_paisa, credit_approved_by_user_id)
VALUES ($1, 'ON_ACCOUNT', $2, $3, $4, $5, 'CONFIRMED', $6, $7, $8, $9)
RETURNING *;

-- Customers owed by a bill's on-account payments (for realtime after a cancel).
-- name: ListInvoiceCreditCustomers :many
SELECT DISTINCT customer_id::uuid FROM payments WHERE invoice_id = $1 AND method = 'ON_ACCOUNT';

-- name: LockRepayment :one
SELECT * FROM repayments WHERE id = $1 FOR UPDATE;

-- name: VoidRepayment :exec
UPDATE repayments SET voided_at = now(), voided_by_user_id = $2, void_reason = $3
WHERE id = $1 AND voided_at IS NULL;

-- name: GetRepaymentDetail :one
SELECT r.*, c.name AS customer_name, COALESCE(u.name, '')::text AS taken_by_name
FROM repayments r
JOIN customers c ON c.id = r.customer_id
LEFT JOIN users u ON u.id = r.taken_by_user_id
WHERE r.id = $1;

-- Newest first, voided included.
-- name: ListRepaymentDetails :many
SELECT r.*, c.name AS customer_name, COALESCE(u.name, '')::text AS taken_by_name
FROM repayments r
JOIN customers c ON c.id = r.customer_id
LEFT JOIN users u ON u.id = r.taken_by_user_id
WHERE r.customer_id = $1
ORDER BY r.created_at DESC, r.number DESC;

-- A voided repayment from a closed shift, counted back in the voider's open shift.
-- name: CreateRepaymentAdjustment :exec
INSERT INTO shift_adjustments (shift_id, original_shift_id, repayment_id, method, amount_paisa, created_by_user_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, now());

-- Dashboard: on-account amounts on issued bills since $1.
-- name: CreditGivenSince :one
SELECT COALESCE(sum(p.amount_paisa), 0)::bigint
FROM payments p JOIN invoices i ON i.id = p.invoice_id
WHERE p.method = 'ON_ACCOUNT' AND i.status = 'ISSUED' AND i.ad_datetime >= $1;

-- Dashboard: repayments (not voided) taken since $1.
-- name: RepaymentsSince :one
SELECT COALESCE(sum(amount_paisa), 0)::bigint FROM repayments WHERE voided_at IS NULL AND created_at >= $1;

-- Dashboard: everything owed now (positive balances only).
-- name: TotalCreditOwed :one
SELECT COALESCE(sum(balance_paisa) FILTER (WHERE balance_paisa > 0), 0)::bigint FROM customer_balances;

-- Loyalty: a loyalty-only member gets a credit account (same record, one per phone).
-- name: OpenCreditAccount :exec
UPDATE customers SET credit_account = true, updated_at = now() WHERE id = $1;
