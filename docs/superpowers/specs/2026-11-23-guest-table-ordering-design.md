# Guest table ordering — design

Status: **designed, not scheduled.** Phase 3 item ("QR table menu / self-ordering") in `RESTAURANT_PLAN.md`. Not part of the MVP; pick this up after the pilot.

## Why

Restaurants want to run with **fewer waiters**. Guests scan a QR code on their table, browse the menu on their own phone and send their order; a staff member confirms it before it reaches the kitchen. Two buttons, **Call waiter** and **Ask for bill**, remove most of the other trips a waiter makes to a table.

## Goals

- Guests order from the table on their own phone, in Nepali or English, without an account or app install.
- Nothing reaches the kitchen until a staff member accepts it.
- Confirmation works from **both** the floor screen and the counter screen; the first to accept wins.
- Guest items join the table's existing order, so billing is unchanged (one table, one bill, same split/merge rules).
- Call waiter and Ask for bill alerts on the same screens.

## Non-goals

- Paying from the phone (eSewa/Khalti/Fonepay). Payment stays at the counter.
- Ordering from outside the restaurant (takeaway, delivery).
- Customer accounts, phone numbers, loyalty.

## Anti-goals (a failure even if it works)

- An order reaching the kitchen without a staff member accepting it.
- A guest request able to touch any staff function (billing, voids, other tables, settings).
- Staff overwhelmed by duplicate or spam alerts.
- A guest item billed twice or lost when a waiter is also adding to the table.

## Constraints

- Web app is a Next.js **static export**: no per-table pages, so the table code goes in the query string.
- Hosting is cloud, one deployment per restaurant: guests can use mobile data, not only restaurant Wi-Fi.
- Built first against the in-browser fake backend (`web/src/lib/api/mock/`), like everything else; the Go developer implements the same contract.
- Every UI string in EN and NE (the parity test enforces it).

## Approaches considered

| | Approach | Trade-off |
|---|---|---|
| **A (chosen)** | Public guest page inside the existing web app, `/menu/?t=<tableCode>` | Reuses menu display, photos, i18n, money formatting, realtime. Next sends guests only this route's code. Must be kept outside the staff login guard and the staff offline outbox. One deployment. |
| B | Separate guest web app on its own subdomain | Total isolation, but duplicated or shared-package UI code, two deployments per restaurant, extra cross-origin setup on the backend. |
| C | Go server renders guest pages | Moves frontend work to the backend developer and reuses nothing. Ruled out. |

Chosen: **A** — about half the work of B, fits one-site-per-restaurant, and isolation is achieved by a separate public-only `GuestApi`.

## Design

### Guest page (`/menu/?t=<tableCode>`)

```
┌─────────────────────────┐
│ Table T4      [नेपाली]   │
│ Momo · Thukpa · Drinks… │   categories, items, photos, variants, modifiers
│ ...                     │   sold-out (86) items shown as unavailable
│ [🙋 Call waiter]         │
│ [🧾 Ask for bill]        │
│ [Cart (3) · Rs 940]     │
└─────────────────────────┘
```

- Build a cart (variants, modifiers, note per line), tap **Send**. Status on the phone: *Waiting for staff* → *Sent to kitchen* or *Not accepted: <reason>*.
- Guests can send further rounds; each is confirmed separately.
- **Your table** section: what the table has ordered so far (from staff and guests) and the running total. Anyone holding that table's QR can see it.
- Call waiter / Ask for bill: one tap creates an alert; tapping again while one is open does nothing new; phone shows *Someone is coming*.
- No staff shell, no login, no service worker offline queue: sending needs a connection, and the page says so when offline.
- When guest ordering is off (restaurant-wide or for this table), the page is a **view-only menu**.

### Staff side (floor + counter)

```
Floor screen                     Counter screen
┌───────────────────┐            ┌─────────────────────────┐
│ T4 🔔 Guest order │            │ Guest orders (1)        │
│ T5                │            │ T4 · 3 items · Rs 940   │
│ T7 🧾 Bill        │            │ [View] [Accept] [Reject]│
└───────────────────┘            └─────────────────────────┘
```

