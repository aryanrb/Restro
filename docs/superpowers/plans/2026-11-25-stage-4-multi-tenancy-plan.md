# Stage 4 — Multi-tenancy (one shared deployment)

Date: 2026-11-25 · Status: **draft for review** · Owner: backend
Source: `docs/handoff/BACKEND-ALIGNMENT-HANDOFF.md` §"Remaining work" item 4.
Prereq: stages 1–3 merged (the three pre-merge items in the handoff done first).

Today the app is **one deployment per restaurant** — CLAUDE.md states "no
`restaurant_id` columns, no tenancy." Stage 4 reverses that: one deployment serves
many restaurants under an organisation → restaurant hierarchy, isolated by Postgres
row-level security (RLS) behind app-level filtering. This plan turns the decided
approach into ordered, reviewable tasks and names the design questions that block
starting.

**Do not start coding before Step 0 is answered** — three of those questions
(staff scope, invoice-number backfill, login selection) change the schema.

## Progress

- **Step 0 decided:** staff scope = **org + `restaurant_staff` grant table** (0.1b);
  username unique per org (0.2); login auto-selects one restaurant / `SelectRestaurant`
  when many (0.3); backfill stamps existing rows with the one seeded restaurant, counters
  continue gap-free (0.4); org-owner self-serve `CreateRestaurant` (0.5); customer phone
  per-restaurant (0.7). 0.6 (export granularity) still open — needed before go-live only.
- **Chunk 1 — DONE (migration 015, tested up/down/up on a fresh DB, full suite green):**
  `organisations`, `restaurants`, `restaurant_staff` tables; one org + one restaurant
  seeded from the current settings name; `users.organisation_id` (NOT NULL, FK, backfilled);
  username unique moved to `(organisation_id, username)`; existing users granted their
  current role at the seeded restaurant; `SeedOwner` attaches the bootstrap owner to the
  seeded org + grants OWNER; `SaveStaff` carries the actor's org onto new users.
  - **Deferred (intentional, forward-compatible):** role checks still read `users.role`,
    not the grant (flip in chunk 3/auth); `SaveStaff` doesn't yet create a grant for new
    staff (no restaurant context on the actor until chunk 3 — nothing reads grants yet, so
    no functional gap; chunk 3 backfills any grant-less users); `restaurant_staff.role`
    currently mirrors `users.role`.
- **Next:** chunk 2 = tenant-column sweep (`restaurant_id` on the ~40 tenant tables) +
  rescope singletons/counters/uniques (Step 2); chunk 3 = RLS + GUC + auth/session context
  (Steps 3–4); then onboarding (Step 5).

---

## The key architectural bet (makes this tractable)

Scoping 40+ tables by hand — rewriting every sqlc query to add `WHERE restaurant_id
= $1` — would be a huge, error-prone diff. We avoid almost all of it:

1. Every tenant table gets `restaurant_id uuid NOT NULL DEFAULT
   current_setting('app.restaurant_id')::uuid`.
2. The app connects as a **non-owner role**; each request runs `SET LOCAL
   app.restaurant_id = <session's restaurant>` at the top of its transaction.
3. RLS policies (`USING restaurant_id = current_setting('app.restaurant_id')::uuid`,
   `FORCE ROW LEVEL SECURITY`) scope every SELECT/UPDATE/DELETE automatically; the
   column DEFAULT scopes every INSERT.

Result: **most existing queries are untouched.** RLS filters reads, the DEFAULT
fills inserts. Only queries that cross the tenant boundary on purpose (login,
onboarding, platform admin) run under a privileged path. App-level `restaurant_id`
filters stay as defence-in-depth on the hottest paths (handoff: RLS *behind*
app-level filtering), not everywhere.

`ponytail:` RLS does the scoping, not per-query edits — upgrade only the queries
that must cross tenants. If a query breaks under RLS, that's the signal it was
reaching across restaurants.

---

## Step 0 — Design decisions to lock first (blocking)

