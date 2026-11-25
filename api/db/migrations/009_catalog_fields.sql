-- 009: admin catalog fields — settings payment QR images + guest ordering flag,
-- and staff phone (loyalty).

ALTER TABLE settings
    ADD COLUMN qr_fonepay_url text NOT NULL DEFAULT '',
    ADD COLUMN qr_other_url   text NOT NULL DEFAULT '',
    ADD COLUMN guest_ordering boolean NOT NULL DEFAULT false;

-- Normalised digits (no +977); '' = none.
ALTER TABLE users
    ADD COLUMN phone text NOT NULL DEFAULT '';

---- create above / drop below ----

ALTER TABLE users
    DROP COLUMN phone;

ALTER TABLE settings
    DROP COLUMN guest_ordering,
    DROP COLUMN qr_other_url,
    DROP COLUMN qr_fonepay_url;
