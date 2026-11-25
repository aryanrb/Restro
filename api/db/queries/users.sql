-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = $1;

-- name: ListActiveUsers :many
SELECT * FROM users WHERE active = true ORDER BY name;

-- name: ListStaff :many
SELECT * FROM users ORDER BY role, name;

-- name: CountActiveOwners :one
SELECT count(*) FROM users WHERE role = 'OWNER' AND active = true;

-- name: CreateUser :one
INSERT INTO users (name, role, username, password_hash, pin_hash, language, active)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: UpdateUser :one
UPDATE users
SET name = $2, role = $3, username = $4, password_hash = $5, pin_hash = $6, active = $7, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetUserLanguage :one
UPDATE users SET language = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: IncrementPinFailure :one
UPDATE users SET failed_pin_attempts = failed_pin_attempts + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ResetPinFailures :exec
-- A correct PIN (or a full login) clears the failure counter, any lock and the
-- lockout escalation.
UPDATE users
SET failed_pin_attempts = 0, pin_locked_until = NULL, pin_lockout_count = 0, updated_at = now()
WHERE id = $1;

-- name: SetUserPin :exec
-- An owner/manager PIN reset unlocks immediately and clears the escalation.
UPDATE users
SET pin_hash = $2, failed_pin_attempts = 0, pin_locked_until = NULL, pin_lockout_count = 0, updated_at = now()
WHERE id = $1;

-- name: ReservePinAttempt :one
-- Atomically counts a PIN attempt before it is checked. Only allowed while the
-- user is unlocked and under the limit, or once a lock has expired (which starts
-- a fresh set of attempts). No row = locked. The attempt that reaches the limit
-- (lock_after) also sets the lock in the same statement: pin_locked_until =
-- now + lock_secs[pin_lockout_count+1] (last entry repeats) and bumps
-- pin_lockout_count. A correct PIN then clears all of it (ResetPinFailures), so
-- concurrent guesses can never exceed the limit. In the SET list, a non-NULL
-- pin_locked_until can only be an expired lock (the WHERE excludes live ones).
UPDATE users SET
    failed_pin_attempts = (CASE WHEN pin_locked_until IS NULL THEN failed_pin_attempts ELSE 0 END) + 1,
    pin_locked_until = CASE
        WHEN (CASE WHEN pin_locked_until IS NULL THEN failed_pin_attempts ELSE 0 END) + 1 >= sqlc.arg(lock_after)::int
        THEN sqlc.arg(now)::timestamptz + make_interval(secs => (sqlc.arg(lock_secs)::int[])[LEAST(pin_lockout_count + 1, cardinality(sqlc.arg(lock_secs)::int[]))])
    END,
    pin_lockout_count = pin_lockout_count + CASE
        WHEN (CASE WHEN pin_locked_until IS NULL THEN failed_pin_attempts ELSE 0 END) + 1 >= sqlc.arg(lock_after)::int
        THEN 1 ELSE 0
    END,
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND ((pin_locked_until IS NULL AND failed_pin_attempts < sqlc.arg(lock_after)::int)
       OR pin_locked_until <= sqlc.arg(now)::timestamptz)
RETURNING *;
