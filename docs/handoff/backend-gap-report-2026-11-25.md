# Frontend ↔ restro-api gap report

Date: 2026-11-25. Compared: frontend `restaurant-management/web` (service interfaces in `web/src/lib/api/`, business rules in the in-browser fake `web/src/lib/api/mock/`) against `aryanrb/Restro` at commit `2194648` (`proto/restaurant/v1/*.proto`, `api/internal/*`). Read-only comparison; file references are to restro-api unless marked FE.

## Summary

| Area | Can the frontend connect now? |
|---|---|
| Transport, auth (password, PIN, device, session, language) | **Yes**, with small fixes below |
| Settings, floor, stations, staff, menu | **Yes** (except settings images), via a mapping layer |
| Shifts | Partly: open / close / current |
| Billing | Partly: preview, cancel, print count. Issue + pay, counter sale and invoice lists need backend work |
| Orders, kitchen | **No**: several gaps would lose data (below) |
| Reports, audit | Screens would be mostly empty: many fields missing |
| Credit, inventory, alerts, analytics, guest ordering, loyalty, reservations, purchases | No backend yet. Specs: `docs/superpowers/specs/2026-11-24-*` |

## 1. Hosting decision (blocks going live)

- The backend has **no CORS** and sets cookies `SameSite=Lax` (`handler.go:31`). The web app is a static export currently on Vercel (another origin). Cookies and the WebSocket only work same-origin.
- **Proposal:** serve the web static export and the API from one origin (Caddy: `/` → web `out/`, `/api` and `/api/ws` → Go). Please confirm, or add CORS with credentials + a fixed origin list.
- WebSocket `/api/ws` has no session check yet (`hub.go:84-89`, "step 4"). When does it land?

## 2. Small fixes

- Health: backend `GET /api/healthz`, frontend calls `/api/health`. (Frontend will switch to `/api/healthz`.)
- Events without `id` (`menu.updated`, `settings.updated`) are dropped by the frontend parser. Please always send `id` (empty string is fine); the frontend will also accept a missing id.
- `invoice.issued` event is listed but not emitted; the frontend needs it.
- PIN length: backend 4–6 digits, frontend 4. Frontend will support 4–6. Please confirm 4–6 is intended.
- Error reasons: please rename/alias `MANAGER_APPROVAL_INVALID` → `MANAGER_PIN_INVALID`, return `NOT_FOUND` instead of Internal for missing area/table/station/category (`floor.go:121`, `menu.go:98`), and add the reasons the frontend uses: `SHIFT_ALREADY_OPEN`, `ALREADY_BILLED`, `PAYMENT_MISMATCH`, `DISCOUNT_TOO_HIGH`, `MANAGER_PIN_LOCKED`, `WRONG_USER`, `ITEM_BILLED`, `RANGE_TOO_LARGE`.

## 3. Security / correctness (please prioritise)

