-- 001: baseline. Real domain tables start at 002 (auth).
-- Records when the schema was first applied; also gives the boot-migration test a
-- concrete object to assert against.
CREATE TABLE schema_baseline (
    id         integer PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

---- create above / drop below ----

DROP TABLE IF EXISTS schema_baseline;
