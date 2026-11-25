-- 013: suspicious activity alerts (spec: suspicious-activity-alerts-design).
-- Rules run in Go when an audit_log row is written, in the same transaction.

-- The owner's settings: one row. Rule settings are stored as overrides of the
-- defaults shipped in code: {"CASH_SHORT": {"enabled": false, "severity": "HIGH",
-- "thresholds": {"amount": 100000}}}.
CREATE TABLE alert_config (
    id                        integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    overrides                 jsonb NOT NULL DEFAULT '{}',
    -- null = not entered; else 7 entries (0 = Sunday) of null | {"open","close"}
    opening_hours             jsonb,
    after_hours_grace_minutes integer NOT NULL DEFAULT 30 CHECK (after_hours_grace_minutes BETWEEN 0 AND 240),
    quiet_from                text,  -- "HH:MM"; both null = no quiet hours
    quiet_to                  text,
    daily_push_cap            integer NOT NULL DEFAULT 5 CHECK (daily_push_cap BETWEEN 0 AND 50),
    recipient_ids             uuid[] NOT NULL DEFAULT '{}',
    updated_at                timestamptz NOT NULL DEFAULT now()
);
INSERT INTO alert_config (id) VALUES (1);

-- Alerts are never deleted (trigger below); only status/resolution and grouping change.
CREATE TABLE alerts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq                 bigint NOT NULL GENERATED ALWAYS AS IDENTITY, -- creation order (ties on last_at)
    rule                text NOT NULL,
    severity            text NOT NULL CHECK (severity IN ('HIGH', 'NORMAL')),
    status              text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'OK', 'PROBLEM')),
    subject_user_id     uuid REFERENCES users (id),
    approver_ids        uuid[] NOT NULL DEFAULT '{}',
    -- everyone the alert is about (subject, approvers, others): a manager recipient never sees these
    involved_ids        uuid[] NOT NULL DEFAULT '{}',
    subject             text NOT NULL DEFAULT '',
    num_params          jsonb NOT NULL DEFAULT '{}',
    text_params         jsonb NOT NULL DEFAULT '{}',
    count               integer NOT NULL DEFAULT 1,
    business_date       date NOT NULL,
    first_at            timestamptz NOT NULL,
    last_at             timestamptz NOT NULL,
    shift_id            uuid,
    invoice_id          uuid,
    audit_ids           bigint[] NOT NULL DEFAULT '{}',
    -- grouping: subject + rule-specific key (shift, day, order...); one OPEN alert per rule + group
    group_key           text NOT NULL,
    -- at most one alert ever per rule + dedup_key (e.g. one per shift)
    dedup_key           text,
    resolved_by_user_id uuid REFERENCES users (id),
    resolved_at         timestamptz,
    note                text NOT NULL DEFAULT '',
    version             bigint NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX alerts_open_group_idx ON alerts (rule, group_key) WHERE status = 'OPEN';
CREATE UNIQUE INDEX alerts_dedup_idx ON alerts (rule, dedup_key) WHERE dedup_key IS NOT NULL;
CREATE INDEX alerts_last_at_idx ON alerts (last_at DESC);
CREATE INDEX alerts_business_date_idx ON alerts (business_date);

CREATE FUNCTION alerts_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'alerts are never deleted';
END $$;
CREATE TRIGGER alerts_no_delete BEFORE DELETE ON alerts
    FOR EACH ROW EXECUTE FUNCTION alerts_no_delete();

-- Immediate notifications (the in-app stand-in for Web Push). exclude_ids: users
-- the push is about (a manager recipient doesn't get pushes about themselves).
CREATE TABLE alert_pushes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    at            timestamptz NOT NULL,
    business_date date NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('ALERT', 'MORE', 'SUMMARY')),
    rule          text NOT NULL DEFAULT '',
    amount_paisa  bigint,
    count         integer,
    alert_id      uuid REFERENCES alerts (id),
    exclude_ids   uuid[] NOT NULL DEFAULT '{}'
);
CREATE UNIQUE INDEX alert_pushes_once_idx ON alert_pushes (business_date, kind) WHERE kind IN ('MORE', 'SUMMARY');
CREATE INDEX alert_pushes_at_idx ON alert_pushes (at);

-- Look-back indexes for the rules.
CREATE INDEX audit_log_action_actor_idx ON audit_log (action, actor_user_id, created_at);

---- create above / drop below ----

DROP INDEX IF EXISTS audit_log_action_actor_idx;
DROP TABLE IF EXISTS alert_pushes;
DROP TABLE IF EXISTS alerts;
DROP FUNCTION IF EXISTS alerts_no_delete();
DROP TABLE IF EXISTS alert_config;
