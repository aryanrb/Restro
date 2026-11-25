-- Loyalty stamp card: settings, members (customers), the append-only ledger.

-- name: GetLoyaltySettings :one
SELECT * FROM loyalty_settings WHERE id;

-- name: UpdateLoyaltySettings :one
UPDATE loyalty_settings
SET enabled = $1, stamps_per_reward = $2, min_spend_paisa = $3, reward_value_paisa = $4,
    expiry_months = $5, credit_bills_earn = $6, birthday_reward_paisa = $7, updated_at = now()
WHERE id
RETURNING *;

-- name: LoyaltyCustomerByPhone :one
SELECT * FROM customers WHERE phone = $1 AND phone <> '';

-- name: LoyaltyCustomerByToken :one
SELECT * FROM customers WHERE card_token = $1 AND loyalty_member;

-- name: LoyaltyGetCustomer :one
SELECT * FROM customers WHERE id = $1;

-- name: LoyaltyLockCustomer :one
SELECT * FROM customers WHERE id = $1 FOR UPDATE;

-- A loyalty-only member: no credit account (hidden from the credit customer list).
-- name: LoyaltyCreateMember :one
INSERT INTO customers (name, phone, created_by_user_id, credit_account, loyalty_member, loyalty_joined_at,
                       loyalty_joined_by_user_id, loyalty_consent_at, birth_month_bs, card_token)
VALUES ($1, $2, $3, false, true, now(), $3, now(), $4, $5)
RETURNING *;

-- An existing customer (e.g. a credit account) joins: same record, one per phone.
-- name: LoyaltyEnroll :one
UPDATE customers
SET loyalty_member = true, loyalty_joined_at = now(), loyalty_joined_by_user_id = $2, loyalty_consent_at = now(),
    birth_month_bs = $3, card_token = $4, updated_at = now()
WHERE id = $1
RETURNING *;

-- Leaving: no card; the phone is erased unless the customer still has a credit account.
-- name: LoyaltyLeave :exec
UPDATE customers
SET loyalty_member = false, card_token = NULL, birth_month_bs = NULL,
    phone = CASE WHEN credit_account THEN phone ELSE '' END, updated_at = now()
WHERE id = $1;

-- name: LoyaltySetCardToken :exec
UPDATE customers SET card_token = $2, updated_at = now() WHERE id = $1;

-- Everything a member's card summary is computed from. stamped_today: an EARN on a
-- bill still issued on business_date. last_visit: joining, a bill still issued
-- with the member on it, or a manual adjustment. 'epoch' = none.
-- name: LoyaltyCardStats :one
SELECT
    COALESCE((SELECT sum(e.stamps) FROM loyalty_entries e WHERE e.customer_id = c.id AND e.status = 'DONE'), 0)::int AS stamps,
    COALESCE((SELECT sum(e.stamps) FROM loyalty_entries e WHERE e.customer_id = c.id AND e.status = 'PENDING'), 0)::int AS pending,
    EXISTS (SELECT 1 FROM loyalty_entries e JOIN invoices i ON i.id = e.invoice_id
            WHERE e.customer_id = c.id AND e.kind = 'EARN' AND e.business_date = sqlc.arg('business_date')::date
              AND i.status = 'ISSUED') AS stamped_today,
    COALESCE((SELECT max(e.created_at) FROM loyalty_entries e JOIN invoices i ON i.id = e.invoice_id
              WHERE e.customer_id = c.id AND e.kind = 'BIRTHDAY' AND i.status = 'ISSUED'), 'epoch')::timestamptz AS last_birthday_at,
    COALESCE(GREATEST(c.loyalty_joined_at,
             (SELECT max(i.ad_datetime) FROM invoices i WHERE i.loyalty_customer_id = c.id AND i.status = 'ISSUED'),
             (SELECT max(e.created_at) FROM loyalty_entries e WHERE e.customer_id = c.id AND e.kind = 'ADJUST')), 'epoch')::timestamptz AS last_visit_at
FROM customers c
WHERE c.id = sqlc.arg('customer_id');

-- name: ListLoyaltyMembers :many
SELECT c.id, c.name, c.phone, c.loyalty_joined_at, c.created_at,
    COALESCE((SELECT sum(e.stamps) FROM loyalty_entries e WHERE e.customer_id = c.id AND e.status = 'DONE'), 0)::int AS stamps,
    COALESCE(GREATEST(c.loyalty_joined_at,
             (SELECT max(i.ad_datetime) FROM invoices i WHERE i.loyalty_customer_id = c.id AND i.status = 'ISSUED'),
             (SELECT max(e.created_at) FROM loyalty_entries e WHERE e.customer_id = c.id AND e.kind = 'ADJUST')), 'epoch')::timestamptz AS last_visit_at
