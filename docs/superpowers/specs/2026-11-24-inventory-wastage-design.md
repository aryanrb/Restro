# Inventory, variance and wastage — design

Status: **Designed, not scheduled.** Owner decisions taken 2026-11-24. Phase 2. Covers the Phase 2 item ("inventory with variance reports, wastage logging") in `RESTAURANT_PLAN.md`. Purchases/suppliers is a separate doc that builds on the stock model defined here (`InventoryItem`, `StockMovement`, unit cost).

## Why

Stock disappears in restaurants: chicken that was never sold, bottles that were never billed, oil that runs out twice as fast as it should. Today nobody can say how much *should* be left, so nobody can say how much went missing. This design works out what should be left from what the kitchen was sent, has staff count what is really there, and shows the difference in rupees. It also gives the kitchen a quick way to record honest waste, so real loss can be told apart from unexplained loss.

It is **optional per item**: a restaurant starts by tracking a handful of expensive items (meat, paneer, alcohol, oil) and adds more only if it finds the result useful.

## Owner decisions

All recommended answers below were accepted by the owner on 2026-11-24.

1. **Which items do we track first?** Recommended: 10–20 expensive or easily stolen items only, such as chicken, mutton, buff, paneer, cheese, bottled beer and spirits, soft-drink bottles and cooking oil. Rice, spices and vegetables stay untracked until the first few counts look right.
2. **When an item is voided after the kitchen started cooking it, is that waste?** Recommended: yes. The ingredients stay deducted, and the void appears in the variance report as "voided after cooking", with the void reason and the manager who approved it.
3. **How often, and who counts?** Recommended: a full count of tracked items **weekly** by the head cook, plus a **daily** count of alcohol by the cashier or bar staff. A manager or owner reviews and posts every count.
4. **Do counters see the expected quantity while counting?** Recommended: **no (blind count).** People who can see the expected number tend to write it down instead of counting.
5. **Should the system 86 an item on its own when an ingredient runs out?** Recommended: **no, suggest it only.** Stock figures drift between counts, so an automatic 86 would sometimes hide a dish the kitchen can still make.
6. **How are staff meals recorded?** Recommended: as waste with the reason **Staff meal**, logged by the kitchen and shown on its own line in reports, rather than as zero-price orders that would distort sales.

## Goals

- Deduct ingredients automatically from what the kitchen is sent, using recipes, for tracked items only.
- A stock count on a tablet, followed by a **variance report**: expected against counted, in quantity and in rupees, for each item and in total.
- Waste logging in a few taps, with a reason and the name of whoever logged it.
- Low-stock alerts, and a one-tap 86 suggestion when a dish can no longer be made.
- One stock ledger that purchases (separate doc) writes into without changing anything here.

## Non-goals

- Purchases, suppliers, purchase orders and the suggested purchase list (separate doc).
- Batch/expiry tracking, FIFO costing, multiple storage locations, transfers between branches.
- Prep recipes (sub-recipes such as momo filling made in bulk). Proposal for later: a prepared item could be an inventory item with its own recipe.
- Food-cost percentage per dish and menu engineering. This design produces the data for them but no screens.

## Anti-goals

- A send to the kitchen that is slowed down or blocked because stock is low or wrong.
- A stock figure that anyone can overwrite. Corrections happen only through counts, which are recorded.
- Loss hidden as "waste" without a name, a reason and, for large amounts, a manager.
- Old movements changing when a recipe changes.
- Staff seeing cost prices, which are only shown to the owner and manager.

## Constraints

- Peak-hour speed rule: no spinners in ordering. Stock work happens on the server inside the existing send transaction and never blocks the response.
- Order items are append-only, voids carry reason/approver, and bill cancellation returns lines to the open order (existing rules).
- Sends can arrive late through the offline outbox (existing). Deduction uses the item's `sentAt`, not the time the server processes it.
- Built first against the in-browser fake backend (`web/src/lib/api/mock/`); the Go developer implements the same contract. All UI strings in EN and NE; inventory item names are `LocalText`.
- Money in integer paisa (existing rule).

## Approaches considered

When to deduct stock:

