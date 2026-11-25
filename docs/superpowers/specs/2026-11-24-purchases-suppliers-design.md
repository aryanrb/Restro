# Purchases & suppliers — design

Status: **Designed, not scheduled.** Owner decisions taken 2026-11-24. Phase 2. Phase 2 item ("purchases/suppliers"; "low stock alerts + suggested purchase list" under *Stock disappearing*) in `RESTAURANT_PLAN.md`. Builds on the inventory & wastage design (drafted in parallel), which owns the stock model.

## Why

Stock comes in through a back door that nobody records. Supplier bills sit in a drawer or get lost. Cash for the vegetable man comes out of the till with no record, so the drawer count at shift close is "short" for no reason. Restaurants buy a lot on credit from suppliers, and the only record of what they owe is the supplier's own notebook. This records every purchase bill against a supplier, adds the stock, tracks what is paid and what is owed, takes till cash out through the shift properly, and gives the accountant a purchase book for VAT input credit.

## Owner decisions

All recommended answers below were accepted by the owner on 2026-11-24.

1. **Who may record purchases and pay suppliers from the till?** Recommended: the cashier records goods received and can pay from the till up to **Rs 5,000 per payment**. Above that, a manager/owner PIN is needed. Only the owner or manager can void a bill, record a return or edit a supplier.
2. **Where does supplier cash come from: the till, or the owner's own cash?** Recommended: allow both. A **From till** payment is a paid-out in the cashier's open shift, so the drawer count stays right. **Owner's cash / bank / QR / cheque** never touches the shift.
3. **How do you settle with credit suppliers: bill by bill, or one lump sum each week or month?** Recommended: keep one running balance per supplier and allow any amount at any time, applied to the oldest unpaid bill first. That covers both habits.
4. **Should every purchase be entered (market vegetables with no bill, gas, cleaning supplies), or only stock items?** Recommended: enter everything. Untracked or non-stock lines are allowed, so the till and payables are complete. Bills with no PAN/VAT bill number stay out of the purchase book.
5. **Does your accountant want the purchase book from this system?** Recommended: yes, a monthly CSV in the purchase book columns below. Confirm the columns with the accountant before building.
6. **How should "suggested purchase" decide quantities?** Recommended: set a **buy-up-to level** per item. Suggestion = buy-up-to − on hand, rounded up to whole purchase units. Usage-based forecasting can come later.

## Goals

- Supplier records with PAN and VAT registration, so purchases can claim input VAT.
- Record a purchase bill in under a minute: supplier, their bill number, lines in **purchase units** (1 crate = 24 pcs), unit cost, VAT. Stock goes up by the right number of stock units.
- Pay now, pay part, or owe it all. See what is owed to each supplier and since when. Take later payments.
- Cash paid from the till during a shift is a recorded **paid-out**, so drawer counts stay right.
- Purchase returns reduce both stock and the amount owed.
- Low-stock list grouped by supplier, turned into a shareable order list.
- Purchase book report (VAT bills only) with CSV export. Every purchase action is in the audit log.

## Non-goals

- Formal purchase orders with approval workflow, sending POs to suppliers, supplier portal or login.
- Double-entry accounting, bank reconciliation, TDS on supplier payments, imports/customs.
- Recipe costing and menu margins (they belong with the inventory design; this doc only provides costs).
- Posting purchases to CBMS (sales only).

## Anti-goals

- Cash leaving the till without a record in a shift.
- Stock going up with no purchase bill behind it (the only other in-flows are COUNT_ADJUST and OPENING, which the inventory doc owns).
- A recorded bill edited in place, or the same supplier bill entered twice.
- A bill with no supplier PAN/VAT bill number counted as VAT-claimable in the purchase book.

## Constraints

