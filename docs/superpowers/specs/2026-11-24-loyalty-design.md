# Loyalty — design

Status: **Designed, not scheduled.** Owner decisions taken 2026-11-24. Phase 3. Covers "loyalty" in the `RESTAURANT_PLAN.md` roadmap. Builds on credit customers (`2026-11-24-credit-customers-design.md`) and suspicious activity alerts (`2026-11-24-suspicious-activity-alerts-design.md`). Every number and rule below is a proposal until the owner answers the questions.

## Why

Small restaurants live on regulars, and many already run paper punch cards ("10th tea free") that get lost, forged or forgotten. A digital card keyed by the guest's phone number costs the guest nothing to carry, and the owner can see who comes back. A loyalty discount is also a new way to leak money: a cashier can put their own number on strangers' bills to collect rewards, or spend a member's reward on a cash bill and keep the difference. The design has to close both leaks.

## Owner decisions

All recommended answers below were accepted by the owner on 2026-11-24.

Each has a recommended answer. The design below assumes the recommendation until the owner says otherwise.

1. **What kind of card?** A stamp card (one stamp per visit above a minimum spend; a reward after N stamps) or points per rupee? — *Recommended:* stamp card: **10 stamps, minimum spend Rs 500 per visit, reward Rs 300 off**. Guests understand it straight away and it rewards coming back, not one big party.
2. **Is the reward an amount off or a free item?** — *Recommended:* a fixed **amount off** (e.g. Rs 300). "Free momo" has to cope with plate sizes, price changes and items that are sold out. Staff can still say "your next momo is on us" because Rs 300 covers it.
3. **When do stamps and rewards expire?** — *Recommended:* the whole card (stamps and unused rewards) expires after **12 months with no visit**. Any visit keeps it alive.
4. **Do bills put on account (udharo) earn a stamp?** — *Recommended:* yes, but the stamp stays **pending until that bill is fully repaid**. No reward is given on money that hasn't come in.
5. **May staff be members, and what happens if a cashier's own number goes on a bill?** — *Recommended:* staff may be members. The system **blocks** a cashier from attaching their own registered number to a bill they issue. Another staff member's number is allowed but raises an alert.
6. **Birthday offer?** — *Recommended:* **off at first**. If you want it, the cashier asks for the birth month (BS) when the guest joins, and during that month the member gets one reward of an amount you choose, shown to the cashier at billing. Nothing is sent to the guest; SMS costs money.

## Goals

- At billing the cashier types the guest's phone number and the bill earns a stamp. That is one field, and on most bills it is the only extra step.
- Rewards are redeemed as a discount line **before VAT**, with a fixed reason "Loyalty reward", and appear correctly on the IRD bill and in the sales book.
- Members are the existing `Customer` records (one per phone), so one guest can have both credit and loyalty.
- The guest sees the card on every receipt and can check it on their phone without an app or login.
- The owner sees members, outstanding rewards (what the restaurant owes in future discounts) and redemptions. Farming and skimming raise alerts.

## Non-goals

- Points per rupee, tiers, cashback, paid memberships, gift cards or vouchers sold for money (a voucher is a prepayment and has different VAT rules).
- Sending SMS, WhatsApp or email to members (paid channels; RESTAURANT_PLAN open question 6).
- Guest accounts, passwords, or a member app. Joining from the QR table menu.
- Sharing a card between branches (Phase 4 multi-branch).

## Anti-goals

- A reward or stamp changing a credit balance. Loyalty never pays off debt and never becomes "credit in the customer's favour".
- Loyalty slowing the bill: no forced lookup, no extra screen when the guest isn't a member.
- A reward that can be given without a member, a bill and an audit record.
- An issued invoice changed by loyalty after issue, or a reprint showing different loyalty numbers from the original.
- A phone number list that any staff member can browse or export.

## Constraints

- Issued invoices are immutable, numbering is gap-free, and reprints must match the original. Loyalty data printed on a bill is snapshotted on the invoice.
- Bill arithmetic (`web/src/lib/billing/compute.ts`): subtotal − discount + service charge → VAT. A bill has one manual `Discount` with a reason; a cashier may give up to 10% without a manager PIN.
- IRD invoice (research 01 §4): discount and taxable amount are shown before VAT. Menu prices are VAT-inclusive.
- Free channels only: the printed receipt, a public status page, and the phone's share sheet.
- Built first against the in-browser fake backend (`web/src/lib/api/mock/`); the Go developer implements the same contract. EN + NE strings.

## Approaches considered

| | Approach | Verdict |
|---|---|---|
| **A (chosen)** | **Stamp card**: 1 stamp per visit above a minimum spend, at most 1 per day; N stamps = fixed amount off | Easiest to explain and to print ("7 of 10"). Rewards frequency. Small whole numbers, so the outstanding amount is easy to count. One stamp a day caps what farming can earn. |
| B | Points per rupee (e.g. 1 point per Rs 100, 1 point = Rs 1) | Rejected for now. It is really 1% cashback: too small to notice, and guests have to do sums. It rewards big parties more than regulars, and splitting a bill makes no difference to fraud. |
| C | Tiers (silver/gold) on top of A or B | Rejected: needs yearly spend tracking and staff training, and is too much for a 10-table restaurant. |
| D | Cashback into a wallet balance | Rejected: a stored rupee balance looks like the credit balance and invites mixing the two; refund requests follow. |
| E | Stamp card with a free item as reward | Possible, but see owner question 2: variants, price changes and sold-out items. Can be added later as a reward type. |

