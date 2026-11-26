-- 015: multi-tenancy foundation (stage 4, chunk 1).
-- Introduces the organisation -> restaurant hierarchy and the per-restaurant staff
-- grant table. Existing single-restaurant data is folded into one seeded org + one
-- seeded restaurant, and every existing user is granted their current role there.
-- Tenant-scoping of the remaining tables (restaurant_id + RLS) is a later chunk.

CREATE TABLE organisations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE restaurants (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organisation_id uuid NOT NULL REFERENCES organisations (id),
    name            text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX restaurants_org_idx ON restaurants (organisation_id);

-- A user belongs to one organisation; a grant row says which restaurants they may
-- work at and in what role. Role checks will read this grant (later chunk); for now
-- users.role is still authoritative and the grant mirrors it.
CREATE TABLE restaurant_staff (
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    restaurant_id uuid NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    role          text NOT NULL CHECK (role IN ('OWNER', 'MANAGER', 'CASHIER', 'WAITER', 'KITCHEN')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, restaurant_id)
);
CREATE INDEX restaurant_staff_restaurant_idx ON restaurant_staff (restaurant_id);

-- Fold the existing single-restaurant deployment into one org + one restaurant,
-- named from the current settings (falling back to a placeholder).
INSERT INTO organisations (name)
    VALUES (COALESCE((SELECT NULLIF(name_en, '') FROM settings WHERE id = 1), 'Restaurant'));
INSERT INTO restaurants (organisation_id, name)
    VALUES ((SELECT id FROM organisations LIMIT 1),
            COALESCE((SELECT NULLIF(name_en, '') FROM settings WHERE id = 1), 'Restaurant'));

-- users.organisation_id: add nullable, backfill to the one org, then enforce.
ALTER TABLE users ADD COLUMN organisation_id uuid REFERENCES organisations (id);
UPDATE users SET organisation_id = (SELECT id FROM organisations LIMIT 1);
ALTER TABLE users ALTER COLUMN organisation_id SET NOT NULL;
CREATE INDEX users_org_idx ON users (organisation_id);

-- Username is unique per organisation now (not globally).
ALTER TABLE users DROP CONSTRAINT users_username_key;
CREATE UNIQUE INDEX users_org_username_key ON users (organisation_id, username)
    WHERE username IS NOT NULL AND username <> '';

-- Grant every existing user their current role at the seeded restaurant.
INSERT INTO restaurant_staff (user_id, restaurant_id, role)
    SELECT u.id, (SELECT id FROM restaurants LIMIT 1), u.role FROM users u;

---- create above / drop below ----

DROP TABLE IF EXISTS restaurant_staff;
DROP INDEX IF EXISTS users_org_username_key;
ALTER TABLE users ADD CONSTRAINT users_username_key UNIQUE (username);
DROP INDEX IF EXISTS users_org_idx;
ALTER TABLE users DROP COLUMN IF EXISTS organisation_id;
DROP TABLE IF EXISTS restaurants;
DROP TABLE IF EXISTS organisations;
