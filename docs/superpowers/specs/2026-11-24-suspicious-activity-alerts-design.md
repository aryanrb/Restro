# Suspicious activity alerts — design

Status: **Designed, not scheduled.** Owner decisions taken 2026-11-24. Phase 2. Covers "suspicious activity alerts" in the `RESTAURANT_PLAN.md` roadmap and "end-of-day summary notification, leakage alerts" from the problems table.

## Why

The MVP already makes leakage *traceable*: voids, discounts, cancellations, reprints, price changes and shift counts all land in the audit log, and the risky ones need a manager PIN. But nobody reads an audit log of 400 lines a day. The owner can't be everywhere, so the system should read it for them: point out the handful of events each day that look like money walking out, tell the owner on their phone, and let them mark each one "checked".

## Owner decisions

All recommended answers below were accepted by the owner on 2026-11-24.

Each has a recommended answer; the design below assumes the recommendation until the owner says otherwise.

1. **Who receives alerts?** Owner only, or managers too? — *Recommended:* owner only. A manager can be added per person, but never sees alerts about their own actions or approvals.
2. **What should interrupt you immediately, and what can wait for the end-of-day summary?** — *Recommended:* only **High** alerts (cash short over Rs 500, bill cancelled and re-billed lower, served but never billed, manager PIN locked out, after-hours login) send a phone notification right away; everything else waits for the summary.
3. **Are the starting thresholds right for your restaurant?** (cash short Rs 500; discount 20% or Rs 1,000; 3 voids by one person in a shift; reprint of a bill older than 1 hour) — *Recommended:* start with these, review after two weeks; all are editable in Settings.
4. **What are your opening hours, and should logins outside them alert?** — *Recommended:* yes, with 30 minutes of grace either side; enter the hours per weekday in Settings.
5. **Where do you want the end-of-day summary?** Phone notification, email, or both? — *Recommended:* phone notification that opens the summary page, plus a **Share** button so you can forward it to a partner on WhatsApp yourself (free). Email only if you want an archive.
6. **Should staff be told the system flags unusual activity?** — *Recommended:* yes. Knowing it is watched prevents more than catching it after. Staff never see the alerts themselves.

## Goals

- A small set of concrete rules that turn audit events into alerts, each with a tunable threshold and on/off switch.
- Alerts in three places: a count on the owner dashboard, an **Alerts** list, and the daily summary.
- Free delivery to the owner's phone: browser push notifications (no SMS, no WhatsApp API).
- An end-of-day summary the owner gets without opening the app at closing time.
- Owner marks each alert **Checked — OK** or **Problem**, with an optional note; the trail is kept.

## Non-goals

- Automatic punishment or blocking (alerts never stop a sale or lock an account).
- Machine-learning "anomaly scores"; rules are plain and explainable.
- Stock/inventory variance alerts (separate Phase 2 inventory item).
- Paid channels: SMS gateways, WhatsApp Business API.

## Anti-goals

- An owner flooded with notifications until they mute everything.
- A manager able to hide, delete or resolve an alert about themselves.
- A missed alert because the event and the alert were written separately.
- Slowing the ordering or billing flow (rules run server-side after the write, no extra taps).

## Constraints

- Hosting is cloud, one deployment per restaurant (MVP spec): the server can send push and run a daily timer; no broker or worker service.
- Web app is a Next.js static export used as a PWA; push must be standard Web Push (VAPID, self-hosted keys, free). Android Chrome works in a tab; iPhone needs the app added to the home screen (iOS 16.4+).
- Alerts are built from what the audit log and shifts already record; a few new audit actions are added (below).
- Built first against the in-browser fake backend (`web/src/lib/api/mock/`); the Go developer implements the same contract. EN + NE strings.

## Approaches considered

| | Approach | Verdict |
|---|---|---|
| **A (chosen)** | Server evaluates rules **when each audit entry is written**, plus one daily job for the summary | Alerts appear within seconds, same transaction as the event so none are lost, rules can look back over the shift/day with a simple query. |
| B | Nightly batch over the day's audit log only | Rejected: a Rs 5,000 cash shortage or a cancel-and-rebill is found the next morning, after the staff member has gone home. |
| C | Compute alerts in the owner's browser from the audit log | Rejected: nothing happens unless the owner opens the app; no push; the rules would live in two places. |

