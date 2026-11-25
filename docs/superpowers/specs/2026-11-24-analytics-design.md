# Analytics and live operations — design

Status: **Designed, not scheduled.** Owner decisions taken 2026-11-24. Phase 4. Covers "analytics" in the `RESTAURANT_PLAN.md` roadmap, in two parts: a **live operations view** (what is happening right now) and **analytics** (trends over days, months and years). Builds on the MVP's live dashboard and daily reports (`web/src/lib/api/reports-api.ts`, `web/src/components/reports/*`). Reads data from credit customers, inventory & wastage, purchases & suppliers, loyalty, reservations and suspicious activity alerts (all `2026-11-24-*-design.md`) **when those features exist**, and works without them. Every number, threshold and screen below is a proposal until the owner answers the questions.

## Why

The MVP tells the owner what happened **today** (dashboard) and lists **what happened on each day** (daily sales, sales book, cancelled bills, audit). It does not answer the questions an owner asks when deciding things: *Are we doing better than last Mangsir? Was this Dashain better or worse than last Dashain? Which hours need two more waiters? Which dishes earn money and which just take up space on the menu? Who gives away the most discounts?* Today the owner would have to export CSVs and build a spreadsheet. This design answers those questions on the owner's phone, in rupees, in BS months, with festivals marked, and with no extra work from staff.

The owner is also often away from the restaurant (at the market, at home, at a second business) and today has to phone the manager to find out how service is going. The dashboard shows totals, but not *which* tables are waiting, which kitchen tickets are late, or that a large discount was given five minutes ago. The live view lets the owner watch the floor, the kitchen and the till from their phone, as it happens.

## Owner decisions

All recommended answers below were accepted by the owner on 2026-11-24.

Each has a recommended answer; the design below assumes the recommendation until the owner says otherwise.

1. **Who sees the live view and analytics?** — *Recommended:* the **live view: owner and managers** (managers run the floor and need it most). **Analytics:** the owner sees everything. Managers see sales trends, busy hours, menu, tables and payment mix, but **not** the staff section (it compares their colleagues and their own approvals). The owner can turn the staff section on for a named manager. Cashiers, waiters and kitchen staff see neither.
2. **Which calendar should comparisons use?** — *Recommended:* **BS months and the fiscal year (Shrawan–Ashadh)** by default, because that is how the owner, the accountant and IRD think. Weeks run Sunday–Saturday. An AD toggle is on every screen.
3. **How should festivals be handled in comparisons?** — *Recommended:* the app ships a list of the main festivals (Dashain, Tihar, Chhath, Holi, Teej, Shivaratri, Lhosar, BS New Year) with this year's and next year's dates. The owner can edit it and add their own days (local jatra, bandh, closed for repairs). Charts mark these days, and the owner can compare **"this Dashain vs last Dashain"** directly. Closed days are left out of averages.
4. **How should staff performance be shown?** — *Recommended:* show it as **rates, not rankings**: sales per cover, voids per 100 items and discount as a % of sales, each next to the restaurant average, with no leaderboard. Only the owner sees it (see 1), and staff never see each other's numbers. It is meant to start a conversation, not to punish anyone.
5. **How far back do you want to look?** — *Recommended:* screens default to the current BS month and offer up to **3 fiscal years** per view. Nothing is ever deleted for analytics. Bills are kept as long as tax law requires (period to confirm with the accountant), and the daily summaries are small enough to keep forever.
6. **Phone or computer?** — *Recommended:* **phone first**: one chart per card, scroll down, tap for numbers. On a computer the same page uses two columns and shows the full tables. Print to PDF is available on both. The live view is the owner's **home screen** on a phone, with Analytics one tap away.
7. **Should staff be told the owner can watch service live, and should the live view show who is serving each table?** — *Recommended:* **yes to both.** Show the waiter's name and the running total on each table, and tell staff (as with alerts) that the owner can see the floor, kitchen and till from their phone. Knowing this makes service more careful, and hiding it would feel like spying once staff found out.

## Goals

