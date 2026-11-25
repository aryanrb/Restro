# Reservations (table bookings) — design

Status: **Designed, not scheduled.** Owner decisions taken 2026-11-24. Phase 3. Covers "reservations" in the `RESTAURANT_PLAN.md` roadmap. Not scheduled.

## Why

Most bookings in Nepal come by **phone call** (some by Facebook/Instagram message or in person) and go into a notebook or someone's memory. Then a walk-in gets the table that was promised for 7:30, a birthday group of 14 arrives to no tables pushed together, or nobody knows whether "Ram, 4 people" is still coming. This puts bookings in the POS, where the floor screen already shows tables: the host sees tonight's list, reserved tables warn before they are given away, and seating a booking opens the table's order in one tap.

## Owner decisions

All recommended answers below were accepted by the owner on 2026-11-24.

Each has a recommended answer; the design below assumes the recommendation until the owner says otherwise.

1. **Should guests be able to book online themselves?** — *Recommended:* **no, not now.** Staff enter every booking (phone, message, in person). A public form brings spam, fake bookings and bookings nobody confirmed, and most of your guests call anyway. Revisit after the pilot; if added, it would create *requests* that staff accept, like guest QR orders.
2. **When a table is reserved, should walk-ins be blocked or just warned?** — *Recommended:* **warn, don't block.** From **60 minutes** before the booking the table shows *Reserved 7:30*; opening it for a walk-in asks *"Reserved for Ram Shrestha at 7:30 (in 40 min). Seat a walk-in anyway?"* Staff can still use it for a quick snack.
3. **How long do you hold a table for a late party?** — *Recommended:* flagged **Late** after 15 minutes; after **30 minutes** the table stops showing as reserved and the host is asked *No-show / Keep waiting*. The system never marks a no-show by itself. Both times editable in Settings.
4. **Who can take and change bookings?** — *Recommended:* **everyone except kitchen** (waiters answer the phone too). Every create, change, cancel and no-show records who did it.
5. **Do you take an advance (deposit) for big parties?** — *Recommended:* **not in the system for now.** Write it in the booking notes and take it as you do today. If advances become common, a later version can hold them as credit in the customer's favour (the credit-customer design already has this).
6. **How do you message guests today, and should the system help?** — *Recommended:* a **Send confirmation / Send reminder** button that opens WhatsApp, Viber or SMS on the staff phone with the message already written (free; staff press send). No paid SMS or WhatsApp API.

## Goals

- Take a booking in under 30 seconds while on the phone: phone number → name fills in → date, time, people → save.
- Host's **day list** for any date (BS shown first), with covers per hour.
- Reserved tables visible on the floor screen ahead of time; walk-ins warned before taking them.
- **Seat** a booking in one tap: opens the table's order with the party size as guest count.
- Late, no-show and cancelled bookings handled explicitly; the customer's no-show history visible next time they book.
- Confirmations and reminders through free channels the staff phone already has.

## Non-goals

- Public online booking page (question 1), booking widgets, Google/Facebook integrations.
- Deposits or prepayment (question 5); pre-ordered set menus (go in notes).
- Paid SMS/WhatsApp APIs; automatic reminders sent without a person.
- Waitlist / queue management for walk-ins (could follow later; separate item).
- Automatic table optimisation or capacity-based refusal of bookings.

## Anti-goals

- Booking slowing down peak-hour ordering: no new step in the walk-in flow except the one warning on a reserved table.
- A table silently double-booked for the same time.
- A booking quietly disappearing: cancelled and no-show bookings are kept, with who and why.
- A second guest/customer list next to the credit-customer one.

## Constraints

- Reuses `Customer` from the credit-customers design (name, phone, notes, optional business fields); new customers start at credit limit Rs 0, so creating one for a booking gives no credit.
- Times stored as UTC, interpreted in **Asia/Kathmandu (+05:45)**; "today" and day boundaries are Kathmandu days. Dates shown BS first, AD second, via `web/src/lib/billing/bs-date.ts`.
- Static export: one `/reservations/` route, the booking chosen by query string (`?id=`), like the guest page.
- Built first against the in-browser fake backend (`web/src/lib/api/mock/`); the Go developer implements the same contract. Every string in EN and NE.
- Order rules unchanged: one table per order, append-only items, `version` checks.

## Approaches considered

| | Approach | Verdict |
|---|---|---|
| **A (chosen)** | Staff-entered bookings; tables optional at booking and assigned on the day; soft hold on the floor screen from a set time before | Matches how bookings arrive (calls), flexible when the floor changes, no hard blocks at peak. Needs a host to assign tables on the day (the day list nudges this). |
| B | Table must be chosen at booking, hard-blocked for a fixed slot | Rejected: the person on the phone rarely knows the best table days ahead; hard blocks leave tables empty when parties are late or small. |
| C | Public booking page + staff entry | Rejected for now: spam and unconfirmed bookings, extra public API surface; most guests call (question 1). |

