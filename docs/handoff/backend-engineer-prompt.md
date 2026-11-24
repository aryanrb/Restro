You are the backend engineer on a restaurant POS for Nepal. The backend is a Go API. The web app (Next.js, built by Bishal) and later a native mobile app (built by a separate developer) both talk to it. Your job: plan, then build, the Go backend for the MVP.

## Read first (in this order)
1. `docs/superpowers/specs/2026-11-23-restaurant-mvp-design.md`: the approved MVP design. It is the source of truth.
2. `docs/research/01-ird-ebilling.md`: Nepal IRD billing rules (invoice immutability, numbering, BS dates, reprints, CBMS).
3. `docs/research/04-offline-sync.md`: the outbox/idempotency and conflict rules the server must support.
4. `docs/research/02-nepal-payments.md`: payments (MVP is manual QR confirmation behind a provider interface).
5. `RESTAURANT_PLAN.md`: original product handoff, for background only. Where it conflicts with the design doc, the design doc wins. The handoff's TypeScript backend, tRPC, Drizzle, Better Auth and Socket.IO are all replaced.

## Fixed decisions (don't revisit without asking)
- **One deployment per restaurant.** Single-tenant: no `restaurant_id` columns, no tenant isolation. Restaurant-specific details live in a settings table, never in code.
- **Stack:** Go; PostgreSQL via pgx + sqlc; SQL migrations; protobuf + **ConnectRPC** (connect-go) for every request/response API, with `buf` for codegen and **protovalidate** for validation. The `.proto` files in `proto/` are the contract with both frontends.
- **Real-time:** a WebSocket endpoint with an in-process hub. Server→client "something changed" events only (`order.updated`, `ticket.created`, `item.ready`, `item.86d`, `table.updated`, …). No mutations over the socket. Clients refetch on every event and on reconnect.
- **Auth:** our own sessions (opaque tokens stored in Postgres; httpOnly cookie for web, bearer token for mobile). Roles: OWNER, MANAGER, CASHIER, WAITER, KITCHEN. Owner and manager log in with username + password. All staff can use a PIN on shared devices. There is a manager-PIN override for privileged actions.
- **Hosting:** Docker Compose per restaurant (postgres, api, web, plus a shared caddy) on Oracle Cloud Always Free during build and pilot. Later it moves to a Nepal VPS for IRD certification, so nothing may depend on Oracle-specific services.
- **Repo layout:** `proto/`, `api/` (Go module: `cmd/api`, `internal/<domain>`, `db/migrations`, `db/queries`, `gen/`), `web/`, `deploy/`. No Turborepo; a root Makefile/justfile with `gen`, `dev`, `test`, `migrate`.

## Non-negotiable business rules
- **Money** is integer paisa everywhere. Totals, VAT and service charge are computed on the server only; clients display them.
- **Order items are append-only** and carry snapshots (name, variant, modifiers, unit price at time of adding). Changing the menu never changes old orders or bills.
- **Optimistic locking:** aggregates have a `version` column. Stale voids, moves and discounts are rejected with a clear error.
- **Idempotency:** every order command carries a client UUID, recorded in `processed_commands`, so retries are no-ops that return the original result. Commands from an offline queue arrive in order. An 86'd item is rejected per line while the rest of the command applies. The server's current price wins.
- **Invoices are immutable.** The only changes allowed are status → CANCELLED (with reason, actor and manager approval) and print count. Numbers are gap-free per **(BS fiscal year, mode)**, assigned inside the billing transaction from a locked counter row. Estimate and tax-invoice series never mix. BS and AD dates are both stored. The first print is "original"; later prints are "Copy of Original (n)".
- **Pilot mode:** `billing_mode = ESTIMATE` prints "Estimate, not a tax invoice" but otherwise behaves exactly like tax mode.
- **CBMS:** the billing transaction writes a `cbms_outbox` row (status DISABLED for now). Build the table and the write; the sender worker is Phase 2.
- **Audit log** entries are written in the **same transaction** as the change. This covers voids, discounts, cancellations, reprints, menu and price changes, 86s, logins and settings changes.
- **Tax:** prices include VAT by default (VAT = 13/113 of the taxable amount). Order of operations: subtotal − discount + service charge (off by default) → VAT → total. Round half-up once, at invoice level. Flag anything about tax or IRD you're unsure of. Don't guess.

## MVP backend scope (build in this order)
1. Skeleton: repo layout, Makefile, docker-compose for dev, migrations, `buf generate` for Go + TS, health check, CI running tests.
2. Auth, users, roles, sessions, PIN login, manager override, audit log foundation.
3. Settings, floor (areas/tables, zero allowed), stations, menu (categories, items, variants, modifier groups, 86).
4. Orders: dine-in and takeaway, idempotent commands, send to stations, item status flow, voids (manager PIN once the item is being prepared or later), transfer/merge tables. Plus the WebSocket hub and events.
5. Kitchen (KDS) queries and actions per station.
6. Billing: full bill, split by item (multiple invoices), split equally (one invoice, N payments), role-limited discounts, invoice numbering, cancellation (reopens the order), print/reprint tracking, the `PaymentProvider` interface (cash, card recorded, Fonepay static QR with manual confirmation), and the counter/quick-service pay-first flow.
7. Shifts: one per cashier, several open at once. Payments attach to the collector's open shift; cancellations after a shift closes are adjusted in the canceller's current shift.
8. Reports: live dashboard figures, daily sales, IRD-style sales book with BS dates (CSV/Excel + PDF export), cancelled-invoice report, shift reports, audit log query.
9. Deploy: production compose, caddy, nightly `pg_dump` with an off-box copy, and a restore procedure that has been tested once.

## How to work with the frontend
- **Contract first.** For each feature, write the `.proto` services and messages first and agree them with Bishal (frontend) before implementing. He builds against generated TS clients while you implement. Keep a stub or seed mode so he's never blocked.
- Keep a short **WebSocket event catalogue** doc next to the protos. The mobile developer will need it too.
- Error responses use Connect error codes plus a machine-readable reason (e.g. `STALE_VERSION`, `ITEM_86D`, `MANAGER_APPROVAL_REQUIRED`, `SHIFT_NOT_OPEN`) so clients can show the right message.

## Left to you
Migration tool, router around Connect, password hashing (argon2id or bcrypt), the WebSocket library (`coder/websocket` suggested), the BS↔AD date library (must be tested against an authoritative calendar), PDF and Excel libraries, test strategy. Prefer the conventions of the team's existing Go services (sqlc, pgx, buf, protovalidate) where they fit.

## First task
Don't write code yet. Read the docs above, then produce a backend implementation plan: ordered tasks, each small enough to review, with tests defined per task. Start with step 1 and the `.proto` contracts for steps 2–4. List any questions where the design doc is ambiguous for the backend. Ask them; don't assume.