FROM customers c
WHERE c.loyalty_member
ORDER BY lower(c.name), c.id;

-- name: InsertLoyaltyEntry :one
INSERT INTO loyalty_entries (customer_id, kind, stamps, status, reward_value_paisa, invoice_id, business_date,
                             by_user_id, approved_by_user_id, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- Newest first, with the bill number and who made it.
-- name: ListLoyaltyEntries :many
SELECT e.*, COALESCE(i.display_number, '')::text AS invoice_number, COALESCE(u.name, '')::text AS by_name
FROM loyalty_entries e
LEFT JOIN invoices i ON i.id = e.invoice_id
LEFT JOIN users u ON u.id = e.by_user_id
WHERE e.customer_id = $1
ORDER BY e.seq DESC;

-- A bill's own entries (not reversals), for its cancellation.
-- name: ListInvoiceLoyaltyEntries :many
SELECT * FROM loyalty_entries WHERE invoice_id = $1 AND kind <> 'REVERSE' ORDER BY seq;

-- The member's other bills (still issued) of the same order, for "a split gives one stamp".
-- name: LoyaltyOrderSiblings :many
SELECT id, ad_datetime, (subtotal_paisa - discount_paisa - reward_paisa)::bigint AS amount
FROM invoices
WHERE order_id = $1 AND loyalty_customer_id = $2 AND status = 'ISSUED';

-- An EARN on an on-account bill counts (DONE) only while every on-account amount
-- on the bill is paid off, oldest bill first per customer (credit allocation);
-- otherwise it waits (PENDING). Sets every such entry to what it should be and
-- returns the members whose card changed.
-- name: SyncLoyaltyPending :many
WITH charges AS (
    SELECT p.customer_id, i.id AS invoice_id,
           sum(sum(p.amount_paisa)) OVER (PARTITION BY p.customer_id ORDER BY i.ad_datetime, i.number, i.id) AS cum
    FROM payments p JOIN invoices i ON i.id = p.invoice_id
    WHERE p.method = 'ON_ACCOUNT' AND i.status = 'ISSUED'
    GROUP BY p.customer_id, i.id
), repaid AS (
    SELECT customer_id, sum(amount_paisa) AS total FROM repayments WHERE voided_at IS NULL GROUP BY customer_id
), unpaid AS (
    SELECT DISTINCT ch.invoice_id FROM charges ch LEFT JOIN repaid r ON r.customer_id = ch.customer_id
    WHERE ch.cum > COALESCE(r.total, 0)
), wanted AS (
    SELECT e.id, CASE WHEN e.invoice_id IN (SELECT invoice_id FROM unpaid) THEN 'PENDING' ELSE 'DONE' END AS status
    FROM loyalty_entries e JOIN invoices i ON i.id = e.invoice_id
    WHERE e.kind = 'EARN' AND i.status = 'ISSUED'
      AND EXISTS (SELECT 1 FROM payments p WHERE p.invoice_id = i.id AND p.method = 'ON_ACCOUNT')
)
UPDATE loyalty_entries e SET status = w.status
FROM wanted w
WHERE e.id = w.id AND e.status <> w.status
RETURNING e.customer_id;

-- Staff whose phone is a member's (any user, active or not).
-- name: LoyaltyStaffByPhone :many
SELECT id, name FROM users WHERE phone = $1 AND phone <> '';

-- --- report ---

-- name: LoyaltyReportEntries :many
SELECT e.kind, e.stamps, e.reward_value_paisa, e.created_at, i.status AS invoice_status,
       i.created_by_user_id AS issued_by, i.issued_by_name
FROM loyalty_entries e
LEFT JOIN invoices i ON i.id = e.invoice_id
WHERE e.created_at >= sqlc.arg('from_ts') AND e.created_at < sqlc.arg('to_ts');

-- name: LoyaltyNewMembers :one
SELECT count(*)::int FROM customers
WHERE loyalty_member AND loyalty_joined_at >= sqlc.arg('from_ts') AND loyalty_joined_at < sqlc.arg('to_ts');

-- --- alert rules ---

-- EARN entries in the window on bills still issued, by the bill's issuer.
-- name: AlertLoyaltyEarnsSince :many
SELECT i.created_by_user_id AS issued_by
FROM loyalty_entries e JOIN invoices i ON i.id = e.invoice_id
WHERE e.customer_id = $1 AND e.kind = 'EARN' AND e.created_at >= $2 AND i.status = 'ISSUED';

-- name: AlertLoyaltyRedemptions :one
SELECT count(*)::int AS count, COALESCE(sum(reward_paisa), 0)::bigint AS total
FROM invoices
WHERE status = 'ISSUED' AND created_by_user_id = $1 AND reward_paisa > 0
  AND ad_datetime >= sqlc.arg('from_ts') AND ad_datetime < sqlc.arg('to_ts');