- **Live view:** every table's state right now, each open order (items, waiter, time since seated and since last send, running total), the kitchen (tickets per station, waiting times, late tickets), the till (open shifts, takings so far by method) and exceptions (voids, discounts, cancellations) as they happen. It updates within seconds and is **read-only**.
- Trends: sales, bills, covers, average bill and average per cover by day / week / BS month / fiscal year, against **the previous period** or **the same period last year**, with festivals marked.
- Busy hours: a weekday × hour heatmap (sales, orders or covers), averaged per weekday so that a month with five Fridays is not skewed.
- Menu: best and worst sellers by quantity and revenue. When inventory recipes and costs exist, **menu engineering** (Star / Plowhorse / Puzzle / Dog).
- Tables: table turnover and dwell time for dine-in; takeaway share.
- Staff (owner): sales, covers, voids and discounts per person, as rates against the average.
- Leakage trends: discounts, voids (before and after cooking), bill cancellations and alerts over time.
- Payment mix over time (cash / Fonepay QR / other QR / card / on account).
- Customers, only where identified (loyalty, credit, reservations): repeat visits, member share of sales, no-show rate.
- Cost side when built: food cost %, wastage and variance trend, purchases by month.
- Every table can be exported to CSV, and every page can be printed to PDF (existing export helpers).

## Non-goals

- Forecasting, "AI insights" and machine-learning predictions. A few plain-language highlights are produced by fixed rules (below).
- A general report builder or custom SQL. The set of screens is fixed and the owner picks filters only.
- Accounting statements (P&L, balance sheet, VAT returns). Those belong to the accountant and IRD reports.
- External BI tools (Metabase, Google Data Studio) and data feeds to third parties.
- Labour cost and staff scheduling (there is no attendance or payroll data).
- Any action from the live view: no editing orders, voiding, approving, 86-ing or messaging staff (see "Why read-only").
- Multi-branch comparison and online ordering/delivery channels: dropped for now. Daily summaries can gain a branch key later.

## Anti-goals

- Analytics that disagree with the daily sales report for the same day. Both use one definition of "sales".
- A staff screen that shames people or leaks to staff, or one that compares a waiter who worked 3 lunch shifts with one who worked 25 dinners as if they were the same.
- Comparisons that quietly mix up a festival and a normal period ("sales up 60%!" because Tihar fell in this month).
- An analytics query that slows down ordering or billing at peak hour.
- Charts that can't be read on a 6-inch phone, or that hide the actual numbers.
- A live view that is silently stale: if the connection drops, the screen must say so and show when it was last updated.
- Watching that turns into remote control, with the owner changing orders from home while staff work on the same table.

## Constraints

- Built first against the in-browser fake backend (`web/src/lib/api/mock/`). The Go developer implements the same contract on Postgres. The web app is a static export, so all computation happens on the server (or the mock), not in page code.
- Money in integer paisa. Display uses `formatMoney` (lakh grouping, `Rs 1,23,456.00`). Chart axes use a short form (`Rs 12.4 lakh`).
- Dates: the business day is the **Kathmandu calendar date** (`kathmanduDate`), the same as `dailySales`. BS dates and the fiscal year come from `toBs`. If a business-day cut-off (for example 4 a.m.) is introduced later, analytics follows it.
- Sales = `totals.total` of **ISSUED** invoices (the definition the dashboard and daily sales use). Estimates are included when the pilot billing mode is on, as in the daily sales report.
- Sends arrive late through the offline outbox, and old bills can be cancelled. Any past day can therefore change after the fact.
- All UI strings in EN and NE. Item names are `LocalText`.
- Realtime today is a thin "something changed" feed (`web/src/lib/realtime/`): `{ type, id, version? }` events (`order.updated`, `table.updated`, `invoice.issued`, `shift.updated`, `item.ready`, `menu.updated`, `settings.updated`). Clients then refetch over the API. The live view keeps this model and does not add a second push channel.

## Approaches considered

How the live view gets its data:

