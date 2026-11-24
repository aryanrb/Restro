# Restaurant Management System — MVP Design

Date: 2026-11-23 · Status: approved
Source: `RESTAURANT_PLAN.md` (original handoff) + brainstorming decisions + research in `docs/research/01–04`.

## 1. Intent

A restaurant POS for Nepal that a new waiter learns in 10 minutes and that stays fast at peak hour. It exists to stop the everyday losses restaurants actually suffer: lost or wrong orders, kitchen/floor out of sync, money leakage through unchecked voids and discounts, and owners who can't be on site. Feature count is not the goal; peak-hour speed and simplicity are.

First customer is one real restaurant (details unknown), but the product is designed generically so the same code can be deployed for other restaurants.

## 2. Goals, non-goals, anti-goals

**Goals (MVP)**
- Complete order → kitchen → bill → payment → shift-close loop for one restaurant, usable entirely from the web app on desktop, tablet and phone.
- Data model and invoice rules shaped for Nepal IRD e-billing from day one, so certification later is a process step, not a rewrite.
- A stable, documented API contract the mobile developer can build against later.

**Non-goals (MVP)** — Phase 2+: credit (udharo) accounts, inventory/recipes/variance, purchases/suppliers, suspicious-activity alerts, reservations, QR self-ordering, loyalty, online delivery, multi-branch, CBMS sync, automatic QR payment confirmation, native mobile app, device-to-device offline sync.

**Anti-goals** (failure even if it technically works)
- Spinners or waits in the ordering flow during peak hour.
- A waiter needing more than ~10 minutes of training.
- Any way to silently edit or delete an issued bill, or to void/discount without a trace.
- Old bills changing when the menu changes.
- Paid SaaS dependencies.

## 3. Constraints

- **Free software only.** Open-source, self-hosted. Hosting during build/pilot is Oracle Cloud Always Free. Later the restaurant pays for a Nepal VPS (see §6).
- **One deployment per restaurant.** Same codebase, separate server/database/config per restaurant. No multi-tenancy, no `restaurantId` columns. Restaurant-specific details live in a settings table, never in code. No per-restaurant code forks.
- **IRD (from research 01, based on the 2074 procedure; 2082 procedure unverified):** issued invoices immutable; gap-free numbering per Bikram Sambat (BS) fiscal year (starts Shrawan 1); BS dates on bills; original printed once, reprints marked "Copy of Original" with count; audit trail; sales-book/master-bill reports exportable. Certified billing requires a Nepal-located server.
- **Languages:** Nepali + English toggle across the whole UI.
- **Developer context:** Go backend is also a learning vehicle; follow the conventions of the developer's work stack (sqlc, pgx, buf/protobuf, protovalidate) where sensible.

## 3a. Team and sequencing

- **Team:** Bishal builds the Next.js web app; a backend team builds the Go API. A separate mobile developer starts after the web MVP ships.
- **No fixed date.** Work is ordered so the core loop (menu → order → kitchen → bill → payment) is usable first; shift closing, reports, audit browser, exports follow.
- **Contract first:** each feature starts with its `.proto` definitions agreed by both sides, so frontend and backend work in parallel (frontend against a generated client + mock/stub server, backend against tests).

## 4. Approaches considered

| Decision | Considered | Chosen | Why |
|---|---|---|---|
| Hosting | Local server in restaurant; multi-tenant cloud SaaS; cloud with one deployment per restaurant | **Cloud, one deployment per restaurant** | Partner ruled out local server; per-restaurant deployment keeps the app single-tenant and simple. |
| Offline | Phone outbox + 4G hotspot advice; local hub sync; device-to-device sync | **Outbox + hotspot advice** | Device-to-device sync parked for later (partner: "good idea, leave it for now"). |
| Compliance hosting | Oracle free → Nepal VPS later; Nepal VPS now; restaurant machine | **Staged: Oracle free for build/pilot, Nepal VPS at IRD enlistment** | Free during build; pilot prints "Estimate"; migration is a DB copy per deployment. |
| Backend language | TypeScript (Fastify + tRPC) | **Go** | Partner's decision. |
| API contract | Protobuf + ConnectRPC; OpenAPI-first; hand-written REST | **Protobuf + ConnectRPC** | One `.proto` → Go handlers + typed clients for TS/Kotlin/Swift/Dart; JSON over HTTP stays curl/Bruno-friendly; matches work stack. |
| Clients | Web responsive covering all + native later; full parity built in both at once; split screens by platform | **Web first with every feature, mobile responsive; native mobile app afterwards at parity, built by a separate mobile developer** | Partner's decision. |