| # | Question | Options | Recommendation |
|---|---|---|---|
| 0.1 | **Staff scope** — can one person work at >1 restaurant? | (a) `users.restaurant_id`: one user = one restaurant; (b) users belong to an **org**, a `restaurant_staff(user_id, restaurant_id, role)` grant table maps access | **DECIDED (b)** — the goal is "multi-branch cheap" with shared staff. Schema supports shipping (a) first and adding the grant table later without a rewrite. |
| 0.2 | **Username uniqueness** | global, or per-org, or per-restaurant | **DECIDED per-org** — matches "one login, many branches". |
| 0.3 | **Restaurant selection at login** | auto (single restaurant) / pick-list after password / subdomain | **DECIDED: auto when one; a `SelectRestaurant` step when many.** Session stores the active restaurant. |
| 0.4 | **Invoice-number backfill** — existing single-restaurant invoices | stamp all with the one seeded restaurant; counters continue from current max | **DECIDED** — confirm with accountant that per-restaurant series continuity across the cutover stays gap-free and does not reset (IRD). |
| 0.5 | **Onboarding** — who creates restaurants | platform-admin only / org-owner self-serve | **DECIDED: org-owner self-serve `CreateRestaurant`**; a platform bootstrap seeds the first org + owner. |
| 0.6 | **Per-restaurant backup/export** granularity | filtered `pg_dump` per restaurant / logical export RPC | **OPEN** — decide before go-live; affects tenant-offboarding. |
| 0.7 | **Customer phone uniqueness** | per-restaurant vs per-org | **DECIDED per-restaurant** (a guest of branch A isn't automatically branch B's credit customer). |

---

## Step 1 — Org/restaurant schema + tenancy column + backfill
One migration, written to be safe on a live DB (add nullable → backfill → set NOT NULL).

- **New tables:** `organisations(id, name, …)`, `restaurants(id, organisation_id FK,
  name, …)`. If 0.1=(b): `restaurant_staff(user_id, restaurant_id, role, PRIMARY KEY
  (user_id, restaurant_id))`.
- **Tenant column:** add `restaurant_id uuid` (nullable first) to every tenant table
  (all tables except the globals below). Default clause
  `DEFAULT current_setting('app.restaurant_id', true)::uuid` added in Step 3 once the
  GUC exists.
- **Globals (no `restaurant_id`):** `organisations`, `restaurants`, `restaurant_staff`,
  `schema_baseline`. `users`/`sessions`/`devices`/`processed_commands` — scope per 0.1
  (see Step 4); these sit on the auth path before a restaurant is selected, so they get
  plain scope columns, not restaurant-RLS.
- **Backfill:** create one org + one restaurant, stamp all existing rows, then
  `ALTER … SET NOT NULL`.
- **Tests:** migration up/down; every tenant table has a NOT NULL `restaurant_id`
  after backfill (assert from `information_schema`); existing rows all point at the
  seeded restaurant.

## Step 2 — Rescope singletons, counters, and uniques
- **Config singletons** (`settings`, `alert_config`, `analytics_settings`): drop the
  `id = 1` PK, make `restaurant_id` the PK (one row per restaurant). Seed defaults per
  restaurant on `CreateRestaurant`.
- **Counters:** `invoice_counters` PK → `(restaurant_id, fiscal_year, mode)`;
  `repayment_counters` likewise. `AllocateInvoiceNumber` / repayment allocator take
  `restaurant_id` (or read the GUC).
- **Uniques to rescope:** `invoices UNIQUE(fiscal_year, mode, number)` →
  `(restaurant_id, …)`; `customers_phone_key` → partial unique on
  `(restaurant_id, phone)`; alert `alerts_open_group_idx` / `alerts_dedup_idx` /
  `alert_pushes_once_idx` → add `restaurant_id`; `users.username` per 0.2 (DONE, chunk 1);
  `shifts_one_open_per_cashier` already scoped via cashier (verify).
- **Tests:** two restaurants allocate invoice numbers independently and gap-free;
  same phone allowed in two restaurants; same username rejected per 0.2's scope.

