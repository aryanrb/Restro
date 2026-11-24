# Backend Implementation Plan — Go API (MVP)

Date: 2026-11-23 · Status: **draft for review** · Owner: backend
Source of truth: `docs/superpowers/specs/2026-11-23-restaurant-mvp-design.md`.
Research: `docs/research/01-ird-ebilling.md`, `02-nepal-payments.md`, `04-offline-sync.md`.

This plan covers the Go backend only (`proto/`, `api/`, `deploy/`). It is ordered so
the core loop (menu → order → kitchen → bill → payment → shift) is usable first.
Each task is small enough to review in one sitting and names its tests.

**Contract-first.** The `.proto` files for steps 2–4 are drafted in this change
(`proto/restaurant/v1/*.proto`) plus a WebSocket event catalogue
(`proto/websocket-events.md`). Agree them with Bishal before implementing; he generates
TS clients and builds against them while the Go side is written against tests.

---

## A. Decisions locked by the backend (were "left to the builder")

These were explicitly delegated in the handoff. Stated here so review can challenge them.

| Area | Decision | Why |
|---|---|---|
| Migrations | **Tern v2**, SQL files embedded via `go:embed`, auto-applied on boot | Matches the team boilerplate; no runtime tool needed in the container |
| DB access | **pgx/v5 + pgxpool + sqlc** (`pgx/v5` driver mode) | Team convention; typed queries, no ORM |
| HTTP mux | **stdlib `net/http.ServeMux`** (Go 1.25 routing), Connect handlers mounted on it; WS and `/healthz` as plain handlers | Connect needs no framework. `ponytail:` dropped Echo from the boilerplate — it buys nothing for Connect-only services; add back only if we grow REST/DTO endpoints |
| RPC | **connect-go** handlers + **protovalidate** interceptor (validates every request from the `.proto` rules before the handler runs) | Handoff requirement |
| Error mapping | A service-layer error type carrying a `restaurant.v1.ErrorReason`; one interceptor renders it as a `connect.Error` with an `ErrorInfo` detail | Clients branch on `reason`, not message text (handoff requirement) |
| Password hashing | **argon2id** (`golang.org/x/crypto/argon2`), per-hash salt, tuned params in settings | Stronger default than bcrypt; no cost to us |
| PIN hashing | argon2id as well; low entropy is covered by **device enrolment + lockout**, not by the hash | PINs are 4–6 digits; the hash can't save them, the lockout does |
| WebSocket | **`coder/websocket`**, one in-process hub, broadcast pokes | Handoff suggestion; one process per restaurant needs no broker |
| BS↔AD dates | **Vendored month-length table + a ~120-line converter**, with a golden test set checked against an authoritative Nepali calendar. No dependency on an unaudited lib's correctness | Compliance-critical; must be provably right (research 01) |
| CSV / Excel | stdlib `encoding/csv`; **`xuri/excelize/v2`** for `.xlsx` | Maintained; Annex-5/6 exports need real xlsx |
| PDF | **`go-pdf/fpdf`** (maintained fork), embedding Noto Sans Devanagari for Nepali | Sales-book/bill PDF; Devanagari must render |
| Tests | Pure table-driven unit tests for money/tax/BS-date; **integration tests against a throwaway Postgres (`testcontainers-go`)** for repositories, numbering concurrency and billing; golden-file tests for invoice/report rendering | Money and numbering bugs are unacceptable; they need a real DB |
| Idempotency store | `processed_commands(command_id uuid pk, user_id, response bytes, created_at)`; replay returns the stored response bytes verbatim; pruned after 7 days | research 04 |
| Invoice immutability | Enforced in the **database** (a trigger blocks `UPDATE`/`DELETE` on `invoices`/`invoice_lines` except the allowlisted `status`, `cancel_*`, `print_count` columns), not only in app code | research 01 §"Must have on day one" #1 |
| Audit log | Append-only table, written in the same tx as the change. `ponytail:` plain append-only for MVP; hash-chaining (research 01 #5 "ideally") deferred to the CBMS phase — named as debt, not silently dropped |

## B. Contract handshake — answers to the web's request list

From `web-01` "What the web needs from the backend developer" and `web-02` Task 2 Step 5.
All of these are satisfied by the drafted protos.

1. **Auth contract** → `proto/restaurant/v1/auth.proto`. `GetSession` never errors when
   logged out (`Session.user` is optional). Password login, PIN picker + PIN login,
   logout, `SetLanguage` all present.
2. **Shared-device security** → **Adopting the web's suggestion.** PIN login works only on a
   device an OWNER/MANAGER enrolled (`EnrollDevice`); `ListPinUsers` returns
   `DEVICE_NOT_SHARED` otherwise. PIN locks after **5** wrong tries; `ResetPin` clears it.
3. **Error reasons** → `restaurant.v1.ErrorReason` (common.proto), sent as an `ErrorInfo`
   detail. Covers invalid credentials, PIN locked, device not shared, plus the later set
   (stale version, 86'd, manager approval, order closed, shift not open, duplicate…).
4. **URLs** → Connect base `/api`, health `/api/healthz`, WebSocket `/api/ws`, same origin.
   (Note: health path is `/api/healthz`; web assumed `/api/health` — pick one, see Q-list.)
5. **Web sessions** → opaque token in an httpOnly, SameSite=Lax cookie on the same origin.
6. **WebSocket format** → `proto/websocket-events.md`; matches `{"type","id","version?"}`.
7. **PIN length** → **4–6 digits** (design §7 wins over the web's fixed-4 assumption). The
   PIN pad must support a confirm button, not 4-digit auto-submit. Flagged to Bishal.
8. **Admin services** → `settings.proto`, `floor.proto`, `menu.proto`, `auth.proto`
   (StaffService). `version` is on Settings, MenuItem and ModifierGroup; reasons
   STALE_VERSION / NOT_ALLOWED / IN_USE / DUPLICATE / INVALID carry a `field`.

---

## Step 1 — Skeleton (detailed)

Goal: a running Connect server with health check, migrations auto-applied on boot,
`buf generate` producing Go, and CI green. No business logic yet.

### Task 1.1 — Repo + Go module + Makefile
- **Files:** `api/go.mod` (module `github.com/<org>/restaurant/api`), root `Makefile`
  (`gen`, `dev`, `test`, `migrate`, `tidy`), `api/.gitignore`, `README` stub.
- **Test:** `make tidy && go build ./...` succeeds (empty `cmd/api/main.go` printing a version).

### Task 1.2 — buf module + codegen for Go and TS
- **Files:** `proto/buf.yaml` (done), `proto/buf.gen.yaml` (Go → `api/gen`, local
  `protoc-gen-go` + `protoc-gen-connect-go`, `paths=source_relative`), `Makefile` `gen`.
- **Test:** `buf lint` passes on all protos; `make gen` produces `api/gen/restaurant/v1/*.connect.go`
  and `*.pb.go`; `go build ./...` still green. (Web runs `buf generate ../proto` with its own
  `buf.gen.yaml` — verified once Bishal pulls.)

### Task 1.3 — Config + DB pool + Tern migrations on boot
- **Files:** `api/internal/config` (env: `DATABASE_URL`, `PORT`, `RUNTIME_ENV`, `SESSION_COOKIE_*`),
  `api/internal/db/db.go` (pool + `RunMigrations` from `go:embed migrations/*.sql`),
  `api/db/migrations/001_init.sql` (just `schema_version` smoke: a `health_check` table or no-op).
- **Test:** integration test spins a Postgres (testcontainers), runs `RunMigrations` twice,
  asserts idempotent and the pool pings.

### Task 1.4 — Connect server wiring + health + protovalidate + error interceptor
- **Files:** `api/internal/server` (ServeMux, mounts services — none yet — WS stub, `/api/healthz`),
  `api/internal/connecterr` (map service error → `connect.Error` + `ErrorInfo`),
  `api/internal/validate` (protovalidate interceptor), `api/cmd/api/main.go`
  (zap logger, graceful shutdown).
- **Tests:** `GET /api/healthz` → 200 `{"status":"ok"}`; the protovalidate interceptor
  rejects a deliberately invalid request with `invalid_argument` (wire a throwaway echo RPC
  behind a build tag, or test the interceptor in isolation).

### Task 1.5 — docker-compose (dev) + CI
- **Files:** `deploy/docker-compose.dev.yaml` (postgres + api), `.env.example`,
  `api/Dockerfile` (distroless), `.github/workflows/ci.yaml` (`make gen` drift check,
  `go test ./...`, `buf lint`, `buf breaking` against `main`).
- **Tests:** CI runs on a scratch branch and is green; `docker compose up` serves `/api/healthz`.

---

## Step 2 — Auth, users, sessions, PIN, manager override, audit foundation
Contract: `auth.proto`. Each task writes its audit-log entry in the same tx.

### Task 2.1 — Schema: users, sessions, devices, audit_log, processed_commands
- **Files:** migration `002_auth.sql` (users, sessions, devices, pin_failures, audit_log,
  processed_commands), sqlc queries.
- **Tests:** migration up/down; unique username; audit_log is insert-only (trigger test).

### Task 2.2 — Password hashing + session store
- **Files:** `internal/auth/hash` (argon2id hash+verify), `internal/auth/session` (create/lookup/revoke).
- **Tests:** hash round-trips; wrong password fails; session lookup after expiry fails;
  argon2 params decode from the stored hash string.

### Task 2.3 — AuthService: GetSession, PasswordLogin, Logout, SetLanguage
- **Files:** `internal/auth` service + handler; cookie writer; audit `login`/`logout`.
- **Tests:** login sets cookie + session row + audit row (one tx); `GetSession` logged-out
  returns `user:null` and does **not** error; wrong password → `unauthenticated` **with**
  reason `INVALID_CREDENTIALS` (so clients don't treat it as session-expired).

### Task 2.4 — Device enrolment + PIN login + lockout
- **Files:** `EnrollDevice`, `ListPinUsers`, `PinLogin`; device cookie/token; `pin_failures` counter.
- **Tests:** `ListPinUsers` on un-enrolled device → `DEVICE_NOT_SHARED`; 5 wrong PINs →
  `PIN_LOCKED` and the 6th correct PIN still locked; `ResetPin` clears it; inactive user
  can't PIN-login and isn't listed.

### Task 2.5 — StaffService + manager override check
- **Files:** `ListStaff`, `SaveStaff`, `ResetPin`, `CheckManagerPin`; role rules.
- **Tests:** manager can't create/edit OWNER/MANAGER (`NOT_ALLOWED`); nobody demotes/deactivates
  self; last active owner protected; duplicate username → `DUPLICATE` field=`username`;
  `CheckManagerPin` ok for active manager, `MANAGER_APPROVAL_INVALID` otherwise; every write audited.

### Task 2.6 — Auth middleware (role gate) + audit helper
- **Files:** interceptor that loads the session, exposes the actor, and enforces per-RPC role;
  a `withAudit(tx, actor, action, before, after)` helper used by every mutating service.
- **Tests:** an RPC requiring OWNER rejects a WAITER with `permission_denied`/`NOT_ALLOWED`;
  the audit helper writes exactly one row inside the caller's tx and rolls back with it.

---

## Step 3 — Settings, floor, stations, menu
Contracts: `settings.proto`, `floor.proto`, `menu.proto`. All writes OWNER/MANAGER, audited,
emit a WS poke. Optimistic `version` where the proto has it.

### Task 3.1 — Settings schema + Get/Update
- **Tests:** seed defaults (VAT 13%, prices include VAT, SC off, ESTIMATE, 80mm); `UpdateSettings`
  bumps version; stale version → `STALE_VERSION`; `billing_mode` is not settable via the API;
  PAN must be blank or 9 digits.

### Task 3.2 — Floor: areas + tables (zero tables allowed)
- **Tests:** save/move/delete area (`IN_USE` when it has tables); table seats 1–50; deactivate
  all tables is allowed; move at the ends is a no-op; `table.updated` emitted.

### Task 3.3 — Stations
- **Tests:** can't delete the last station or one an item uses (`IN_USE`); rename works.

### Task 3.4 — Menu: categories, items, variants, modifier groups
- **Tests:** item with variants ignores base price but needs each variant priced; without
  variants base price > 0; delete category/modifier-group `IN_USE` guards; `MoveItem` ordering;
  stale item/group save → `STALE_VERSION`; color must be in the palette.

### Task 3.5 — 86 toggle + item image upload
- **Tests:** `Set86([a,b], true)` sets both and emits `item.86d` per id; image > 1 MB or wrong
  type → `INVALID` field=`image`; a valid image stores and returns `image_url`.

---

## Step 4 — Orders + WebSocket hub
Contract: `order.proto`. The heart of the offline/idempotency model (research 04).

### Task 4.1 — WebSocket hub
- **Files:** `internal/realtime` hub (register/unregister/broadcast), `/api/ws` handler (auth'd upgrade).
- **Tests:** a client receives a broadcast poke; a slow/closed client is dropped without blocking
  the hub; unauthenticated upgrade rejected.

### Task 4.2 — Order schema + idempotency plumbing
- **Files:** migration (orders, order_items with snapshot columns, `client_id` uniques),
  a `runCommand(command_id, fn) → (response, replayed)` wrapper around `processed_commands`.
- **Tests:** same `command_id` twice runs `fn` once and returns identical bytes; concurrent
  duplicates don't double-apply (one wins, the other replays).

### Task 4.3 — CreateOrder + GetOrder + ListOpenOrders
- **Tests:** create with client id is idempotent; DINE_IN requires a table, TAKEAWAY must not
  have one; `GetOrder` by `id` or `client_id`; `order.updated` emitted.

### Task 4.4 — AddItems (snapshot + 86 partial accept + server price wins)
- **Tests:** price/name/variant/modifiers snapshotted from the server at add time; an 86'd line
  is rejected (`ITEM_86D` in `rejected`) while the rest apply; a stale menu price is ignored and
  the server price snapshotted; adds don't check `base_version`; replays are no-ops.

### Task 4.5 — SendItems (route to stations)
- **Tests:** DRAFT→SENT only; empty `item_ids` sends all DRAFT; `ticket.created` emitted per
  station touched; already-SENT lines unchanged.

### Task 4.6 — VoidItem (reason + manager approval tiers + optimistic lock)
- **Tests:** void DRAFT is free (no reason); void SENT needs a reason; void PREPARING/READY/SERVED
  needs `ManagerApproval` → `MANAGER_APPROVAL_REQUIRED` without it, `MANAGER_APPROVAL_INVALID` on
  a bad PIN; `base_version` mismatch → `STALE_VERSION`; the approver is recorded and audited.

### Task 4.7 — MarkServed, TransferTable, MergeTables
- **Tests:** `MarkServed` READY/SENT→SERVED; transfer with stale version → `STALE_VERSION`,
  success bumps version and emits `table.updated` on both tables; merge moves live items
  append-only, closes the source, is idempotent; committing an order already CLOSED/BILLED → `ORDER_CLOSED`.

---

## Step 5 — Kitchen (KDS)
New proto `kitchen.proto` (agree before build): per-station ticket queries + status actions.

### Task 5.1 — KDS queries
- **Files:** `ListStationTickets(station_id)` returning open tickets with the oldest-first age.
- **Tests:** returns only SENT/PREPARING items for the station; served/voided excluded; ordering by age.

### Task 5.2 — KDS actions: start, ready, bump, 86-from-KDS
- **Tests:** SENT→PREPARING→READY transitions; `item.ready` emitted to waiters; bump marks the
  ticket done without changing item snapshots; 86 from KDS reuses `Set86` and emits `item.86d`.

---

## Step 6 — Billing (IRD-critical)
New `billing.proto`. **This is where the IRD rules bite — see the open questions.**

### Task 6.1 — BS↔AD date library + fiscal-year resolver
- **Tests (golden):** a table of known BS↔AD pairs across several years converts both ways;
  Shrawan-1 rollover picks the right FY; rejects out-of-range dates. *Must pass before any
  numbering work.*

### Task 6.2 — Invoice schema + immutability trigger + counter table
- **Files:** `invoices`, `invoice_lines`, `invoice_counters(fy, mode) UNIQUE`, `cbms_outbox`,
  a trigger blocking UPDATE/DELETE except allowlisted columns.
- **Tests:** trigger rejects editing a line and deleting an invoice; allows `print_count`/`status`
  updates; counter row locks with `SELECT … FOR UPDATE`.

### Task 6.3 — Totals engine (money/VAT/service charge)
- **Tests (pure, table-driven):** subtotal − discount + SC → VAT=13/113 of taxable → total, all
  integer paisa, round half-up once at invoice level; SC off by default; non-VAT path has no VAT
  line; `round_total_to_rupee` adds an explicit rounding line. Fixtures reviewed against §8 Tax.

### Task 6.4 — Issue invoice (whole bill) + gap-free numbering in one tx
- **Tests (integration + concurrency):** two concurrent issues against the same (FY,mode) get
  consecutive gap-free numbers, no duplicates (hammer with N goroutines); ESTIMATE and TAX series
  never interleave; `cbms_outbox` row written `DISABLED`; audit + `invoice.issued` emitted;
  order → BILLED/CLOSED.

### Task 6.5 — Split by item (N invoices) and split equally (1 invoice, N payments)
- **Tests:** split-by-item produces N invoices whose lines partition the order; split-equally is
  one invoice with N payment rows summing to the total (remainder allocation rule pinned by test).

### Task 6.6 — Discounts (role-limited) + PaymentProvider interface
- **Files:** `internal/billing/payment` with `PaymentProvider` (cash, card-recorded,
  Fonepay static QR manual-confirm); discount limits per role + manager override.
- **Tests:** a cashier discount beyond the limit without approval → `MANAGER_APPROVAL_REQUIRED`;
  cash change computed; QR payment starts `awaiting_confirmation` then confirms with actor recorded.

### Task 6.7 — Cancellation (reopens order) + print/reprint tracking + counter pay-first
- **Tests:** cancel needs reason + manager approval, flips status to CANCELLED (never deletes),
  reopens the lines' order to OPEN, audited; first print = "original", next = "Copy of Original (n)";
  print count only ever increments; counter pay-first issues before sending to stations.

---

## Step 7 — Shifts
New `shift.proto`.
### Task 7.1 — Shift schema + open/close
- **Tests:** several shifts open at once (one per cashier); taking payment with no open shift →
  `SHIFT_NOT_OPEN`; payment attaches to the collector's open shift; expected cash by method computed.
### Task 7.2 — Close reconciliation + post-close cancellation adjustment
- **Tests:** counted − expected = difference recorded; an invoice cancelled after its shift closed
  books the adjustment into the canceller's current shift.

---

## Step 8 — Reports
New `report.proto`. Read-only aggregations + exports.
### Task 8.1 — Live dashboard figures — **Tests:** today's sales, open tables/orders, void/discount counts, top items match seeded fixtures.
### Task 8.2 — Daily sales + IRD sales book (Annex-5/6 shape, BS dates) — **Tests:** row shape matches Annex 5 fields; BS dates correct; cancelled rows flagged.
### Task 8.3 — CSV/Excel/PDF export — **Tests:** CSV parses back to the same rows; xlsx opens; PDF renders Devanagari (golden byte-size/҂text-extract check).
### Task 8.4 — Cancelled-invoice report, shift reports, audit-log query — **Tests:** filters by date/actor/action; cancelled report lists reason + approver.

---

## Step 9 — Deploy
### Task 9.1 — Production compose + Caddy — **Tests:** `docker compose -f deploy/docker-compose.yaml up` serves the web behind Caddy TLS (staging cert) and proxies `/api`.
### Task 9.2 — Nightly `pg_dump` + off-box copy — **Tests:** the backup script produces a restorable dump; a scripted restore into a fresh DB reproduces row counts (the "tested once" restore drill, documented).

---

## C. Open questions (don't assume — need a human)

**Flagged to the partner / accountant / IRD** (block billing, step 6, not steps 1–4):

1. **Tax order-of-operations & rounding** — confirm with an accountant: VAT = 13/113 of
   (subtotal − discount + service charge), round half-up **once** at invoice level. Research 01
   found *no official IRD rounding rule*. I will implement the design's §8 rule but it needs sign-off.
2. **2082 Electronic Billing Procedure** — research 01's biggest caveat: the current procedure
   (2082) couldn't be read. Someone must read it before we finalise numbering/fields/CBMS. Do the
   Annex-5/6 field names still hold?
3. **Estimate-mode pilot** — will the local IRO accept "Estimate — not a tax invoice" printouts
   during the pilot? Affects whether we can launch before enlistment.
4. **Service charge legality** — struck down by the Supreme Court (research 01 §5). Kept off by
   default and configurable. Confirm it's acceptable to even expose the toggle.

**Backend-shaping product decisions — RESOLVED 2026-11-23:**

- **Manager override transport** → **inline PIN on the command** (`ManagerApproval` embedded in
  void/discount/cancel). Server validates and records the approver in the same tx. Already in the protos.
- **Partial billing / order lifecycle** → **per-line / partial-capable.** An order tracks billing
  state per line; split-by-item can invoice some lines while the rest stay OPEN and addable; the
  order closes when every line is billed+paid. Step 6 schema: `order_items.invoice_id` nullable +
  a per-line billing status, and `ORDER_STATUS_BILLED` means "all lines invoiced." The cancel path
  (reopen those lines to OPEN) relies on this. No change to the step 2–4 protos.
- **Shared-device PIN enrolment + lockout** → **adopt it.** PIN login only on enrolled shared
  devices; lock after 5 wrong tries. Already in `auth.proto` (`EnrollDevice`) and §B #2.
- **Health path & PIN length** (my call, no ambiguity): health is **`/api/healthz`** (tell Bishal to
  use it, not `/api/health`); PIN is **4–6 digits** per design §7 (web's fixed-4 pad needs a confirm button).
