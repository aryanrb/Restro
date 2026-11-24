#!/usr/bin/env bash
# Run all Go tests, including DB integration tests against a local throwaway DB.
# Creates restro_test locally if missing. Tests TRUNCATE tables — never point
# TEST_DATABASE_URL at a real database.
set -euo pipefail
cd "$(dirname "$0")/../api"

createdb restro_test 2>/dev/null || true
export TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://$(whoami)@localhost:5432/restro_test?sslmode=disable}"

echo "→ go test -p 1 ./...   (TEST_DATABASE_URL set to a local throwaway)"
go test -p 1 ./...
