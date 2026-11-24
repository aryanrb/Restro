-- name: ListStations :many
SELECT * FROM stations ORDER BY sort, name_en;

-- name: GetStation :one
SELECT * FROM stations WHERE id = $1;

-- name: CountStations :one
SELECT count(*) FROM stations;

-- name: NextStationSort :one
SELECT COALESCE(MAX(sort), 0) + 1 FROM stations;

-- name: CreateStation :one
INSERT INTO stations (name_en, name_ne, sort) VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateStation :one
UPDATE stations SET name_en = $2, name_ne = $3 WHERE id = $1 RETURNING *;

-- name: DeleteStation :exec
DELETE FROM stations WHERE id = $1;

-- name: CountItemsUsingStation :one
SELECT count(*) FROM menu_items WHERE station_id = $1;
