-- 002: auth foundation — users, devices, sessions, audit log, idempotency store.

CREATE TABLE users (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                text NOT NULL,
    role                text NOT NULL CHECK (role IN ('OWNER', 'MANAGER', 'CASHIER', 'WAITER', 'KITCHEN')),
    username            text UNIQUE,           -- owner/manager only
    password_hash       text,                  -- argon2id encoded string; owner/manager only
    pin_hash            text,                  -- argon2id encoded string; all staff
    language            text NOT NULL DEFAULT 'en' CHECK (language IN ('en', 'ne')),
    active              boolean NOT NULL DEFAULT true,
    failed_pin_attempts integer NOT NULL DEFAULT 0,  -- locked when >= 5; cleared on reset / successful login
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- A device is "enrolled" by an owner/manager so staff may PIN-login on it. The
-- client holds an opaque device token; we store only its hash.
CREATE TABLE devices (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash text NOT NULL UNIQUE,
    label      text NOT NULL,
    shared     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Opaque session tokens; we store only the hash so a DB leak can't impersonate.
CREATE TABLE sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash text NOT NULL UNIQUE,
    user_id    uuid NOT NULL REFERENCES users (id),
    device_id  uuid REFERENCES devices (id),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Append-only. Written in the SAME transaction as the change it records.
CREATE TABLE audit_log (
    id                bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
    actor_user_id     uuid,
    action            text NOT NULL,
    entity            text NOT NULL,
    entity_id         text,
    before            jsonb,
    after             jsonb,
    reason            text,
    approver_user_id  uuid,
    device_label      text,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_created_at_idx ON audit_log (created_at);
CREATE INDEX audit_log_entity_idx ON audit_log (entity, entity_id);

-- Idempotency: a client command UUID maps to its original serialized response, so
-- retries from an offline queue are no-ops. Pruned after ~7 days (step 4 worker).
CREATE TABLE processed_commands (
    command_id uuid PRIMARY KEY,
    user_id    uuid,
    response   bytea,
    created_at timestamptz NOT NULL DEFAULT now()
);

---- create above / drop below ----

DROP TABLE IF EXISTS processed_commands;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS users;