| | Approach | Verdict |
|---|---|---|
| **A (chosen)** | **When the item is sent to the kitchen** (status becomes `SENT`). A void before cooking reverses the deduction; a void after cooking does not. | Matches when food is really made. Shows the leak that matters most, food cooked but never billed. Works the same for table service, counter sales (pay first, then send) and accepted guest orders. |
| B | At billing (invoice issued) | Rejected: food that is cooked and eaten but never billed is never deducted, which is exactly the loss we want to find. Bill cancel and re-bill would also have to move stock back and forth. |
| C | No automatic deduction; usage = opening + purchases − closing count | Rejected as the main approach because it can't tell sold from stolen. It is still what happens for untracked items, and the variance report uses the same arithmetic. |

## Design

### Inventory items (Admin → Inventory, owner/manager)

```
Inventory                                [+ Item]  [Count]  [Log waste]
┌─────────────────────────────────────────────────────────────────────┐
│ Chicken (boneless)   g    On hand 4,200 g   Low at 3,000   ● tracked│
│ Beer Gorkha 650ml    pcs  On hand 18 pcs    Low at 24  ⚠ LOW        │
│ Paneer               g    On hand −350 g  ⚠ check purchases/count   │
│ Rice                 g    —                 (not tracked)           │
└─────────────────────────────────────────────────────────────────────┘
```

- Fields: name (EN/NE), base **unit** (`g`, `ml`, `pcs`), tracked, low-stock level, active, unit cost (owner/manager only), optional **pack sizes** used for counting and purchases (e.g. "Crate = 12 pcs", "Tin = 15,000 ml", "kg = 1,000 g").
- **Units (proposal):** each item has one base unit, and all movements and recipes use it. `kg` and `L` are input conveniences that convert to `g`/`ml` as you type. There are no conversions between kinds (no "1 cup of rice in grams"); recipes are written in the base unit.
- **Tracked = false:** the item can appear in recipes and purchases but gets no SALE movements, no low-stock alerts and no counts. Turning tracking on asks for an **opening count**, which writes an `OPENING` movement.
- **Negative on hand is allowed** (a send is never blocked). It is shown in red as a hint that a purchase or count is missing.
- On hand = sum of movements (server-computed).

### Recipes (on the menu item edit page)

```
Chicken Momo — Recipe                          Food cost Rs 96 (owner only)
  Item (all variants)  Chicken (boneless) 120 g · Flour 80 g · Oil 10 ml
  Variant "Half"       ☑ own recipe: Chicken 60 g · Flour 40 g · Oil 5 ml
  Modifier "Extra cheese" (Modifiers page)   + Cheese 30 g
```

- **Item recipe:** list of (inventory item, quantity in base unit) per 1 unit sold.
- **Variant (proposal):** a variant uses the item recipe unless it has its **own recipe**, which replaces the item recipe entirely. Replacing is simpler to reason about than scaling factors.
- **Modifier (proposal):** a modifier may have recipe lines that are **added** (e.g. extra cheese +30 g). There are no "minus" lines; "no onion" is not worth tracking.
- **No recipe:** selling the item deducts nothing, and it appears on a "Menu items without a recipe" list so the owner can decide. Bottled drinks get a one-line recipe ("1 pcs Beer Gorkha").
- Recipe lines that point at untracked items are kept, but they deduct nothing until the item is tracked.
- Recipe changes apply **from now on only**. Past movements are never recalculated. Recipe changes are audit-logged.

### Stock flow from orders