## Design

### Day list (`/reservations/`)

```
Reservations          ◀  मंसिर ९, २०८३ (Tue 24 Nov)  ▶  [Today]
Lunch 3 bookings · 9 covers     Dinner 7 bookings · 31 covers
┌──────────────────────────────────────────────────────────┐
│ 12:30  Sita K.      2  T3          Booked     [Seat]      │
│ 19:00  Ram Shrestha 4  —  (Garden) Booked     [Assign]    │
│ 19:30  Office party 14 T8+T9+T10   Booked ⚠ notes         │
│ 19:30  Hari B.      3  T5          Late 18m  [Seat][No-show]│
│ 18:00  Maya T.      2  T2          Seated                 │
│ 13:00  Bina R.      5  —           Cancelled              │
└──────────────────────────────────────────────────────────┘
                                         [+ New booking]
```

- Sorted by time; finished (seated/cancelled/no-show) rows greyed below the active ones (proposal).
- Covers per hour shown next to total seats, as a hint only (no refusal).
- Unassigned bookings for today show **Assign**: suggests the smallest free table(s) in the preferred area that fit the party and are not booked in that window (proposal).
- Navigation: a **Reservations** entry for all roles except kitchen; the floor screen shows a strip *Next: 19:00 Ram (4) · 19:30 Office (14)* that opens the list.

### New / edit booking

```
┌ New booking ─────────────────────────────┐
│ Phone  [98XXXXXXXX]  → Ram Shrestha ✓     │
│        2 visits · 1 no-show               │
│ Date   [मंसिर ९ (Tue)]  Time [19:00]       │
│ People [ 4 ]   Area [Any ▾]  Table [Later ▾]│
│ Notes  [birthday, cake at 8]              │
│ Came by [Phone ▾]                         │
│                 [Save]  [Save & send msg] │
└──────────────────────────────────────────┘
```

- **Phone first**: matching customers appear as you type; no match → enter a name and the customer is created inline on save. Name required; phone optional but strongly prompted (needed for messages and lookup) (proposal).
- History line from past bookings (visits, no-shows) helps judge regulars and repeat no-shows; derived, not stored.
- Date picker shows BS with AD beneath; quick chips *Today / Tomorrow*. Time in 15-minute steps.
- **Expected duration** defaults from Settings (90 min, 120 min for 9+ people) and is editable under "More" (proposal).
- Tables are optional; choosing them reserves them for the window. Several tables allowed (large groups), or **Whole area** for events (books every active table in that area).
- Source: Phone, In person, Message, Other (for reports only).

### On the floor screen

- From **hold-before** time (default 60 min) until the booking is seated, cancelled or released, an assigned table shows a badge *Reserved 19:30 · 4* (a new `reserved` tile state, only while the table has no open order).
- Opening a reserved table to start a walk-in order shows the warning from question 2; *Seat anyway* proceeds (normal order). The booking stays and the host sees *Table T5 now in use* on its row so they can reassign.
- After the booking time + grace (default 15 min) the badge turns *Late*; after **release-after** (default 30 min) the badge disappears and the row asks *No-show / Keep waiting* (keep = hold another 15 min).

### Seating

- **Seat** on a booking (day list, or from the reserved tile) → confirm table(s) and guest count (prefilled with party size) → creates a `DINE_IN` order on the first table with `guestCount` and `reservationId`; the waiter lands on that order screen.
- Extra tables for a big group show *With T8 (Office party)* until the order closes; this uses the booking's table list, not new orders (proposal). Bills stay one order, so split/merge rules are unchanged.
- If the chosen table already has an open order: pick another table, or seat onto that order (adds the link; no new order).
- **Arrived without booking record** (walk-in who says "I called"): find by phone in the list and seat; if none, it is just a walk-in.
- Seating needs a connection (like accepting a guest QR order). If offline, staff open the table normally and mark the booking seated later.

### Cancel, no-show, change

- **Cancel**: reason (presets *Guest cancelled*, *Restaurant cancelled*, free text); keeps the record.
- **No-show**: only by staff, only after the booking time.
- Edits (time, people, tables) re-check table conflicts. Undo a no-show/cancel within the same day back to Booked (mistakes) (proposal).

### Messages (free)

- **Send confirmation** (at save) and **Send reminder** (day list, any time) build a message in the customer's chosen language (EN/NE toggle in the dialog):
  *"Namaste Ram, your table for 4 at <Restaurant> is booked for Tue, मंसिर ९ at 7:00 PM. Call <phone> to change. — <Restaurant>"*
