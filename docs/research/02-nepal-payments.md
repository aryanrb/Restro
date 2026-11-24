# 02 — Nepal QR / Wallet Payments for the Restaurant POS

Researched: 2026-11-23. Scope: eSewa, Khalti, Fonepay, NepalPay QR (NCHL) / NepalQR interoperability.
Stack context: TypeScript/Node backend (one cloud deployment per restaurant), Expo Android app. Constraint: no paid SaaS. Merchant transaction fees are acceptable only if unavoidable; they are flagged below.

**Confidence key:** **H** = official provider or regulator doc read directly. **M** = several independent secondary sources agree. **L** = one secondary source, or sources conflict. **UNVERIFIED** = not confirmed; check before relying on it.

> Caveat: Fonepay and NCHL do not publish full public developer portals. Most Fonepay API detail comes from developer blogs dated 2026. The eSewa developer portal renders with JavaScript, so its pages came through search snippets and mirrors and could not be fetched in full.

---

## 1. Comparison table

| | **Fonepay QR** | **NepalPay QR (NCHL)** | **eSewa** | **Khalti** |
|---|---|---|---|---|
| What it is | QR network/switch (bank-owned, F1Soft). eSewa runs on it | NRB-backed interoperable QR network run by NCHL on the NepalQR (EMVCo) standard | Wallet + online gateway (ePay v2). Its business QR is Fonepay-based | Wallet + online gateway (KPG-2). Also offers "Khalti Pasal" counter QR |
| Static QR | Yes, free, from the merchant portal/app (H) | Yes, issued via bank/acquirer (M) | Yes, the "eSewa Business QR" (M) | Yes, Khalti Pasal QR (L) |
| Dynamic QR (amount + bill ref) from own software | **Yes**, via the "thirdPartyDynamicQr" merchant API. Credentials come from Fonepay or your bank on request, not self-serve (M) | Spec has `POST /qr/generateQR` (dynamic, PoI=12), but access goes through an acquirer/NPI member. No self-serve path found (M/L) | No QR API. Online alternative is the ePay v2 redirect checkout (H) | No QR API. Online alternative is the KPG-2 `payment_url` checkout (H) |
| Public docs | No. Blogs only | Partial (doc.connectips.com) | Yes (developer.esewa.com.np) | Yes (docs.khalti.com) |
| Sandbox / test creds | No public sandbox found (UNVERIFIED) | No public UAT found | Yes: product code `EPAYTEST`, public test secret key (M/H) | Yes: dev.khalti.com, IDs 9800000000–05, MPIN 1111, OTP 987654 (H) |
| Auto-confirm | Yes: WebSocket push + status API `thirdPartyDynamicQrGetStatus` (M) | Spec has a `/nepalpay/confirm-payment` callback + `merchanttxnreport` (M) | Yes: status-check API (GET) (H) | Yes: `/epayment/lookup/` (H). No webhook documented (H for absence in docs) |
| Merchant fee (domestic) | **Free** per Fonepay schedule, effective 2082-04-01 (17 Jul 2025). The acquiring bank may pass on charges (H for Fonepay's schedule, L for bank pass-through) | Not published (UNVERIFIED) | Domestic QR "free" per an eSewa blog. Gateway "nominal merchant service fee", rate not published (L). **Flag** | Gateway commission set in the merchant agreement, not published (M). **Flag** |
| Payers accepted | Nearly all bank apps + eSewa, Khalti, MyPay, etc. (H, Fonepay site) | Bank apps, connectIPS, Khalti. **eSewa cannot pay NepalPay QR** (M) | eSewa wallet only (gateway) | Khalti wallet + linked banks (gateway) |
| Onboarding | Bank or Fonepay enrolment form. PAN or non-PAN merchant, settlement bank/wallet account (M) | Through the merchant's bank (acquirer) (M) | Registered business, PAN/VAT, citizenship, bank details. Tax clearance listed for API merchants (L/M) | Business registration or individual, PAN, settlement bank account (M). Self-serve sandbox signup |

---

## 2. Findings per provider

### 2.1 Fonepay (most important for a restaurant)

- **Static vs dynamic.** Fonepay's business page offers static QR (download it from the portal) and dynamic QR "generated within your POS / billing system". You request dynamic QR by email or phone, or through your bank. (H, [fonepay.com/business](https://fonepay.com/business), fetched 2026-11-23)
- **API shape.** Taken from 2026 developer blogs, not official docs (M, consistent across 3+ independent guides):
  - Base: `https://merchantapi.fonepay.com/api`
  - Create QR: `POST /merchant/merchantDetailsForThirdParty/thirdPartyDynamicQrDownload` with `amount, remarks1, remarks2, prn, merchantCode, dataValidation, username, password, taxAmount, taxRefund`
  - `dataValidation` = HMAC-SHA512 (hex) of `amount,prn,merchantCode,remarks1,remarks2`, keyed with the merchant secret
  - Response: `qrMessage` (EMV payload to render) and an optional `thirdpartyQrWebSocketUrl`
  - Status: `POST .../thirdPartyDynamicQrGetStatus` with `prn, merchantCode, username, password, dataValidation` (HMAC of `prn,merchantCode`). It returns `paymentStatus` (`success`/`pending`/other) and `fonepayTraceId`.
  - Recommended flow: the WebSocket event updates the UI to "verifying", then the backend confirms via the status API. **The WebSocket message format is not documented (UNVERIFIED).**
  - Sources: [baralkushal blog, Sep/Oct 2026](https://blog.baralkushal.com.np/blog/integrate-fonepay/), [poudelraj Node.js guide](https://poudelraj.com.np/blog/fonepay-dynamic-qr-integration-nodejs), [bwishernepal guide](https://blog.bwishernepal.com/implementing-dynamic-qr-code-payments-with-fonepay-nepal-in-node-js-a-complete-developer-guide/)
- **Sandbox.** No public sandbox or UAT host was found. Guides mention that "UAT vs live responses differ slightly", which implies Fonepay or the bank grants UAT on request. (L / UNVERIFIED)
- **Fees.** Fonepay's official Fees & Charges page lists "QR Payments at Merchant Outlets: Any amount: Free", effective Shrawan 01, 2082 (17 Jul 2025). Network fees are charged to member banks, so ask your acquiring bank whether it passes anything on. Instant settlement may carry a small charge. Multi-session settlement is free. (H for the schedule, [fonepay.com/content/fees-and-charges](https://fonepay.com/content/fees-and-charges); L for settlement, [paybridgenp, Oct 2026](https://paybridgenp.com/blog/fonepay-merchant-registration-guide)). **API/dynamic-QR pricing is not published (UNVERIFIED; may be free, may be bank-negotiated).**
- **Onboarding.** Use the enrolment form (Fonepay Business app or website) or go through a member bank. The form asks for owner citizenship number, contact details, business type, and whether you are a PAN or non-PAN merchant. You also choose a settlement bank or wallet. (M)
- **Interoperability.** Fonepay states that almost all banks and wallets can pay its QR, including eSewa, Khalti, MyPay, and Namaste Pay, plus UnionPay, Alipay, and UPI. (H, Fonepay site; M, [gadgetbyte, Mar 2026](https://www.gadgetbytenepal.com/business-qr-in-nepal/))

### 2.2 NepalPay QR (NCHL) / NepalQR

- **Standard.** NRB issued the *NepalQR Standardization Framework and Guidelines* in Feb 2021 (effective 1 Mar 2021). It is EMVCo merchant-presented, aimed at interoperability. (H, [NRB](https://nrb.org.np/psd/nepalqr-standardization-framework-and-guidelines), [PDF](https://www.nrb.org.np/contents/uploads/2021/01/QR-Code-Guidelines-and-Framework-and-Specifications.pdf))
- **Interoperability directive.** An NRB circular of 4 Feb 2022 required digital payment providers to become interoperable within 6 months. (M, [Kathmandu Post, 2022-02-09](https://kathmandupost.com/money/2022/02/09/central-bank-enforces-interoperability-to-promote-digital-payments)) **In practice, as of 2026, interoperability is incomplete.** Khalti pays Fonepay, NepalPay, and SmartQR. eSewa pays Fonepay only. eSewa↔Khalti direct transfers are still not possible. (M, [gadgetbyte, Mar 2026](https://www.gadgetbytenepal.com/business-qr-in-nepal/), [digitalsolutionnepal](https://digitalsolutionnepal.com/khalti-esewa-qr-interoperability-nepal/))
- **API.** The NEPALPAY QR API spec on doc.connectips.com lists `POST /qr/generateQR` (static PoI 11 / dynamic PoI 12, returns `qrString` + `validationTraceId`), Basic auth + SHA256withRSA message signing, a merchant-side callback `POST /nepalpay/confirm-payment`, and `merchanttxnreport` for lookups. Finality: `debitStatus=000` and `creditStatus` in {000, DEFER, 999}. It does not publish a UAT host. Per NCHL operating rules, APIs are exposed via NPI to network members/acquirers. (M, [API spec](https://doc.connectips.com/docs/NepalPAY-QR/NEPALPAY%20API%20Specification/API%20Specification%20Details), [operating rules PDF, 2023](https://nchl.com.np/wp-content/uploads/2023/02/1.pdf)) **Whether a single restaurant can get direct access is UNVERIFIED. Likely it needs an acquiring bank to sponsor it.**
- **Fees.** No published merchant MDR found. (UNVERIFIED)

### 2.3 eSewa

- **QR.** The eSewa Business QR is a static counter QR on Fonepay rails. No public dynamic-QR API was found. (M)
- **Online gateway (ePay v2).** Official docs (H/M; the portal renders with JS, so details were cross-checked with mirrors and SDK READMEs):
  - Form POST: UAT `https://rc-epay.esewa.com.np/api/epay/main/v2/form`, prod `https://epay.esewa.com.np/api/epay/main/v2/form`
  - Signature: HMAC-SHA256 (base64) over `signed_field_names` = `total_amount,transaction_uuid,product_code`
  - Status check: `GET /api/epay/transaction/status/?product_code=&total_amount=&transaction_uuid=`. **The UAT host is ambiguous: `rc.esewa.com.np` vs `uat.esewa.com.np` (L — test both).** Statuses: COMPLETE, PENDING, FULL_REFUND, AMBIGUOUS, NOT_FOUND, CANCELED.
  - Test creds: product code `EPAYTEST`, public test secret `8gBm/:&EnhH.1/q` (M, widely documented). Test eSewa user logins (e.g. 9806800001 / Nepal@123 / token 123456) appear in community code. (L / UNVERIFIED)
  - No webhook is documented. Confirmation works through the redirect with a base64 `data` payload plus the status API. (M)
  - Sources: [Epay-V2 docs](https://developer.esewa.com.np/pages/Epay-V2), [jentic mirror](https://jentic.com/apis/esewa.com.np/esewa.md), [Drupal module](https://www.drupal.org/i/3378772)
- **POS use.** The gateway is browser-redirect based. A POS could encode a URL to its own backend page in a QR. That page auto-submits the signed form, the customer pays on their phone, and the backend polls the status API. This is technically feasible but awkward compared with scanning a merchant QR inside the wallet app. (Inference)
- **Fees / onboarding.** Requirements: registered business, PAN/VAT, citizenship, photos, and tax clearance for API integration. Domestic QR is "free". The gateway charges a "nominal merchant service fee" whose rate is not published. **Flag.** (L, [eSewa blog docs list](https://blog.esewa.com.np/documents-required-esewa-merchant-api-integration), [UnionPay post](https://blog.esewa.com.np/unionpay-on-esewa-business-qr))

### 2.4 Khalti

- **KPG-2 (official, H, [docs.khalti.com](https://docs.khalti.com/khalti-epayment/), fetched 2026-11-23):**
  - `POST https://dev.khalti.com/api/v2/epayment/initiate/` (prod `khalti.com/api/v2`), header `Authorization: Key <secret>`. Amount is in **paisa**. Returns `pidx` and a `payment_url` (expires in 60 min).
  - `POST /epayment/lookup/` with `pidx` returns Completed / Pending / Initiated / Refunded / Expired / User canceled. Only `Completed` counts as paid.
  - Sandbox: self-serve merchant signup at test-admin.khalti.com. Test IDs 9800000000–9800000005, MPIN 1111, OTP 987654.
  - **No webhook is documented.** The redirect to `return_url` plus lookup is the confirmation path.
- **POS use.** You can render `payment_url` as a QR on the POS screen. The customer scans it, opens Khalti web checkout, and pays. The backend polls lookup. This is fully automatable and has a real sandbox, but it covers Khalti payers only. (Inference, H on the APIs)
- **Counter QR.** Khalti Pasal static QR pays into the merchant wallet. (L)
- **Fees.** Gateway commission is per merchant agreement and not published. **Flag.** (M, [paybridgenp Khalti guide, Oct 2026](https://paybridgenp.com/blog/khalti-merchant-account-guide))

---

## 3. Can a POS reliably auto-confirm a QR payment?

| Approach | Auto-confirm? | Notes |
|---|---|---|
| Static QR (any network) | **No.** Staff check the merchant app or bank SMS | Zero integration and zero fees. The common practice in Nepali restaurants today |
| Static QR with amount injected (EMVCo tag 54, PoI→12) | No (amount is pre-filled, but confirmation stays manual or comes from SMS parsing) | Field-tested on one Fonepay QR with eSewa. **Probably outside the merchant agreement, so treat it as a grey area.** Tag 62 broke NMB SMS alerts in one test. (L, [nepalqr repo](https://github.com/anishbhattarai-asm/nepalqr)) |
| Static QR + bank SMS/notification parsing on an Android device | Semi-auto | Fragile: alerts carry no order ID, so you have to match on amount (with paisa offsets) or the RRN. Needs an SMS-reading Android app, which brings Play Store policy issues (Inference) |
| **Fonepay dynamic QR API** | **Yes** (WebSocket + status API) | Best fit: one QR accepted by almost all banks, eSewa, and Khalti. Blocked on getting credentials from Fonepay or your bank. No public sandbox |
| NepalPay dynamic QR | Yes (callback + report API) | Needs an acquirer/NPI relationship. eSewa users can't pay |
| Khalti / eSewa gateway link-as-QR | Yes (lookup / status API) | Each covers one wallet only. Gateway commission applies. Good sandboxes |

---

## 4. Open-source Node/TypeScript code

| Package / repo | What | Confidence / notes |
|---|---|---|
| [anishbhattarai-asm/nepalqr](https://github.com/anishbhattarai-asm/nepalqr) | Zero-dep JS, MIT. EMVCo/NepalQR parse/build, static→dynamic amount injection, bank-alert parser, paisa-offset tab matching | 1 star, 9 commits. Useful as a reference for the EMV TLV + CRC16 code |
| [poudelraj Fonepay Node guide](https://poudelraj.com.np/blog/fonepay-dynamic-qr-integration-nodejs), [bwishernepal guide](https://blog.bwishernepal.com/implementing-dynamic-qr-code-payments-with-fonepay-nepal-in-node-js-a-complete-developer-guide/) | Fonepay dynamic QR in Node (blog code) | M. Small enough to write ourselves (HMAC-SHA512 + 2 POSTs + WebSocket) |
| `esewajs`, `esewa-pay`, `nestjs-esewa`, `neppayments`, `@nabwin/paisa` (npm) | eSewa / Khalti wrappers | L. Small community packages. Check maintenance and supply-chain risk (Socket flags exist). Both APIs are simple enough to implement directly with `fetch` + `crypto` |
| [broisnees/nepal-payments-skill](https://github.com/broisnees/nepal-payments-skill) | Notes on NepalPay QR / HamroPay / GetPay | L |

Paid aggregators exist (PayBridgeNP, APINepal, Blanxer at 2.75–3.9%). They are **excluded by the no-paid-SaaS constraint**.

---

## 5. Recommendation for MVP

1. **MVP (v1): static interoperable QR + manual confirmation. No integration, no fees.**
   - The restaurant uses its existing **Fonepay** static QR from its bank or Fonepay. Fonepay has the widest payer coverage (all banks, eSewa, Khalti). Optionally it can also have a NepalPay QR.
   - The POS stores the merchant's QR payload or image in settings and shows it on the bill/checkout screen with the amount in large text.
   - Payment method "QR (Fonepay/NepalPay)" moves to `awaiting_confirmation`. A cashier taps **Confirm received** after checking the Fonepay Business app or bank SMS. Record who confirmed it, when, an optional last-4 of the trace ID, and payer name, for end-of-day reconciliation.
   - Add an end-of-day reconciliation screen that lists QR payments, so staff can tick them off against the Fonepay merchant statement.
2. **Optional v1.5: amount pre-fill.** Render a dynamic-style QR by injecting tag 54 into the merchant's static payload, which cuts customer typos. **Only do this after confirming with the acquiring bank that it is permitted**, and test it per bank. Default it to off.
3. **v2: automated confirmation via the Fonepay Dynamic QR API.** Design a `PaymentProvider` interface now (`createQr(amount, billRef) → {qrPayload, providerRef}`, `getStatus(ref)`, optional push channel). The backend (not the Expo app) holds the credentials, opens the WebSocket, polls the status API with backoff as a fallback, and marks the bill paid idempotently on `success` + `fonepayTraceId`. Each restaurant deployment stores its own merchant code and secret.
   - Action item: have the pilot restaurant ask its bank or Fonepay for dynamic QR / API credentials and UAT access, and get any fee in writing. Merchant QR is free per Fonepay's schedule, but API fees are unpublished.
4. **Later or optional:** Khalti KPG-2 / eSewa ePay "link-as-QR" for online or pre-orders, which come with gateway commission (**flagged**). NepalPay dynamic QR only if an acquiring bank offers it. Keep SMS parsing out of scope (fragile, Play Store SMS-permission policy).

**Bottom line:** the only route to auto-confirmed QR payments that is fee-free and covers all wallets looks like the **Fonepay dynamic QR API**. It is gated by credentials, not code. Ship static QR with manual confirmation first, and build the provider abstraction so automation slots in without UI changes.

---

## 6. Open questions / to verify

- Fonepay: is dynamic-QR/API access charged? Is there a UAT host? What is the WebSocket message schema? Is the merchant-facing status API rate-limited? (UNVERIFIED)
- Does the pilot restaurant's acquiring bank pass on any QR MDR? (UNVERIFIED)
- NepalPay QR: can a single merchant get `generateQR` access, or only through its acquirer? Is there a merchant MDR? (UNVERIFIED)
- eSewa: which UAT status host is current (`rc.` vs `uat.`)? Exact gateway service fee? (L)
- Is injecting an amount into a static QR allowed under Fonepay/bank merchant T&Cs? (UNVERIFIED)
- One secondary source (nepaldatabase.com) claims wallet MDR of "1.2–1.5%" and NRB circulars 02/2026 and 07/2026. These were **not verified** and conflict with Fonepay's official "free" schedule. Ignore them until confirmed on nrb.org.np.

## 7. Sources (accessed 2026-11-23 unless noted)

- Fonepay business page: https://fonepay.com/business
- Fonepay Fees & Charges (effective 17 Jul 2025): https://fonepay.com/content/fees-and-charges
- Fonepay merchant enrollment: https://fonepay.com/public/business/merchant-enrollment
- Fonepay dynamic QR guide (Sep 2026, upd. Oct 2026): https://blog.baralkushal.com.np/blog/integrate-fonepay/
- Fonepay Node.js guide: https://poudelraj.com.np/blog/fonepay-dynamic-qr-integration-nodejs
- Fonepay registration guide (PayBridgeNP, Jul 2026, upd. Oct 2026, vendor-biased): https://paybridgenp.com/blog/fonepay-merchant-registration-guide
- Merchant QR cost in Nepal (Aug 2026): https://digitalsolutionnepal.com/merchant-qr-cost-nepal/
- Business QRs in Nepal (Gadgetbyte, 18 Mar 2026): https://www.gadgetbytenepal.com/business-qr-in-nepal/
- Khalti/eSewa interoperability gap: https://digitalsolutionnepal.com/khalti-esewa-qr-interoperability-nepal/
- NRB NepalQR framework (Feb 2021): https://nrb.org.np/psd/nepalqr-standardization-framework-and-guidelines
- NRB interoperability circular coverage (Kathmandu Post, 9 Feb 2022): https://kathmandupost.com/money/2022/02/09/central-bank-enforces-interoperability-to-promote-digital-payments
- NEPALPAY QR API spec: https://doc.connectips.com/docs/NepalPAY-QR/NEPALPAY%20API%20Specification/API%20Specification%20Details
- NEPALPAY QR operating rules (NCHL, 2023): https://nchl.com.np/wp-content/uploads/2023/02/1.pdf
- NEPALPAY QR overview: https://connectips.com/knowmore/nepalpayqr/
- eSewa ePay v2 docs: https://developer.esewa.com.np/pages/Epay-V2
- eSewa API mirror: https://jentic.com/apis/esewa.com.np/esewa.md
- eSewa Drupal module (test endpoints): https://www.drupal.org/i/3378772
- eSewa merchant API documents: https://blog.esewa.com.np/documents-required-esewa-merchant-api-integration
- eSewa UnionPay on Business QR (fees): https://blog.esewa.com.np/unionpay-on-esewa-business-qr
- Khalti KPG-2 docs: https://docs.khalti.com/khalti-epayment/
- Khalti merchant account guide (PayBridgeNP, Jul 2026, upd. Oct 2026): https://paybridgenp.com/blog/khalti-merchant-account-guide
- nepalqr toolkit: https://github.com/anishbhattarai-asm/nepalqr
