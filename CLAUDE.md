# CLAUDE.md — Restaurant POS backend

Context for AI agents working in this repo. Go API for a single-restaurant POS
(Nepal). One deployment per restaurant — **no `restaurant_id` columns, no tenancy.**

## Stack
Go 1.26 · **ConnectRPC** (connect-go) over **Echo** · PostgreSQL 17 · pgx/v5 ·
**sqlc** · **Tern** migrations (embedded, auto-applied on boot) · **buf** +
**protovalidate** · zap · argon2id · coder/websocket.

This follows the One Accord boilerplate (handler→service→db split, sqlc, Tern, zap,
typed errors, graceful shutdown) with ConnectRPC as the transport instead of REST:
the `proto/` contract is the source of truth for both frontends (web + mobile).

## Layout (do not flatten)
```
proto/                         # buf module: the API contract (.proto) + websocket-events.md
  restaurant/v1/*.proto
buf.gen.yaml                   # proto -> api/gen (Go), web generates its own TS
api/
  cmd/api/main.go              # wiring + graceful shutdown; seeds first owner from env
  internal/
    config/                    # env-loaded Config
    db/                        # pgx pool + Tern runner; db/sqlc/ is GENERATED
    connecterr/                # service errors -> connect.Error + machine-readable ErrorInfo
    realtime/                  # in-process websocket hub (server->client pokes only)
    server/                    # Echo bootstrap, middleware, mounts Connect services under /api
    <domain>/                  # auth, (later) settings, menu, floor, orders, kitchen, billing, ...
      service.go               #   business logic: owns rules, DB access, errors, audit
      handler.go               #   thin Connect impl: proto <-> service, cookies
      middleware.go            #   (auth) session-loading interceptor
  db/migrations/*.sql          # Tern (up ---- create above / drop below ---- down)
  db/queries/*.sql             # sqlc source
  gen/                         # buf-generated Go (committed)
  sqlc.yaml
deploy/                        # docker-compose, Caddy (later)
```

## Conventions
1. **Contract first.** New feature → write/agree the `.proto` in `proto/restaurant/v1/`,
   `make gen`, then implement. The generated Go service interface drives the handler.
2. **Handler/service/db split.** Handlers parse the proto request, call the service,
   convert the result back to proto, set cookies. No business logic or DB in handlers.
   Services own rules, DB access and errors. No SQL in Go — only via sqlc.
3. **sqlc only.** Add queries to `db/queries/*.sql`, run `make gen`. Never edit `internal/db/sqlc/`.
4. **Money is integer paisa; rates are basis points.** Totals/VAT/service-charge are
   computed server-side only. Round half-up once, at invoice level.
5. **Errors.** Services return `*connecterr.Error` (constructors in `internal/connecterr`),
   which the interceptor renders as a `connect.Error` + a machine-readable `ErrorReason`
   detail. Clients branch on the reason (`STALE_VERSION`, `ITEM_86D`, `MANAGER_APPROVAL_REQUIRED`…),
   never on message text. A bare `Unauthenticated` carries no reason (→ web redirects to login).
6. **Audit in the same transaction.** Any void/discount/cancel/reprint/menu-or-price
   change/86/login/settings change writes an `audit_log` row inside the same tx (use
   `pool.Begin` + `q.WithTx(tx)`). Never a second connection.
7. **Optimistic locking.** Mutable aggregates have a `version`; destructive/structural
   commands carry `base_version` and are rejected with `STALE_VERSION` on mismatch.
8. **Idempotency.** Order commands carry a client UUID recorded in `processed_commands`;
   retries return the original result.
9. **Immutability.** Issued invoices never UPDATE/DELETE except status→CANCELLED and
   print_count — enforced by a DB trigger, not only app code.
10. **Logging.** Structured zap only. Log at boundaries (service error paths), not hot loops.
11. **Migrations.** One concern per file; always write a working `down`; never edit a
    merged migration — add a new one. sqlc reads the up DDL (it ignores the down section).
12. **Config.** New env var → add to `api/.env.example` in the same change.

## Add an RPC
1. Add the message/method to the relevant `proto/restaurant/v1/*.proto`; `make gen`.
2. Add logic to `internal/<domain>/service.go`.
3. Implement the generated handler method in `internal/<domain>/handler.go`.
4. Mount the service in `internal/server/server.go` (once per service).
5. Add a test.

## Add a table
1. `make new name=add_<entity>_table`, edit up + down.
2. Add queries to `db/queries/<entity>.sql`; `make gen`.
3. `make dev` (or a test) applies it — migrations also auto-apply on boot.

## Testing
`make test` (= `go test -p 1 ./...`). Pure logic (money/VAT/BS-date, hashing) → fast
table-driven unit tests. DB-backed behaviour → integration tests gated by
`TEST_DATABASE_URL` (they `t.Skip` without it); `-p 1` serializes them so packages
don't race on the shared database. CI runs both against a Postgres service.

## Real-time
One websocket endpoint `/api/ws`, in-process hub, server→client pokes only
(`{"type","id","version"}`). No mutations over the socket — clients refetch via
Connect on every event and on reconnect. Catalogue: `proto/websocket-events.md`.