## Step 3 — RLS + non-owner app role + per-tx tenant GUC
- **Migration:** create a non-owner app role; `GRANT` DML on tenant tables;
  `ALTER TABLE … ENABLE ROW LEVEL SECURITY; FORCE ROW LEVEL SECURITY;` on each tenant
  table; policy `USING (restaurant_id = current_setting('app.restaurant_id')::uuid)
  WITH CHECK (…)`. Add the `restaurant_id` column DEFAULT now that the GUC is defined.
- **Pool/db (`internal/db`, `internal/dbx`):** connect as the app role (migrations
  keep running as owner — owners bypass RLS, which is what the migrator needs). Add a
  tenant-scoped tx helper that runs `SET LOCAL app.restaurant_id = $1` before handing
  the querier to the service. A privileged helper (no GUC / owner role) for the
  cross-tenant paths in Step 4–5.
- **Tests:** a tx scoped to restaurant A cannot SELECT/UPDATE/INSERT restaurant B's
  rows (RLS denies); INSERT without an explicit `restaurant_id` lands in the GUC's
  restaurant; the privileged path sees all.

## Step 4 — Auth, session, device, context
- **`Actor`** (`internal/auth/context.go`) gains `RestaurantID` and `OrganisationID`.
- **Session** carries the active `restaurant_id`. `PasswordLogin`: resolve the user's
  restaurants (0.1); one → auto-select and set it on the session; many → return a
  selection step. New RPCs: `ListMyRestaurants`, `SelectRestaurant(restaurant_id)`
  (re-scopes the session). `GetSession` returns the active restaurant.
- **Interceptor** (`internal/auth/middleware.go`): after loading the session, read its
  `restaurant_id` and make the request tx set the GUC (Step 3 helper). Reject a session
  with no active restaurant on tenant RPCs with a new `NO_RESTAURANT_SELECTED` reason.
- **Role checks read the grant** (`restaurant_staff.role`) for the active restaurant,
  not `users.role`.
- **Devices** enrolled per restaurant; PIN login scoped to the device's restaurant.
- **`processed_commands`** keyed per restaurant (idempotency can't leak across tenants).
- **Tests:** multi-restaurant user must `SelectRestaurant` before tenant RPCs; wrong
  restaurant on a device → rejected; PIN user list is per-restaurant.

## Step 5 — Onboarding / seeding
- Replace `seedOwner` (single owner) with a bootstrap that seeds org + first restaurant
  + owner from env. Add `CreateRestaurant` (org-owner only): creates the restaurant,
  seeds its config singletons, owner gets a staff grant.
- **Tests:** bootstrap on empty DB creates one org/restaurant/owner; `CreateRestaurant`
  by a non-owner → `NOT_ALLOWED`; new restaurant starts with default settings and empty
  menu/floor.

## Step 6 — Per-restaurant backup/export (per 0.6)
- Filtered export (pg_dump with per-table `restaurant_id` predicate, or a logical
  export RPC) + a documented restore drill. `ponytail:` smallest thing that offboards a
  tenant; full self-serve export is later.

## Step 7 — Docs + config
- Rewrite CLAUDE.md's tenancy rule (it currently forbids `restaurant_id`); document the
  RLS + GUC pattern so new tables inherit it by default.
- New env: app DB role URL (non-owner) vs migrator URL (owner); add to `.env.example`
  (CLAUDE.md §12).

---

## Risks / notes
- **IRD numbering is compliance-critical.** The cutover (0.4) must not reset or gap a
  restaurant's invoice series. Hammer-test concurrent issue per restaurant *and* across
  restaurants.
- **Immutability trigger** (mig 005) must keep allowing only the allowlisted columns —
  verify it composes with the new `restaurant_id` column (it's set once at insert, never
  updated, so it should sit outside the allowlist check; add a test).
- **RLS footguns:** table *owners* bypass RLS unless `FORCE` — the app must not connect
  as the table owner. Every `SET LOCAL` must be inside the request tx (not the pooled
  connection) or it leaks across requests. Cover both with tests.
- **This is the largest migration in the project.** Keep it one-concern-per-file where
  possible, but the column-add touches every tenant table — script it rather than hand-
  writing 40 `ALTER`s.