## Design

### Joining (at billing)

```
Bill · T4 · Rs 1,240
┌──────────────────────────────────────┐
│ Loyalty  [98________]  [Find]        │
│ Not a member · [Join]                │
│  Name ______  Birth month (BS) ▾     │
│  [✓] Guest agreed to join            │
└──────────────────────────────────────┘
```

- The cashier types the phone number (10 digits, 97/98). If it is found, the card shows. If not, **Join** creates the member with a first name and phone. Birth month is shown only if birthday offers are on.
- **Consent:** the cashier ticks "Guest agreed to join" (required). The time and the cashier are recorded. A member can leave at any time (owner/manager): the stamps are removed, and the phone number is erased unless the customer still has a credit account.
- If the phone belongs to an existing credit `Customer`, the cashier is asked to confirm the name, and that customer is enrolled. There is no second record.

### At billing (member attached)

```
┌──────────────────────────────────────┐
│ ★ Ram S. · 98XXXXX123                │
│ Stamps 10/10 · 1 reward ready        │
│ [ Use Rs 300 reward ]                │
│ This bill earns 1 stamp → 1/10       │
└──────────────────────────────────────┘
```

- The member must be attached **before the bill is issued**. Adding one later is not possible, because a late attach is how strangers' bills get farmed.
- **Earning (proposal):** the qualifying amount is subtotal − discount − reward, in menu-price terms. It excludes service charge, and because menu prices include VAT, the minimum spend is the figure the guest sees on the menu. If it is at least the minimum spend, the bill earns 1 stamp. Only **one stamp per member per business day** (same business day as alerts); a split bill gives one stamp across its parts.
- **On-account bills:** the stamp is *pending* until the credit design's oldest-first allocation marks the bill fully repaid. If the bill is cancelled first, the stamp is dropped.
- **Redeeming:** **Use reward** is shown only when a reward is ready. The full reward is used or none of it (proposal). It needs subtotal − discount ≥ reward value, otherwise "Bill too small for the reward" and the error is `REWARD_EXCEEDS_BILL`. There is one reward per bill. No manager PIN is needed, because the amount is fixed by the owner and earned. It does **not** count toward the cashier's 10% manual discount limit, which is still checked on the subtotal alone.
- A guest who doesn't want to give a number pays as usual. The loyalty row stays collapsed until tapped.

### On the bill (VAT)

- A reward is a price reduction at the time of sale. No money was paid for the stamps, so it is a **discount before VAT**, not a payment method. Order: subtotal − manual discount − reward → service charge (if ever on) → VAT on that → total.
- The invoice prints the manual discount and the reward as separate lines ("Discount (Regular guest)", "Loyalty reward"), then taxable amount, VAT 13% and total. The sales book "Discount" column is manual discount + reward. Reports keep the two separate.
- Receipt footer (snapshotted on the invoice, so reprints are identical):

```
Loyalty: Ram S. · 98XXXXX123
Stamps 3/10 · 7 more for Rs 300 off
Card valid until 2084.08.09 · Scan to check ▣
```

### Status page (proposal; can follow later)

- A public page `/card/?k=<token>`. It works like the guest menu page: static export, no login, and a token that can't be guessed. The page shows the first name, stamps, rewards ready and the expiry date, but no phone number and no bill history. The receipt QR opens it. Staff can also share the link from the member screen through the phone's share sheet (free).
- The owner or manager can issue a **New link**, which invalidates the old one.

### Cancellation and expiry

- When an invoice is cancelled (existing flow: reason + manager PIN), its stamp is reversed and any reward used on it is restored. A stamp that was already spent on a reward can leave the card at −1; the next visit fills it.
- If a re-bill after a cancellation is on the same day, it can earn again, because cancelled bills don't count toward the one stamp a day.
- A daily job (the same timer as the alerts summary) expires cards with no visit in 12 months and writes an `EXPIRE` entry.

### Fraud controls

- **Own number blocked:** users get an optional `phone`. Attaching a member whose phone matches the issuing user's phone fails with `OWN_NUMBER`.
- **One stamp per day**, **attach only before issue**, and **a fixed reward value**. Together these cap farming at one stamp per day per number.
- **Manual adjustments** (add/remove stamps) are owner/manager only, need a reason and are audit-logged. The alerts "Self-approved" rule covers a manager approving their own adjustment.
- New alert rules (added to the alerts design table; Normal severity; thresholds are proposals):

| Rule | Fires when | Start threshold |
|---|---|---|
| Staff number on a bill | A member's phone matches any staff user's phone | any |
| One cashier feeds a member | In 30 days a member earns ≥ N stamps and ≥ P% of them on bills issued by one cashier | 6 / 80% |
| Many redemptions | One cashier redeems ≥ N rewards in a business day | 3 |
| Large stamp adjustment | A manual adjustment of ≥ N stamps | 3 |