## 5. Architecture

```
restaurant-management/
├── proto/            # buf module: *.proto service + message definitions, protovalidate rules
├── api/              # Go module
│   ├── cmd/api/      # main: HTTP server (Connect handlers + WebSocket + health)
│   ├── internal/     # domain packages (auth, menu, floor, orders, kitchen, billing, shifts, reports, audit, realtime, settings)
│   ├── db/migrations # SQL migrations
│   ├── db/queries    # sqlc queries → generated Go
│   └── gen/          # buf-generated Go code
├── web/              # Next.js (App Router), Tailwind + shadcn/ui, responsive, installable PWA
│   └── src/gen/      # buf-generated TS Connect clients
├── deploy/           # docker-compose (api, postgres, web, caddy), backup script, env template
└── docs/             # research, specs, plans, mobile-dev handoff
```

- **Go API:** Connect handlers (connect-go) for all request/response calls; validation via protovalidate; PostgreSQL via pgx + sqlc; SQL migrations (tool left to builder; prefer the one used at work).
- **Real-time:** a WebSocket endpoint with an in-process hub (one deployment = one process, no broker needed). Server → client events only (e.g. `order.updated`, `ticket.created`, `item.ready`, `item.86d`, `table.updated`). Clients treat events as "something changed" hints and refetch via Connect; on reconnect they always refetch. Mutations never go over the socket.
- **Shared TS:** no separate packages. Generated clients live in `web/src/gen`. Money/VAT/BS-date maths is authoritative on the server; the web only displays server-computed totals (it may preview totals while composing an order).
- **Monorepo tooling:** no Turborepo. A root `Makefile` (or `justfile`) with targets like `gen`, `dev`, `test`, `migrate`.

## 6. Hosting and deployment

- **Build + pilot:** Oracle Cloud Always Free VM (Ampere/ARM). One `docker compose` stack per restaurant: `postgres`, `api`, `web`, plus a shared `caddy` reverse proxy for TLS (subdomain per restaurant). Several small restaurants can share one VM.
- **Backups:** nightly `pg_dump`, kept on the VM and copied off-box to free object storage (Oracle Always Free object storage). Restore procedure documented and tested once.
- **Pilot billing mode:** `billing_mode = ESTIMATE` → bills print "Estimate — not a tax invoice"; restaurant keeps its legal bill book. Everything else (numbering, immutability, audit) behaves exactly as in tax mode.
- **Certified mode (post-MVP):** move the deployment to a Nepal-registered VPS (restaurant pays), enlist product with IRD, switch `billing_mode = TAX_INVOICE`. Migration = dump/restore + DNS change.

## 7. Domain model (MVP)

Money stored as integer paisa. Every table has `created_at/updated_at`; mutable aggregates have a `version` column for optimistic locking.