| | Approach | Verdict |
|---|---|---|
| L1 | Reuse the floor, kitchen and cashier screens' queries as they are, side by side | Rejected: three to five queries per change, logic about states repeated in the client, and staff-screen APIs that include actions the owner must not have. |
| **L2 (chosen)** | **One read-only `live.snapshot()` built on the server (open orders, stations, open shifts, today's totals, recent exceptions), refetched when any existing realtime event arrives (throttled), with a 30 s poll as backup.** Timers tick on the client from timestamps. | One small query over open orders only, so it is cheap. Table and kitchen states are computed once on the server so the mock and Go agree. No new transport is needed. |
| L3 | Push full state diffs over the socket | Rejected for now: changes the realtime contract every screen uses. It could be added later if the snapshot gets heavy. |

How analytics is computed:

| | Approach | Verdict |
|---|---|---|
| A | Live queries over invoices/orders for every screen | Rejected as the only approach: a year of heatmap or menu data scans tens of thousands of lines on the same database that serves peak-hour ordering. Fine for today and short ranges. |
| **B (chosen)** | **Daily summary tables per business day (totals, per item, per hour, per staff, per table) for past days, plus a live query for today.** Summaries are rebuilt from raw data, so they can always be thrown away and rebuilt. | Fast at any range, cheap on one small cloud database, and simple for the Go developer (plain `GROUP BY` into small tables). Late changes mark the day **dirty**, and it is rebuilt. |
| C | Separate analytics database / warehouse / external BI | Rejected: one deployment per small restaurant, and it adds cost and a second system to run. |
| D | Send raw data to the browser and compute there | Rejected: slow on phones, leaks cost and staff data to the client, and the static app has no secure place to do it. |

## Design

### Live view (Owner → Live, `/admin/live/`), phone layout

```
Live ● updated 3 s ago                      Tue 19:42 · 26 Mangsir
┌────────────────────────────────────────┐
│ Sales today Rs 61,200 · 74 bills       │
│ Tables 11/18 busy · 38 guests seated   │
│ Kitchen: 2 late ● 3 slow   Alerts: 1 ⚠ │
└────────────────────────────────────────┘
 [Floor] [Kitchen] [Till] [Exceptions]
 Floor — sorted by "needs attention"     [Map | List]
┌────────────────────────────────────────┐
│ T4  🧾 Bill asked 6 m · Ram · 4 guests  │
│     Rs 3,450 · seated 52 m             │
│ T9  🔥 Cooking · late 22 m (Momo ×2)    │
│     Sita · 2 guests · Rs 1,180         │
│ T2  ✅ Ready 4 m — not served           │
│ T7  🍽 Served · last send 35 m · Rs 2,900│
│ T1  🪑 Seated 9 m · nothing ordered     │
│ T3 T5 T6 T8 … Free                     │
│ Takeaway  #12 Cooking 8 m · Rs 640     │
└────────────────────────────────────────┘
```

- **Table states** (computed on the server, in order of display priority): `BILL_REQUESTED` (guest "Ask for bill" from the guest-ordering design, or a waiter's **Bill please** tap; proposal) → `READY` (an item is ready and not served) → `COOKING` (an item is `PREPARING`) → `ORDERED` (items `SENT`, kitchen not started) → `SERVED` (all live items served) → `SEATED` (order open, nothing sent) → `BILLED` (bills issued for some or all items, order still open, e.g. pay-first or a split) → `FREE`. A table with several orders shows the most urgent state and a count badge, as `TableTile` does today.
- **Map** reuses the floor screen's area grid and `TableTile` (extended to these states, with colour plus an icon and a word so it doesn't rely on colour). **List** (the default on a phone) puts the tables that need attention first: bill asked, late, ready-not-served, seated with nothing ordered for more than 10 min (proposal).
- **Tap a table** → read-only order card: waiter, guests, seated at / for, last send, running total (sum of live lines at their snapshotted prices, before service charge and VAT, labelled "approx."), and each item with its status and minutes in that status. Voided lines are struck through with the reason and approver. Billed items are marked with the bill number.

```
Kitchen                                  yellow 10 m · red 20 m (settings)
┌────────────────────────────────────────┐
│ Main kitchen  6 tickets · 14 items     │
│   oldest 24 m ● · 2 late · 1 slow       │
│ Bar           2 tickets · 3 items · 4 m │
│ Late: T9 Momo ×2 22 m · #12 Chowmein 21 m│
└────────────────────────────────────────┘
Till
│ Gita (cashier) since 10:02 · float Rs 5,000            │
│ Cash 38,400 · Fonepay 17,900 · Other QR 0 · Card 4,900 │
│ Expected in drawer Rs 43,400                           │
│ Money on tables (not yet billed) Rs 12,730             │
Exceptions (today, newest first)
│ 19:31 Discount 15% Rs 690 · T4 · by Ram · ok'd Hari  ⚠ alert │
│ 19:05 Void after cooking · Chicken Chilli · T7 · Sita        │
│ 18:40 Bill E-2083/84-311 cancelled Rs 2,100 · re-billed 1,800│
```

- **Kitchen** uses the same ticket grouping and `kdsYellowMinutes` / `kdsRedMinutes` as the KDS (`kitchen-view.tsx`, `ticket-card.tsx`): per station, the number of tickets and items by status, the oldest wait, and slow (yellow) and late (red) counts, plus a list of late items. 86'd items appear on a single line.
- **Till:** each open shift with cashier, opened at, float, takings so far by method, and expected cash. Counted cash appears only after closing (blind count, as in the shift design). "Money on tables" = running totals of open orders not yet billed. Credit given today is shown when credit customers exist.
- **Exceptions:** voids, discounts, bill cancellations, reprints, 86s, shift closes with a difference, and manager PIN lockouts, taken from the audit log as they happen. Each one links to its alert when the alerts design has raised one; the alerts design still decides what pushes to the phone. The live view itself sends no notifications.
- **Freshness:** a dot shows `● Live`, `◌ Reconnecting…` or `○ Offline — showing 19:38`. Minute counters keep ticking locally from timestamps. While offline, the screen greys out and says so.

### Why read-only

- The owner is usually not in the room. Changing an order from afar collides with the waiter or cashier working on the same order (version conflicts) and confuses guests and kitchen.
- Voids, discounts and cancellations need a reason and an in-person manager PIN. That trail would be weakened if the owner could do them remotely from a monitoring screen.
- Keeping the view free of actions also keeps its API free of write rights, so a lost owner phone can watch but not change anything. When the owner is on site and wants to act, they use the normal staff screens with their own login. From afar, they call the manager.

### Analytics home (Owner → Analytics), phone layout

```
Analytics        [Mangsir 2083 ▾]  vs [Kartik ▾ | Mangsir 2082]
┌────────────────────────────────────────┐
│ Sales Rs 24.8 lakh   ▲ 12% vs Kartik   │
│ ⓘ Kartik had Tihar (5 days)            │  ← festival note
│ ╱╲__╱‾‾╲_╱‾  (this)   ┄┄┄ (compared)   │  line, compared period ghosted
├────────────────────────────────────────┤
│ Bills 3,104 ▲4% · Avg bill Rs 799 ▲8%  │
│ Covers 7,950 · Per cover Rs 312        │
├────────────────────────────────────────┤
│ Highlights                             │
│ • Saturdays 12–2 pm are your busiest   │
│ • Chicken Chilli sales down 30%        │
│ • Discounts 4.1% of sales (avg 2.6%)   │
└────────────────────────────────────────┘
 [Trend] [Hours] [Menu] [Tables] [Staff] [Money] [Customers]
```

- Period picker: Today, This week, BS month, Fiscal year, Custom, plus **festival periods** from the calendar list. The comparison is either the previous period of equal length or the same BS period last year (same festival when a festival is picked).
- **Highlights (proposal):** up to 5 sentences picked by fixed rules (busiest slot; an item up or down more than 25% with at least 20 sold; discounts/voids above 1.5× their 3-month average; payment mix shift above 10 points; a festival in only one of the two periods). There is no free text generation.
- Tabs that need a feature which isn't built (Customers without loyalty/credit, cost columns without inventory) are hidden. `analytics.meta()` says which ones exist.

### Trend

- Grain: day (with weekday letters and festival markers), week, BS month, fiscal year. Metric picker: sales, bills, covers, average bill, per cover, dine-in vs takeaway.
- Under the chart: a table with one row per period (BS label, AD range, value, compared value, change %). CSV export.
- Closed days (calendar kind `CLOSED`, or zero bills on a day marked closed) are shown as gaps and left out of daily averages.

### Busy hours

```
        6  8  10 12 14 16 18 20 22
Sun     ·  ░  ▒  ▓  ▒  ░  ▒  ▓  ░
...
Sat     ·  ░  ▓  █  ▓  ▒  ▓  █  ▒     tap a cell → "Sat 13:00 · avg Rs 18,400 · 41 covers"
```

- Metric: sales (by bill `issuedAt`), orders or covers (by order `createdAt`, when guests arrive). The value is an **average per weekday occurrence** in the range. The hour range shown is trimmed to opening hours (from alerts settings when they exist; otherwise hours with any activity).
- On a phone the grid is 7 rows × about 17 columns of small squares. A single-day view shows a bar chart by hour instead.

### Menu

- **Best/worst sellers:** quantity, revenue, share of sales and change vs the compared period, filtered by category. On a phone these are horizontal bar lists (top 10 and bottom 10). Items with zero sales in the range are listed separately ("on the menu, never ordered").
- **Menu engineering (only with inventory recipes and costs):** cost per unit sold is the **actual** cost from the `SALE` stock movements of that order item (unit cost snapshot), so it is never recomputed from today's prices. Margin = price − cost. Each item in a category is placed by popularity (share ≥ 70% of an equal share, the usual Kasavana–Smith rule) and margin (≥ the category's weighted average):

