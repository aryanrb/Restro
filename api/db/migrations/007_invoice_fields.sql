-- 007: invoice fields captured at issue time (seller snapshot, display number,
-- issuer, place, order client id), line client ids + bilingual variant, and a
-- per-print log (IRD sales book: Printed_Time / Printed_by).

ALTER TABLE invoices
    ADD COLUMN display_number        text    NOT NULL DEFAULT '',
    ADD COLUMN seller_name_en        text    NOT NULL DEFAULT '',
    ADD COLUMN seller_name_ne        text    NOT NULL DEFAULT '',
    ADD COLUMN seller_address        text    NOT NULL DEFAULT '',
    ADD COLUMN seller_pan_number     text    NOT NULL DEFAULT '',
    ADD COLUMN seller_vat_rate_bp    integer NOT NULL DEFAULT 0,
    ADD COLUMN seller_vat_registered boolean NOT NULL DEFAULT false,
    ADD COLUMN seller_logo_url       text    NOT NULL DEFAULT '',
    ADD COLUMN issued_by_name        text    NOT NULL DEFAULT '',
    ADD COLUMN place                 text    NOT NULL DEFAULT '',
    ADD COLUMN order_client_id       uuid;

ALTER TABLE invoice_lines
    ADD COLUMN order_item_client_id uuid,
    ADD COLUMN variant_name_ne      text NOT NULL DEFAULT '';

-- Backfill existing invoices. The seller snapshot can only come from today's
-- settings (the best record there is); display numbers are derived from the
-- stored fiscal year "2083.084" -> "2083/84".
UPDATE invoices i SET
    display_number = CASE WHEN i.mode = 'ESTIMATE' THEN 'E-' ELSE '' END
        || split_part(i.fiscal_year, '.', 1) || '/' || right(split_part(i.fiscal_year, '.', 2), 2)
        || '-' || i.number,
    seller_name_en = s.name_en,
    seller_name_ne = s.name_ne,
    seller_address = s.address,
    seller_pan_number = s.pan_number,
    seller_vat_rate_bp = s.vat_rate_bp,
    seller_vat_registered = s.vat_registered,
    seller_logo_url = s.logo_url,
    issued_by_name = COALESCE((SELECT u.name FROM users u WHERE u.id = i.created_by_user_id), ''),
    order_client_id = (SELECT o.client_id FROM orders o WHERE o.id = i.order_id),
    place = COALESCE((
        SELECT CASE WHEN o.type = 'TAKEAWAY' THEN 'Takeaway' ELSE COALESCE(t.name, '') END
        FROM orders o LEFT JOIN dining_tables t ON t.id = o.table_id
        WHERE o.id = i.order_id), '')
FROM settings s
WHERE s.id = 1;

-- Lines are immutable by trigger; lift it just for the backfill.
ALTER TABLE invoice_lines DISABLE TRIGGER invoice_lines_immutable_trg;
UPDATE invoice_lines l SET
    order_item_client_id = oi.client_id,
    variant_name_ne = oi.variant_name_ne
FROM order_items oi
WHERE oi.id = l.order_item_id;
ALTER TABLE invoice_lines ENABLE TRIGGER invoice_lines_immutable_trg;

-- Every print of an invoice; the first is the original. Written in the same
-- transaction as the print_count increment. Append-only.
CREATE TABLE invoice_prints (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    invoice_id         uuid NOT NULL REFERENCES invoices (id),
    printed_at         timestamptz NOT NULL DEFAULT now(),
    printed_by_user_id uuid NOT NULL REFERENCES users (id)
);
CREATE INDEX invoice_prints_invoice_idx ON invoice_prints (invoice_id, printed_at);

CREATE FUNCTION invoice_prints_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'invoice prints are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER invoice_prints_immutable_trg BEFORE UPDATE OR DELETE ON invoice_prints FOR EACH ROW EXECUTE FUNCTION invoice_prints_immutable();

-- The issue-time snapshot joins the immutable fields.
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
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS invoice_prints_immutable_trg ON invoice_prints;
DROP FUNCTION IF EXISTS invoice_prints_immutable();
DROP TABLE IF EXISTS invoice_prints;

ALTER TABLE invoice_lines
    DROP COLUMN IF EXISTS variant_name_ne,
    DROP COLUMN IF EXISTS order_item_client_id;

ALTER TABLE invoices
    DROP COLUMN IF EXISTS order_client_id,
    DROP COLUMN IF EXISTS place,
    DROP COLUMN IF EXISTS issued_by_name,
    DROP COLUMN IF EXISTS seller_logo_url,
    DROP COLUMN IF EXISTS seller_vat_registered,
    DROP COLUMN IF EXISTS seller_vat_rate_bp,
    DROP COLUMN IF EXISTS seller_pan_number,
    DROP COLUMN IF EXISTS seller_address,
    DROP COLUMN IF EXISTS seller_name_ne,
    DROP COLUMN IF EXISTS seller_name_en,
    DROP COLUMN IF EXISTS display_number;
