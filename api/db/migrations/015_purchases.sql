-- 015: purchases & suppliers (spec: purchases-suppliers-design).
-- * suppliers: PAN + VAT registration, optional opening balance (owed before go-live).
-- * purchases + purchase_lines: a purchase bill = goods received + supplier invoice.
--   Never edited; voided (status) with reversing PURCHASE_RETURN movements.
-- * purchase_returns (+ lines): reduce stock and what is owed; a cash refund lands
--   in the returner's open shift (a negative paid-out).
-- * supplier_payments: TILL payments are paid-outs in the payer's open shift; a
--   TILL payment voided after its shift closed puts the cash back in the voider's
--   open shift (void_shift_id).
-- * item_purchase_settings: preferred supplier, default purchase unit, buy-up-to.
-- Supplier balance is derived (supplier_balances view), never stored.

CREATE TABLE suppliers (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  text NOT NULL,
    phone                 text NOT NULL DEFAULT '',
    address               text NOT NULL DEFAULT '',
    pan                   text NOT NULL DEFAULT '',
    vat_registered        boolean NOT NULL DEFAULT false,
    contact_person        text NOT NULL DEFAULT '',
    notes                 text NOT NULL DEFAULT '',
    active                boolean NOT NULL DEFAULT true,
    opening_balance_paisa bigint NOT NULL DEFAULT 0 CHECK (opening_balance_paisa >= 0),
    created_by_user_id    uuid REFERENCES users (id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (pan = '' OR pan ~ '^[0-9]{9}$'),
    CHECK (NOT vat_registered OR pan <> '')
);
CREATE UNIQUE INDEX suppliers_name_key ON suppliers (lower(name));

-- Gap-free per-kind counters: 'PURCHASE' (P-n) and 'PAYMENT' (SP-n).
CREATE TABLE purchase_counters (
    kind        text PRIMARY KEY CHECK (kind IN ('PURCHASE', 'PAYMENT')),
    next_number integer NOT NULL
);
INSERT INTO purchase_counters (kind, next_number) VALUES ('PURCHASE', 1), ('PAYMENT', 1);

CREATE TABLE purchases (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number              integer NOT NULL UNIQUE,
    supplier_id         uuid NOT NULL REFERENCES suppliers (id),
    supplier_name       text NOT NULL, -- snapshots
    supplier_pan        text NOT NULL DEFAULT '',
    supplier_bill_no    text NOT NULL DEFAULT '',
    bill_date           date NOT NULL,
    bill_date_bs        text NOT NULL,
    fiscal_year         text NOT NULL,
    is_vat_bill         boolean NOT NULL DEFAULT false,
    discount_paisa      bigint NOT NULL DEFAULT 0 CHECK (discount_paisa >= 0),
    taxable_paisa       bigint NOT NULL,
    exempt_paisa        bigint NOT NULL,
    vat_paisa           bigint NOT NULL CHECK (vat_paisa >= 0),
    total_paisa         bigint NOT NULL,
    status              text NOT NULL DEFAULT 'RECORDED' CHECK (status IN ('RECORDED', 'VOIDED')),
    void_reason         text NOT NULL DEFAULT '',
    voided_by_user_id   uuid REFERENCES users (id),
    voided_at           timestamptz,
    recorded_by_user_id uuid NOT NULL REFERENCES users (id),
    at                  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX purchases_supplier_idx ON purchases (supplier_id);
CREATE INDEX purchases_bill_date_idx ON purchases (bill_date);
-- The same supplier bill can't be entered twice (among recorded bills).
CREATE UNIQUE INDEX purchases_supplier_bill_key ON purchases (supplier_id, lower(supplier_bill_no))
    WHERE status = 'RECORDED' AND supplier_bill_no <> '';

CREATE TABLE purchase_lines (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    purchase_id        uuid NOT NULL REFERENCES purchases (id),
    sort               integer NOT NULL,
    inventory_item_id  uuid REFERENCES inventory_items (id), -- NULL = non-stock line
    name_en            text NOT NULL,
    name_ne            text NOT NULL DEFAULT '',
    stock_unit         text NOT NULL DEFAULT '',
    quantity_milli     bigint NOT NULL CHECK (quantity_milli > 0), -- purchase units
    unit_key           text NOT NULL DEFAULT '',
    unit_label_en      text NOT NULL DEFAULT '',
    unit_label_ne      text NOT NULL DEFAULT '',
    factor_milli       bigint NOT NULL CHECK (factor_milli > 0),
    unit_cost_paisa    bigint NOT NULL CHECK (unit_cost_paisa >= 0),
    vat_applies        boolean NOT NULL DEFAULT false,
    amount_paisa       bigint NOT NULL,
    discount_paisa     bigint NOT NULL DEFAULT 0,
    vat_paisa          bigint NOT NULL DEFAULT 0,
    stock_unit_cost_e4 bigint NOT NULL DEFAULT 0,
    returned_milli     bigint NOT NULL DEFAULT 0 CHECK (returned_milli >= 0 AND returned_milli <= quantity_milli)
);
CREATE INDEX purchase_lines_purchase_idx ON purchase_lines (purchase_id, sort);
CREATE INDEX purchase_lines_item_idx ON purchase_lines (inventory_item_id) WHERE inventory_item_id IS NOT NULL;

CREATE TABLE purchase_returns (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    purchase_id     uuid NOT NULL REFERENCES purchases (id),
    note_no         text NOT NULL DEFAULT '',
    reason          text NOT NULL,
    taxable_paisa   bigint NOT NULL,
    exempt_paisa    bigint NOT NULL,
    vat_paisa       bigint NOT NULL,
    total_paisa     bigint NOT NULL,
    refund_paisa    bigint NOT NULL DEFAULT 0 CHECK (refund_paisa >= 0),
    refund_shift_id uuid REFERENCES shifts (id),
    by_user_id      uuid NOT NULL REFERENCES users (id),
    at              timestamptz NOT NULL DEFAULT now(),
    bs_date         text NOT NULL,
    CHECK ((refund_paisa > 0) = (refund_shift_id IS NOT NULL))
);
CREATE INDEX purchase_returns_purchase_idx ON purchase_returns (purchase_id);
CREATE INDEX purchase_returns_refund_shift_idx ON purchase_returns (refund_shift_id) WHERE refund_shift_id IS NOT NULL;

-- Each returned line with its value (net of discount, and VAT), so later returns
-- of the same line add up exactly.
CREATE TABLE purchase_return_lines (
    return_id        uuid NOT NULL REFERENCES purchase_returns (id),
    purchase_line_id uuid NOT NULL REFERENCES purchase_lines (id),
    quantity_milli   bigint NOT NULL CHECK (quantity_milli > 0),
    net_paisa        bigint NOT NULL,
    vat_paisa        bigint NOT NULL,
    PRIMARY KEY (return_id, purchase_line_id)
);

CREATE TABLE supplier_payments (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number              integer NOT NULL UNIQUE,
    supplier_id         uuid NOT NULL REFERENCES suppliers (id),
    supplier_name       text NOT NULL,
    amount_paisa        bigint NOT NULL CHECK (amount_paisa > 0),
    method              text NOT NULL CHECK (method IN ('TILL', 'OWNER_CASH', 'BANK_QR', 'CHEQUE')),
    reference           text NOT NULL DEFAULT '',
    note                text NOT NULL DEFAULT '',
    shift_id            uuid REFERENCES shifts (id),
    paid_by_user_id     uuid NOT NULL REFERENCES users (id),
    approver_user_id    uuid REFERENCES users (id),
    at                  timestamptz NOT NULL DEFAULT now(),
    bs_date             text NOT NULL,
    balance_after_paisa bigint NOT NULL,
    voided_at           timestamptz,
    voided_by_user_id   uuid REFERENCES users (id),
    void_reason         text NOT NULL DEFAULT '',
    void_shift_id       uuid REFERENCES shifts (id),
    CHECK ((method = 'TILL') = (shift_id IS NOT NULL)),
    CHECK (void_shift_id IS NULL OR voided_at IS NOT NULL)
);
CREATE INDEX supplier_payments_supplier_idx ON supplier_payments (supplier_id);
CREATE INDEX supplier_payments_shift_idx ON supplier_payments (shift_id) WHERE shift_id IS NOT NULL;
CREATE INDEX supplier_payments_void_shift_idx ON supplier_payments (void_shift_id) WHERE void_shift_id IS NOT NULL;

CREATE TABLE item_purchase_settings (
    inventory_item_id     uuid PRIMARY KEY REFERENCES inventory_items (id),
    preferred_supplier_id uuid REFERENCES suppliers (id),
    default_unit_key      text, -- NULL = the stock unit
    buy_up_to_milli       bigint CHECK (buy_up_to_milli >= 0),
    updated_at            timestamptz NOT NULL DEFAULT now()
);

-- opening + recorded bills - their returns - live payments + refunds handed back.
-- Negative = credit with the supplier.
CREATE VIEW supplier_balances AS
SELECT s.id AS supplier_id,
       (s.opening_balance_paisa
        + COALESCE((SELECT sum(p.total_paisa) FROM purchases p WHERE p.supplier_id = s.id AND p.status = 'RECORDED'), 0)
        - COALESCE((SELECT sum(r.total_paisa) FROM purchase_returns r JOIN purchases p ON p.id = r.purchase_id
                    WHERE p.supplier_id = s.id AND p.status = 'RECORDED'), 0)
        - COALESCE((SELECT sum(sp.amount_paisa) FROM supplier_payments sp WHERE sp.supplier_id = s.id AND sp.voided_at IS NULL), 0)
        + COALESCE((SELECT sum(r.refund_paisa) FROM purchase_returns r JOIN purchases p ON p.id = r.purchase_id
                    WHERE p.supplier_id = s.id), 0))::bigint AS balance_paisa
FROM suppliers s;

---- create above / drop below ----

DROP VIEW IF EXISTS supplier_balances;
DROP TABLE IF EXISTS item_purchase_settings;
DROP TABLE IF EXISTS supplier_payments;
DROP TABLE IF EXISTS purchase_return_lines;
DROP TABLE IF EXISTS purchase_returns;
DROP TABLE IF EXISTS purchase_lines;
DROP TABLE IF EXISTS purchases;
DROP TABLE IF EXISTS purchase_counters;
DROP TABLE IF EXISTS suppliers;
