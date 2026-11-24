-- 003: settings (single row), floor (areas/tables), stations, menu.

CREATE TABLE settings (
    id                     integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    version                bigint NOT NULL DEFAULT 1,
    name_en                text NOT NULL DEFAULT '',
    name_ne                text NOT NULL DEFAULT '',
    address                text NOT NULL DEFAULT '',
    phone                  text NOT NULL DEFAULT '',
    pan_number             text NOT NULL DEFAULT '',
    vat_registered         boolean NOT NULL DEFAULT true,
    prices_include_vat     boolean NOT NULL DEFAULT true,
    vat_rate_bp            integer NOT NULL DEFAULT 1300,
    service_charge_enabled boolean NOT NULL DEFAULT false,
    service_charge_rate_bp integer NOT NULL DEFAULT 1000,
    billing_mode           text NOT NULL DEFAULT 'ESTIMATE' CHECK (billing_mode IN ('ESTIMATE', 'TAX_INVOICE')),
    receipt_width          text NOT NULL DEFAULT '80MM' CHECK (receipt_width IN ('58MM', '80MM')),
    round_total_to_rupee   boolean NOT NULL DEFAULT false,
    default_language       text NOT NULL DEFAULT 'en' CHECK (default_language IN ('en', 'ne')),
    kds_yellow_minutes     integer NOT NULL DEFAULT 10,
    kds_red_minutes        integer NOT NULL DEFAULT 20,
    logo_url               text NOT NULL DEFAULT '',
    updated_at             timestamptz NOT NULL DEFAULT now()
);
INSERT INTO settings (id) VALUES (1);

CREATE TABLE areas (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name_en text NOT NULL,
    name_ne text NOT NULL DEFAULT '',
    sort    integer NOT NULL DEFAULT 0
);

CREATE TABLE dining_tables (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    area_id uuid NOT NULL REFERENCES areas (id),
    name    text NOT NULL,
    seats   integer NOT NULL DEFAULT 1,
    sort    integer NOT NULL DEFAULT 0,
    active  boolean NOT NULL DEFAULT true
);
CREATE INDEX dining_tables_area_idx ON dining_tables (area_id);

CREATE TABLE stations (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name_en text NOT NULL,
    name_ne text NOT NULL DEFAULT '',
    sort    integer NOT NULL DEFAULT 0
);

CREATE TABLE menu_categories (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name_en text NOT NULL,
    name_ne text NOT NULL DEFAULT '',
    sort    integer NOT NULL DEFAULT 0,
    active  boolean NOT NULL DEFAULT true
);

CREATE TABLE menu_items (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version     bigint NOT NULL DEFAULT 1,
    category_id uuid NOT NULL REFERENCES menu_categories (id),
    name_en     text NOT NULL,
    name_ne     text NOT NULL DEFAULT '',
    price_paisa bigint NOT NULL DEFAULT 0,
    color       text NOT NULL DEFAULT '',
    image_url   text NOT NULL DEFAULT '',
    station_id  uuid NOT NULL REFERENCES stations (id),
    active      boolean NOT NULL DEFAULT true,
    is_86d      boolean NOT NULL DEFAULT false,
    sort        integer NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX menu_items_category_idx ON menu_items (category_id);
CREATE INDEX menu_items_station_idx ON menu_items (station_id);

CREATE TABLE item_variants (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id     uuid NOT NULL REFERENCES menu_items (id) ON DELETE CASCADE,
    name_en     text NOT NULL,
    name_ne     text NOT NULL DEFAULT '',
    price_paisa bigint NOT NULL DEFAULT 0,
    sort        integer NOT NULL DEFAULT 0
);
CREATE INDEX item_variants_item_idx ON item_variants (item_id);

CREATE TABLE modifier_groups (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version    bigint NOT NULL DEFAULT 1,
    name_en    text NOT NULL,
    name_ne    text NOT NULL DEFAULT '',
    min_select integer NOT NULL DEFAULT 0,
    max_select integer NOT NULL DEFAULT 1
);

CREATE TABLE modifiers (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id          uuid NOT NULL REFERENCES modifier_groups (id) ON DELETE CASCADE,
    name_en           text NOT NULL,
    name_ne           text NOT NULL DEFAULT '',
    price_delta_paisa bigint NOT NULL DEFAULT 0,
    sort              integer NOT NULL DEFAULT 0
);
CREATE INDEX modifiers_group_idx ON modifiers (group_id);

CREATE TABLE item_modifier_groups (
    item_id  uuid NOT NULL REFERENCES menu_items (id) ON DELETE CASCADE,
    group_id uuid NOT NULL REFERENCES modifier_groups (id),
    PRIMARY KEY (item_id, group_id)
);

---- create above / drop below ----

DROP TABLE IF EXISTS item_modifier_groups;
DROP TABLE IF EXISTS modifiers;
DROP TABLE IF EXISTS modifier_groups;
DROP TABLE IF EXISTS item_variants;
DROP TABLE IF EXISTS menu_items;
DROP TABLE IF EXISTS menu_categories;
DROP TABLE IF EXISTS stations;
DROP TABLE IF EXISTS dining_tables;
DROP TABLE IF EXISTS areas;
DROP TABLE IF EXISTS settings;
