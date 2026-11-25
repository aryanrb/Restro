-- 006: PIN lockouts auto-expire with progressive durations (2 min, 5 min, then
-- 30 min). pin_locked_until is when the current lock lifts (NULL = not locked);
-- pin_lockout_count is how many locks in a row the user has hit, driving the
-- escalation. Both are cleared by a correct PIN or an owner/manager PIN reset.

ALTER TABLE users
    ADD COLUMN pin_locked_until  timestamptz,
    ADD COLUMN pin_lockout_count integer NOT NULL DEFAULT 0;

-- Users locked under the old rule (failed_pin_attempts >= 5, no expiry) become a
-- first lockout that lifts after 2 minutes.
UPDATE users
SET pin_locked_until = now() + interval '2 minutes', pin_lockout_count = 1
WHERE failed_pin_attempts >= 5;

---- create above / drop below ----

ALTER TABLE users
    DROP COLUMN pin_lockout_count,
    DROP COLUMN pin_locked_until;
