-- 019: Web Push subscriptions (spec: suspicious-activity-alerts-design, "Push setup").
-- One row per browser/device a user turned push on in. endpoint is unique: a
-- device that re-subscribes (or is used by another user) replaces its row.
-- Rows whose push service answers 404/410 are deleted by the sender.

CREATE TABLE push_subscriptions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id),
    endpoint     text NOT NULL UNIQUE,
    p256dh       text NOT NULL,
    auth         text NOT NULL,
    device_label text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX push_subscriptions_user_idx ON push_subscriptions (user_id);

---- create above / drop below ----

DROP TABLE IF EXISTS push_subscriptions;
