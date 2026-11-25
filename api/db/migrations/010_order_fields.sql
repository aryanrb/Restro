-- 010: order/line fields the front ends need: a free-text order label (takeaway
-- name), and per line when it was sent and who added it.

ALTER TABLE orders ADD COLUMN label text NOT NULL DEFAULT '';

ALTER TABLE order_items
    ADD COLUMN sent_at          timestamptz,                    -- set when the line becomes SENT
    ADD COLUMN added_by_user_id uuid REFERENCES users (id);

-- Existing lines already past DRAFT: best estimate is their creation time.
UPDATE order_items SET sent_at = created_at
WHERE status IN ('SENT', 'PREPARING', 'READY', 'SERVED');

---- create above / drop below ----

ALTER TABLE order_items DROP COLUMN IF EXISTS added_by_user_id;
ALTER TABLE order_items DROP COLUMN IF EXISTS sent_at;
ALTER TABLE orders DROP COLUMN IF EXISTS label;