| Event | Stock effect |
|---|---|
| Item sent to kitchen (waiter send, counter sale, accepted guest order) | `SALE` movement per tracked ingredient: −(recipe qty × quantity), `refId` = order item id, `at` = `sentAt` |
| Void while `SENT` (kitchen hasn't started) | `SALE_REVERSAL` (+ the same quantities) |
| Void when `PREPARING`/`READY`/`SERVED` | No reversal. Shown as "voided after cooking" in variance and waste reports (owner question 2) |
| Bill issued / bill cancelled / order moved or merged | No stock effect (lines are unchanged) |

### Stock count (tablet, kitchen or store room)

```
Count #14 · weekly · started by Hari 10:05     [Save]  [Submit for review]
┌──────────────────────────────────────────────────────────┐
│ Chicken (boneless)   [ 2 ] kg + [ 350 ] g        ✓ 10:07 │
│ Beer Gorkha 650ml    [ 1 ] crate + [ 4 ] pcs     ✓ 10:09 │
│ Paneer               [       ] g                         │
└──────────────────────────────────────────────────────────┘
```

- Start a count for **all tracked items** or a saved **list** (e.g. "Bar daily": alcohol only). Items are ordered as they sit on the shelves (a sort field on the item).
- Big number inputs in pack sizes plus loose units. **Blind** by default (owner question 4). It saves as you go, so a count can be finished later or on another tablet.
- Each line records **when it was counted**. Expected = on hand **at that moment**, so sales during the count don't create false variance.
- **Submit** → the count waits for a manager or owner to review the variance report. They can ask for a **recount** of selected lines, **post** the count, or **discard** it. Posting writes one `COUNT_ADJUST` movement per line (counted − expected).
- A **single-item count** (same flow, one line) is the only way to correct one item's stock; there is no "edit on hand" field.

### Variance report (owner/manager)

```
Variance · Count #14 (since Count #13, 7 days)       Total −Rs 3,840
Item        Opening  +Purch  −Sales  −Waste  Expected  Counted  Var      Rs
Chicken     5,000g   20,000g 17,900g   400g   6,700g   6,350g   −350g  −210
Beer 650ml  48       96      110       2      32       28       −4    −1,640
  ⓘ voided after cooking this period: 6 items, Rs 1,150
```

- Covers the period since the previous posted count of each item. Sorted by rupee loss, biggest first; lines above a threshold (proposal: Rs 500 or 10%) are highlighted.
- Variance value = variance qty × unit cost **at posting**. CSV and PDF export use the existing report export.
- Also available: a **history** of each item (all movements, filterable by type) and a trend of total variance across counts.

### Waste logging (kitchen, cashier, manager, owner)

```
Log waste
  What:   [Chicken (boneless) ▾]  or  [Menu item: Chicken Momo ▾ Half]
  How much: [ 400 ] g        Reason: (Spoiled) (Dropped) (Burnt/mistake)
                                     (Staff meal) (Returned by guest) (Other…)
  Note: ______              Logged by: Hari          [Save]
```

- Choose an **inventory item** (raw waste) or a **menu item** with variant/modifiers (a dropped plate, expanded through its recipe into WASTE movements).
- Reason is required; "Other" needs a note. The person logging is the logged-in user.
- **Proposal:** waste worth more than Rs 1,000 in one entry needs a manager PIN (same approval dialog as voids). Cost isn't shown to kitchen staff; the server decides whether approval is needed.
- Logging waste never 86es anything. A **wastage report** (date range, by reason, by person, by item, in Rs) sits under Reports.

### Low stock and 86 suggestions

- On hand ≤ low-stock level → the item is flagged on the Inventory page, a **Low stock** card appears on the owner dashboard, and a realtime toast goes to manager/owner screens (once per crossing, not on every sale).
- **86 suggestion:** when a tracked ingredient's on hand is less than one portion of a menu item's recipe, the KDS and manager screens show "Chicken Momo may be out: chicken 80 g left [86 it] [Dismiss]". One tap uses the existing `set86`. The system never 86es on its own (owner question 5).

### Roles (proposal)

| | Owner | Manager | Cashier | Kitchen | Waiter |
|---|---|---|---|---|---|
| Items, recipes, low levels, unit cost | ✓ | ✓ | | | |
| Do a count (enter numbers) | ✓ | ✓ | ✓ | ✓ | |
| Post / discard a count, see variance in Rs | ✓ | ✓ | | | |
| Log waste | ✓ | ✓ | ✓ | ✓ | |
| See cost values, wastage and variance reports | ✓ | ✓ | | | |

## Data (contract for the backend)

- `InventoryItem`: id, version, name (`LocalText`), unit (`"g" | "ml" | "pcs"`), tracked, lowStockLevel, active, **unitCost**, packs (`{ name: LocalText, size }[]`), sort.
- **Unit cost (for purchases):** `InventoryItem.unitCost` = **paisa per base unit, a decimal with 4 places** (Rs 600/kg chicken = 60.0000 paisa/g). Purchases update it as a **moving weighted average** when a `PURCHASE` is posted: `(onHand × oldCost + qty × purchaseCost) / (onHand + qty)`, using `max(onHand, 0)`. Until purchases ship, the owner types it in.
- `StockMovement`: id, inventoryItemId, quantity (signed, base unit, decimal up to 3 places), type (`SALE | SALE_REVERSAL | WASTE | COUNT_ADJUST | PURCHASE | PURCHASE_RETURN | OPENING`), refId, reason, byId, at, **unitCost** (snapshot at the time of the movement; for `PURCHASE` it is the purchase price per base unit). Movements are never edited or deleted.
- `refId` by type: SALE/SALE_REVERSAL → order item id; WASTE → waste entry id; COUNT_ADJUST/OPENING → stock count id; PURCHASE/PURCHASE_RETURN → purchase line id (purchases doc).
- `RecipeLine`: inventoryItemId, quantity. Recipe lines live on `MenuItem.recipe`, `Variant.recipe` (`null` = use the item recipe) and `Modifier.recipe` (added).
- `WasteEntry`: id, inventoryItemId | (menuItemId, variantId, modifierIds), quantity, reason (`SPOILED | DROPPED | MISTAKE | STAFF_MEAL | RETURNED | OTHER`), note, byId, approvedById, at, value (server-computed).
- `StockCount`: id, number, status (`OPEN | SUBMITTED | POSTED | DISCARDED`), listId, startedById, submittedById, postedById, timestamps, lines `{ inventoryItemId, counted, countedAt, countedById, expected, variance, value }` (expected/variance/value filled in by the server when posting).
- `CountList`: id, name, inventoryItemIds.

## APIs

- `inventory.list({ includeInactive? })` (with on hand), `save(input)`, `setTracked(id, tracked, openingQty?)`, `movements({ inventoryItemId, from, to, type? })`.
- `menu.saveRecipe({ menuItemId, recipe, variantRecipes, baseVersion })`, `menu.saveModifierRecipe({ modifierId, recipe })`.
- `waste.log({ target, quantity, reason, note, approval?, requestId })`, `waste.list(range)`.
- `counts.start({ listId? })`, `counts.saveLines({ countId, lines })`, `counts.submit(id)`, `counts.recount({ countId, itemIds })`, `counts.post(id)`, `counts.discard(id, reason)`, `counts.get(id)`, `counts.list(range)`, `countLists.save/list`.
- `reports.variance({ countId })`, `reports.wastage(range)`, `reports.lowStock()`, `reports.itemsWithoutRecipe()`.
- Realtime: `stock.low` (item crossed its low level), `stock.86suggest` (menu item, ingredient, left), `count.updated`.

## Server rules

- SALE/SALE_REVERSAL movements are written **in the same transaction** as the send/void. Stock never rejects a send or a void.
- Deduction uses the recipe in force at the time the server applies the send; `at` = the item's `sentAt` (offline sends).
- Only tracked, active inventory items get SALE, WASTE or COUNT movements; others → `ITEM_UNTRACKED` (waste/count).
- Unit cost, values and variance in Rs are stripped from responses for cashier/kitchen roles.
- Waste over the approval threshold without a valid approval → `APPROVAL_REQUIRED` (same PIN lockout as voids). `requestId` makes waste logging idempotent.
- Count lines can only be edited while `OPEN`; post/discard by owner/manager only (`NOT_ALLOWED`); posting twice → `COUNT_ALREADY_POSTED`. Expected is computed at `countedAt`; posting is one transaction.
- Saving an item or recipe uses `baseVersion` (`STALE_VERSION`). A unit can't be changed once an item has movements (`UNIT_LOCKED`).
- Audit actions (same transaction): `INVENTORY_ITEM_CHANGED`, `RECIPE_CHANGED`, `WASTE_LOGGED` (with approver), `STOCK_COUNT_POSTED` (total variance in detail), `STOCK_COUNT_DISCARDED`, `TRACKING_CHANGED`.

## Calls made (correct when picking this up)

- Deduct at send; reverse only voids made before cooking starts.
- Variant recipes replace the item recipe; modifier recipes add to it.
- One base unit per item (`g`/`ml`/`pcs`), with pack sizes for entry; no cross-kind conversions.
- Negative stock allowed and flagged, never blocking.
- Corrections only through counts (including single-item counts); there is no free-form adjustment.
- Unit cost is a moving weighted average stored on the item and snapshotted on each movement.
- Waste approval threshold Rs 1,000 and variance highlight Rs 500 / 10% are starting values (settings).

## Left to the builder

Screen layouts and where the count screen lives (proposed: Admin → Inventory, plus a **Count** and **Log waste** entry on the kitchen screen), pack-size input widget, report print layout, wording of waste reasons (EN + NE), how the 86 suggestion looks on the KDS, toast throttling.
