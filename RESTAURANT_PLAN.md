# Restaurant Management System — Planning Handoff

Context carried over from a planning chat. Use this as the starting point for continued planning and research in Claude Code (with the `superplanning` skill).

## Project goal

Build the best practical restaurant management product: very easy to use, fast during peak hours, and focused on solving the real problems restaurants face (not feature count). Target market: restaurants in Nepal first.

## Hard constraints

- **Everything free.** No paid services or SaaS dependencies. Open-source and self-hosted where possible.
- **Web + mobile app.** Mobile app for staff (waiter, cashier, kitchen); web dashboard for owner/manager.
- **TypeScript everywhere.** Developer is strongest in TypeScript and Next.js, expanding into backend.
- **Practical scope.** Ship a usable MVP early; avoid over-engineering.

## Architecture decisions (so far)

Monorepo with shared types:

```
restaurant-app/
├── apps/
│   ├── api/        → Node backend (Hono or Fastify) + tRPC + Socket.IO
│   ├── web/        → Next.js admin dashboard (owner/manager)
│   └── mobile/     → Expo React Native app (waiter, cashier, kitchen — role-based screens)
└── packages/
    ├── db/         → Drizzle schema + migrations
    ├── validators/ → Zod schemas
    └── types/      → shared TS types
```

- Tooling: Turborepo + pnpm workspaces
- Database: PostgreSQL (self-hosted)
- ORM: Drizzle
- Auth: Better Auth, role-based (Owner, Manager, Cashier, Waiter, Kitchen) + PIN login on shared devices
- Real-time: Socket.IO on the API server (no Pusher/Ably)
- Mobile UI: NativeWind; Web UI: Tailwind + shadcn/ui
- Push: Expo Notifications
- Printing: ESC/POS thermal printers (Bluetooth/network)
- Why a separate API: Next.js Server Actions can't serve the mobile app.

### Hosting (leaning)

- **Preferred: local server in the restaurant** (old PC / mini PC running API + Postgres on restaurant Wi-Fi). Free, and keeps working when internet drops. Optional Cloudflare Tunnel for owner remote access.
- **Alternative: Oracle Cloud Always Free VM.**
- Avoid for production: Render free (sleeps, breaks sockets), Supabase/Neon free (pausing, limits). Fine for dev.

### Mobile distribution

- Android first. Install APKs directly on staff devices (free). Play Store ($25 one-time) / App Store ($99/yr) optional later.
- Bluetooth printing needs an Expo development build (`npx expo run:android`), not Expo Go.

### Data model notes

Core entities: Restaurant, User, Table, MenuCategory, MenuItem, ItemVariant, Modifier, Order, OrderItem, Payment, InventoryItem, Recipe, StockMovement, Customer (with credit account), Reservation, Shift, AuditLog.

- Snapshot price on OrderItem (menu edits must not change old bills).
- Stock as a StockMovement ledger (purchase, sale, wastage, adjustment), not a mutable number.
- Optimistic locking (version column) on orders for concurrent edits.
- Issued invoices are never edited/deleted, only voided with a record.

## Real problems → features

| Real problem | Features |
|---|---|
| Orders lost or wrong | 3-tap ordering, one-tap modifier buttons, auto-routing to stations (bar, kitchen stations), course timing (hold/fire), repeat last order |
| Kitchen and floor out of sync | KDS with wait timers (yellow → red), "ready" vibration on waiter's phone, item-level status, instant 86 (out of stock greys out everywhere) |
| Money leakage | Void/cancel requires reason; post-kitchen voids need manager PIN; full audit log; discount limits by role; shift cash closing (expected vs actual); suspicious activity alerts to owner |
| Stock disappearing | Recipe-based deduction, periodic stock count with variance report, wastage logging, low stock alerts + suggested purchase list. Optional per item (track expensive items only at first) |
| Credit (udharo) in notebooks | Customer credit accounts, one-tap "on account", credit limits, statements, partial payments |
| Owner can't be everywhere | Live owner dashboard on phone, end-of-day summary notification, leakage alerts |
| Slow, unfair billing | Split by item/person/equal, merge/transfer tables, dynamic QR for eSewa/Khalti/Fonepay, correct VAT (13%) + service charge (10%), proper invoice numbering |
| High staff turnover | 10-minute learnability, role-limited screens, images/colors on menu items |
| Internet/power drops | Local-first server, offline queue on devices, clear connection status |

## Ease-of-use principles (product rules)

1. A new waiter learns it in 10 minutes.
2. Nepali + English language toggle.
3. Big touch targets, minimal typing.
4. Images or color codes for menu items.
5. Each role sees only what it needs.
6. Peak-hour speed is a feature: no spinners in the ordering flow.
7. Easy undo before sending to kitchen; controlled changes after.

## Roadmap

**MVP**
1. Monorepo setup, DB schema, auth + roles
2. Menu management (web)
3. Waiter ordering + table management (mobile)
4. KDS on kitchen tablet with timers, ready notifications, 86-ing (Socket.IO)
5. Billing: split bills, VAT + service charge, QR payment display, receipt printing
6. Void/discount controls + audit log
7. Shift cash closing
8. Owner live dashboard + daily sales report

**Phase 2:** credit customers, inventory with variance reports, wastage logging, suspicious activity alerts, purchases/suppliers.

**Phase 3:** reservations, QR table menu / self-ordering, loyalty.

**Phase 4:** online ordering/delivery, multi-branch, analytics.

## Open questions (need answers)

- Single restaurant client or multi-tenant SaaS? (affects `restaurantId` on every table, tenant isolation, MVP scope)
- Will the restaurant have a PC that stays on during business hours? (local server vs Oracle Cloud)
- Is there a real restaurant to test with / interview?
- Android only for MVP, or iOS too?

## Research needed

1. **Nepal IRD e-billing compliance:** current rules for electronic billing software, certification requirements, turnover thresholds, CBMS (Central Billing Monitoring System) integration, invoice numbering/void rules, required fields on a VAT bill.
2. **Nepal payments:** eSewa, Khalti, Fonepay dynamic QR / merchant APIs — what's free, merchant onboarding requirements, payment confirmation webhooks vs manual confirmation.
3. **Thermal printing from Expo/React Native:** maintained ESC/POS libraries (Bluetooth + network), Expo dev build compatibility, and how to print Devanagari (many printers lack Nepali fonts — likely need to render text as image).
4. **Offline sync approach:** compare free options (custom outbox queue with IndexedDB/SQLite, WatermelonDB, ElectricSQL, etc.) for a local-first Expo app; conflict handling for orders.
5. **Local network deployment:** how mobile devices discover the local server (fixed IP vs mDNS), running API + Postgres reliably on a cheap PC (Docker, auto-start, backups), Cloudflare Tunnel setup.
6. **Free notification channels:** sending credit statements and end-of-day summaries for free. Note: WhatsApp Business API and Nepali SMS gateways are generally paid — find free alternatives (Expo push, email, share-sheet links).
7. **Competitor analysis:** existing restaurant POS products used in Nepal and nearby markets (e.g. Petpooja in India) plus global references (Toast, Square) — what owners like, complain about, and pay for.
8. **Restaurant owner/staff interviews:** validate which problems hurt most; draft an interview script.
9. **Oracle Cloud Always Free:** current availability/limits in the Asia region as a fallback host.

## Developer context

- Name: Bishal. Frontend developer (TypeScript, Next.js), expanding into backend; PGDCA student.
- Working style: practical scope over complexity, minimal-side-effect changes consistent with existing conventions, learns by doing.
