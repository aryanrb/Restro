-- 018: guest table ordering (QR menu).
-- * dining_tables.guest_code: the random token in the table's QR (/menu/?t=<code>);
--   regenerating it invalidates the old QR. guest_ordering: per-table on/off
--   (the restaurant-wide switch is settings.guest_ordering).
-- * guest_requests: a guest order (lines priced by the server at submit, kept as
--   a JSON snapshot until a staff member accepts it), or a Call waiter / Ask for
--   bill alert. request_id makes a guest submit idempotent; at most one open alert
--   per table and kind.
-- * orders.bill_requested_at: set by Ask for bill, cleared by Done or a bill.
-- * order_items.source: GUEST for lines accepted from a guest order.

ALTER TABLE dining_tables
    ADD COLUMN guest_code text NOT NULL DEFAULT replace(gen_random_uuid()::text, '-', ''),
    ADD COLUMN guest_ordering boolean NOT NULL DEFAULT true;
CREATE UNIQUE INDEX dining_tables_guest_code_idx ON dining_tables (guest_code);

CREATE TABLE guest_requests (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    table_id           uuid NOT NULL REFERENCES dining_tables (id),
    kind               text NOT NULL CHECK (kind IN ('ORDER', 'CALL_WAITER', 'BILL')),
    status             text NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'ACCEPTED', 'REJECTED', 'DONE')),
    request_id         text NOT NULL DEFAULT '',
    lines              jsonb NOT NULL DEFAULT '[]',
    reject_reason      text,
    order_id           uuid REFERENCES orders (id),
    handled_by_user_id uuid REFERENCES users (id),
    handled_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX guest_requests_request_idx ON guest_requests (table_id, request_id) WHERE kind = 'ORDER';
CREATE UNIQUE INDEX guest_requests_open_alert_idx ON guest_requests (table_id, kind) WHERE kind <> 'ORDER' AND status = 'PENDING';
CREATE INDEX guest_requests_status_idx ON guest_requests (status, created_at);
CREATE INDEX guest_requests_table_idx ON guest_requests (table_id, created_at);

ALTER TABLE orders ADD COLUMN bill_requested_at timestamptz;
ALTER TABLE order_items ADD COLUMN source text NOT NULL DEFAULT 'STAFF' CHECK (source IN ('STAFF', 'GUEST'));

---- create above / drop below ----

ALTER TABLE order_items DROP COLUMN IF EXISTS source;
ALTER TABLE orders DROP COLUMN IF EXISTS bill_requested_at;
DROP TABLE IF EXISTS guest_requests;
DROP INDEX IF EXISTS dining_tables_guest_code_idx;
ALTER TABLE dining_tables DROP COLUMN IF EXISTS guest_ordering, DROP COLUMN IF EXISTS guest_code;
