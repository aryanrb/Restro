-- name: ListAreas :many
SELECT * FROM areas ORDER BY sort, name_en;

-- name: GetArea :one
SELECT * FROM areas WHERE id = $1;

-- name: NextAreaSort :one
SELECT COALESCE(MAX(sort), 0) + 1 FROM areas;

-- name: CreateArea :one
INSERT INTO areas (name_en, name_ne, sort) VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateArea :one
UPDATE areas SET name_en = $2, name_ne = $3 WHERE id = $1 RETURNING *;

-- name: UpdateAreaSort :exec
UPDATE areas SET sort = $2 WHERE id = $1;

-- name: DeleteArea :exec
DELETE FROM areas WHERE id = $1;

-- name: CountTablesInArea :one
SELECT count(*) FROM dining_tables WHERE area_id = $1;

-- name: ListTables :many
SELECT * FROM dining_tables ORDER BY sort, name;

-- name: GetTable :one
SELECT * FROM dining_tables WHERE id = $1;

-- name: NextTableSort :one
-- Table sort is per area.
SELECT COALESCE(MAX(sort), 0) + 1 FROM dining_tables WHERE area_id = $1;

-- name: CreateTable :one
INSERT INTO dining_tables (area_id, name, seats, active, sort) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: UpdateTable :one
UPDATE dining_tables SET area_id = $2, name = $3, seats = $4, active = $5, sort = $6 WHERE id = $1 RETURNING *;

-- name: ListTablesInArea :many
SELECT * FROM dining_tables WHERE area_id = $1 ORDER BY sort, name;

-- name: UpdateTableSort :exec
UPDATE dining_tables SET sort = $2 WHERE id = $1;