- A table with a pending guest order or alert is highlighted, with a sound.
- Opening a guest order shows its lines. Staff may **edit before accepting** (remove a sold-out item, change a quantity).
- **Accept** adds the lines to the table's open order (creating the order if the table is empty) and sends them to the kitchen exactly like a waiter's send.
- **Reject** needs a reason (presets: *Item finished*, *Not at this table*, plus free text); the guest sees it.
- Two people accepting at once: one wins, the other sees *Already accepted by <name>*.
- Alerts (Call waiter, Ask for bill) are cleared with **Done** by anyone. Ask for bill also flags the table on the cashier screen.

### Admin

- Settings: **Guest ordering** on/off for the restaurant; per-table on/off on the Floor & tables page.
- Floor & tables: **Print QR codes** (one card per table: table name, QR, "Scan to order"); **New code** for a table invalidates its old QR.

### Data (contract for the backend)

- `Table.guestCode`: random, unguessable token (≥ 8 chars), regenerable. The QR encodes `https://<restaurant>/menu/?t=<guestCode>`.
- `GuestRequest`: `id`, `tableId`, `kind` (`ORDER` | `CALL_WAITER` | `BILL`), `status` (`PENDING` | `ACCEPTED` | `REJECTED` | `DONE`), `lines` (same shape as order lines, kind `ORDER` only), `createdAt`, `handledById`, `handledAt`, `rejectReason`, `requestId` (idempotency from the guest phone).
- Settings: `guestOrdering: boolean`; Table: `guestOrdering: boolean`.

### APIs

`GuestApi` — public, no session, identified only by `tableCode`; can never reach a staff function:

- `menu(tableCode)` → restaurant name, table name, whether ordering is on, menu with availability.
- `submitOrder({ tableCode, lines, requestId })` → the pending request (prices computed by the server, never the client).
- `call({ tableCode, kind: "CALL_WAITER" | "BILL" })` → the open alert (existing one if already open).
- `status(tableCode)` → the table's ordered items and running total, plus this table's pending/handled requests.

Staff additions (logged-in floor/counter roles):

- `listGuestRequests()` → pending requests and open alerts.
- `acceptGuestOrder({ requestId, lines, baseVersion })` → the updated order.
- `rejectGuestOrder({ requestId, reason })`.
- `resolveAlert({ requestId })`.

Realtime events: `guest.request` (new or changed request, staff screens), `guest.status` (per table, for the guest page).

### Server rules

- Unknown or regenerated `tableCode` → `TABLE_CODE_INVALID`. Guest ordering off → `GUEST_ORDERING_OFF` (menu still readable).
- Limits: max 30 lines and max quantity 20 per line per guest order; max 3 pending guest orders per table; per-IP/per-table rate limit on submit and call. Errors: `TOO_MANY_PENDING`, `RATE_LIMITED`, `INVALID`.
- Sold-out items rejected on submit (`ITEM_86D`) and re-checked on accept.
- Accept is atomic with adding the items (same transaction), uses the order's version (`STALE_VERSION`), and a request can be accepted or rejected only once (`ALREADY_HANDLED`).
- Audit log (same transaction): new actions `GUEST_ORDER_ACCEPTED` (with edits noted), `GUEST_ORDER_REJECTED` (with reason), actor = the staff member.
- Order items accepted from a guest are marked as such (e.g. `source: "GUEST"`) for reports.

## Calls made (correct when picking this up)

- Static per-table QR, no per-seating code; staff confirmation is the protection against copied QR codes.
- Guests may order at an empty table.
- The table's running order and total are visible to anyone holding its QR.
- Limits above (30 lines, 20 qty, 3 pending) are starting values.

## Left to the builder

Exact screen layout and sounds, QR card design, how the floor screen badges combine with existing table states, wording of guest-facing messages (EN + NE).
