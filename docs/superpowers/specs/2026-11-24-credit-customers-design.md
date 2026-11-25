# Credit customers (udharo) — design

Status: **designed, not scheduled.** Phase 2 item ("credit customers") in `RESTAURANT_PLAN.md`.

## Why

Restaurants keep credit in paper notebooks: entries get lost, disputed, or never collected, and a cashier can quietly mark a cash-paying table as "on credit" and keep the cash. This replaces the notebook with customer accounts, a one-tap **On account** payment at billing, limits set by the owner or a manager, repayments, and statements.

Credit customers are **mostly friends and regulars** who settle whenever they come in, plus **some offices and businesses** that pay a monthly total.

## Goals

- Put all or part of a bill on a customer's account at billing, within that account's limit.
- Take repayments (any amount, any time) at the counter, counted in the cashier's shift.
- Statements for any date range, printable or saved as PDF from a phone.
- Owner sees who owes what and for how long; every credit action is in the audit log.

## Non-goals

- Interest, late fees, reminders by SMS/WhatsApp (paid channels).
- Credit notes / partial returns (separate Phase 2 IRD item).
- Customer-facing app or login.

## Anti-goals

- A cashier able to give credit beyond the limit without a manager.
- A debt that isn't linked to the bill it came from.
- Shift drawer counts going wrong because of credit sales or repayments.
- Bills edited after issue, or gaps in the invoice number sequence.

## Constraints

- Issued invoices are immutable; numbering is gap-free per fiscal year (existing billing rules).
- The order closes when everything is billed and handed over (existing rule) — credit must not keep tables open.
- Payments attach to the collecting user's open shift (existing rule).
- Built first against the in-browser fake backend; the Go developer implements the same contract. EN + NE strings.

## Approaches considered

| | Approach | Verdict |
|---|---|---|
| **A (chosen)** | **On account** is a payment method on the issued bill; the customer's balance is derived from on-account payments minus repayments | Keeps invoices immutable and gap-free, order closes normally, VAT on the sale date, cancelling a bill reverses the debt automatically, limit enforced at billing. |
| B | Leave the bill unpaid until settled | Rejected: keeps orders/tables open for weeks, needs invoice state changes after issue, VAT on the wrong date. |
| C | Separate digital notebook; bill recorded as cash, debt typed in by hand | Rejected: no link between debt and bill, no limit at billing, reopens the leak. |

## Design

### Accounts

- Fields: name, phone, notes, **credit limit** (paisa), active flag; optional **business fields**: PAN, address, contact person.
- **Anyone who can bill** (cashier, manager, owner) can create an account. (User decision.)
- **Only owner or manager** can set or change the limit. (User decision.) New accounts start at **limit Rs 0**, so no credit is possible until a limit is set or a manager approves a one-off.
- An account with a balance can be **deactivated but not deleted**: no new credit; repayments still accepted.
- Creating, editing, limit changes, deactivating → audit log.

### At billing

```
Payment
┌──────────────────────────────────────┐
│ Cash · Fonepay · Other QR · Card ·   │
│ [On account]                         │
│                                      │
│ Customer: Ram Shrestha  (98XXXXXXXX) │
│ Owes Rs 2,400 · Limit Rs 5,000       │
│ This bill Rs 940 → owes Rs 3,340     │
│                          [Issue bill]│
└──────────────────────────────────────┘
```

- Cashier taps **On account**, searches by name or phone (active accounts only), or creates a new account inline.
- Shows current balance, limit, available credit, and balance after this bill.
- **Over the limit:** blocked for the cashier; a manager/owner PIN approves this one bill (same approval flow as large discounts, same PIN lockout). (My call.)
- **Mixed payment** allowed: e.g. part cash, rest on account. (My call.)
- Business account chosen → its name and PAN fill the bill's buyer name/PAN.
- Bill is issued and numbered as usual; receipt shows "On account: <customer>" and the balance after.