- Buttons: **WhatsApp** (`https://wa.me/977…?text=`), **SMS** (`sms:` link), **Share…** (Web Share API, reaches Viber/Messenger), **Copy**. The staff member's own phone sends; the system records `confirmedAt` / `remindedAt` when tapped (it cannot know if the message went).
- Day list filter *Not reminded* for tomorrow's bookings, as the habit to build (proposal).

### Settings (owner/manager)

Reservations on/off (hides the menu entry and floor badges); default duration (normal / large party threshold); hold-before minutes; grace minutes; release-after minutes.

### Reports and audit

- Dashboard: *Bookings today: 10 · 40 covers · 1 no-show*.
- **Reservations report** for a date range: bookings, covers, seated / cancelled / no-show counts, by source; CSV. Customer page lists their bookings.
- Audit actions `RESERVATION_CREATED`, `RESERVATION_CHANGED` (before/after), `RESERVATION_CANCELLED` (reason), `RESERVATION_NO_SHOW`, `RESERVATION_SEATED` (order id). Low-risk, but the trail answers "who gave away my booking?"

### Data (contract for the backend)

- `Reservation`: id, customerId, partySize, startsAt (ISO UTC), durationMin, areaId (nullable preference), tableIds[] (may be empty), wholeArea (bool), status (`BOOKED` | `SEATED` | `CANCELLED` | `NO_SHOW`), source (`PHONE` | `IN_PERSON` | `MESSAGE` | `OTHER`), notes, orderId (when seated), seatedAt, seatedById, cancelReason, cancelledAt, cancelledById, noShowAt, noShowById, confirmedAt, remindedAt, holdExtendedMin, createdById, createdAt, updatedById, updatedAt, version.
- "Late" and "released" are **derived** from startsAt, settings and now; not stored.
- `Order.reservationId` (nullable, new optional field).
- `Settings`: `reservationsEnabled`, `reservationDurationMin`, `reservationLargePartySize`, `reservationLargeDurationMin`, `reservationHoldBeforeMin`, `reservationGraceMin`, `reservationReleaseAfterMin`.
- `Customer`: **no new stored fields.** Optional additions to the `customers.get`/`list` response: `bookingCount`, `noShowCount`, `lastVisitAt` (derived). Phone stays optional on `Customer`; it is the search key.

### APIs

- `reservations.list({ from, to, status? })` (Kathmandu dates), `get(id)`.
- `reservations.create({ customerId | newCustomer: { name, phone }, partySize, startsAt, durationMin?, areaId?, tableIds?, wholeArea?, source, notes, requestId })`.
- `reservations.update({ id, baseVersion, ...fields })`, `cancel({ id, reason })`, `markNoShow({ id })`, `restore({ id })`, `extendHold({ id, minutes })`.
- `reservations.seat({ id, tableIds, guestCount, existingOrderId?, commandId })` → the order.
- `reservations.suggestTables({ startsAt, durationMin, partySize, areaId? })` → table sets.
- `reservations.markMessaged({ id, kind: "CONFIRMATION" | "REMINDER" })`.
- `reports.reservations({ from, to })`. Realtime: `reservation.updated`; floor screens refetch on it and on a minute tick for hold/late states.

### Server rules

- partySize 1–200; startsAt not more than 15 min in the past on create, at most 365 days ahead → `INVALID`.
- A table cannot be in two `BOOKED` reservations whose windows (startsAt → +duration) overlap → `TABLE_ALREADY_BOOKED` (lists the clashing booking). Unassigned bookings never clash.
- Status changes only `BOOKED → SEATED | CANCELLED | NO_SHOW`, and `CANCELLED | NO_SHOW → BOOKED` via `restore` the same Kathmandu day → else `INVALID_STATUS`. `NO_SHOW` before startsAt → `TOO_EARLY`.
- `seat` creates the order and updates the reservation in one transaction; idempotent on `commandId`; table with an open order and no `existingOrderId` → `TABLE_OCCUPIED`; `version` → `STALE_VERSION`.
- Edits/cancel/no-show allowed for OWNER, MANAGER, CASHIER, WAITER; KITCHEN → `NOT_ALLOWED`. Reservations disabled → `RESERVATIONS_OFF`.
- `requestId` makes create idempotent (double tap while on the phone). Audit entries in the same transaction.

## Calls made (correct when picking this up)

- Customer reused as the booker; no separate guest entity; no new stored Customer fields.
- Tables optional at booking; soft hold, never a hard block; no automatic no-shows.
- Large groups = multiple tables or whole area, one order on the first table.
- Durations, hold, grace and release times are starting values in Settings.
- Messages go from the staff phone via links/share sheet; "confirmed/reminded" means the button was tapped.

## Left to the builder

Screen layouts and colours of the `reserved`/`late` tile states and how they combine with guest-order badges; date/time picker details; exact message wording (EN + NE); where the Reservations entry sits in each role's navigation; suggestion algorithm details beyond "smallest fitting free tables in the preferred area".
