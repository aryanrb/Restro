-- 016: reservations (table bookings).
-- * reservation_settings: single row, starting values from the owner decisions.
-- * reservations: staff-entered bookings by a credit customer. "Late" and
--   "released" are derived from starts_at + settings, never stored. request_id
--   makes create idempotent. table_ids may be empty (assigned on the day).
-- * orders.reservation_id: the booking seated onto the order.

CREATE TABLE reservation_settings (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    enabled            boolean NOT NULL DEFAULT true,
    duration_min       integer NOT NULL DEFAULT 90,
    large_party_size   integer NOT NULL DEFAULT 9,
    large_duration_min integer NOT NULL DEFAULT 120,
    hold_before_min    integer NOT NULL DEFAULT 60,
    grace_min          integer NOT NULL DEFAULT 15,
    release_after_min  integer NOT NULL DEFAULT 30,
    updated_at         timestamptz NOT NULL DEFAULT now()
);
INSERT INTO reservation_settings (id) VALUES (true);

CREATE TABLE reservations (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id           uuid NOT NULL UNIQUE,
    customer_id          uuid NOT NULL REFERENCES customers (id),
    party_size           integer NOT NULL CHECK (party_size BETWEEN 1 AND 200),
    starts_at            timestamptz NOT NULL,
    duration_min         integer NOT NULL CHECK (duration_min BETWEEN 15 AND 720),
    area_id              uuid REFERENCES areas (id),
    table_ids            uuid[] NOT NULL DEFAULT '{}',
    whole_area           boolean NOT NULL DEFAULT false,
    status               text NOT NULL DEFAULT 'BOOKED' CHECK (status IN ('BOOKED', 'SEATED', 'CANCELLED', 'NO_SHOW')),
    source               text NOT NULL DEFAULT 'PHONE' CHECK (source IN ('PHONE', 'IN_PERSON', 'MESSAGE', 'OTHER')),
    notes                text NOT NULL DEFAULT '',
    order_id             uuid REFERENCES orders (id),
    seated_at            timestamptz,
    seated_by_user_id    uuid REFERENCES users (id),
    cancel_reason        text,
    cancelled_at         timestamptz,
    cancelled_by_user_id uuid REFERENCES users (id),
    no_show_at           timestamptz,
    no_show_by_user_id   uuid REFERENCES users (id),
    confirmed_at         timestamptz,
    reminded_at          timestamptz,
    hold_extended_min    integer NOT NULL DEFAULT 0,
    created_by_user_id   uuid NOT NULL REFERENCES users (id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_by_user_id   uuid NOT NULL REFERENCES users (id),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    version              bigint NOT NULL DEFAULT 1
);
CREATE INDEX reservations_starts_at_idx ON reservations (starts_at);
CREATE INDEX reservations_customer_idx ON reservations (customer_id);

ALTER TABLE orders ADD COLUMN reservation_id uuid REFERENCES reservations (id);

---- create above / drop below ----

ALTER TABLE orders DROP COLUMN IF EXISTS reservation_id;
DROP TABLE IF EXISTS reservations;
DROP TABLE IF EXISTS reservation_settings;
