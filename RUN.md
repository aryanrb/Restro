# Running the backend

**Migrations run automatically on startup** (`main.go` → `db.RunMigrations`), and the
first owner is seeded from `SEED_OWNER_*` when the database has no users. You never
run migrations by hand — just start the app against an empty (or existing) database.

Default login after first boot: **owner / owner123** (PIN **1111**). Change these in
`api/.env` (local) or the compose file (Docker) before production.

## Option A — Local (Go on host, any Postgres)

Uses `api/.env` for `DATABASE_URL` (currently your Neon cloud DB).

```bash
make run              # = ./scripts/dev.sh  → go run ./cmd/api
# watch for: "migrations applied"  then  "api started"
curl localhost:8080/api/healthz          # {"status":"ok"}
```

To run against a **local** Postgres instead, point `DATABASE_URL` in `api/.env` at it,
e.g. `postgres://$(whoami)@localhost:5432/restro?sslmode=disable`, create the DB once
(`createdb restro`), then `make run`. For a clean slate later: `make reset-db name=restro`.

## Option B — Full stack in Docker (one command)

Brings up Postgres + API together; migrations and the owner seed run on boot.

```bash
make up               # docker compose -f deploy/docker-compose.yaml up --build
curl localhost:8080/api/healthz
make down             # stop
```

## Option C — Dockerized Postgres, API on host

```bash
make dev              # starts Postgres in Docker, then `go run` on the host
```

## Tests

```bash
make test             # creates a local throwaway DB (restro_test) and runs everything
```

Integration tests `TRUNCATE` tables — `TEST_DATABASE_URL` must be a throwaway DB,
never your real one. `scripts/test.sh` defaults it to local `restro_test`.

## Common tasks

| Command | What it does |
|---|---|
| `make run` | Run the API locally (migrations auto-apply) |
| `make up` / `make down` | Full Docker stack up / down |
| `make test` | All tests against a local throwaway DB |
| `make reset-db name=restro` | Drop & recreate a local DB |
| `make migrate` | Apply migrations manually against `$DATABASE_URL` (rarely needed) |
| `make new name=add_x` | Scaffold a new migration |
| `make gen` | Regenerate proto + sqlc code |

## Smoke-test the full loop

With the server running on `:8080`:

```bash
python3 - <<'PY'  # or see scripts in your scratchpad
import urllib.request,json,http.cookiejar
# login -> create order -> bill -> pay ... (see the smoke script)
PY
```

The quickest check is the health endpoint plus a login:

```bash
curl -s -c /tmp/ck -X POST localhost:8080/api/restaurant.v1.AuthService/PasswordLogin \
  -H 'Content-Type: application/json' -d '{"username":"owner","password":"owner123"}'
```