```
             High margin               Low margin
Popular      ★ Star: keep, feature     🐴 Plowhorse: re-price or cut cost
Unpopular    ? Puzzle: promote/rename  ✗ Dog: consider removing
```

- On a phone this is four lists. On a wide screen it is also a scatter chart. Items without a recipe are listed as "no cost yet" and are not classified. Food cost % per item and overall comes from the same data.

### Tables

- Dine-in only. **Dwell time** = order `createdAt` → last invoice `issuedAt` of that order (there is no "table cleared" event; proposal). Shown as median and 90th percentile by table, area, weekday and lunch/dinner. Merged and moved orders count once, on the final table.
- **Turnover** = dine-in orders per table per open day, and covers per seat when seat counts exist. With reservations: bookings vs walk-ins, covers from bookings, no-show rate.

### Staff (owner; managers only if enabled per question 1)

| Person | Days worked | Sales (as waiter) | Per cover | Voids / 100 items | Discount % of sales they billed | After-cooking voids approved |
|---|---|---|---|---|---|---|

- Waiter figures use `Order.waiterId`. Discounts and cancellations use `Invoice.issuedById` / `cancelledById`. Voids use `voidedById`, and approvals use `voidApprovedById`/`cancelApprovedById`.
- **Fairness:** "days worked" = business days with at least one order or bill by that person. Figures are per day or per cover, each shown next to the restaurant average. People with fewer than 5 days in the range are greyed out as "too little data". There is no sorting by "worst". Alert counts per person (alerts doc) link to the alerts list rather than being scored.

