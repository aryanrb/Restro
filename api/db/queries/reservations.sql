-- Reservations (table bookings). Business rules live in internal/reservations.

-- name: GetReservationSettings :one
SELECT * FROM reservation_settings WHERE id;

-- name: SaveReservationSettings :one
UPDATE reservation_settings
SET enabled = $1, duration_min = $2, large_party_size = $3, large_duration_min = $4,
    hold_before_min = $5, grace_min = $6, release_after_min = $7, updated_at = now()
WHERE id
RETURNING *;

-- Serialises booking writes so two overlapping bookings can't both pass the clash check.
-- name: LockReservations :exec
SELECT pg_advisory_xact_lock(7016);

-- name: GetReservationView :one
SELECT sqlc.embed(r), c.name AS customer_name, c.phone AS customer_phone
FROM reservations r JOIN customers c ON c.id = r.customer_id
WHERE r.id = $1;

-- name: LockReservation :one
SELECT * FROM reservations WHERE id = $1 FOR UPDATE;

-- name: GetReservationByRequestID :one
SELECT * FROM reservations WHERE request_id = $1;

-- name: ListReservationsBetween :many
SELECT sqlc.embed(r), c.name AS customer_name, c.phone AS customer_phone
FROM reservations r JOIN customers c ON c.id = r.customer_id
WHERE r.starts_at >= @from_at AND r.starts_at < @to_at
ORDER BY r.starts_at, r.created_at;

-- name: ListCustomerReservations :many
SELECT sqlc.embed(r), c.name AS customer_name, c.phone AS customer_phone
FROM reservations r JOIN customers c ON c.id = r.customer_id
WHERE r.customer_id = $1
ORDER BY r.starts_at DESC;

-- A BOOKED booking (other than exclude_id) holding one of the tables in an overlapping window.
-- name: FindReservationClash :one
SELECT id FROM reservations
WHERE status = 'BOOKED' AND id <> @exclude_id
  AND table_ids && @table_ids::uuid[]
  AND starts_at < @ends_at
  AND starts_at + make_interval(mins => duration_min) > @starts_at
ORDER BY starts_at
LIMIT 1;

-- Tables held by BOOKED bookings in an overlapping window (for suggestions).
-- name: BookedTablesInWindow :many
SELECT DISTINCT unnest(table_ids)::uuid AS table_id FROM reservations
WHERE status = 'BOOKED' AND id <> @exclude_id
  AND starts_at < @ends_at
  AND starts_at + make_interval(mins => duration_min) > @starts_at;

-- BOOKED bookings that may hold a table now (the live view picks HOLD/LATE in Go).
-- name: ActiveBookedReservations :many
SELECT r.id, r.starts_at, r.party_size, r.table_ids, r.hold_extended_min, c.name AS customer_name
FROM reservations r JOIN customers c ON c.id = r.customer_id
WHERE r.status = 'BOOKED' AND cardinality(r.table_ids) > 0
  AND r.starts_at > @since AND r.starts_at < @until
ORDER BY r.starts_at;

-- name: InsertReservation :one
INSERT INTO reservations (
    request_id, customer_id, party_size, starts_at, duration_min, area_id, table_ids, whole_area,
    source, notes, created_by_user_id, updated_by_user_id
) VALUES (
    @request_id, @customer_id, @party_size, @starts_at, @duration_min, @area_id, @table_ids::uuid[], @whole_area,
    @source, @notes, @user_id, @user_id
)
RETURNING *;

-- name: UpdateReservationFields :exec
UPDATE reservations
SET party_size = @party_size, starts_at = @starts_at, duration_min = @duration_min, area_id = @area_id,
    table_ids = @table_ids::uuid[], whole_area = @whole_area, source = @source, notes = @notes,
    updated_by_user_id = @user_id, updated_at = now(), version = version + 1
WHERE id = @id;

-- name: SetReservationCancelled :exec
UPDATE reservations
SET status = 'CANCELLED', cancel_reason = @reason, cancelled_at = now(), cancelled_by_user_id = @user_id,
    updated_by_user_id = @user_id, updated_at = now(), version = version + 1
WHERE id = @id;

-- name: SetReservationNoShow :exec
UPDATE reservations
SET status = 'NO_SHOW', no_show_at = now(), no_show_by_user_id = @user_id,
    updated_by_user_id = @user_id, updated_at = now(), version = version + 1
WHERE id = @id;

-- name: RestoreReservation :exec
UPDATE reservations
SET status = 'BOOKED', cancel_reason = NULL, cancelled_at = NULL, cancelled_by_user_id = NULL,
    no_show_at = NULL, no_show_by_user_id = NULL,
    updated_by_user_id = @user_id, updated_at = now(), version = version + 1
WHERE id = @id;

-- name: ExtendReservationHold :exec
UPDATE reservations
SET hold_extended_min = hold_extended_min + @minutes,
    updated_by_user_id = @user_id, updated_at = now(), version = version + 1
WHERE id = @id;

-- name: SetReservationSeated :exec
UPDATE reservations
SET status = 'SEATED', table_ids = @table_ids::uuid[], order_id = @order_id, seated_at = now(), seated_by_user_id = @user_id,
    updated_by_user_id = @user_id, updated_at = now(), version = version + 1
WHERE id = @id;

-- Not an edit: no version bump, so an open edit dialog isn't made stale.
-- name: MarkReservationConfirmed :exec
UPDATE reservations SET confirmed_at = now() WHERE id = $1;

-- name: MarkReservationReminded :exec
UPDATE reservations SET reminded_at = now() WHERE id = $1;

-- Booking history per customer (derived; nothing stored on the customer).
-- name: CustomerBookingStats :many
SELECT customer_id,
       count(*)::int AS booking_count,
       count(*) FILTER (WHERE status = 'NO_SHOW')::int AS no_show_count,
       coalesce(max(coalesce(seated_at, starts_at)) FILTER (WHERE status = 'SEATED'), 'epoch'::timestamptz)::timestamptz AS last_visit_at
FROM reservations
WHERE customer_id = ANY(@ids::uuid[])
GROUP BY customer_id;

-- name: ListBookingCustomers :many
SELECT id, name, phone, notes FROM customers ORDER BY lower(name), created_at, id;

-- name: OpenOrdersOnTables :many
SELECT id, table_id, status FROM orders
WHERE status IN ('OPEN', 'BILLED') AND table_id = ANY(@table_ids::uuid[]);
