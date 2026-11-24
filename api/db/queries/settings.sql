-- name: GetSettings :one
SELECT * FROM settings WHERE id = 1;

-- name: UpdateSettings :one
UPDATE settings SET
    version = version + 1,
    name_en = $2, name_ne = $3, address = $4, phone = $5, pan_number = $6,
    vat_registered = $7, prices_include_vat = $8, vat_rate_bp = $9,
    service_charge_enabled = $10, service_charge_rate_bp = $11,
    receipt_width = $12, round_total_to_rupee = $13, default_language = $14,
    kds_yellow_minutes = $15, kds_red_minutes = $16, updated_at = now()
WHERE id = 1 AND version = $1
RETURNING *;
