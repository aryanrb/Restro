-- Organisation / restaurant hierarchy and staff grants (stage 4).

-- name: FirstOrganisation :one
-- The bootstrap organisation (one exists after migration 015).
SELECT * FROM organisations ORDER BY created_at LIMIT 1;

-- name: FirstRestaurant :one
-- The bootstrap restaurant (one exists after migration 015).
SELECT * FROM restaurants ORDER BY created_at LIMIT 1;

-- name: CreateOrganisation :one
INSERT INTO organisations (name) VALUES ($1) RETURNING *;

-- name: CreateRestaurant :one
INSERT INTO restaurants (organisation_id, name) VALUES ($1, $2) RETURNING *;

-- name: ListRestaurantsForUser :many
-- Restaurants a user may work at, via their grants.
SELECT r.* FROM restaurants r
JOIN restaurant_staff rs ON rs.restaurant_id = r.id
WHERE rs.user_id = $1
ORDER BY r.name;

-- name: GetStaffGrant :one
SELECT * FROM restaurant_staff WHERE user_id = $1 AND restaurant_id = $2;

-- name: UpsertStaffGrant :exec
INSERT INTO restaurant_staff (user_id, restaurant_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, restaurant_id) DO UPDATE SET role = EXCLUDED.role;