1. **No role checks** on order and kitchen RPCs (only `RequireActor`, `server.go:53-57`). Orders: owner/manager/waiter/cashier; kitchen: owner/manager/kitchen.
2. **Manager PIN approval has no lockout** (`auth/service.go:427-450`): void approval PINs can be brute-forced. Lockout exists only on PIN login.
3. **ResetPin** has no rank check: a manager can reset the owner's PIN; unknown user not rejected.
4. **Double billing race:** two different command ids can bill the same items concurrently (no `FOR UPDATE`; `SetOrderItemsInvoice` doesn't check `invoice_id IS NULL`, `billing.sql:40-41`).
5. **Void** has no ITEM_BILLED guard; no OPEN check on SendItems/VoidItem/MarkServed/TransferTable/MergeTables.
6. **BS calendar table ends at BS 2083** (`bsdate.go:29-38`): invoice issue will fail after ~mid-April 2027. 2083 month lengths look copied from 2082 — please verify.
7. Immutability trigger doesn't cover discount/service charge/rounding/buyer fields, allows CANCELLED → ISSUED, and `payments` / `audit_log` have no protection.
8. Payment's shift lookup is outside the transaction (a shift closing at the same moment can still receive a payment).

## 4. Orders + kitchen (blocking the frontend's ordering screens)

The frontend queues order commands offline (client ids, idempotent `commandId`) — the backend's `processed_commands` replay matches this well. Gaps:

- `AddItems` creates DRAFT lines; a separate `SendItems` sends them. Can AddItems send directly (or `send: true`), so one queued command = one atomic call?
- Item-level RPCs (`VoidItem`, `MarkServed`, kitchen) take **server** item ids; the frontend only knows its line `client_id` (offline). Please accept `client_id`.
- `CreateOrderItem` has no `ON CONFLICT (client_id) DO NOTHING` (`orders.sql:45-50`): a re-sent line fails the whole command.
- Missing: `SetGuestCount(command_id, base_version)`; `TransferTable` to no table (→ takeaway); `WRONG_USER` (commands should carry the user and reject a mismatch, also on replay).
- **Close rule:** frontend closes an order when everything is billed **and** handed over (SERVED, or READY for takeaway); backend closes on full payment (`billing/service.go:497-523`). Pay-first counter sales rely on the frontend rule — please adopt it (run it on MarkServed and kitchen READY too). What does order-level BILLED mean?
- `unit_price_paisa` excludes modifier price deltas (`commands.go:178-183`); frontend includes them. Unknown variants fall back to base price; bad modifiers are skipped silently; inactive items aren't rejected. Please validate and return INVALID_OPTIONS / ITEM_INACTIVE.
- Order/item fields missing: `label` (takeaway name), `waiter_name`, `created_at`, `sent_at` per line, billed item ids.
- Merge: frontend marks the source CANCELLED and sums guest count; backend marks CLOSED and doesn't sum. Which is intended?
- `ListOpenOrders` also returns BILLED orders. Kitchen RPCs return `{}` (frontend needs the order) and `order.updated` from kitchen has no version.

## 5. Billing, shifts, reports

- **Issue + pay is two calls** (`IssueFullBill` then N × `RecordPayment`): no check that payments add up, overpay allowed, and an issued-but-unpaid bill is possible if payment fails with SHIFT_NOT_OPEN. Please add one atomic `IssueBill(order_client_id, item_ids?, discount, buyer, payments[], command_id)` that requires an open shift; plus `CounterSale` and `ListInvoices` (by order / by day).
- Invoice is missing: seller snapshot (reprints must show the details at issue time), `display_number`, `discount_reason`, cancel fields (in DB, not proto), `issued_by` name, `place`, `order_item_id` on lines, bilingual variant name, and a per-print log (IRD sales book needs printed time/by).
- Fiscal year string: backend `2083.084`, frontend `2083/84` (display `E-2083/84-12`). Which is canonical?
- Maths: percent discount — backend truncates (`service.go:567`), frontend rounds half-up; cashier 10% limit compared differently at the edges. Has the accountant confirmed IRD rounding?
- Shifts: no shift history (`ListShifts`); cancelled bills still count in shift takings; no adjustment in the canceller's shift for bills cancelled after their shift closed; `CloseShift` overwrites the opening note.
- Reports: dashboard lacks by-method, discount **amount**, void/cancelled values; daily sales lacks AD date, discounts, service charge, by-method, cancelled; sales book lacks estimate filter and IRD print columns; cancelled bills lack display number and names; audit lacks actor filter, names, readable subject, detail, device, and uses different action names (`invoice.issue` vs `BILL_ISSUED`). Please agree one audit vocabulary.

## 6. Admin services (settings, floor, stations, staff, menu)

All map except settings images. Please consider:
- Settings image RPC (logo, Fonepay/other QR) + `qr_images`; `guest_ordering` flag.
- `StaffMember.has_password`, `phone`.
- SaveItem / SaveModifierGroup delete and recreate variants/modifiers, changing their ids every save — please upsert by id (recipes, inventory and analytics key on these ids).
- MoveItem / MoveTable swap in the global list, not within the category/area (looks like a bug); moving a table to another area doesn't re-sort.
- SetItemImage bumps the item version (frontend fake doesn't) — fine if intended; frontend will use the returned version.
- Set86 open to any role and ignores unknown ids — intended?
- Manager can't edit own account; promotion to manager doesn't require a password — intended?
- Server-side checks missing: empty `name.en`, KDS yellow < red, item colour palette.
- Images stored as data URLs in Postgres — long-term plan?

## 7. TypeScript client generation

The frontend will generate TS clients with `protoc-gen-es` from this repo's `proto/` (pinned to a commit) into `web/src/gen`. Tell us if you'd rather publish a package.