### Money

- **Payment mix:** a stacked bar per period by method (+ `ON_ACCOUNT` when credit exists). Fonepay share is useful for reconciling with the merchant statement.
- **Leakage:** discounts (Rs and % of sales, top reasons by `discountReason`), voids before/after cooking, bill cancellations, cash short/over from shifts, and alerts by rule per week (when alerts exist).
- **Cost side (when built):** purchases by month and supplier, wastage by reason, stock variance per count, credit owed at month end, and loyalty rewards given.

### Customers (only when loyalty, credit or reservations exist)

- Identified share of sales (bills with `customerId`), new vs returning members, repeat rate (members with 2+ visits in the range), average days between visits, top customers by spend (owner only; phone numbers masked). Labelled clearly as **identified customers only**: walk-in guests without a number are invisible.

### Exports and retention

- Every table: CSV (`toCsv`). Every page: print/PDF with the period, comparison and "figures up to 10:42" in the header. **Monthly summary PDF (proposal):** one BS month on two pages for a partner or the accountant.
- Analytics never deletes source data. Summaries are kept forever; the retention of raw bills follows tax law (question 5).

## Data (contract for the backend)

Live view (computed on each call, nothing new stored except one optional field):

- `Order.billRequestedAt` (nullable; **optional addition**), set by a waiter's "Bill please" tap or an accepted guest `BILL` request, and cleared when a bill is issued.
- `LiveSnapshot`: `at`, `today` {sales, bills, covers, byMethod, alertsOpen}, `tables` [{tableId, areaId, state, orders: [`LiveOrder`]}], `takeaway` [`LiveOrder`], `stations` [{stationId, tickets, items: {sent, preparing, ready}, oldestMinutes, slow, late, lateItems: [{orderId, place, name, quantity, minutes}]}], `items86` [name], `tills` [{shiftId, userName, openedAt, openingFloat, takings, expectedCash}], `moneyOnTables`, `creditToday?`, `exceptions` [`AuditEntry` + alertId?] (today, newest 50).
- `LiveOrder`: id, type, place, waiterName, guestCount, createdAt, lastSentAt, billRequestedAt, state, runningTotal, billedTotal, itemCounts by status, oldestWaitingMinutes.