### Owner side

- **Settings → Loyalty:** on/off, stamps per reward, minimum spend, reward value, expiry months, whether on-account bills earn, birthday reward (0 = off). Changes apply to stamps from then on; rewards already earned keep the value they were earned at.
- **Admin → Customers:** a Members filter, and each member's card history (earned, pending, redeemed, reversed, expired, adjusted, each with its bill link).
- **Loyalty report (date range):** members (total, new, active in 90 days), stamps earned, rewards redeemed (count, Rs), **outstanding liability** = rewards ready × their value + partial stamps × (reward ÷ N), and redemptions by cashier. CSV export, without phone numbers except for the owner.
- **Dashboard:** "Rewards given today Rs …". Daily summary: redemptions count and Rs.

### Roles

- Cashier, manager, owner: find a member, enroll a member, attach a member, redeem. Search returns only exact phone matches; there is no browsable list.
- Manager, owner: member list and history, adjust, leave, new status link.
- Owner only: loyalty settings and phone-number export. Waiter and kitchen: none.

### Data (contract for the backend)

- `Customer` **optional additions**: `loyaltyMember` (bool), `loyaltyJoinedAt`, `loyaltyJoinedById`, `loyaltyConsentAt`, `birthMonthBs` (1–12, nullable), `cardToken` (nullable). **Change to the credit design:** `phone` is normalized and unique among customers that have one.
- `User` addition: `phone` (optional, normalized).
- `LoyaltyEntry` (ledger, append-only): id, customerId, kind (`EARN`, `REDEEM`, `REVERSE`, `EXPIRE`, `ADJUST`, `BIRTHDAY`), stamps (+/−), status (`PENDING` | `DONE`, for on-account earns), rewardValue (paisa, for `REDEEM`/`BIRTHDAY`), invoiceId?, businessDate, byId, approvedById?, reason?, at.
- Stamps = sum of `DONE` entries. Rewards ready = floor(stamps ÷ N). Both are server-computed, like the credit balance.
- `Invoice` additions: `customerId` (loyalty member, nullable); `totals.reward` (paisa); `loyaltySnapshot` {maskedPhone, firstName, stampsAfter, perReward, rewardValue, validUntilBs} for printing and reprints.
- `BillRequest` additions: `customerId?`, `redeemReward?: boolean`.
- Settings: `loyalty: { enabled, stampsPerReward, minSpend, rewardValue, expiryMonths, creditBillsEarn, birthdayReward }`.

### APIs

- `loyalty.find({ phone })` → member summary or null. `loyalty.join({ phone, name, birthMonthBs?, consent })` → Customer.
- `loyalty.member(customerId)` → card and entries. `loyalty.adjust({ customerId, stamps, reason, approval? })`. `loyalty.leave(customerId)`. `loyalty.newCardLink(customerId)`.
- `loyalty.settings()`, `loyalty.updateSettings(input)`. `reports.loyalty({ from, to })`.
- Billing: preview, `issue` and `counterSale` take `customerId` / `redeemReward`; the preview returns `reward` and `stampsAfter`.
- Public: `card.status(token)` → first name, stamps, rewards ready, valid until.
- Realtime: `customer.updated` (existing) also fires on card changes.

### Server rules

- Earning, redeeming and reversing happen **in the same transaction** as issuing or cancelling the invoice. The reward check and the invoice number are assigned together, so two tills can't both spend the last reward.
- Errors: `LOYALTY_OFF`, `NOT_A_MEMBER`, `CONSENT_REQUIRED`, `PHONE_TAKEN`, `OWN_NUMBER`, `NO_REWARD`, `REWARD_EXCEEDS_BILL`, `CARD_LINK_INVALID`, `NOT_ALLOWED`.
- Loyalty never writes to credit balances or repayments. `ON_ACCOUNT` amounts are computed after the reward.
- Audit actions: `LOYALTY_JOINED`, `LOYALTY_LEFT`, `LOYALTY_REDEEMED` (bill, value), `LOYALTY_ADJUSTED` (reason, approver), `LOYALTY_SETTINGS_CHANGED`, `LOYALTY_LINK_RESET`. Earns are visible in the ledger and are not audited separately.
- Fake backend: the ledger lives in `mock-store.ts`. Card expiry runs when the store loads.

## Calls made (correct when picking this up)

- The reward is a discount before VAT and not a payment, because no money was paid for stamps. The restaurant's auditor should confirm this before tax mode.
- The full reward is used or none; one per bill; it doesn't count toward the cashier's discount limit.
- One stamp per member per business day; attach before issue only; no late attach.
- A cancellation reverses stamps and restores rewards, and the card may go to −1.
- Settings changes are not retroactive for rewards already earned.
- The status page uses an unguessable token and shows no phone number.

## Left to the builder

Screen layouts, how the loyalty row fits the payment panel, receipt footer wording and QR size on 58 mm paper, status page design, phone normalization details, EN/NE wording, and where Members sits in Admin → Customers.
