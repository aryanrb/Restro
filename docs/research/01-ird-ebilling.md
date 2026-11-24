# 01: Nepal IRD e-billing, CBMS and invoice rules for a restaurant POS

Research date: **2026-11-23** (BS 2083 Mangsir). Current Nepali fiscal year: **2083/84** (Shrawan 1, 2083 to Asar end, 2084; roughly mid-Jul 2026 to mid-Jul 2027).

Confidence levels: **High** = read in a primary IRD or government text. **Medium** = several consistent secondary sources, or a primary text that may have been superseded. **Low / UNVERIFIED** = a single secondary source, conflicting sources, or inference.

> **Biggest caveat.** A new **Electronic Billing Procedure, 2082** (विद्युतीय बीजक सम्बन्धी कार्यविधि, २०८२) exists as a PDF on a government CDN. Its preamble says it repeals the 2074 procedure where the two conflict. I could not download it: the host refused connections from this environment. Everything below that comes from the procedure is based on the **2074 procedure (amended)**, which I did read in full. Its technical standards are very likely carried over, but **someone must read the 2082 text before we build the compliance features**. Link: [2082 procedure PDF](https://giwmscdntwo.gov.np/media/pdf_upload/%E0%A4%B5%E0%A4%BF%E0%A4%A6%E0%A5%8D%E0%A4%AF%E0%A5%81%E0%A4%A4%E0%A5%80%E0%A4%AF%20%E0%A4%AC%E0%A5%80%E0%A4%9C%E0%A4%95%20%E0%A4%B8%E0%A4%AE%E0%A5%8D%E0%A4%AC%E0%A4%A8%E0%A5%8D%E0%A4%A7%E0%A5%80%20%E0%A4%95%E0%A4%BE%E0%A4%B0%E0%A5%8D%E0%A4%AF%E0%A4%B5%E0%A4%BF%E0%A4%A7%E0%A4%BF,%20%E0%A5%A8%E0%A5%A6%E0%A5%AE%E0%A5%A8_ab3ktjz.pdf).

---

## 1. Does billing software need IRD approval?

**Short answer: yes. There are two separate steps.**

1. **The software is enlisted** (सूचीकरण) by its producer or vendor with the IRD.
2. **Each business gets a user permit** (अनुमतिपत्र) from its own tax office before it may issue invoices electronically.

### Legal basis
- The Electronic Billing Procedure, 2074 was issued under VAT Act 2052 §14(5) and §16क(2). It took effect on 2074-09-16 BS and was amended four times; the latest consolidated copy on IRD is dated 2077-05-02. **High.** ([IRD page](https://ird.gov.np/content/5490/tax-laws-16910453028/), [full text copy (2076)](https://www.dumarwanacampus.edu.np/storage/app/public/notice/file/20190703082608.9808.pdf))
- The Electronic Billing Procedure, 2082 replaces it where they conflict. Contents are **UNVERIFIED** (see the caveat above).
- Finance Act 2083 reportedly rewrote VAT Act §14क. That section now lets the IRD require taxpayers to connect to CBMS, or to use a billing system the IRD provides. It also adds graded fines: **Rs 5 lakh** for using software that can delete or alter billing data, and **Rs 1 lakh** for other e-billing breaches. **Medium.** All sources are secondary and the Gazette text was not read. ([nexusadvisory, checked 2026-10-08](https://nexusadvisory.com.np/ird-approved-billing-software-nepal/), [neuralyug 2026-09-15](https://neuralyug.com/blog/ird-e-billing-cbms-automation-nepal/), [Nepal Business](https://nepalbusiness.com/News/Details/7162))

### Process under the 2074 procedure (primary text, clauses 3–4)

| Step | Who | What | Clause |
|---|---|---|---|
| Enlistment | **Producer** (domestic software) or **authorised distributor** (imported software) | Self-certifies in the Annex 1 format that the software meets all Chapter 4 standards, and submits it to the IRD. Attachments: PAN, company registration and tax clearance certificates. The IRD publishes the list of enlisted software. | 3(क), 3(ख) |
| Consent letter | Vendor → each customer | Annex 2 letter consenting to the customer's use of the software. The vendor also informs the IRD. | 3(ग) |
| User permit | **The business** | Applies to its tax office (IRO, LTO or TSO) in the Annex 3 format, listing servers, server locations and terminal count. Attachments: device spec, user manual, data-integrity method, backup and recovery plan, system architecture, PAN, registration and tax clearance certificates, the setup file on a pen drive, and **20 sample bills in Excel**. | 4(ख) |
| Review | Office chief + IT officer | The IT officer checks that the software is enlisted and meets Chapter 4. For **cloud** systems, the office must also confirm that **bill data reaches CBMS at the moment of issue**. | 4(ग), 4(ङ) |
| Permit | Office chief | Issues the Annex 4 permit for the head office or a branch. | 4(घ), 4(च) |

- The applicant (the business) is responsible for proving to the tax office that the system complies (4(छ)). **High.**
- Today the user permit is filed online through the Taxpayer Portal ("Electronic Billing User Permission" menu). Reported supporting documents include sample prints and reprints, a credit note, a cancelled bill, an audit-trail report and a CBMS test report. **Medium.** ([nexusadvisory](https://nexusadvisory.com.np/ird-approved-billing-software-nepal/))
- **Cost:** the procedure names no fee. **Low:** none found, not confirmed.
- **Timeline:** there is no published SLA. **UNVERIFIED.** Vendor blogs describe weeks to months.
- Some secondary sources mention **source-code submission** and **re-enlistment** of each major version. **UNVERIFIED and conflicting.** ([ekagajpatra](https://www.ekagajpatra.com/blog/Process-and-Documents-Required-for-IRD-Approval-of-e-billing%3A))
- Once a business has a permit, **every sale must be billed through it**. Using another method (for example, a manual pad during an outage) needs the tax office's prior approval (8(क), 8(ख)). **High.**

### Technical standards (2074 procedure, clause 6 software, clause 7 cloud, clause 8 operation). **High** for 2074; check against 2082.

- **6(क)** Built on a real DBMS that can process SQL queries.
- **6(ख), 6(ट)** No deletion of any entered data. Corrections end a record's validity and add a new record. The software must provide a viewable and printable **report of corrected records**.
- **6(ग)** Audit trail / log archive enabled for every database action. Backup and recovery supported.
- **6(घ)** Every record carries its entry date, time, validity status and user identity.
- **6(ङ)** A table or materialized view in the **Annex 5** shape, with invoice numbers issued **sequentially per fiscal year**, plus a viewable and printable report. Annex 5 fields: `Fiscal Year, Bill_no, Customer_name, Customer_Pan, Bill_Date, Amount, Discount, Taxable_Amount, Tax_Amount, Total_Amount, Sync_with_IRD, IS_Bill_Printed, Is_bill_Active, Printed_Time, Entered_By, Printed_by, Is_realtime`.
- **6(च)** A bill can be printed **only once** as the original. Any reprint must clearly show **"प्रतिलिपि" / "Copy of Original"** and **how many times it has been printed**. The print count is stored and available for investigation.
- **6(छ)** The printed bill follows the format of the relevant Act (VAT, Income Tax, Excise and so on).
- **6(ज)** Cancellation is allowed **with a recorded reason**. The cancellation updates the master bill (Annex 5), and a cancelled-bills report is required.
- **6(झ)** Invoice and **Sales Book** in the Annex 6 formats (see §4).
- **6(ञ)** A per-user audit trail / activity-log report, viewable and printable.
- **6(ठ)** Every table exportable to **Excel or XML, and PDF**.
- **6(ड)** An authorised user can back up the database and the log archive.
- **6(ढ)** User manual in Nepali or English, printed and also as in-app help.
- **6(ण)** Can send data to **CBMS via web service / Web API**.
- **7 Cloud:** the **central server must be inside Nepal**, its location must be reported to the IRD and the tax office in advance, and the IRD must have access to it. On a **rented server**:
  - the server operator must be registered in Nepal;
  - the architecture must be multi-tenant with each customer's data stored separately;
  - there must be an **Annex 7 tripartite data-storage and security agreement** between vendor, server operator and business, with a copy filed with the IRD.
  - Only multinationals may host abroad, and only with a separate audit-log server in Nepal (7(ग)).
- **8(घ)** Back up each fiscal year's database and logs, and **keep them for the full statutory record-keeping period**. That period is commonly 6 years under the Income Tax Act. **Medium:** inferred, not stated in the procedure.

---

## 2. Turnover thresholds: who must use e-billing and CBMS

| Who | Threshold | Status | Confidence |
|---|---|---|---|
| **All taxpayers that bill consumers directly** (banks and financial institutions excluded) | Annual turnover **above Rs 20 crore** must e-bill **and connect to CBMS at the moment each invoice is issued** | IRD notice of **4 Baisakh 2083** (≈2026-04-17), citing the IRD decision of 2082-12-27 | **High** ([IRD notice](https://ird.gov.np/content/13488/cbms-notice-01-04/)) |
| **Hotels, restaurants, canteens**, ISPs | **Above Rs 5 crore** must e-bill | Notice of 29 Falgun 2075 (≈2019-03-13), reaffirmed in later notices (20 Jestha 2079). Whether CBMS real-time sync is also mandatory at 5 crore is unclear; one vendor guide says restaurants above 5 crore must sync with CBMS. | **Medium** ([nexusadvisory](https://nexusadvisory.com.np/ird-approved-billing-software-nepal/), [Tally help](https://help.tallysolutions.com/?p=191764)) |
| Department stores, marts | Above Rs 10 crore | Same notices | Medium |
| General line (announced) | Rs 10 crore, for businesses already issuing e-bills | Budget 2083/84 speech (15 Jestha 2083). **No implementing IRD notice found as of Oct–Nov 2026.** | Medium |
| Earlier history | Rs 25 crore (notice of Dec 2023 / FY 2080/81) → Rs 20 crore (2026) | Superseded | Medium |

**Key point for restaurants.** Below Rs 5 crore, e-billing is **voluntary**, but clause 4(क) says that **anyone who chooses to issue bills electronically must hold a permit**. A legal tax invoice printed by our software therefore needs an enlisted product plus a user permit, whatever the restaurant's turnover. **High** (primary text).

Also relevant: VAT Rules Rule 17 and an IRD notice of 26 Asar 2083 (2026-07-10) require an invoice for **every** sale, enforced by market monitoring. **High** ([IRD notice](https://ird.gov.np/content/13574/notice-regarding-issuance-of-will-invoice-/))

---

## 3. CBMS integration

- **API exists and is public enough to build against.** Base URL `https://cbapi.ird.gov.np`. Older documents show `202.166.207.75:9050`. **Medium–High.** ([IRD sample doc (gov CDN, ~2022)](https://giwmscdnone.gov.np/media/pdf_upload/976029276_qtd1hcg.pdf), [Frappe forum](https://discuss.frappe.io/t/central-billing-monitoring-system-api-documentation-for-nepal/34814), [ird-cbms client README](https://github.com/Manjish/ird-cbms))
  - `POST /api/bill` posts a sale.
  - `POST /api/billreturn` posts a sales return / credit note.
- **Format:** JSON. Credentials go **in the body** on every call: `username`, `password` (the taxpayer's own IRD portal login) and `seller_pan`. No OAuth or tokens. **Medium.**
- **Bill fields:**
  - `fiscal_year` (e.g. `"2083.084"`), `invoice_number`, `invoice_date` (**BS, `YYYY.MM.DD`**)
  - `buyer_pan`, `buyer_name`
  - `total_sales`, `taxable_sales_vat`, `vat`, `excisable_amount`, `excise`, `taxable_sales_hst`, `hst`, `amount_for_esf`, `esf`, `export_sales`, `tax_exempted_sales`
  - `isrealtime` (bool), `datetimeClient` (casing varies between sources)
- **Return fields:** the bill fields plus `ref_invoice_number`, `credit_note_number`, `credit_note_date`, `reason_for_return`, with amounts as positive numbers. **Medium.**
- **Response codes** (field-observed):

  | Code | Meaning |
  |---|---|
  | 200 | OK |
  | 100 | Bad credentials |
  | 101 | Bill already exists. Treat as success: it makes retries idempotent. |
  | 102 | Save exception |
  | 103 | Unknown error |
  | 104 | Invalid model |
  | 105 | Bill not found for return |

  **Medium.**
- **Test account:** shared, `Test_CBMS` / `test@321`, PAN `999999999`. Use unique invoice numbers. **Medium.**
- **Real-time vs batch:** the procedure (4(ङ), 8(ग)) requires data to reach CBMS **at the moment of issue** for cloud systems and for designated taxpayers. Practice: `isrealtime=true` when the bill posts within about 5 minutes, otherwise `false` (late or queued). There is no batch endpoint; each bill is one call. **Medium.**
- **Validation is weak.** CBMS does not check VAT arithmetic, so correctness is on us. **Medium** (field-observed).
- **No official current spec page on ird.gov.np was found.** Get the current spec from the IRD IT section when applying. **UNVERIFIED** whether field names changed under the 2082 procedure.

---

## 4. Invoice rules

### VAT tax invoice (कर बीजक). VAT Act §14, VAT Rules 2053 Rule 17, Schedule 5/5A; e-billing Annex 6
Required on the face. **High** for the Annex 6 layout; **Medium** for the Rule 17 details.
- Heading **"Tax Invoice" / कर बीजक**
- Bill number
- **Transaction date** and **invoice issue date** (two separate fields)
- Seller's name, address, PAN
- Purchaser's name, address, PAN (needed when the buyer is registered and wants input credit)
- Method of payment (cash, cheque, credit, other)
- Line items: serial number, description (type, size, model, brand where relevant), quantity, rate, amount
- Discount
- **Taxable amount**
- **VAT 13%** shown separately
- **Total**
- **Amount in words**
- Authorised signature

Rule 17 assumes **triplicate** copies for paper (original to the buyer, one for the tax office, one for the seller). In e-billing this is replaced by the original print plus stored data, with reprints marked "Copy of Original (n)". **Medium.**

### PAN (non-VAT) bill. Income Tax Rules 2059, Rule 23(4); Annex 6(घ)
- Heading **"Invoice"**
- Bill number, transaction date, issue date
- Seller's name, address, PAN
- Purchaser's name, address, PAN
- Payment method
- Line items, discount, total, amount in words, signature
- **No VAT line**
- Sequential numbering

**Medium** (Annex 6 is primary; the rule citation is secondary).

### Numbering and dates
- Invoice numbers must be **sequential within each fiscal year** (6(ङ), 6(च)), so they reset or prefix per FY. Gap-free is the safe reading. **High.**
- The Nepali FY runs Shrawan 1 to Asar end.
- **Bikram Sambat dates:** CBMS **requires BS dates** (`YYYY.MM.DD`) and a BS fiscal year. **Medium–High.** The procedure's print template does not explicitly mandate BS on paper, but Nepali practice prints BS, often with AD alongside. **Low** that AD alone is non-compliant; printing both is safest.
- **Branches:** each outlet is listed in the permit. Per-outlet series are common practice. **UNVERIFIED.**

### Copy marking and reprints
- The original prints **once**. Every later print shows **"Copy of Original"** (or प्रतिलिपि) **plus the print count**, and the count is stored (6(च)). **High.**

### Void, cancel, returns
- **Cancel:** allowed with a reason. The bill stays in the database flagged inactive (`Is_bill_Active`), the master bill is updated, and a cancelled-bills report is required (6(ज)). Never delete (6(ख), 6(ट)). **High.**
  - Field practice reported to IRD: full cancellation ("reverse entry") on the same day. **Low / UNVERIFIED.**
- **Returns and price changes:** a **credit note** (or debit note) under VAT Rule 20 with:
  - a serial number
  - date
  - supplier's name, address, PAN
  - recipient's details if registered
  - **the original invoice number and date**
  - particulars and reason
  - amount and VAT credited or debited

  A monthly register of notes is required, and returns are posted to CBMS `billreturn`. **Medium–High.** ([nepaltaxes 2023-05-17](https://nepaltaxes.com/provisions-relating-to-invoices-guide-to-invoice-abbreviated-tax-invoice/), [LexisNexis Tolley](https://www.lexisnexis.co.uk/tolley/tax/commentary/eu-and-global-vat/32-nepal/credit-notes-debit-notes-nepal))

### Abbreviated tax invoice (संक्षिप्त कर बीजक). VAT Rule 18, Schedule 6
- **Needs the tax officer's prior permission.**
- For retail sales up to **Rs 10,000 per bill (VAT-inclusive)**. **Medium:** two secondary sources agree; the rule text was not read.
- The buyer **cannot claim input credit**, and the seller must issue a full tax invoice on request.
- VAT is computed as price × 13/113 (VAT-inclusive pricing).
- The Annex 6 e-billing template for it has no separate VAT line. **High.**
- **For MVP, always issue full tax invoices.** It is simpler and needs no extra permission.

---

## 5. VAT 13% and the 10% service charge

- **The service charge was struck down.** The Supreme Court constitutional bench annulled Labour Act 2074 §87(3) and Labour Rules 2075 Rule 82, effective **Magh 11, 2079 (≈2023-01-25)**, so hotels and restaurants may not add a 10% service charge. **High** (consistent reporting across many outlets). ([Kathmandu Post 2023-01-27](https://kathmandupost.com/money/2023/01/27/no-more-service-charge-anywhere-top-court-says), [Republica](https://myrepublica.nagariknetwork.com/index.php/news/sc-issues-mandamus-now-hotels-and-restaurants-cannot-take-service-charge), [Khabarhub](https://english.khabarhub.com/2023/26/290818/))
  - The full text was reportedly published around Baishakh 19, 2082 (≈May 2025). **Low.** ([Sharehub 2025-05-02](https://sharehubnepal.com/news/45958-artha-sarokar-service-charge-abolishment-harms-hotel-workers-nepal))
  - As of **Aug 2026** lawmakers were still asking for a Labour Act amendment to restore it, so **it was not reinstated as of then**. **Medium.** ([Onlinekhabar 2026-08](https://www.onlinekhabar.com/2026/08/1999567/hotel-workers-demand-continuation-of-10-percent-service-fee))
- **Menu prices must be VAT-inclusive.** The Department of Commerce told hotels and restaurants to show prices **including VAT** (and formerly the service charge) on menus. Adding VAT on top of menu prices risks fines under consumer-protection rules. **Medium.** ([Kathmandu Post 2022-09-28](https://kathmandupost.com/money/2022/09/28/vat-service-charge-added-to-food-bill-deemed-illegal), [Kathmandu Post 2023-01-06](https://kathmandupost.com/money/2023/01/06/restaurants-adding-vat-on-menu-price-to-face-fines))
  - The tax invoice must still show taxable value and VAT separately, back-calculated as **VAT = inclusive price × 13/113**.
- **Calculation order, if a service charge is ever charged:**
  1. Service charge = 10% of the (discounted) subtotal.
  2. VAT = 13% of (subtotal + service charge). The service charge is part of the taxable consideration under VAT Act §12.

  Effective multiplier ≈ 1.243. **Medium:** standard practice and consistent across sources, but no IRD circular was found.
- **Rounding:**
  - No official IRD rounding rule was found. **UNVERIFIED.**
  - CBMS takes decimal amounts. Store money in paisa (integers) and send values with 2 decimals.
  - Compute VAT on the invoice taxable total, not per line, to avoid drift.
  - If the total is rounded to the rupee, show the rounding as an explicit line. Practice, **Low.**
- Side note: the IRD runs a **10% VAT refund to consumers** who pay hotels, restaurants and bars electronically (QR, card, wallet). Businesses must display information about it. **Low–Medium.** ([ShareSansar 2024-09-10](https://www.sharesansar.com/index.php/newsdetail/ird-introduces-10-vat-rebate-for-electronic-payments-at-hotels-restaurants-and-bars-2024-09-10))

---

## Implications for MVP

### Does certification block launch?
- **Ordering, KOT, table management, kitchen display and the dashboard: no.** IRD rules cover the **invoice**, not order taking.
- **Printing a legal tax invoice or PAN bill from our software: yes, legally.** Clause 4(क) requires the business to hold a user permit, and that requires our product to be enlisted, **for any restaurant that bills electronically, at any turnover**. Above Rs 5 crore (restaurants) or Rs 20 crore (everyone) it is mandatory with CBMS.
- **Interim option:** pilot restaurants keep issuing manual IRD-format bills. Our printouts say **"Estimate / Not a tax invoice"** until enlistment and the permits are granted. One reviewed implementation does exactly this. **Low–Medium** that tax offices accept this during a pilot; **confirm with the local IRO**.
- **Critical path:** start the enlistment conversation with the IRD early, in parallel with the build.

### Hosting constraint (architecture-level, decide now)
- **The production server must be physically in Nepal** (clause 7). That rules out AWS, GCP or Azure regions outside Nepal for the billing database. **High** for 2074; re-check against 2082.
- **One deployment per restaurant** fits the multi-tenancy / data-separation clause well.
- Each deployment needs:
  - a Nepal-registered hosting operator
  - the server location reported to the IRD
  - IRD access to the server, so plan a read-only auditor role
  - an Annex 7 tripartite agreement

### Must have on day one, built into the data model even if CBMS is switched on later
1. **Immutable invoices.** No UPDATE or DELETE on invoices or invoice lines, enforced in the database (triggers or permissions), not only in application code. Corrections happen only through cancellation with a reason, or a credit note.
2. **Gap-free invoice numbers per fiscal year** (BS FY, with Shrawan 1 rollover), allocated server-side in a transaction. Separate series for credit notes.
3. **BS date support** throughout. Store AD timestamps plus a computed BS date and FY. Print BS (and AD). CBMS needs `YYYY.MM.DD` BS.
4. **Print control.** The first print is the original. Every later print is "Copy of Original (n)", and each print is logged with time and user. The Android app must print **through the server** so the count is authoritative.
5. **Audit trail.** Every create, cancel, print and login is recorded with timestamp and user. Keep it append-only, and ideally hash-chained.
6. **Annex 5 master-bill view** and **Annex 6 Sales Book** reports, exportable to Excel, XML and PDF.
7. **Invoice templates** for VAT tax invoice and PAN bill with all required fields, amount in words, and buyer PAN capture.
8. **VAT-inclusive menu pricing**, with taxable value and VAT back-calculated at 13/113. Money stored in integer paisa.
9. **Service charge configurable and off by default** (the Supreme Court ruling). If it is ever enabled, VAT applies after it.
10. **Backup and restore**, with year-wise database and log archives retained about 6 years or more.
11. **A CBMS outbox:** an outbox row written in the same transaction as the invoice, plus a retry worker. Treat 101 as success, set `isrealtime` when the bill posts within 5 minutes, store credentials encrypted, and show the sync status per bill.

### Can wait
- Abbreviated tax invoices (they need separate permission, and full invoices work everywhere).
- Debit notes.
- Purchase book.
- HS codes.
- The IRD consumer VAT-refund display.
- Offline invoice issue on Android: avoid it at first, because it complicates numbering and real-time CBMS.
- Live CBMS posting can stay switched off for clients below the thresholds, but **build it in v1**: it is needed for the permit application (CBMS test report) and for any restaurant above Rs 5 crore.

### Open questions to resolve with the IRD or a Nepali tax advisor
1. Read the full **2082 procedure**: changes to enlistment, fees, source-code or version re-enlistment, and new fields.
2. Whether restaurants between Rs 5 and 20 crore must sync with CBMS in real time, or only e-bill.
3. Enlistment timeline and fee.
4. Whether a SaaS vendor can file one enlistment covering all per-restaurant deployments.
5. The current CBMS spec and any new auth scheme.
6. The status of the Rs 10 crore general threshold.
7. The rounding convention.

---

### Sources (access date 2026-11-23)
- IRD: Electronic Billing Procedure 2074 (4th amendment), page dated 2077-05-02. https://ird.gov.np/content/5490/tax-laws-16910453028/
- Full text of the 2074 procedure as amended (copy, 2019/2076). https://www.dumarwanacampus.edu.np/storage/app/public/notice/file/20190703082608.9808.pdf
- Electronic Billing Procedure 2082 (gov CDN, not retrieved). Link in the caveat above.
- IRD notice of 2083-01-04 (Rs 20 crore + CBMS). https://ird.gov.np/content/13488/cbms-notice-01-04/
- IRD notice of 2083-03-26 (invoice for every sale). https://ird.gov.np/content/13574/notice-regarding-issuance-of-will-invoice-/
- IRD e-billing urgent notice of 2079-03-03 (body not readable). https://ird.gov.np/content/8070/notice-16542603731/
- IRD CBMS API sample (gov CDN). https://giwmscdnone.gov.np/media/pdf_upload/976029276_qtd1hcg.pdf
- ird-cbms client README (field-tested notes). https://github.com/Manjish/ird-cbms
- Frappe forum, CBMS API. https://discuss.frappe.io/t/central-billing-monitoring-system-api-documentation-for-nepal/34814
- Nexus Advisory (checked 2026-10-08). https://nexusadvisory.com.np/ird-approved-billing-software-nepal/
- NeuralYug (2026-09-15). https://neuralyug.com/blog/ird-e-billing-cbms-automation-nepal/
- Reference implementation PR mapping the 2082 clauses (2026-11-12). https://github.com/sawrozpdl/go-serve/pull/33
- NepalTaxes, invoice and abbreviated invoice guide (2023-05-17). https://nepaltaxes.com/provisions-relating-to-invoices-guide-to-invoice-abbreviated-tax-invoice/
- Types of invoices / PAN bill (2023-03). https://embfnp.blogspot.com/2023/03/types-of-invoices-in-nepal.html
- Kathmandu Post on the service charge ruling (2023-01-27), the VAT/service-charge add-on (2022-09-28) and menu pricing (2023-01-06). Links in §5.
- Onlinekhabar, hotel workers seek service charge (2026-08). https://www.onlinekhabar.com/2026/08/1999567/hotel-workers-demand-continuation-of-10-percent-service-fee
- ShareSansar, 10% VAT refund on e-payments (2024-09-10). Link in §5.