Analytics:

- `AnalyticsDay` (one row per business date): date, dateBs, fiscalYear, sales, bills, covers, dineInOrders, takeawayOrders, discounts, serviceCharge, vat, byMethod, voidsBefore {count, value}, voidsAfter {count, value}, cancelled {count, value}, cashDifference, dwellMedianMin, dirty, builtAt.
- `AnalyticsItemDay`: date, menuItemId, variantId, name (`LocalText` snapshot), categoryId, quantity, revenue, cost (null without inventory), voidedQty.
- `AnalyticsHour`: date, hour (0–23, Kathmandu), sales, bills, orders, covers.
- `AnalyticsStaffDay`: date, userId, ordersAsWaiter, salesAsWaiter, covers, itemsAdded, itemsVoided, voidValue, billsIssued, discountGiven, salesBilled, cancellations, approvals.
- `AnalyticsTableDay`: date, tableId, orders, covers, dwellMinutes[] (or sum/count plus a percentile sketch; left to the builder).
- `CalendarEvent`: id, name (`LocalText`), from, to (AD dates), kind (`FESTIVAL | CLOSED | OTHER`), builtIn, createdById. Built-ins are seeded per fiscal year and can be edited.
- Settings: `analytics: { managersSeeStaff: userId[], highlightRules: on/off }`.

## APIs

- `live.snapshot()` → `LiveSnapshot`. `live.order(orderId)` → the read-only order card (items with status, timestamps, void reason/approver, bill numbers). Neither exposes any write.
- `orders.requestBill(orderId)` / `orders.clearBillRequest(orderId)` (waiter, cashier, manager) for the optional "Bill please" state. Emits `order.updated`.
- **Realtime:** the live view refetches the snapshot (at most once every 2 s, trailing) on the existing `order.updated`, `table.updated`, `invoice.issued`, `shift.updated`, `item.ready`, `menu.updated` (86) and `settings.updated` (timer thresholds). **New events:** `audit.created` `{ id, action }` for exceptions (sent only to owner/manager sessions), `invoice.cancelled` `{ id }` (also useful to the cashier list), and `guestRequest.updated` from the guest-ordering design when built. With alerts built, `alert.created` / `alert.updated` mark the exceptions. On reconnect the view refetches once (existing provider behaviour), and it polls every 30 s as a backup.
- `analytics.meta()` → modules available (inventory, loyalty, credit, reservations, alerts), first business date with data, last build time.
- `analytics.trend({ range, grain: "DAY"|"WEEK"|"MONTH_BS"|"FY", compare: "PREVIOUS"|"LAST_YEAR"|"NONE" })` → series of periods with all headline metrics + compared values + festivals in each period.
- `analytics.hours({ range, metric: "SALES"|"ORDERS"|"COVERS" })` → 7 × 24 averages + occurrence counts.
- `analytics.menu({ range, compare, categoryId? })` → items with quantity, revenue, share, change, cost/margin/class (or null).
- `analytics.tables({ range })`, `analytics.staff({ range })`, `analytics.money({ range, grain })`, `analytics.customers({ range })`, `analytics.highlights({ range, compare })`.
- `calendar.list({ from, to })`, `calendar.save(event)`, `calendar.remove(id)`.
- `analytics.rebuild({ from, to })` (owner only; for repairs after a data fix).
- All ranges are `DateRange` (AD, inclusive), as in `ReportsApi`. The UI converts BS months and fiscal years to AD ranges with the existing BS helpers.

