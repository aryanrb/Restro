# Backend alignment — handoff

Branch `backend-alignment` (from `master` at `2194648`). Written 2026-11-25 for whoever continues the backend.

The web frontend (Next.js, repo BsalRbh/POS, branch `feat/real-backend`) talks to this backend through Connect with generated TS clients. Its in-browser fake backend (`web/src/lib/api/mock/`) encodes every business rule and has ~950 tests — **treat it as the executable spec** when in doubt.

## ⚠️ Must be done before this branch is merged

Do **not** merge `backend-alignment` into `master` until all three are finished (details under "Remaining work" below):

1. **Full test suite green** on a fresh database — it was not re-run after `a1197fb`.
2. **Stage 3 wave 2 built and tested**: purchases + suppliers, reservations, loyalty, guest table ordering — plus the alert rules waiting on them (loyalty rules, LARGE_PAID_OUT).
3. **The smaller gaps closed**: server preview for counter lines / item subsets, one audit action vocabulary, READY → PREPARING, live view alerts + bill-requested + `audit.created`, the missing ErrorReasons, Web Push for alerts.

Multi-tenancy and go-live (items 4–5) come after the merge.

## What this branch contains

| Commit | Contents |
|---|---|
| `9d97bbc` | Stage 1 safety: manager-approval PIN lockout (atomic, shared with PIN login), ResetPin rank rules, role checks on order/kitchen RPCs, OPEN-order guards (MarkServed also on BILLED), void refuses billed items, billing row lock (no double billing), BS calendar to BS 2090 |
| `4a0da03` | Progressive PIN auto-unlock (2 → 5 → 30 min) (mig 006); `unit_price_paisa` includes modifier deltas; AddItems validation per line; `AddItems{send:true}`; per-line idempotency; close rule = every live line billed AND handed over (SERVED, or READY for takeaway) |
| `c9ae185` | Atomic `IssueBill` (open shift required, payments must equal total, subsets with discount), `CounterSale`, `ListInvoices` |
| `7d41194` | Stage 2: invoice seller snapshot + display number + print log (mig 007); shift history, all-method payments, cancel adjustments, closing note (mig 008); report fields + richer audit log; stable variant/modifier ids, moves within category/area, settings logo/QR images, guest_ordering, staff phone/has_password, NOT_FOUND errors (mig 009); order label/waiter/times/billed, SetGuestCount, transfer to takeaway, user_id check, merge rules, kitchen RPCs return the order, WebSocket requires a session (mig 010) |
| `433261b` | Wiring: cancel adjusts shifts, counter-sale label, sales-book last print, audit device label |
| `9f9a3eb` | Stage 3 wave 1a: credit customers (mig 011), inventory + wastage + recipes (mig 012) |
| `a1197fb` | Stage 3 wave 1b: suspicious-activity alerts (mig 013), live view + analytics + calendar (mig 014). **The combined test suite was not re-run after this commit** (each part's suite passed separately) — run it first. |

## Running it

- `~/Work/go.work` on the original machine excluded this module: run Go commands with `GOWORK=off` if you hit "outside modules listed in go.work".
- Local Postgres: `docker run -d --name restro-pg -e POSTGRES_USER=restro -e POSTGRES_PASSWORD=restro -e POSTGRES_DB=restro -p 55432:5432 postgres:17-alpine`
- Tests: `cd api && TEST_DATABASE_URL=postgres://restro:restro@localhost:55432/restro_test?sslmode=disable go test -count=1 -p 1 ./...` (`-p 1` is required: packages share the test DB). If a migration was edited, drop/recreate the test DB.
- Run: `DATABASE_URL=… PORT=8091 go run ./cmd/api` (env overrides `api/.env` — **api/.env points at a live Neon DB; don't run tests against it**).
- Proto: `buf generate proto`, `buf lint proto`, `buf breaking proto --against '.git#branch=master,subdir=proto'`. All proto changes on this branch are additive.
- Frontend live end-to-end test (16 steps, a full service day): in the web repo, `RESTRO_LIVE_API=http://localhost:8092/api pnpm vitest run src/lib/api/real/live.e2e.test.ts` with this backend running on 8092.

## Plans (all in `docs/`)

- `docs/handoff/backend-gap-report-2026-11-25.md` — original frontend↔backend comparison (most items now fixed; see below).
- `docs/superpowers/specs/` — design docs (owner decisions included) for every feature:
  credit-customers, inventory-wastage, suspicious-activity-alerts, analytics (live + analytics), **purchases-suppliers, reservations, loyalty, guest-table-ordering** (not built yet), and the MVP design.

## Remaining work, in order

1. **Run the full test suite** on a fresh DB (see above) and fix anything wave 1b broke.
2. **Stage 3 wave 2** — build from the specs, mirroring the web fakes (`web/src/lib/api/mock/mock-*.ts` + tests):
   - purchases + suppliers (builds on inventory: PURCHASE/PURCHASE_RETURN movements, moving average via `inventory.MovingAverageE4`, till paid-outs in shifts),
   - reservations (customers from credit; phone normalised + unique),
   - loyalty (stamp card on Customer; reward is a discount before VAT; pending stamps on on-account bills until repaid),
   - guest table ordering (public guest API by table code, staff accept/reject, call waiter / ask for bill → `Order.bill_requested_at`).
   - Then add the alert rules left as planned: loyalty rules and LARGE_PAID_OUT.
3. **Smaller gaps still open** (frontend has `// GAP:` comments for each):
   - no server preview for counter lines / item-subset bills (frontend estimates client-side);
   - audit action names are dotted (`invoice.issue`) vs the frontend's (`BILL_ISSUED`) — agree one vocabulary or keep the frontend mapping; credit actions need mapping (`customer.change`, `customer.credit_limit`, `credit.give`, `credit.repay`, `credit.repay_void`);
   - kitchen `StartItems` ignores READY → PREPARING;
   - item-level RPCs take server item ids (frontend keeps a client↔server id map);
   - live view: alert counts / exception alert ids not wired, no bill-requested state, no `audit.created` event;
   - missing ErrorReasons the frontend uses (WRONG_USER, ITEM_BILLED, PAYMENT_MISMATCH, …) are sent as existing reasons with a field + message;
   - Web Push for alerts not implemented (in-app polling via `ListNotifications`).
4. **Multi-tenancy (decided)** — before go-live, move from one deployment per restaurant to one shared deployment: `restaurant_id` on every row, Postgres row-level security (FORCE RLS + non-owner app role) behind app-level filtering; invoice numbering per restaurant/fiscal year/mode; organisation → restaurants hierarchy (makes multi-branch cheap). Needs a short design first (restaurant selection at login, cross-restaurant staff, onboarding, per-restaurant backup/export).
5. **Go-live** — same-origin hosting (Caddy: `/` → web static export, `/api` + `/api/ws` → Go; cookies are SameSite=Lax and there is no CORS), production secrets, deploy; frontend switches with `NEXT_PUBLIC_API_MODE=real`.

## Open questions for the owner / accountant

- IRD rounding and percent-discount rounding (backend truncates? frontend half-up) — confirm with the accountant.
- BS calendar years after 2083 are projections from `nepali-date-converter`; re-check against the official panchang yearly.
- Lunar festival dates in the analytics calendar seed need checking.
