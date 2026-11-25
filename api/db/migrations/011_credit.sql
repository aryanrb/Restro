-- 011: credit customers (udharo).
-- * customers: credit accounts. Phone stored normalised and unique when set.
-- * payments: method ON_ACCOUNT (not money) with the customer it is owed by and a
--   receipt snapshot (name, balance after). The balance is derived, never stored:
--   on-account payments on ISSUED invoices - repayments not voided, so cancelling a
--   bill reverses its debt automatically.
-- * repayments: money taken against a balance, counted in the collector's shift,
--   with their own gap-free sequence (repayment_counters).
-- * shift_adjustments: a voided repayment from a closed shift is counted back in
--   the voider's shift, like a cancelled bill (repayment_id instead of payment_id).

CREATE TABLE customers (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL,
    phone              text NOT NULL DEFAULT '',
    notes              text NOT NULL DEFAULT '',
    credit_limit_paisa bigint NOT NULL DEFAULT 0 CHECK (credit_limit_paisa >= 0),
    active             boolean NOT NULL DEFAULT true,
    pan                text NOT NULL DEFAULT '',
    address            text NOT NULL DEFAULT '',
    contact_person     text NOT NULL DEFAULT '',
    created_by_user_id uuid REFERENCES users (id),
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX customers_phone_key ON customers (phone) WHERE phone <> '';

ALTER TABLE payments DROP CONSTRAINT payments_method_check;
ALTER TABLE payments ADD CONSTRAINT payments_method_check
    CHECK (method IN ('CASH', 'CARD', 'QR_FONEPAY', 'QR_OTHER', 'ON_ACCOUNT'));
ALTER TABLE payments
    ADD COLUMN customer_id uuid REFERENCES customers (id),
    ADD COLUMN customer_name text NOT NULL DEFAULT '',
    ADD COLUMN balance_after_paisa bigint NOT NULL DEFAULT 0,
    ADD COLUMN credit_approved_by_user_id uuid REFERENCES users (id);
ALTER TABLE payments ADD CONSTRAINT payments_on_account_customer
    CHECK ((method = 'ON_ACCOUNT') = (customer_id IS NOT NULL));
CREATE INDEX payments_customer_idx ON payments (customer_id) WHERE customer_id IS NOT NULL;

-- Single-row gap-free counter for repayment receipt numbers.
CREATE TABLE repayment_counters (
    id          boolean PRIMARY KEY DEFAULT true CHECK (id),
    next_number integer NOT NULL
);

CREATE TABLE repayments (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number            integer NOT NULL UNIQUE,
    customer_id       uuid NOT NULL REFERENCES customers (id),
    amount_paisa      bigint NOT NULL CHECK (amount_paisa > 0),
    method            text NOT NULL CHECK (method IN ('CASH', 'CARD', 'QR_FONEPAY', 'QR_OTHER')),
    reference         text NOT NULL DEFAULT '',
    note              text NOT NULL DEFAULT '',
    shift_id          uuid NOT NULL REFERENCES shifts (id),
    taken_by_user_id  uuid NOT NULL REFERENCES users (id),
    bs_date           text NOT NULL,
    balance_after_paisa bigint NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    voided_at         timestamptz,
    voided_by_user_id uuid REFERENCES users (id),
    void_reason       text NOT NULL DEFAULT ''
);
CREATE INDEX repayments_customer_idx ON repayments (customer_id);
CREATE INDEX repayments_shift_idx ON repayments (shift_id);
CREATE INDEX repayments_created_at_idx ON repayments (created_at);

-- Each customer's balance (negative = in the customer's favour).
CREATE VIEW customer_balances AS
SELECT c.id AS customer_id,
       (COALESCE((SELECT sum(p.amount_paisa) FROM payments p JOIN invoices i ON i.id = p.invoice_id
                  WHERE p.customer_id = c.id AND p.method = 'ON_ACCOUNT' AND i.status = 'ISSUED'), 0)
        - COALESCE((SELECT sum(r.amount_paisa) FROM repayments r
                    WHERE r.customer_id = c.id AND r.voided_at IS NULL), 0))::bigint AS balance_paisa
FROM customers c;

ALTER TABLE shift_adjustments
    ALTER COLUMN invoice_id DROP NOT NULL,
    ALTER COLUMN payment_id DROP NOT NULL,
    ADD COLUMN repayment_id uuid UNIQUE REFERENCES repayments (id);
ALTER TABLE shift_adjustments ADD CONSTRAINT shift_adjustments_source
    CHECK ((payment_id IS NULL) <> (repayment_id IS NULL) AND (payment_id IS NULL) = (invoice_id IS NULL));

---- create above / drop below ----

DELETE FROM shift_adjustments WHERE repayment_id IS NOT NULL;
ALTER TABLE shift_adjustments DROP CONSTRAINT IF EXISTS shift_adjustments_source;
ALTER TABLE shift_adjustments DROP COLUMN IF EXISTS repayment_id;
ALTER TABLE shift_adjustments ALTER COLUMN invoice_id SET NOT NULL, ALTER COLUMN payment_id SET NOT NULL;
DROP VIEW IF EXISTS customer_balances;
DROP TABLE IF EXISTS repayments;
DROP TABLE IF EXISTS repayment_counters;
DELETE FROM payments WHERE method = 'ON_ACCOUNT';
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_on_account_customer;
ALTER TABLE payments
    DROP COLUMN IF EXISTS credit_approved_by_user_id,
    DROP COLUMN IF EXISTS balance_after_paisa,
    DROP COLUMN IF EXISTS customer_name,
    DROP COLUMN IF EXISTS customer_id;
ALTER TABLE payments DROP CONSTRAINT payments_method_check;
ALTER TABLE payments ADD CONSTRAINT payments_method_check
    CHECK (method IN ('CASH', 'CARD', 'QR_FONEPAY', 'QR_OTHER'));
DROP TABLE IF EXISTS customers;