## Server rules

- `live.*`: owner and manager only (`NOT_ALLOWED`). Read-only: these endpoints sit in their own handler group with no write routes. The snapshot reads open orders, open shifts and today's audit only, and is rate-limited per session (proposal: 1 per second). Table and kitchen states use the same rules and thresholds as the floor screen and the KDS (one shared function in the mock, one in Go).
- `audit.created` is emitted in the same transaction as the audit entry and only to owner/manager sockets. Staff sockets never receive it.
- Analytics: owner and manager only (`NOT_ALLOWED` for others). `analytics.staff` and top-customer lists: owner, or managers listed in `managersSeeStaff`. Cost and margin fields follow the inventory rule (owner/manager).
- Today and any **dirty** day are computed live. Past clean days are read from summaries. A response that includes live data says so ("figures up to HH:MM").
- Any write that touches a business date other than today (bill cancelled, late offline send, void, shift close, posted count) marks that date dirty in the same transaction. A background job rebuilds dirty days within 5 minutes (proposal), and a nightly job rebuilds yesterday. Rebuilding a day is idempotent: delete and re-insert from raw data.
- Daily summary sales for a day must equal `dailySales` for that day. A test compares them over the mock data.
- Range limit: 3 fiscal years per call (`RANGE_TOO_LARGE`). Analytics queries run on a read-only connection with a statement timeout so they can never block ordering.
- Saving or removing a calendar event is audit-logged (`CALENDAR_CHANGED`). Analytics reads are not audit-logged.

## Calls made (correct when picking this up)

- Live view is read-only, built from one server snapshot, refetched on the existing thin realtime events. No new push channel.
- Live view for owner and manager. It needs no summaries, so it can ship **before** analytics.
- "Bill requested" is an optional `Order.billRequestedAt`. Without it (and without guest ordering) the state simply never appears.
- Precomputed daily summaries + live today (approach B). Summaries are disposable and rebuilt from raw data.
- One definition of sales, shared with the dashboard and daily sales. Business day = Kathmandu calendar date.
- BS month and fiscal year by default. Festivals come from an editable list, not computed from a lunar calendar.
- Dwell time ends at the last bill, because there is no "table cleared" event.
- Item cost comes from stock movement snapshots, never today's cost. Items without recipes are not classified.
- Staff shown as per-day / per-cover rates against the average, with a minimum of 5 days and no ranking.
- Optional modules are detected with `analytics.meta()`, and their tabs or columns are hidden when absent.
- Highlights come from fixed rules with fixed thresholds (starting values, editable later).

## Left to the builder

Live view icons and colours per state (reuse the `TableTile` palette), the "needs attention" sort details, the throttle and poll intervals, and whether the owner's phone keeps the screen awake. Chart library or hand-drawn SVG (it must work in a static export, be light on phones and support NE labels), colours and heatmap shading, the exact highlight wording (EN + NE), the percentile method for dwell time, the job scheduler for rebuilds, the monthly summary PDF layout, and a mock data generator that seeds about 400 days of believable history (with Dashain/Tihar peaks and closed days) so the screens can be built and demoed against the fake backend.