## Design

### Rules (proposed starting set — all thresholds are proposals, editable in Settings)

| Rule | Fires when | Start threshold | Severity |
|---|---|---|---|
| Void after kitchen started | Item voided while `PREPARING`/`READY`/`SERVED` and value ≥ X | Rs 500 | Normal |
| Repeated voids | One person's post-send voids in one shift ≥ N | 3 | Normal |
| Self-approved | A manager approves their own void/discount/cancel with their own PIN | any | Normal |
| High discount | One bill's discount ≥ P% **or** ≥ amount | 20% / Rs 1,000 | Normal |
| Discount total | One person's discounts in a business day ≥ amount | Rs 3,000 | Normal |
| Cancelled and re-billed lower | Bill cancelled, then the same order re-billed for less within 2 h | any decrease | **High** |
| Late cancellation | Bill cancelled more than M minutes after issue | 60 min | Normal |
| Reprint of an old bill | Reprint of a bill issued more than H hours ago, or 3rd+ reprint | 1 h / 3 | Normal |
| Cash short | Shift closed with `difference` ≤ −X | Rs 500 | **High** |
| Cash over | Shift closed with `difference` ≥ +X (unrecorded sales) | Rs 1,000 | Normal |
| Repeated shortages | Same person short in ≥ N shifts in 7 days | 3 | **High** |
| Served but never billed | Order cancelled/closed while it has `SERVED` items not on any issued bill | any | **High** |
| After-hours login | Any login outside opening hours ± grace | 30 min | **High** |
| Manager PIN locked | Manager PIN approvals locked out after failed attempts | any | **High** |
| Price dip | An item's price is lowered and raised back within the same day, with sales in between | any | Normal |
| Credit over limit | On-account bill over the limit approved (credit design) | any | Normal |
| Credit to a new account | Credit given to an account created the same day by the same cashier | any | Normal |
| Large till paid-out | Supplier paid from the till above the till limit, or one person's paid-outs in a shift ≥ amount (purchases design) | Rs 5,000 / Rs 10,000 | Normal |
| High waste or stock loss | Waste logged in a business day ≥ amount, or a posted count with loss ≥ amount (inventory design) | Rs 2,000 / Rs 3,000 | Normal |
| Loyalty rules (4) | Staff number on a bill, one cashier feeding one member, many redemptions, large stamp adjustment — defined in `2026-11-24-loyalty-design.md` | see loyalty design | Normal |

- Business day = opening time to opening time next day (so late-night bills count on the right day).
- Rules that look back (repeated voids, shortages, price dip) query the audit log / shifts at write time; no extra state.

### Not overwhelming the owner

- **Grouping:** a second firing of the same rule for the same person in the same shift updates the open alert ("Repeated voids · Hari · 5 voids") instead of creating a new one.
- **Only High alerts push immediately**; Normal ones wait for the daily summary (owner can switch any rule to High).
- **Push cap:** at most 5 immediate pushes per business day; after that, one "more alerts waiting" push and the rest go to the summary.
- **Quiet hours** for push (default: none; owner can set e.g. 23:00–07:00); alerts still collect.
- Per-rule **off switch**. Turning a rule off is audit-logged.

### Owner dashboard

```
┌──────────────────────────────────────┐
│ Today  Sales Rs 84,200 · 112 bills   │
│ ⚠ Alerts: 2 high · 4 to check   [>] │
│ Voids 6 · Discounts Rs 2,150 · ...   │
└──────────────────────────────────────┘
```

### Alerts list (Owner → Alerts)

```
┌───────────────────────────────────────────────┐
│ [To check (6)] [Checked] [All]   Rule ▾ Staff ▾│
│ HIGH  Cash short Rs 1,200 · Sita · Shift 2    │
│       closed 22:41 · expected 18,450 counted… │
│ HIGH  Cancelled & re-billed lower · E-2083/84-41│
│       Rs 2,340 → Rs 1,640 · Hari, approved Ram│
│       Repeated voids · Hari · 4 voids · T4,T7 │
│ ─────────────────────────────────────────────  │
│ Tap → detail: what happened, linked audit     │
│ entries, bill/shift links,                    │
│ [Checked — OK]  [Problem]  note: ________     │
└───────────────────────────────────────────────┘
```

