#!/usr/bin/env bash
# Drop & recreate a LOCAL Postgres database for a clean dev slate, then let the app
# re-apply migrations on next start. Usage: scripts/reset-db.sh [dbname]  (default: restro)
set -euo pipefail
db="${1:-restro}"
echo "→ Resetting local database '$db' (this deletes all its data)…"
dropdb --if-exists "$db"
createdb "$db"
echo "✓ '$db' recreated. Start the app (scripts/dev.sh) to apply migrations."
