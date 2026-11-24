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
UPDATE users SET failed_pin_attempts = 0, updated_at = now() WHERE id = $1;

-- name: SetUserPin :exec
UPDATE users SET pin_hash = $2, failed_pin_attempts = 0, updated_at = now() WHERE id = $1;