- Stock model is owned by the inventory design: `InventoryItem` { id, name (LocalText), unit, tracked, lowStockLevel, active } and `StockMovement` { id, inventoryItemId, quantity (signed, item's unit), type, refId, reason, byId, at }. On hand = sum of movements. This doc only writes `PURCHASE` (+) and `PURCHASE_RETURN` (−) movements, with `refId` = the purchase line or return line id.
- Shifts: payments attach to the collecting user's open shift. Paid-outs follow the same rule (existing).
- Money in integer paisa. AD timestamps plus BS date and fiscal year (existing).
- Built first against the in-browser fake backend (`web/src/lib/api/mock/`); the Go developer implements the same contract. EN + NE strings.

## Approaches considered

| | Approach | Verdict |
|---|---|---|
| **A (chosen)** | **Purchase bill = received goods + supplier invoice in one record**, recorded when goods arrive. Supplier balance is derived from bills − payments − returns. | One screen, matches how small restaurants work (goods and bill arrive together), stock and payable stay in one transaction. |
| B | Purchase order → goods received note → supplier invoice (three documents) | Rejected for now: too heavy for a 1–2 person back office. The suggested-purchase list covers the "what to order" half. |
| C | Stock-in screen only; money tracked elsewhere | Rejected: till paid-outs and supplier credit stay invisible, and there is no purchase book. |

## Design

### Suppliers

- Fields: name, phone, address, **PAN**, **VAT registered** (bool), contact person, notes, active. Optional **opening balance** (owed before go-live), recorded once as an opening entry.
- Deactivate, don't delete, while a balance exists. Create/edit/deactivate → audit log.

### Purchase units (proposal)

- Each inventory item can have saved **purchase units**: label (EN/NE) + factor in stock units. Examples: `crate = 24 pcs`, `kg = 1000 g`, `jar (5 L) = 5000 ml`. The stock unit itself always exists with factor 1.
- A line stores the unit label and factor **as a snapshot**, so later edits to the unit don't change old bills.

### Recording a purchase

```
New purchase                                     
┌──────────────────────────────────────────────────┐
│ Supplier: Himalayan Dairy ▾   PAN 301234567 (VAT)│
│ Their bill no: 2451        Bill date: 2083.08.08 │
│ ☑ VAT bill (claimable)                           │
│                                                  │
│ Item           Qty  Unit       Rate     Amount   │
│ Milk           20   L (1000ml) 110.00   2,200.00 │
│ Paneer          3   kg         950.00   2,850.00 │
│ Coke 250ml      2   crate (24) 600.00   1,200.00 │
│ + Add line   + Non-stock line (gas, cleaning…)   │
│                                                  │
│ Taxable 4,050 · Exempt 2,200 · VAT 526.50        │
│ Total 6,776.50                                   │
│ Paid now: [From till ▾] Rs [ 2,000 ]             │
│ Owed after this bill: Rs 4,776.50 (total 9,100)  │
│                                     [Record bill]│
└──────────────────────────────────────────────────┘
```

- Lines: inventory item (or a **non-stock line** with a free-text description and no stock movement), quantity, purchase unit, **unit cost per purchase unit excluding VAT**, a **VAT applies** flag (many basic foods are VAT-exempt), line amount (computed).
- VAT: the system computes 13% on taxable lines. The person types the **VAT amount printed on the bill**. A difference over Rs 1 shows a warning and needs a confirm, never a block. The printed figure is what gets stored. (Proposal.)
- Bill-level discount is allowed and is spread over the lines by amount for costing.
- **VAT bill** is ticked only when the supplier is VAT-registered with a PAN and a bill number is entered. Only VAT bills go in the purchase book.
- Untracked items: the line is recorded (cost, purchase book) and still writes a PURCHASE movement. Whether untracked items show stock is the inventory doc's call.
- Recording writes, in one transaction: the bill, its lines, one `PURCHASE` movement per stock line (quantity × factor), any paid-now payment, and the audit entry. A recorded bill is never edited.
- Mistakes: the owner/manager **voids** the bill with a reason. That writes reversing `PURCHASE_RETURN` movements (reason "void"), drops it from the balance and the purchase book, and turns any payments on it into supplier credit. A bill that already has returns can't be voided.

### Paying suppliers

- Methods: **From till** (cash from the drawer), **Owner's cash**, **Bank / QR**, **Cheque** (with number). Any amount up to what is owed, applied to the oldest unpaid bill first. Paying more than is owed is blocked (no advances, proposal).
- **From till** needs an open shift for the paying user. It is recorded as a **paid-out** on that shift: `expectedCash = openingFloat + cash takings − paid-outs`. The shift close screen lists paid-outs (supplier, amount, by). Above the till limit (question 1), a manager PIN is needed.
- Printed or shared **payment slip** (supplier, amount, method, balance after, paid by, date BS/AD), with its own simple sequence. It is not an invoice.
- Owner/manager can void a payment with a reason. Voiding a till payment from a closed shift adjusts the voider's open shift, like bill cancellations.

### Purchase returns

- From a recorded bill: choose lines and quantities, up to bought − already returned. Add a reason and optionally the **supplier's credit/debit note number**.
- Writes `PURCHASE_RETURN` movements (−quantity × factor) and reduces the amount owed at the original unit cost and VAT. If the bill was already paid, the excess becomes **credit with that supplier**, used against the next bill. A cash refund handed back is recorded as **refund into till** (a paid-in on the shift). (Proposal.)

### Suggested purchase list

```
Buy list                                    [Share] [Print]
┌───────────────────────────────────────────────────────┐
│ Himalayan Dairy                                       │
│  Paneer     on hand 0.8 kg · low 2 kg · buy 3 kg      │
│  Milk       on hand 6 L    · low 10 L · buy 15 L      │
│ Sharma Traders                                        │
│  Coke 250ml on hand 30 pcs · low 48 · buy 2 crates    │
│ No supplier set                                       │
│  Basmati rice  on hand 4 kg · buy 21 kg               │
└───────────────────────────────────────────────────────┘
```

- Tracked, active items where on hand ≤ `lowStockLevel`. Quantity = **buy-up-to** − on hand, rounded up to the item's **default purchase unit** (question 6). Grouped by **preferred supplier**, showing the last price paid.
- Per-item purchase settings (owned here): preferred supplier, default purchase unit, buy-up-to level.
- Share as text through the phone's share sheet (free; WhatsApp works this way with no API) or print. Tapping a supplier group opens **New purchase** pre-filled when the goods arrive.
- The **low-stock alert** itself (badge, dashboard count) belongs to the inventory design. This list is the next step from it.

### Reports

- **Purchase book** (per BS month or date range, VAT bills only). One row per bill: BS date, supplier bill no, supplier name, supplier PAN, total purchase, exempt amount, taxable amount, VAT. Returns appear as negative rows with the note number. Totals row, CSV export. Columns are a **proposal**: the research marks the purchase book as "can wait" and does not give its official layout. Confirm with a tax advisor and the owner's accountant.
- **Payables**: every supplier with a balance, oldest unpaid bill date, last payment. Oldest debt first, CSV export.
- **Supplier statement** (date range): opening balance, bills, returns, payments, closing balance. Print or PDF.
- **Purchases** list: filter by supplier, date, item. CSV export.
- Dashboard tiles: **Purchases today**, **Paid out from till today**, **Owed to suppliers**.

### Cost for stock valuation (interface point — inventory doc to confirm)

- Cost per stock unit on a line = (line amount after discount spread) ÷ (quantity × factor). VAT is **excluded** when the restaurant is VAT-registered and the bill is a VAT bill (claimable). Otherwise VAT is included in cost.
- **Average cost** follows the inventory design: `InventoryItem.unitCost` (paisa per base unit) is a **moving weighted average** updated when a `PURCHASE` movement is posted. Each purchase line writes a `PURCHASE` movement whose `unitCost` snapshot is the cost per stock unit above (claimable VAT excluded). A `PURCHASE_RETURN` goes out at the returned line's own cost and leaves the average unchanged. (Aligned with `2026-11-24-inventory-wastage-design.md`.)

### Data (contract for the backend)

- `Supplier`: id, name, phone, address, pan, vatRegistered, contactPerson, notes, active, openingBalance, createdById, createdAt.
- `PurchaseUnit`: id, inventoryItemId, label (LocalText), factor (stock units, > 0).
- `ItemPurchaseSettings`: inventoryItemId, preferredSupplierId, defaultPurchaseUnitId, buyUpTo.
- `Purchase`: id, supplierId, supplierBillNo, billDate (AD + BS), fiscalYear, isVatBill, discount, taxable, exempt, vat, total, status (`RECORDED` | `VOIDED`), voidReason, voidedById, recordedById, at.
- `PurchaseLine`: id, purchaseId, inventoryItemId (null = non-stock), description, quantity, unitLabel, factor, unitCost (per purchase unit, excl. VAT), vatApplies, amount.
- `PurchaseReturn`: id, purchaseId, noteNo, reason, lines [{ purchaseLineId, quantity }], amount, vat, byId, at.
- `SupplierPayment`: id, number, supplierId, amount, method (`TILL` | `OWNER_CASH` | `BANK_QR` | `CHEQUE`), reference, note, shiftId (TILL only), paidById, approverId, at, voidedAt, voidedById, voidReason.
- Shift gains `paidOuts: { kind: "SUPPLIER_PAYMENT" | "SUPPLIER_REFUND", refId, supplierName, amount, at }[]` (refund is a negative paid-out). `expectedCash` subtracts them.
- Supplier balance = opening + recorded bills − returns − non-voided payments. Server-computed, never sent by the client.

### APIs

- `suppliers.list({ query?, includeInactive? })`, `get(id)` (with balance), `create`, `update`, `setActive`.
- `purchaseUnits.list(itemId)`, `save(...)`. `purchaseSettings.get/save(itemId, ...)`.
- `purchases.record({ supplierId, supplierBillNo, billDate, isVatBill, lines, discount, vat, payNow?, approval?, requestId })`, `purchases.void({ id, reason })`, `purchases.return({ purchaseId, noteNo, reason, lines, requestId })`, `purchases.list(filter)`, `purchases.get(id)`. (Stock value and cost come from the inventory API, not from purchases.)
- `supplierPayments.pay({ supplierId, amount, method, reference, note, approval?, requestId })`, `supplierPayments.void({ id, reason })`.
- `purchases.suggested()`, `reports.purchaseBook(range)`, `reports.payables()`, `suppliers.statement({ supplierId, from, to })`.
- Realtime: `purchase.recorded`, `supplier.updated`. Stock change events are the inventory doc's.

### Server rules

- Duplicate (supplierId, supplierBillNo) among non-voided bills → `DUPLICATE_SUPPLIER_BILL`. Inactive supplier → `SUPPLIER_INACTIVE`. Inactive item → `ITEM_INACTIVE`.
- `isVatBill` without supplier PAN + VAT registration + bill number → `NOT_A_VAT_BILL`.
- Payment > balance → `PAYMENT_TOO_HIGH`. TILL without an open shift → `SHIFT_NOT_OPEN`. TILL over the limit without a valid manager approval → `PAID_OUT_LIMIT`.
- Return quantity > bought − returned → `RETURN_TOO_HIGH`. Void with returns → `PURCHASE_HAS_RETURNS`. Role not allowed → `NOT_ALLOWED`.
- Bill, lines, movements, payment and audit entry in one transaction. `requestId` makes record/return/pay idempotent.
- Audit actions: `SUPPLIER_CHANGED`, `PURCHASE_RECORDED`, `PURCHASE_VOIDED`, `PURCHASE_RETURNED`, `SUPPLIER_PAID` (with approver when over the till limit), `SUPPLIER_PAYMENT_VOIDED`.

## Calls made (correct when picking this up)

- Goods and bill recorded together. No purchase orders.
- VAT is typed from the bill, with a computed check that warns but doesn't block.
- Non-stock lines allowed on purchase bills.
- No supplier advances. Over-returns become supplier credit.
- Till payments are shift paid-outs; other methods don't touch shifts.
- Payment slips have their own sequence.
- Purchase book columns pending a tax advisor.
- Cost excludes claimable VAT; the item's average cost is the inventory design's moving weighted average.

## Left to the builder

Screen layouts, item search and quick-add on the purchase form, share-text format of the buy list, statement print layout, menu placement (proposed: Admin → Purchases with Suppliers and Buy list tabs, plus **Pay supplier** on the cashier screen), EN/NE wording.
