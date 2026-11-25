-- 014: analytics (daily summaries, festival / closed-day calendar, settings).
-- * analytics_days: one disposable summary per past Kathmandu business day,
--   rebuilt from raw bills, orders, items and shifts (approach B). Today is never
--   stored; it is always computed live. Deleting rows only means they are rebuilt.
-- * analytics_dirty_days: past days whose raw data changed after the fact (a bill
--   cancelled, a late void, a shift closed, an order closed later...). Marked by
--   the triggers below, in the same transaction as the change, so no code path can
--   forget; the next analytics read rebuilds those days before using them.
-- * calendar_events: festivals, closed days and other marked days (AD dates,
--   inclusive). Built-ins seeded for FY 2082/83–2084/85; the owner edits them.
-- * analytics_settings: which managers may see the staff section; highlights on/off.

CREATE TABLE analytics_days (
    day      date PRIMARY KEY,
    summary  jsonb NOT NULL,
    built_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE analytics_dirty_days (
    day       date PRIMARY KEY,
    marked_at timestamptz NOT NULL DEFAULT now()
);

-- Marks the Kathmandu business day of ts dirty when it is before today (today is
-- always computed live, so the hot path of a normal sale writes nothing here).
CREATE FUNCTION analytics_mark_dirty(ts timestamptz) RETURNS void AS $$
DECLARE
    d date;
BEGIN
    IF ts IS NULL THEN
        RETURN;
    END IF;
    d := (ts AT TIME ZONE 'Asia/Kathmandu')::date;
    IF d < (now() AT TIME ZONE 'Asia/Kathmandu')::date THEN
        INSERT INTO analytics_dirty_days (day) VALUES (d) ON CONFLICT (day) DO NOTHING;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE FUNCTION analytics_dirty_invoices() RETURNS trigger AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        PERFORM analytics_mark_dirty(OLD.ad_datetime);
        PERFORM analytics_mark_dirty(OLD.cancelled_at);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM analytics_mark_dirty(NEW.ad_datetime);
        PERFORM analytics_mark_dirty(NEW.cancelled_at);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER analytics_dirty_invoices_trg AFTER INSERT OR UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION analytics_dirty_invoices();

-- A payment belongs to its bill's day (payment mix) and to its shift's close day
-- (cash difference).
CREATE FUNCTION analytics_dirty_payments() RETURNS trigger AS $$
DECLARE
    r record;
BEGIN
    IF TG_OP = 'DELETE' THEN
        r := OLD;
    ELSE
        r := NEW;
    END IF;
    PERFORM analytics_mark_dirty((SELECT ad_datetime FROM invoices WHERE id = r.invoice_id));
    PERFORM analytics_mark_dirty((SELECT closed_at FROM shifts WHERE id = r.shift_id));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER analytics_dirty_payments_trg AFTER INSERT OR UPDATE OR DELETE ON payments
    FOR EACH ROW EXECUTE FUNCTION analytics_dirty_payments();

CREATE FUNCTION analytics_dirty_orders() RETURNS trigger AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        PERFORM analytics_mark_dirty(OLD.created_at);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM analytics_mark_dirty(NEW.created_at);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER analytics_dirty_orders_trg AFTER INSERT OR UPDATE OR DELETE ON orders
    FOR EACH ROW EXECUTE FUNCTION analytics_dirty_orders();

-- Items count on the day they were sent (a later void belongs to the send).
CREATE FUNCTION analytics_dirty_order_items() RETURNS trigger AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        PERFORM analytics_mark_dirty(OLD.sent_at);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM analytics_mark_dirty(NEW.sent_at);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER analytics_dirty_order_items_trg AFTER INSERT OR UPDATE OR DELETE ON order_items
    FOR EACH ROW EXECUTE FUNCTION analytics_dirty_order_items();

CREATE FUNCTION analytics_dirty_shifts() RETURNS trigger AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        PERFORM analytics_mark_dirty(OLD.closed_at);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM analytics_mark_dirty(NEW.closed_at);
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER analytics_dirty_shifts_trg AFTER INSERT OR UPDATE OR DELETE ON shifts
    FOR EACH ROW EXECUTE FUNCTION analytics_dirty_shifts();

-- An adjustment changes the takings (and so the cash difference) of its shift.
CREATE FUNCTION analytics_dirty_shift_adjustments() RETURNS trigger AS $$
DECLARE
    r record;
BEGIN
    IF TG_OP = 'DELETE' THEN
        r := OLD;
    ELSE
        r := NEW;
    END IF;
    PERFORM analytics_mark_dirty((SELECT closed_at FROM shifts WHERE id = r.shift_id));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER analytics_dirty_shift_adjustments_trg AFTER INSERT OR UPDATE OR DELETE ON shift_adjustments
    FOR EACH ROW EXECUTE FUNCTION analytics_dirty_shift_adjustments();

CREATE TABLE calendar_events (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name_en            text NOT NULL,
    name_ne            text NOT NULL DEFAULT '',
    from_date          date NOT NULL,
    to_date            date NOT NULL,
    kind               text NOT NULL CHECK (kind IN ('FESTIVAL', 'CLOSED', 'OTHER')),
    built_in           boolean NOT NULL DEFAULT false,
    created_by_user_id uuid REFERENCES users (id),
    key                text,
    approximate        boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (from_date <= to_date)
);
CREATE INDEX calendar_events_range_idx ON calendar_events (from_date, to_date);

-- Built-in festivals (same list as the web's calendar-seed.ts). Lunar dates are
-- APPROXIMATE — entered from memory; verify against the Government of Nepal holiday
-- list. They show as "date to check" until the owner saves them. BS New Year
-- (Baisakh 1) and Tamu Lhosar (Poush 15) are exact (fixed BS dates).
INSERT INTO calendar_events (key, name_en, name_ne, from_date, to_date, kind, built_in, approximate) VALUES
    -- FY 2082/83
    ('teej', 'Teej', 'तीज', '2025-08-26', '2025-08-26', 'FESTIVAL', true, true),
    ('dashain', 'Dashain', 'दशैँ', '2025-09-22', '2025-10-06', 'FESTIVAL', true, true),
    ('tihar', 'Tihar', 'तिहार', '2025-10-19', '2025-10-23', 'FESTIVAL', true, true),
    ('chhath', 'Chhath', 'छठ', '2025-10-27', '2025-10-28', 'FESTIVAL', true, true),
    ('sonam-lhosar', 'Sonam Lhosar', 'सोनाम ल्होसार', '2026-01-19', '2026-01-19', 'FESTIVAL', true, true),
    ('shivaratri', 'Maha Shivaratri', 'महाशिवरात्रि', '2026-02-15', '2026-02-15', 'FESTIVAL', true, true),
    ('gyalpo-lhosar', 'Gyalpo Lhosar', 'ग्याल्पो ल्होसार', '2026-02-18', '2026-02-18', 'FESTIVAL', true, true),
    ('holi', 'Holi', 'होली', '2026-03-02', '2026-03-03', 'FESTIVAL', true, true),
    -- FY 2083/84
    ('teej', 'Teej', 'तीज', '2026-09-14', '2026-09-14', 'FESTIVAL', true, true),
    ('dashain', 'Dashain', 'दशैँ', '2026-10-11', '2026-10-25', 'FESTIVAL', true, true),
    ('tihar', 'Tihar', 'तिहार', '2026-11-06', '2026-11-11', 'FESTIVAL', true, true),
    ('chhath', 'Chhath', 'छठ', '2026-11-15', '2026-11-16', 'FESTIVAL', true, true),
    ('sonam-lhosar', 'Sonam Lhosar', 'सोनाम ल्होसार', '2027-01-08', '2027-01-08', 'FESTIVAL', true, true),
    ('gyalpo-lhosar', 'Gyalpo Lhosar', 'ग्याल्पो ल्होसार', '2027-02-07', '2027-02-07', 'FESTIVAL', true, true),
    ('shivaratri', 'Maha Shivaratri', 'महाशिवरात्रि', '2027-03-06', '2027-03-06', 'FESTIVAL', true, true),
    ('holi', 'Holi', 'होली', '2027-03-21', '2027-03-22', 'FESTIVAL', true, true),
    -- FY 2084/85
    ('teej', 'Teej', 'तीज', '2027-09-03', '2027-09-03', 'FESTIVAL', true, true),
    ('dashain', 'Dashain', 'दशैँ', '2027-09-30', '2027-10-14', 'FESTIVAL', true, true),
    ('tihar', 'Tihar', 'तिहार', '2027-10-27', '2027-10-31', 'FESTIVAL', true, true),
    ('chhath', 'Chhath', 'छठ', '2027-11-04', '2027-11-05', 'FESTIVAL', true, true),
    ('sonam-lhosar', 'Sonam Lhosar', 'सोनाम ल्होसार', '2028-01-28', '2028-01-28', 'FESTIVAL', true, true),
    ('shivaratri', 'Maha Shivaratri', 'महाशिवरात्रि', '2028-02-23', '2028-02-23', 'FESTIVAL', true, true),
    ('gyalpo-lhosar', 'Gyalpo Lhosar', 'ग्याल्पो ल्होसार', '2028-02-26', '2028-02-26', 'FESTIVAL', true, true),
    ('holi', 'Holi', 'होली', '2028-03-10', '2028-03-11', 'FESTIVAL', true, true),
    -- Exact (fixed BS dates)
    ('tamu-lhosar', 'Tamu Lhosar', 'तमु ल्होसार', '2025-12-30', '2025-12-30', 'FESTIVAL', true, false),
    ('bs-new-year', 'New Year 2083', 'नयाँ वर्ष 2083', '2026-04-14', '2026-04-14', 'FESTIVAL', true, false),
    ('tamu-lhosar', 'Tamu Lhosar', 'तमु ल्होसार', '2026-12-30', '2026-12-30', 'FESTIVAL', true, false),
    ('bs-new-year', 'New Year 2084', 'नयाँ वर्ष 2084', '2027-04-14', '2027-04-14', 'FESTIVAL', true, false),
    ('tamu-lhosar', 'Tamu Lhosar', 'तमु ल्होसार', '2027-12-30', '2027-12-30', 'FESTIVAL', true, false),
    ('bs-new-year', 'New Year 2085', 'नयाँ वर्ष 2085', '2028-04-13', '2028-04-13', 'FESTIVAL', true, false);

CREATE TABLE analytics_settings (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    managers_see_staff uuid[] NOT NULL DEFAULT '{}',
    highlight_rules    boolean NOT NULL DEFAULT true,
    updated_at         timestamptz NOT NULL DEFAULT now()
);
INSERT INTO analytics_settings (id) VALUES (true);

---- create above / drop below ----

DROP TABLE IF EXISTS analytics_settings;
DROP TABLE IF EXISTS calendar_events;
DROP TRIGGER IF EXISTS analytics_dirty_shift_adjustments_trg ON shift_adjustments;
DROP TRIGGER IF EXISTS analytics_dirty_shifts_trg ON shifts;
DROP TRIGGER IF EXISTS analytics_dirty_order_items_trg ON order_items;
DROP TRIGGER IF EXISTS analytics_dirty_orders_trg ON orders;
DROP TRIGGER IF EXISTS analytics_dirty_payments_trg ON payments;
DROP TRIGGER IF EXISTS analytics_dirty_invoices_trg ON invoices;
DROP FUNCTION IF EXISTS analytics_dirty_shift_adjustments();
DROP FUNCTION IF EXISTS analytics_dirty_shifts();
DROP FUNCTION IF EXISTS analytics_dirty_order_items();
DROP FUNCTION IF EXISTS analytics_dirty_orders();
DROP FUNCTION IF EXISTS analytics_dirty_payments();
DROP FUNCTION IF EXISTS analytics_dirty_invoices();
DROP FUNCTION IF EXISTS analytics_mark_dirty(timestamptz);
DROP TABLE IF EXISTS analytics_dirty_days;
DROP TABLE IF EXISTS analytics_days;
