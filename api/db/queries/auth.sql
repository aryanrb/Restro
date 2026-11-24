-- name: CreateDevice :one
INSERT INTO devices (token_hash, label, shared) VALUES ($1, $2, true) RETURNING *;

-- name: GetDeviceByTokenHash :one
SELECT * FROM devices WHERE token_hash = $1;

-- name: CreateSession :one
INSERT INTO sessions (token_hash, user_id, device_id, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetActiveSessionByTokenHash :one
SELECT sqlc.embed(sessions), sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = $1
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > now();

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: InsertAudit :exec
INSERT INTO audit_log (actor_user_id, action, entity, entity_id, before, after, reason, approver_user_id, device_label)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