### Repayments

- Any cashier with an open shift takes a repayment: amount, method (cash, QR, card), optional note. Counted in that shift's takings by method, so the drawer count stays right.
- Cannot exceed the balance owed. Printed repayment receipt (customer, amount, balance after, cashier, date BS/AD). Repayment receipts are **not** invoices and have their own simple sequence.
- Applied **oldest unpaid bills first**, so a statement can show which bills are settled.
- A repayment can be voided by owner/manager with a reason (mistakes), audit-logged; voiding inside a closed shift adjusts the voider's open shift, like bill cancellations.

### Cancelling a bill that was on account

- Cancelling (existing flow: reason + manager PIN) removes its on-account amount from the balance automatically. If that would make the balance negative (already repaid), the excess shows as **credit in the customer's favour**, used against their next bill. (My call.)

### Statements

- For any date range: opening balance, each on-account bill (number, date, amount), each repayment, closing balance.
- Print or save as PDF (same print approach as reports); shareable from the phone's share sheet. Business accounts show PAN and address.

### Owner visibility

- Dashboard: **Credit given today**, **Repayments today**, **Total owed**.
- New **Credit** report: every account with balance, limit, oldest unpaid bill date, last repayment; sortable, oldest debt first; CSV export.
- Daily sales and the sales book list on-account bills normally (payment method "credit", as IRD allows).

### Data (contract for the backend)

- `Customer`: id, name, phone, notes, creditLimit, active, pan, address, contactPerson, createdById, createdAt.
- `phone` is stored normalised (Nepali mobile, digits only) and is **unique** per restaurant: reservations and loyalty find the same customer by phone (see `2026-11-24-reservations-design.md`, `2026-11-24-loyalty-design.md`).
- Payment method `ON_ACCOUNT` with `customerId` on the invoice payment line.
- `Repayment`: id, number, customerId, amount, method, reference, note, shiftId, takenById, at, voidedAt, voidedById, voidReason.
- Balance = sum of on-account payments on ISSUED invoices − sum of non-voided repayments. Server-computed, never sent by the client.

### APIs

- `customers.list({ query?, includeInactive? })`, `get(id)` (with balance), `create(input)`, `update(input)` (limit fields only for owner/manager), `setActive(id, active)`.
- Billing: `issue`/`counterSale` accept `ON_ACCOUNT` payments with `customerId` and optional `approval` for over-limit.
- `credit.repay({ customerId, amount, method, reference, note, requestId })`, `credit.voidRepayment({ id, reason })`.
- `credit.statement({ customerId, from, to })`, `reports.credit()`.
- Realtime: `customer.updated`.

### Server rules

- On-account amount > available credit without a valid manager approval → `CREDIT_LIMIT_EXCEEDED`; inactive account → `CUSTOMER_INACTIVE`.
- Limit change by cashier → `NOT_ALLOWED`.
- Repayment > balance → `REPAYMENT_TOO_HIGH`; no open shift → `SHIFT_NOT_OPEN`; `requestId` makes repayments idempotent.
- Balance check and bill issue in the same transaction (two tills can't both use the last of the limit).
- Audit actions: `CUSTOMER_CHANGED`, `CREDIT_LIMIT_CHANGED`, `CREDIT_GIVEN` (with approver when over limit), `REPAYMENT_TAKEN`, `REPAYMENT_VOIDED`.

## Calls made (correct when picking this up)

- One-off manager PIN over the limit (rather than a hard stop).
- Mixed cash + on-account payments allowed.
- No interest or late fees.
- Oldest-first allocation of repayments; overpayment after a cancellation becomes credit in the customer's favour.
- Repayment receipts have their own simple sequence, separate from invoices.

## Left to the builder

Screen layouts, search behaviour, statement print layout, where Customers lives in the admin menu (proposed: Admin → Customers, plus inline create at billing), EN/NE wording.
