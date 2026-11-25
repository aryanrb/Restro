-- 017: loyalty stamp card (loyalty design).
-- * customers: members are the existing Customer records (one per phone).
--   credit_account = false marks a loyalty-only member (hidden from the credit
--   customer list, no on-account billing until a credit account is opened).
-- * loyalty_settings: single row, owner-tuned card rules.
-- * loyalty_entries: append-only ledger (only status PENDING <-> DONE changes, for
--   an on-account bill's stamp). Stamps = sum of DONE entries.
-- * invoices: the member on the bill, the reward (a discount before VAT, kept apart
--   from the manual discount) and the printed card snapshot, all immutable.

ALTER TABLE customers
    ADD COLUMN credit_account            boolean NOT NULL DEFAULT true,
    ADD COLUMN loyalty_member            boolean NOT NULL DEFAULT false,
    ADD COLUMN loyalty_joined_at         timestamptz,
    ADD COLUMN loyalty_joined_by_user_id uuid REFERENCES users (id),
    ADD COLUMN loyalty_consent_at        timestamptz,
    ADD COLUMN birth_month_bs            smallint CHECK (birth_month_bs BETWEEN 1 AND 12),
    ADD COLUMN card_token                text;
CREATE UNIQUE INDEX customers_card_token_key ON customers (card_token) WHERE card_token IS NOT NULL;

CREATE TABLE loyalty_settings (
    id                    boolean PRIMARY KEY DEFAULT true CHECK (id),
    enabled               boolean NOT NULL DEFAULT true,
    stamps_per_reward     integer NOT NULL DEFAULT 10 CHECK (stamps_per_reward BETWEEN 1 AND 100),
    min_spend_paisa       bigint NOT NULL DEFAULT 50000 CHECK (min_spend_paisa >= 0),
    reward_value_paisa    bigint NOT NULL DEFAULT 30000 CHECK (reward_value_paisa > 0),
    expiry_months         integer NOT NULL DEFAULT 12 CHECK (expiry_months BETWEEN 1 AND 120),
    credit_bills_earn     boolean NOT NULL DEFAULT true,
    birthday_reward_paisa bigint NOT NULL DEFAULT 0 CHECK (birthday_reward_paisa >= 0),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
INSERT INTO loyalty_settings DEFAULT VALUES;

ALTER TABLE invoices
    ADD COLUMN loyalty_customer_id uuid REFERENCES customers (id),
    ADD COLUMN reward_paisa        bigint NOT NULL DEFAULT 0 CHECK (reward_paisa >= 0),
    ADD COLUMN loyalty_snapshot    jsonb;
CREATE INDEX invoices_loyalty_customer_idx ON invoices (loyalty_customer_id) WHERE loyalty_customer_id IS NOT NULL;

CREATE TABLE loyalty_entries (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq                 bigserial NOT NULL UNIQUE,
    customer_id         uuid NOT NULL REFERENCES customers (id),
    kind                text NOT NULL CHECK (kind IN ('EARN', 'REDEEM', 'REVERSE', 'EXPIRE', 'ADJUST', 'BIRTHDAY')),
    stamps              integer NOT NULL,
    status              text NOT NULL CHECK (status IN ('PENDING', 'DONE')),
    reward_value_paisa  bigint,
    invoice_id          uuid REFERENCES invoices (id),
    business_date       date NOT NULL,
    by_user_id          uuid REFERENCES users (id),
    approved_by_user_id uuid REFERENCES users (id),
    reason              text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX loyalty_entries_customer_idx ON loyalty_entries (customer_id, seq);
CREATE INDEX loyalty_entries_invoice_idx ON loyalty_entries (invoice_id) WHERE invoice_id IS NOT NULL;

-- The loyalty fields join the immutable invoice fields.
CREATE OR REPLACE FUNCTION invoices_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoices cannot be deleted';
    END IF;
    IF NEW.number <> OLD.number OR NEW.fiscal_year <> OLD.fiscal_year OR NEW.mode <> OLD.mode
        OR NEW.subtotal_paisa <> OLD.subtotal_paisa OR NEW.taxable_paisa <> OLD.taxable_paisa
        OR NEW.vat_paisa <> OLD.vat_paisa OR NEW.total_paisa <> OLD.total_paisa
        OR NEW.bs_date <> OLD.bs_date OR NEW.ad_datetime <> OLD.ad_datetime
        OR NEW.reward_paisa <> OLD.reward_paisa THEN
        RAISE EXCEPTION 'issued invoice identity/financial fields are immutable';
    END IF;
    IF NEW.display_number IS DISTINCT FROM OLD.display_number
        OR NEW.seller_name_en IS DISTINCT FROM OLD.seller_name_en OR NEW.seller_name_ne IS DISTINCT FROM OLD.seller_name_ne
        OR NEW.seller_address IS DISTINCT FROM OLD.seller_address OR NEW.seller_pan_number IS DISTINCT FROM OLD.seller_pan_number
        OR NEW.seller_vat_rate_bp IS DISTINCT FROM OLD.seller_vat_rate_bp OR NEW.seller_vat_registered IS DISTINCT FROM OLD.seller_vat_registered
        OR NEW.seller_logo_url IS DISTINCT FROM OLD.seller_logo_url
        OR NEW.issued_by_name IS DISTINCT FROM OLD.issued_by_name OR NEW.place IS DISTINCT FROM OLD.place
        OR NEW.order_client_id IS DISTINCT FROM OLD.order_client_id
        OR NEW.discount_reason IS DISTINCT FROM OLD.discount_reason
        OR NEW.created_by_user_id IS DISTINCT FROM OLD.created_by_user_id
        OR NEW.loyalty_customer_id IS DISTINCT FROM OLD.loyalty_customer_id
        OR NEW.loyalty_snapshot::text IS DISTINCT FROM OLD.loyalty_snapshot::text THEN
        RAISE EXCEPTION 'issued invoice snapshot fields are immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

---- create above / drop below ----

CREATE OR REPLACE FUNCTION invoices_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoices cannot be deleted';
    END IF;
    IF NEW.number <> OLD.number OR NEW.fiscal_year <> OLD.fiscal_year OR NEW.mode <> OLD.mode
        OR NEW.subtotal_paisa <> OLD.subtotal_paisa OR NEW.taxable_paisa <> OLD.taxable_paisa
        OR NEW.vat_paisa <> OLD.vat_paisa OR NEW.total_paisa <> OLD.total_paisa
        OR NEW.bs_date <> OLD.bs_date OR NEW.ad_datetime <> OLD.ad_datetime THEN
        RAISE EXCEPTION 'issued invoice identity/financial fields are immutable';
    END IF;
    IF NEW.display_number IS DISTINCT FROM OLD.display_number
        OR NEW.seller_name_en IS DISTINCT FROM OLD.seller_name_en OR NEW.seller_name_ne IS DISTINCT FROM OLD.seller_name_ne
        OR NEW.seller_address IS DISTINCT FROM OLD.seller_address OR NEW.seller_pan_number IS DISTINCT FROM OLD.seller_pan_number
        OR NEW.seller_vat_rate_bp IS DISTINCT FROM OLD.seller_vat_rate_bp OR NEW.seller_vat_registered IS DISTINCT FROM OLD.seller_vat_registered
        OR NEW.seller_logo_url IS DISTINCT FROM OLD.seller_logo_url
        OR NEW.issued_by_name IS DISTINCT FROM OLD.issued_by_name OR NEW.place IS DISTINCT FROM OLD.place
        OR NEW.order_client_id IS DISTINCT FROM OLD.order_client_id
        OR NEW.discount_reason IS DISTINCT FROM OLD.discount_reason
        OR NEW.created_by_user_id IS DISTINCT FROM OLD.created_by_user_id THEN
        RAISE EXCEPTION 'issued invoice snapshot fields are immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TABLE IF EXISTS loyalty_entries;
DROP INDEX IF EXISTS invoices_loyalty_customer_idx;
ALTER TABLE invoices
    DROP COLUMN IF EXISTS loyalty_snapshot,
    DROP COLUMN IF EXISTS reward_paisa,
    DROP COLUMN IF EXISTS loyalty_customer_id;
DROP TABLE IF EXISTS loyalty_settings;
DROP INDEX IF EXISTS customers_card_token_key;
ALTER TABLE customers
    DROP COLUMN IF EXISTS card_token,
    DROP COLUMN IF EXISTS birth_month_bs,
    DROP COLUMN IF EXISTS loyalty_consent_at,
    DROP COLUMN IF EXISTS loyalty_joined_by_user_id,
    DROP COLUMN IF EXISTS loyalty_joined_at,
    DROP COLUMN IF EXISTS loyalty_member,
    DROP COLUMN IF EXISTS credit_account;
