-- 012: inventory — stock items, the stock ledger, recipes, waste and stock counts
-- (spec: inventory-wastage-design). Quantities are bigint thousandths of the item's
-- base unit (*_milli); unit cost is paisa per base unit ×10,000 (*_e4).

CREATE TABLE inventory_items (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version         bigint NOT NULL DEFAULT 1,
    name_en         text NOT NULL,
    name_ne         text NOT NULL DEFAULT '',
    unit            text NOT NULL CHECK (unit IN ('g', 'ml', 'pcs')),
    tracked         boolean NOT NULL DEFAULT false,
    low_stock_milli bigint NOT NULL DEFAULT 0 CHECK (low_stock_milli >= 0),
    active          boolean NOT NULL DEFAULT true,
    -- Owner-typed until purchases exist; purchases then keep it as the moving
    -- weighted average. Every movement snapshots it.
    unit_cost_e4    bigint NOT NULL DEFAULT 0 CHECK (unit_cost_e4 >= 0),
    packs           jsonb NOT NULL DEFAULT '[]', -- [{"name_en","name_ne","size_milli"}]
    sort            integer NOT NULL DEFAULT 0,
    -- Cache of Σ stock_movements.quantity_milli, updated in the same statement batch
    -- as every movement insert (row lock serialises concurrent sends).
    on_hand_milli   bigint NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- The stock ledger. Never edited or deleted (trigger below).
CREATE TABLE stock_movements (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq               bigint NOT NULL GENERATED ALWAYS AS IDENTITY UNIQUE, -- insertion order (ties on `at`)
    inventory_item_id uuid NOT NULL REFERENCES inventory_items (id),
    quantity_milli    bigint NOT NULL, -- signed
    type              text NOT NULL CHECK (type IN
        ('SALE', 'SALE_REVERSAL', 'WASTE', 'COUNT_ADJUST', 'PURCHASE', 'PURCHASE_RETURN', 'OPENING')),
    ref_id            uuid NOT NULL,   -- order item / waste entry / stock count / inventory item / purchase line
    reason            text NOT NULL DEFAULT '',
    by_user_id        uuid REFERENCES users (id),
    at                timestamptz NOT NULL,
    unit_cost_e4      bigint NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stock_movements_item_at_idx ON stock_movements (inventory_item_id, at, seq);
CREATE INDEX stock_movements_ref_idx ON stock_movements (ref_id, type);

CREATE FUNCTION stock_movements_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'stock movements are never edited or deleted';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER stock_movements_immutable_trg BEFORE UPDATE OR DELETE ON stock_movements
    FOR EACH ROW EXECUTE FUNCTION stock_movements_immutable();

-- Recipes (per 1 unit sold). An item's recipe; a variant's own recipe replaces it
-- (a row in variant_recipes marks "own recipe", possibly empty); modifier lines add.
CREATE TABLE menu_item_recipe_lines (
    menu_item_id      uuid NOT NULL REFERENCES menu_items (id) ON DELETE CASCADE,
    inventory_item_id uuid NOT NULL REFERENCES inventory_items (id),
    quantity_milli    bigint NOT NULL CHECK (quantity_milli > 0),
    sort              integer NOT NULL DEFAULT 0,
    PRIMARY KEY (menu_item_id, inventory_item_id)
);

CREATE TABLE variant_recipes (
    variant_id uuid PRIMARY KEY REFERENCES item_variants (id) ON DELETE CASCADE
);

CREATE TABLE variant_recipe_lines (
    variant_id        uuid NOT NULL REFERENCES variant_recipes (variant_id) ON DELETE CASCADE,
    inventory_item_id uuid NOT NULL REFERENCES inventory_items (id),
    quantity_milli    bigint NOT NULL CHECK (quantity_milli > 0),
    sort              integer NOT NULL DEFAULT 0,
    PRIMARY KEY (variant_id, inventory_item_id)
);

CREATE TABLE modifier_recipe_lines (
    modifier_id       uuid NOT NULL REFERENCES modifiers (id) ON DELETE CASCADE,
    inventory_item_id uuid NOT NULL REFERENCES inventory_items (id),
    quantity_milli    bigint NOT NULL CHECK (quantity_milli > 0),
    sort              integer NOT NULL DEFAULT 0,
    PRIMARY KEY (modifier_id, inventory_item_id)
);

-- What a line was ordered as, so a later send can expand its recipe. NULL/empty on
-- lines added before this migration (they deduct the item recipe).
ALTER TABLE order_items
    ADD COLUMN variant_id   uuid,
    ADD COLUMN modifier_ids uuid[] NOT NULL DEFAULT '{}';

CREATE TABLE waste_entries (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id          uuid NOT NULL UNIQUE, -- idempotency
    inventory_item_id   uuid REFERENCES inventory_items (id),
    menu_item_id        uuid,
    variant_id          uuid,
    modifier_ids        uuid[] NOT NULL DEFAULT '{}',
    name_en             text NOT NULL, -- snapshot: "Chicken" or "Chicken Momo · Half"
    name_ne             text NOT NULL DEFAULT '',
    unit                text NOT NULL DEFAULT '', -- '' for portions of a menu item
    quantity_milli      bigint NOT NULL CHECK (quantity_milli > 0),
    reason              text NOT NULL CHECK (reason IN
        ('SPOILED', 'DROPPED', 'MISTAKE', 'STAFF_MEAL', 'RETURNED', 'OTHER')),
    note                text NOT NULL DEFAULT '',
    by_user_id          uuid NOT NULL REFERENCES users (id),
    approved_by_user_id uuid REFERENCES users (id),
    at                  timestamptz NOT NULL,
    value_paisa         bigint NOT NULL DEFAULT 0,
    CHECK ((inventory_item_id IS NULL) <> (menu_item_id IS NULL))
);
CREATE INDEX waste_entries_at_idx ON waste_entries (at);

CREATE TABLE count_lists (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL,
    inventory_item_ids uuid[] NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE stock_counts (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number               integer NOT NULL GENERATED ALWAYS AS IDENTITY UNIQUE,
    status               text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'SUBMITTED', 'POSTED', 'DISCARDED')),
    list_id              uuid REFERENCES count_lists (id) ON DELETE SET NULL,
    label                text NOT NULL DEFAULT '',
    started_by_user_id   uuid NOT NULL REFERENCES users (id),
    started_at           timestamptz NOT NULL DEFAULT now(),
    submitted_by_user_id uuid REFERENCES users (id),
    submitted_at         timestamptz,
    posted_by_user_id    uuid REFERENCES users (id),
    posted_at            timestamptz,
    discarded_reason     text NOT NULL DEFAULT '',
    discarded_at         timestamptz,
    total_value_paisa    bigint
);

CREATE TABLE stock_count_lines (
    count_id           uuid NOT NULL REFERENCES stock_counts (id) ON DELETE CASCADE,
    inventory_item_id  uuid NOT NULL REFERENCES inventory_items (id),
    sort               integer NOT NULL DEFAULT 0,
    counted_milli      bigint,
    counted_at         timestamptz,
    counted_seq        bigint, -- last stock_movements.seq when counted (ties on counted_at)
    counted_by_user_id uuid REFERENCES users (id),
    recount            boolean NOT NULL DEFAULT false,
    expected_milli     bigint, -- filled in when posted
    variance_milli     bigint,
    value_paisa        bigint,
    PRIMARY KEY (count_id, inventory_item_id)
);

---- create above / drop below ----

DROP TABLE IF EXISTS stock_count_lines;
DROP TABLE IF EXISTS stock_counts;
DROP TABLE IF EXISTS count_lists;
DROP TABLE IF EXISTS waste_entries;
ALTER TABLE order_items DROP COLUMN IF EXISTS modifier_ids;
ALTER TABLE order_items DROP COLUMN IF EXISTS variant_id;
DROP TABLE IF EXISTS modifier_recipe_lines;
DROP TABLE IF EXISTS variant_recipe_lines;
DROP TABLE IF EXISTS variant_recipes;
DROP TABLE IF EXISTS menu_item_recipe_lines;
DROP TRIGGER IF EXISTS stock_movements_immutable_trg ON stock_movements;
DROP FUNCTION IF EXISTS stock_movements_immutable();
DROP TABLE IF EXISTS stock_movements;
DROP TABLE IF EXISTS inventory_items;
