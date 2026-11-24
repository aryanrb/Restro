-- 004: orders. Append-only order items with snapshots; optimistic version.

CREATE TABLE orders (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id      uuid NOT NULL UNIQUE,            -- client-generated, stable across offline creation
    type           text NOT NULL CHECK (type IN ('DINE_IN', 'TAKEAWAY')),
    table_id       uuid REFERENCES dining_tables (id),
    waiter_user_id uuid NOT NULL REFERENCES users (id),
    status         text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'BILLED', 'CLOSED', 'CANCELLED')),
    guest_count    integer NOT NULL DEFAULT 0,
    version        bigint NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_status_idx ON orders (status);
CREATE INDEX orders_table_idx ON orders (table_id);

CREATE TABLE order_items (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id                 uuid NOT NULL REFERENCES orders (id),
    client_id                uuid NOT NULL UNIQUE,
    menu_item_id             uuid NOT NULL,
    name_en                  text NOT NULL,          -- snapshot
    name_ne                  text NOT NULL DEFAULT '',
    variant_name_en          text NOT NULL DEFAULT '',
    variant_name_ne          text NOT NULL DEFAULT '',
    has_variant              boolean NOT NULL DEFAULT false,
    unit_price_paisa         bigint NOT NULL,        -- snapshot (server price at add time)
    quantity                 integer NOT NULL,
    note                     text NOT NULL DEFAULT '',
    station_id               uuid NOT NULL,
    status                   text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'SENT', 'PREPARING', 'READY', 'SERVED', 'VOIDED')),
    void_reason              text NOT NULL DEFAULT '',
    voided_by_user_id        uuid,
    void_approved_by_user_id uuid,
    created_at               timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_items_order_idx ON order_items (order_id);
CREATE INDEX order_items_station_status_idx ON order_items (station_id, status);

CREATE TABLE order_item_modifiers (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_item_id     uuid NOT NULL REFERENCES order_items (id) ON DELETE CASCADE,
    name_en           text NOT NULL,
    name_ne           text NOT NULL DEFAULT '',
    price_delta_paisa bigint NOT NULL DEFAULT 0
);
CREATE INDEX order_item_modifiers_item_idx ON order_item_modifiers (order_item_id);

---- create above / drop below ----

DROP TABLE IF EXISTS order_item_modifiers;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