- **Settings** (single row): restaurant name (EN/NE), address, PAN/VAT number, logo, VAT registered (bool), `prices_include_vat` (default true), VAT rate (13%), service charge enabled (default **false**) + rate, `billing_mode`, receipt width (58/80 mm), static QR image(s) per payment provider, default language, fiscal-year info.
- **User**: name, role (`OWNER`, `MANAGER`, `CASHIER`, `WAITER`, `KITCHEN`), username + password hash (owner/manager), 4–6 digit PIN hash (all staff), active flag. **Session**: opaque token, user, device label, expiry.
- **Floor**: `Area` (name, sort) → `Table` (name/number, seats, area, sort, active). Zero tables is valid (counter-only restaurants).
- **Station**: name (default one: "Kitchen"); optional more (Bar, Tandoor…).
- **Menu**: `MenuCategory` → `MenuItem` (name EN/NE, price, color, image, station, active, `is_86d`) → `ItemVariant` (e.g. half/full, own price) ; `ModifierGroup` (min/max selections) → `Modifier` (name, price delta); items link to modifier groups.
- **Order**: type (`DINE_IN`, `TAKEAWAY`), table (nullable), waiter, status (`OPEN`, `BILLED`, `CLOSED`, `CANCELLED`), guest count, version. **OrderItem**: snapshots of name/variant/modifiers/unit price at time of adding, quantity, note, station, status (`DRAFT` → `SENT` → `PREPARING` → `READY` → `SERVED`, or `VOIDED` with reason/by/approved-by), course (nullable, only if courses enabled). Order items are append-only; changes after sending are voids + new items. (Courses / hold-and-fire are **not** in the MVP; no `course` column yet.)
- **Invoice**: fiscal year (BS), sequential number (gap-free per **(fiscal year, mode)**, assigned inside the billing transaction from a locked counter row — estimates and tax invoices never share a series; estimates are excluded from the tax sales book and archived, not migrated as tax history, when moving to the Nepal VPS), BS + AD date/time, mode (`ESTIMATE`/`TAX_INVOICE`), buyer name/PAN (optional), lines (snapshot), subtotal, discount, service charge, taxable amount, VAT, total, status (`ISSUED`, `CANCELLED` with reason/by/approved-by/at), print count. Never updated except status → `CANCELLED` and print count. An order may produce multiple invoices (split by item). Split equally = **one** invoice paid by N payments. Cancelling an invoice returns its lines' order to `OPEN` so it can be billed again.
- **CbmsOutbox** (schema only in MVP): invoice/cancellation id, payload, status (`PENDING`/`SENT`/`FAILED`/`DISABLED`), attempts, last error. The billing transaction writes a row (status `DISABLED` while CBMS is off); the sending worker is Phase 2. **CreditNote** (partial returns, own gap-free series) is Phase 2; full corrections in MVP are invoice cancellation.
- **Payment**: invoice, method (`CASH`, `QR_FONEPAY`, `QR_OTHER`, `CARD`), amount, reference note, confirmed-by, shift.
- **Shift**: cashier, opened/closed at, opening float, expected cash (computed), counted cash, difference, note. Several shifts may be open at once (one per cashier). Taking payment requires the collecting user to have an open shift; payments attach to that shift. An invoice cancelled after its shift closed is recorded as an adjustment in the canceller's current shift. Only OWNER/MANAGER/CASHIER can take payment in the MVP.
- **ProcessedCommand**: client command UUID, user, result, created_at — for idempotent retries.
- **AuditLog**: actor, action, entity, entity id, before/after JSON, reason, approver, device, timestamp. Written in the same transaction as the change.

## 8. Features and key behaviour (MVP)

