#!/usr/bin/env bash
# Run the API locally. Migrations auto-apply on startup; the first owner is seeded
# from SEED_OWNER_* in api/.env when the database has no users. Reads api/.env.
set -euo pipefail
cd "$(dirname "$0")/../api"

if [ ! -f .env ]; then
  echo "api/.env is missing. Copy api/.env.example to api/.env and set DATABASE_URL." >&2
  exit 1
fi

echo "→ Starting API (migrations run on boot). Ctrl-C to stop."
exec go run ./cmd/api