- States: **To check** → **Checked — OK** or **Problem** (note optional for OK, required for Problem). Resolving many at once allowed ("mark all Normal as OK").
- Alerts are never deleted; resolution can be changed by the owner, and every resolution is audit-logged.

### End-of-day summary

- Generated once per business day at **closing time + 30 min**, or when the last open shift closes, whichever is later (proposal).
- Contents: sales, bills, by method, discounts, voids, cancellations, each shift's expected vs counted cash, credit given/repaid (when credit exists), and the day's alerts grouped by rule.
- Delivered by push ("Today: Rs 84,200 · 6 alerts to check") opening the summary page; the page has **Share** (phone share sheet → WhatsApp/Viber/email, sent by the owner, free) and **Print/PDF**.
- Optional email through an SMTP account the owner provides (e.g. their own Gmail app password); off by default.

### Push setup

- Owner → Settings → Notifications → **Turn on for this phone** (browser permission prompt), **Send test**. Several phones per owner allowed; each listed with device name and a remove button.
- Push text shows rule and amount, not staff names (lock-screen privacy); names appear in the app.

### Data (contract for the backend)

- `AlertRuleSetting`: rule (enum), enabled, severity, thresholds (rule-specific numbers: paisa, basis points, counts, minutes). Defaults shipped in code; only overrides stored.
- `Alert`: id, rule, severity, status (`OPEN`, `OK`, `PROBLEM`), subjectUserId, subject (bill/shift/item label), detail, count, businessDate, firstAt, lastAt, shiftId?, invoiceId?, auditEntryIds[], resolvedById, resolvedAt, note.
- `PushSubscription`: id, userId, endpoint, p256dh, auth, deviceLabel, createdAt.
- `DaySummary`: businessDate, payload (JSON totals), generatedAt, pushedAt, emailedAt.
- Settings additions: opening hours per weekday, after-hours grace, push quiet hours, daily push cap, summary email + SMTP (secret stored server-side only), alert recipients (user ids).
- New audit actions: `MANAGER_PIN_LOCKED`, `ORDER_CANCELLED`, `ALERT_RESOLVED`, `ALERT_RULES_CHANGED`, `NOTIFICATIONS_CHANGED`.

### APIs

- `alerts.list({ status?, from?, to?, rule?, userId? })`, `alerts.get(id)` (with linked audit entries), `alerts.resolve({ ids, status, note })`.
- `alerts.rules()`, `alerts.updateRules(settings)`.
- `notifications.vapidKey()`, `notifications.subscribe({ endpoint, keys, deviceLabel })`, `notifications.unsubscribe(id)`, `notifications.list()`, `notifications.test()`.
- `reports.daySummary({ date })`.
- `Dashboard` gains `alerts: { high: number; open: number }`.
- Realtime: `alert.created`, `alert.updated` — sent only to sessions of alert recipients.

### Server rules

- Rules run in the same transaction as the audit entry that triggers them; a rule error is logged and never fails the sale/void/close.
- Alerts can't be deleted. Only the owner (or a listed recipient) can read or resolve; a recipient never sees or resolves alerts where they are the subject or approver → `NOT_ALLOWED`.
- Only the owner changes rules, recipients and notification settings → `NOT_ALLOWED`.
- Push sending is fire-and-forget after commit; a subscription returning 404/410 is removed.
- The daily summary job is an in-process timer (one deployment = one process); on restart it generates any missed summary for the previous business day once.
- Fake backend: rules run inside `audit()` in `mock-store.ts`; push is simulated with the browser Notification API on the same device.

## Calls made (correct when picking this up)

- Alerts never block anything; they only inform.
- Grouping per rule + person + shift; push cap 5/day; Normal alerts only in the summary.
- Web Push + share sheet as the free channels; email optional via the owner's own SMTP.
- Push text omits staff names.
- Business day runs opening time to opening time.
- Summary at closing + 30 min or last shift close, whichever is later.

## Left to the builder

Screen layouts, exact rule copy in EN/NE, summary page layout, where Alerts lives in the owner menu (proposed: Owner → Alerts, plus the dashboard tile), service worker details for push, how look-back queries are indexed.