1. **Setup & auth** — owner/manager log in with username + password; shared devices (kitchen tablet, counter PC, waiter phones) use quick staff PIN login with a staff picker. Role decides visible screens. Manager PIN override dialog for privileged actions without switching user.
2. **Settings & menu (web, owner/manager)** — restaurant profile, tax settings, areas/tables, stations, staff, menu categories/items/variants/modifiers with colors/images, bulk 86 toggle.
3. **Ordering (waiter, phone-first layout)** — table grid with status colors → tap table → category tabs → tap item (variant/modifier sheet only when the item has them) → cart of DRAFT items, freely editable/undoable → "Send" sends to stations. Repeat last item, notes, guest count, takeaway orders without a table, transfer/merge tables. 86'd items greyed everywhere instantly. Target: common order in ≤3 taps per item, no blocking spinner (optimistic UI).
4. **Kitchen screen (KDS, tablet in browser, full screen)** — tickets per station with age timer (normal → yellow → red, thresholds in settings), item-level status taps (preparing/ready), bump ticket, 86 an item from the KDS. Ready items notify the waiter (in-app toast + sound + `navigator.vibrate` where supported).
5. **Counter / quick-service flow (cashier)** — for restaurants with no table service: one screen to build a takeaway or counter order, bill and take payment immediately (pay-first), then send to stations. Same order/invoice rules as table service.
6. **Billing (cashier)** — open orders list → bill whole order, split by item, or split equally → discount (role-limited % / amount, reason required) → server computes totals → payment (cash with change calc, Fonepay static QR shown on screen with manual "Paid by QR" confirm, card, mixed) → invoice issued with next number → print. Reprint marks "Copy of Original (n)". Cancel invoice requires reason + manager PIN.
7. **Voids & audit** — void before send: free (it's a draft). Void after send: reason required; manager PIN if item already `PREPARING`/`READY`/`SERVED`. Every void, discount, cancellation, reprint, price/menu change, 86 and login goes to the audit log. Owner can browse/filter the audit log.
8. **Shift cash closing** — cashier opens shift with float; closing shows expected cash by method, cashier enters counted cash; difference recorded; manager sees shift reports. Includes QR payment list for reconciling against the Fonepay merchant statement.
9. **Owner dashboard & reports** — live today view (sales, open tables, open orders, voids/discounts count, top items), daily sales report, IRD-style sales book (date range, BS dates) exportable to CSV/Excel and PDF, cancelled-invoice report.
10. **i18n** — every label in EN and NE; menu items have both names; per-user language preference.

### Tax calculation (my call; confirm with an accountant)
- If `prices_include_vat`: line price is VAT-inclusive; VAT = taxable × 13/113 extracted at invoice level.
- Order: subtotal → minus discount → plus service charge (if enabled) → VAT on (subtotal − discount + service charge) → total.
- All arithmetic in integer paisa; round half-up once at the invoice-level VAT, then total; display rounded to the rupee only if the setting `round_total_to_rupee` is on (with a rounding line).
- Non-VAT-registered restaurants: no VAT lines, bill titled as PAN bill.

## 9. Offline behaviour (web)

- App shell cached by a service worker, so an open/reloaded app still loads offline.
- Menu, tables, open orders cached locally (TanStack Query persisted to IndexedDB).
- **Order commands** (create order, add items, send to kitchen, mark served) go through an IndexedDB outbox: each command has a client UUID; sent in order, one at a time; server stores the UUID in `ProcessedCommand` so retries are no-ops. Clear banner: "Offline — 3 orders waiting".
- **Conflict rules (server is authority):** added items always merge (append-only); voids/moves/discounts carry the order `version` and are rejected if stale; 86'd items are rejected per line, rest accepted; server price at apply time wins and is snapshotted; rejections surface in a "Needs attention" list on the device.
- **Online-only:** billing, payments, invoice cancellation, shift open/close, settings, menu edits. Buttons are disabled with an explanation when offline.
- Connectivity = "API/WebSocket reachable" (heartbeat), not the browser's `navigator.onLine` alone.
- Operational advice to restaurants: keep a 4G hotspot as backup internet.

## 10. Printing (web)

- **Bills:** browser print to the counter PC's thermal printer via its normal OS driver. Print-specific CSS templates for 58 mm and 80 mm. Nepali renders natively through browser fonts (bundle Noto Sans Devanagari). Server increments print count and returns "original" vs "copy" before the print renders.
- **Kitchen tickets:** the KDS screen is primary. Optional per-station auto-print from the KDS browser; silent printing needs Chrome kiosk mode (`--kiosk-printing`) on that device — document the setup.
- **MVP requirement:** bill printing needs a counter computer (or tablet with Chrome) connected to a USB/network thermal printer. A phones-only restaurant can run everything except printed bills until the native app ships.
- Phone browsers can't print to Bluetooth thermal printers; that arrives with the native app (research 03 is the mobile developer's starting point).

## 11. Payments

- MVP: cash, card (recorded only), Fonepay static QR (image uploaded in settings, shown on cashier screen and optionally printed on the bill) with manual confirmation, end-of-shift QR list for reconciliation.
- A `PaymentProvider` interface in Go so Fonepay dynamic QR + automatic confirmation can be added later.
- Action for the pilot restaurant now: ask its bank for Fonepay merchant API credentials (lead time can be weeks).

## 12. Mobile developer handoff (deliverable of the MVP)

- `proto/` contract with generated-client instructions for their chosen stack.
- WebSocket event catalogue.
- Offline/idempotency rules (§9) the native app must follow.
- Research 03 (printing, Devanagari as image) and 04 (outbox) as starting guidance.

## 13. Decisions left to the builder

Migration tool; HTTP router/mux around Connect; password hashing algorithm (argon2id or bcrypt); BS↔AD date library or table (must be tested against an authoritative calendar); PDF/Excel export libraries; Next.js data layer details; exact WebSocket library (`coder/websocket` suggested); test strategy per package; KDS timer default thresholds.

## 14. Before pilot launch — verify

- IRD Electronic Billing Procedure 2082 vs our rules (ask a tax office or accountant).
- Current legality of service charge; whether pilot "estimate" printing is acceptable to the local tax office.
- Oracle Always Free ARM capacity in an Asian region (fallback: any free VM for dev; Nepal VPS sooner).
- Fonepay merchant credential request started.
