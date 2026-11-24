-- 005: shifts, billing (immutable invoices, gap-free numbering), payments, CBMS outbox.

CREATE TABLE shifts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cashier_user_id     uuid NOT NULL REFERENCES users (id),
    opened_at           timestamptz NOT NULL DEFAULT now(),
    closed_at           timestamptz,
    opening_float_paisa bigint NOT NULL DEFAULT 0,
    counted_cash_paisa  bigint,
    note                text NOT NULL DEFAULT '',
    status              text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'CLOSED'))
);
-- At most one open shift per cashier.
CREATE UNIQUE INDEX shifts_one_open_per_cashier ON shifts (cashier_user_id) WHERE status = 'OPEN';

-- Gap-free counter per (BS fiscal year, mode). Estimate and tax series never mix.
CREATE TABLE invoice_counters (
    fiscal_year text NOT NULL,
    mode        text NOT NULL,
    next_number integer NOT NULL DEFAULT 1,
    PRIMARY KEY (fiscal_year, mode)
);

CREATE TABLE invoices (
    id                           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    fiscal_year                  text NOT NULL,
    number                       integer NOT NULL,
    mode                         text NOT NULL CHECK (mode IN ('ESTIMATE', 'TAX_INVOICE')),
    bs_date                      text NOT NULL,       -- YYYY.MM.DD
    bs_year                      integer NOT NULL,
    bs_month                     integer NOT NULL,
    bs_day                       integer NOT NULL,
    ad_datetime                  timestamptz NOT NULL DEFAULT now(),
    order_id                     uuid REFERENCES orders (id),
    buyer_name                   text NOT NULL DEFAULT '',
    buyer_pan                    text NOT NULL DEFAULT '',
    subtotal_paisa               bigint NOT NULL,
    discount_paisa               bigint NOT NULL,
    service_charge_paisa         bigint NOT NULL,
    taxable_paisa                bigint NOT NULL,
    vat_paisa                    bigint NOT NULL,
    rounding_paisa               bigint NOT NULL DEFAULT 0,
    total_paisa                  bigint NOT NULL,
    discount_reason              text NOT NULL DEFAULT '',
    discount_approved_by_user_id uuid,
    status                       text NOT NULL DEFAULT 'ISSUED' CHECK (status IN ('ISSUED', 'CANCELLED')),
    cancel_reason                text NOT NULL DEFAULT '',
    cancelled_by_user_id         uuid,
    cancel_approved_by_user_id   uuid,
    cancelled_at                 timestamptz,
    print_count                  integer NOT NULL DEFAULT 0,
    created_by_user_id           uuid REFERENCES users (id),
    created_at                   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (fiscal_year, mode, number)
);
CREATE INDEX invoices_ad_datetime_idx ON invoices (ad_datetime);
CREATE INDEX invoices_status_idx ON invoices (status);

CREATE TABLE invoice_lines (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id       uuid NOT NULL REFERENCES invoices (id),
    order_item_id    uuid,
    name_en          text NOT NULL,
    name_ne          text NOT NULL DEFAULT '',
    variant_name     text NOT NULL DEFAULT '',
    quantity         integer NOT NULL,
    unit_price_paisa bigint NOT NULL,
    modifiers_json   jsonb,
    line_total_paisa bigint NOT NULL
);
CREATE INDEX invoice_lines_invoice_idx ON invoice_lines (invoice_id);

CREATE TABLE payments (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id          uuid NOT NULL REFERENCES invoices (id),
    method              text NOT NULL CHECK (method IN ('CASH', 'CARD', 'QR_FONEPAY', 'QR_OTHER')),
    amount_paisa        bigint NOT NULL,
    reference           text NOT NULL DEFAULT '',
    confirmed_by_user_id uuid NOT NULL REFERENCES users (id),
    shift_id            uuid NOT NULL REFERENCES shifts (id),
    status              text NOT NULL DEFAULT 'CONFIRMED' CHECK (status IN ('CONFIRMED', 'AWAITING')),
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_invoice_idx ON payments (invoice_id);
CREATE INDEX payments_shift_idx ON payments (shift_id);

-- CBMS outbox: a row is written in the billing transaction, status DISABLED for now.
CREATE TABLE cbms_outbox (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id uuid NOT NULL REFERENCES invoices (id),
    kind       text NOT NULL DEFAULT 'BILL' CHECK (kind IN ('BILL', 'RETURN')),
    payload    jsonb,
    status     text NOT NULL DEFAULT 'DISABLED' CHECK (status IN ('PENDING', 'SENT', 'FAILED', 'DISABLED')),
    attempts   integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE order_items ADD COLUMN invoice_id uuid REFERENCES invoices (id);

-- Immutability (research 01 #1): issued invoices allow only status->CANCELLED and
-- print_count/cancel fields; lines never change; nothing is ever deleted.
CREATE FUNCTION invoices_immutable() RETURNS trigger AS $$
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
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER invoices_immutable_trg BEFORE UPDATE OR DELETE ON invoices FOR EACH ROW EXECUTE FUNCTION invoices_immutable();

CREATE FUNCTION invoice_lines_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'invoice lines are immutable';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER invoice_lines_immutable_trg BEFORE UPDATE OR DELETE ON invoice_lines FOR EACH ROW EXECUTE FUNCTION invoice_lines_immutable();

---- create above / drop below ----

DROP TRIGGER IF EXISTS invoice_lines_immutable_trg ON invoice_lines;
DROP TRIGGER IF EXISTS invoices_immutable_trg ON invoices;
DROP FUNCTION IF EXISTS invoice_lines_immutable();
DROP FUNCTION IF EXISTS invoices_immutable();
ALTER TABLE order_items DROP COLUMN IF EXISTS invoice_id;
DROP TABLE IF EXISTS cbms_outbox;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS invoice_lines;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS invoice_counters;
DROP TABLE IF EXISTS shifts;
