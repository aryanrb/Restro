# Backend alignment — handoff

Originally branch `backend-alignment` (merged as PR #1). Continued on `feat/wave-2` (from `master` at `2fca449`). Updated 2026-11-25 for whoever continues the backend.

The web frontend (Next.js, repo BsalRbh/POS) talks to this backend through Connect with generated TS clients. Its in-browser fake backend (`web/src/lib/api/mock/`) encodes every business rule and has ~1000 tests — **treat it as the executable spec** when in doubt. With `NEXT_PUBLIC_API_MODE=real` every feature, wave 2 included, now uses this backend.

## Status of `feat/wave-2`

- **Full test suite green** on a fresh database (all 22 packages, migrations 001–018 from scratch) on 2026-11-25.
- **Stage 3 wave 2 built**: purchases + suppliers, reservations, loyalty, guest table ordering, plus the alert rules that waited on them (loyalty rules 19–22, LARGE_PAID_OUT 23).
- ⚠️ **Merging into `master` deploys to Render and runs migrations 015–018 on the live database on boot.** They are additive (new tables; new columns on `customers`, `invoices`, `orders`, `order_items`, `dining_tables`), but tell whoever runs the live deploy before merging, and take a Neon backup/branch first.

## What has been built

| Commit | Contents |
|---|---|
| `9d97bbc` | Stage 1 safety: manager-approval PIN lockout (atomic, shared with PIN login), ResetPin rank rules, role checks on order/kitchen RPCs, OPEN-order guards (MarkServed also on BILLED), void refuses billed items, billing row lock (no double billing), BS calendar to BS 2090 |
| `4a0da03` | Progressive PIN auto-unlock (2 → 5 → 30 min) (mig 006); `unit_price_paisa` includes modifier deltas; AddItems validation per line; `AddItems{send:true}`; per-line idempotency; close rule = every live line billed AND handed over (SERVED, or READY for takeaway) |
| `c9ae185` | Atomic `IssueBill` (open shift required, payments must equal total, subsets with discount), `CounterSale`, `ListInvoices` |
| `7d41194` | Stage 2: invoice seller snapshot + display number + print log (mig 007); shift history, all-method payments, cancel adjustments, closing note (mig 008); report fields + richer audit log; stable variant/modifier ids, moves within category/area, settings logo/QR images, guest_ordering, staff phone/has_password, NOT_FOUND errors (mig 009); order label/waiter/times/billed, SetGuestCount, transfer to takeaway, user_id check, merge rules, kitchen RPCs return the order, WebSocket requires a session (mig 010) |
| `433261b` | Wiring: cancel adjusts shifts, counter-sale label, sales-book last print, audit device label |
| `9f9a3eb` | Stage 3 wave 1a: credit customers (mig 011), inventory + wastage + recipes (mig 012) |
| `a1197fb` | Stage 3 wave 1b: suspicious-activity alerts (mig 013), live view + analytics + calendar (mig 014) |
| `e6ff3f3` | Wave 2: purchases + suppliers (mig 015; till paid-outs in shifts, LARGE_PAID_OUT alert), reservations (mig 016) |
| `d21ab5a` | Wave 2b: loyalty stamp card (mig 017; reward = pre-VAT discount, pending stamps on on-account bills), guest table ordering (mig 018; public `GuestService` without a session, staff `GuestDeskService`, call waiter / ask for bill → `Order.bill_requested_at`, public socket `/api/guest/ws?t=<code>`) |

### Deliberate deviations the frontend maps back

- ErrorReason numbers ran short, so some reasons go out as existing ones with a field: `PAID_OUT_LIMIT` → MANAGER_APPROVAL_REQUIRED `amount`; `SUPPLIER_INACTIVE` → INVALID `supplier_id`; `ITEM_INACTIVE` → ITEM_UNTRACKED `lines`; `PURCHASE_HAS_RETURNS` → IN_USE; reservations `TABLE_OCCUPIED` → IN_USE `table_ids`; `PHONE_TAKEN` → DUPLICATE `phone`; `CONSENT_REQUIRED` → INVALID `consent`; `CARD_LINK_INVALID` → NOT_FOUND `token`.
- Audit names: some packages use dotted names (`invoice.issue`, `guest.accept`, `reservation.*`), others the web names (`PURCHASE_RECORDED`, `LOYALTY_*`). The frontend maps both (`web/src/lib/api/real/real-reports-api.ts`).
- Loyalty-only members have `credit_account=false` and are hidden from ListCustomers; existing customers must come back as `credit_account=true`.
- Ask-for-bill sets/clears `bill_requested_at` without bumping the order version.

## Running it

- `~/Work/go.work` on the original machine excluded this module: run Go commands with `GOWORK=off` if you hit "outside modules listed in go.work".
- Local Postgres: `docker run -d --name restro-pg -e POSTGRES_USER=restro -e POSTGRES_PASSWORD=restro -e POSTGRES_DB=restro -p 55432:5432 postgres:17-alpine`
- Tests: `cd api && TEST_DATABASE_URL=postgres://restro:restro@localhost:55432/restro_test?sslmode=disable go test -count=1 -p 1 ./...` (`-p 1` is required: packages share the test DB). If a migration was edited, drop/recreate the test DB.
- Run: `DATABASE_URL=… PORT=8091 go run ./cmd/api` (env overrides `api/.env` — **api/.env points at a live Neon DB; don't run tests against it**).
- Proto: `buf generate proto`, `buf lint proto`, `buf breaking proto --against '.git#branch=master,subdir=proto'`. All proto changes are additive.
- Frontend live end-to-end test (16 steps, a full service day): in the web repo, `RESTRO_LIVE_API=http://localhost:8092/api pnpm vitest run src/lib/api/real/live.e2e.test.ts` with this backend running on 8092.

## Plans (all in `docs/`)

- `docs/handoff/backend-gap-report-2026-11-25.md` — original frontend↔backend comparison (most items now fixed).
- `docs/superpowers/specs/` — design docs (owner decisions included) for every feature: credit-customers, inventory-wastage, suspicious-activity-alerts, analytics (live + analytics), purchases-suppliers, reservations, loyalty, guest-table-ordering, and the MVP design.

## Remaining work, in order

1. **Login from the Vercel frontend**: set `COOKIE_SAMESITE=none` on Render (cookies become `SameSite=None; Secure`) and make sure `CORS_ORIGINS` lists the Vercel URL. Default stays `Lax` for same-origin hosting and local dev.
2. **Smaller gaps still open** (frontend has `// GAP:` comments for each):
   - no server preview for counter lines / item-subset bills (frontend estimates client-side; with a loyalty reward it can't show whether a stamp is earned);
   - loyalty not on the older `IssueFullBill` / `SplitByItem` RPCs; no reward column in the sales book; no "rewards today" on the dashboard;
   - one audit action vocabulary (see deviations above);
   - kitchen `StartItems` ignores READY → PREPARING;
   - item-level RPCs take server item ids (frontend keeps a client↔server id map);
   - live view: alert counts / exception alert ids not wired, no `audit.created` event;
   - Web Push for alerts not implemented (in-app polling via `ListNotifications`);
   - untested so far: supplier statement, purchase book, buy list, loyalty expiry/birthday/leave/report, guest rate limits and socket forwarding.
3. **Multi-tenancy (decided)** — before go-live, move from one deployment per restaurant to one shared deployment: `restaurant_id` on every row, Postgres row-level security (FORCE RLS + non-owner app role) behind app-level filtering; invoice numbering per restaurant/fiscal year/mode; organisation → restaurants hierarchy (makes multi-branch cheap). Needs a short design first (restaurant selection at login, cross-restaurant staff, onboarding, per-restaurant backup/export).
4. **Go-live** — production secrets, backups, deploy; frontend switches with `NEXT_PUBLIC_API_MODE=real`.
5. Optional: same-origin hosting (Caddy: `/` → web static export, `/api` + `/api/ws` → Go) removes the cookie/CORS issue entirely.

## Open questions for the owner / accountant

- IRD rounding and percent-discount rounding (backend truncates? frontend half-up) — confirm with the accountant.
- BS calendar years after 2083 are projections from `nepali-date-converter`; re-check against the official panchang yearly.
- Lunar festival dates in the analytics calendar seed need checking.
