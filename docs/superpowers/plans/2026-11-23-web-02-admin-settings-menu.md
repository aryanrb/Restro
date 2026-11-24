# Web Plan 2 — Admin: Settings, Floor, Staff and Menu Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Owners and managers can set up a restaurant entirely in the web app against the fake backend: restaurant profile and tax settings, areas and tables, kitchen stations, staff accounts and PINs, menu categories, items (with variants, colors and photos), modifier groups, and marking items out of stock (86).

**Architecture:** Same pattern as Plan 1. Screens call web-side API interfaces (`SettingsApi`, `FloorApi`, `StationApi`, `StaffApi`, `MenuApi`) through `useApi()`. A `localStorage`-backed fake implements each one now, and the real Connect adapters arrive with the backend developer's protos (one adapter task per service, like Plan 1 Task 10, in a later plan). Validation rules live in pure functions that both the forms and the fake backend use. TanStack Query holds each domain under one query key, and every save invalidates it.

**Tech Stack:** As Plan 1 (Next.js static export, React 19, TypeScript strict, Tailwind v4 + shadcn/ui, TanStack Query v5, Vitest, Playwright). New shadcn components: `dialog`, `select`, `switch`, `badge`, `tabs`. No new npm dependencies beyond those.

**Spec:** `docs/superpowers/specs/2026-11-23-restaurant-mvp-design.md` §7 (Settings, User, Floor, Station, Menu), §8.2, §8.10. Builds on `docs/superpowers/plans/2026-11-23-web-01-foundation.md`. Read its Global Constraints and the "Rulings" lines in `.superpowers/sdd/2026-11-23-web-01-foundation/progress.md`.

## Global Constraints

- Everything from Plan 1's Global Constraints still applies: only `web/` changes, `t()` with EN + NE for every string, 48 px touch targets, static export only, and components import types from `@/lib/api/types` (now also `@/lib/api/admin-types`), never from `@/gen`.
- Money is integer **paisa** as a JS `number` (Rs 1 = 100 paisa). The web never does tax arithmetic; it only stores the rates.
- Rates are stored in **basis points**: 13% = `1300`. Spec defaults: VAT 13% (`1300`), `pricesIncludeVat` true, service charge **off**, rate 10% (`1000`).
- Only OWNER and MANAGER reach `/admin/...` (Plan 1 `canAccess`).
- `billingMode` is shown, never edited, in the web. Switching to `TAX_INVOICE` is a server/deployment step (spec §6).
- No hard deletes of anything an order or bill could reference (tables, items, stations in use, staff). Use the `active` flag. Only empty areas, empty categories, unused stations and unused modifier groups can be deleted.
- A menu change must never alter old orders. That is the server's job (snapshots); the web just saves the current values.
- Out of scope here: the restaurant logo and the payment QR images from spec §7 Settings. Both are only used on bills and the payment screen, so they come with Plan 5 (billing).
- Every save of an entity with a `version` sends the version it was loaded with. A stale save is rejected with `STALE_VERSION`.

## Mock-first note

There are no backend contracts yet for these services. The interfaces below are the web's request to the backend developer. Add them to the "What the web needs from the backend developer" list when sharing (Task 2 Step 5).

---

## Review Focus

1. **Two managers edit the same menu item:** the second save is stale. Expected: the form says "This was changed by someone else. Latest version loaded", shows the latest values, and loses nothing silently. Test in Task 9.
2. **Prices typed the way people type them:** `1,250`, `1250.5`, `Rs 250`, ` 99 `, `12.345`, `-5`, `abc`, an empty field, `99999999`. Expected: the first four are accepted (`1,250` → 125000 paisa), the rest are rejected with a clear message, and nothing is silently rounded. Test in Task 1.
3. **An item with variants whose base price is empty, or all variants removed:** Expected: with variants, each variant needs a price and the base price is hidden; with no variants, the base price is required. Test in Task 2.
4. **Locking yourself out:** a manager deactivates themselves, demotes themselves, or edits an owner account; or the last active owner is deactivated. Expected: blocked with a clear message. Test in Task 3 (fake backend) and Task 8 (UI).
5. **Nepali name left blank:** Expected: every place that shows the name falls back to English, never an empty label. Test in Task 1.

---

## File structure (all under `web/`)

```
src/lib/money.ts                       # formatMoney, parseMoney, formatPercent, parsePercent
src/lib/i18n/local-text.ts             # LocalText helpers: localName
src/lib/api/admin-types.ts             # Settings, Area, Table, Station, StaffMember, Category, MenuItem, ...
src/lib/api/admin-api.ts               # SettingsApi, FloorApi, StationApi, StaffApi, MenuApi
src/lib/admin/validate.ts              # validateSettings/Table/Staff/Item/ModifierGroup → field errors
src/lib/admin/colors.ts                # ITEM_COLORS palette
src/lib/admin/use-admin.ts             # query hooks + save mutations per domain
src/lib/api/mock/mock-store.ts         # shared localStorage store (accounts + admin data), seed, reset
src/lib/api/mock/mock-staff-api.ts
src/lib/api/mock/mock-admin-api.ts     # settings, floor, stations, menu fakes
src/components/admin/admin-nav.tsx
src/components/admin/field.tsx         # label + input + error message wrapper
src/components/admin/local-text-fields.tsx   # EN + NE name inputs
src/components/admin/money-input.tsx
src/components/admin/stale-notice.tsx
src/app/(staff)/admin/layout.tsx       # AdminNav above children
src/app/(staff)/admin/page.tsx         # links to the sub-pages
src/app/(staff)/admin/settings/page.tsx
src/app/(staff)/admin/floor/page.tsx
src/app/(staff)/admin/stations/page.tsx
src/app/(staff)/admin/staff/page.tsx
src/app/(staff)/admin/menu/page.tsx
src/app/(staff)/admin/modifiers/page.tsx
src/components/admin/*.test.tsx, src/lib/**/*.test.ts
e2e/admin.spec.ts
```

---

### Task 1: Money and localized-name helpers

**Files:**
- Create: `web/src/lib/money.ts`, `money.test.ts`, `web/src/lib/i18n/local-text.ts`, `local-text.test.ts`
- Before starting: `git switch -c web/admin` from `web/foundation`.

**Interfaces:**
- Produces:
  - `formatMoney(paisa: number): string` → `"Rs 1,23,456.50"`. Uses Nepali/Indian digit grouping (last 3 digits, then groups of 2). Always two decimals. Negative values are shown as `"-Rs 50.00"`.
  - `parseMoney(input: string): number | null` → paisa or `null` when invalid. Trims spaces; strips a leading `Rs`/`रु` (any case, optional dot and space) and commas; accepts at most 2 decimals; rejects negatives, empty input, non-numbers and values above `MAX_PRICE_PAISA = 10_000_000` (Rs 1,00,000).
  - `formatPercent(bp: number): string` (`1300` → `"13"`, `1250` → `"12.5"`); `parsePercent(input: string): number | null` (0–100, at most 2 decimals, returns basis points).
  - `type LocalText = { en: string; ne: string }`; `localName(text: LocalText, language: Language): string` → the `ne` text if language is `"ne"` and it isn't blank, otherwise `en`.

- [ ] **Step 1: Write failing tests**

```ts
// money.test.ts
test.each([
  [0, "Rs 0.00"], [5, "Rs 0.05"], [25000, "Rs 250.00"], [125050, "Rs 1,250.50"],
  [12345650, "Rs 1,23,456.50"], [1234567800, "Rs 1,23,45,678.00"], [-5000, "-Rs 50.00"],
])("formatMoney(%i) = %s", (p, s) => expect(formatMoney(p)).toBe(s));

test.each([
  ["250", 25000], ["1,250", 125000], ["1250.5", 125050], ["Rs 250", 25000], ["rs.250", 25000],
  ["रु 99", 9900], [" 99 ", 9900], ["0", 0], ["100000", 10000000],
])("parseMoney(%s) = %i", (s, p) => expect(parseMoney(s)).toBe(p));

test.each(["", "  ", "abc", "-5", "12.345", "100000.01", "1.2.3", "Rs"])(
  "parseMoney rejects %j", (s) => expect(parseMoney(s)).toBeNull());

test("percent round trip", () => {
  expect(formatPercent(1300)).toBe("13");
  expect(formatPercent(1250)).toBe("12.5");
  expect(parsePercent("13")).toBe(1300);
  expect(parsePercent("12.5")).toBe(1250);
  for (const bad of ["", "-1", "101", "1.234", "x"]) expect(parsePercent(bad)).toBeNull();
});
```

```ts
// local-text.test.ts
test("Nepali falls back to English when blank", () => {
  expect(localName({ en: "Momo", ne: "मःम" }, "ne")).toBe("मःम");
  expect(localName({ en: "Momo", ne: "" }, "ne")).toBe("Momo");
  expect(localName({ en: "Momo", ne: "   " }, "ne")).toBe("Momo");
  expect(localName({ en: "Momo", ne: "मःम" }, "en")).toBe("Momo");
});
```

Run: `pnpm test src/lib/money src/lib/i18n/local-text` → Expected: FAIL (modules missing).

- [ ] **Step 2: Implement.** Do the money math on strings and integers only (split on `.`, pad decimals), never `parseFloat(x) * 100`.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): money and localized name helpers"`

---

### Task 2: Admin types, API interfaces and validation

**Files:**
- Create: `web/src/lib/api/admin-types.ts`, `web/src/lib/api/admin-api.ts`, `web/src/lib/admin/validate.ts`, `validate.test.ts`, `web/src/lib/admin/colors.ts`
- Modify: `web/src/lib/api/types.ts` (extend `ApiErrorReason`), `web/src/lib/api/api-context.tsx` (`Api` gains the new services), `web/src/app/providers.tsx` (accept `api?: Partial<Api>`), `web/src/test/render.tsx` (`api?: Partial<Api>` option; keep `auth` working)

**Interfaces:**
- Produces, in `admin-types.ts`:

```ts
export type BillingMode = "ESTIMATE" | "TAX_INVOICE";
export type Settings = {
  version: number;
  name: LocalText; address: string; phone: string; panNumber: string;
  vatRegistered: boolean; pricesIncludeVat: boolean; vatRateBp: number;
  serviceChargeEnabled: boolean; serviceChargeRateBp: number;
  billingMode: BillingMode; receiptWidthMm: 58 | 80; roundTotalToRupee: boolean;
  defaultLanguage: Language; kdsYellowMinutes: number; kdsRedMinutes: number;
};
export type SettingsInput = Omit<Settings, "version" | "billingMode">;

export type Area = { id: string; name: LocalText; sort: number };
export type Table = { id: string; areaId: string; name: string; seats: number; sort: number; active: boolean };
export type Floor = { areas: Area[]; tables: Table[] };          // both sorted by `sort`
export type Station = { id: string; name: LocalText; sort: number };

export type StaffMember = { id: string; name: string; role: Role; active: boolean; username: string | null; pinLocked: boolean };
export type StaffInput = { id?: string; name: string; role: Role; active: boolean; username?: string; password?: string; pin?: string };

export type Category = { id: string; name: LocalText; sort: number; active: boolean };
export type Variant = { id: string; name: LocalText; price: number };
export type Modifier = { id: string; name: LocalText; priceDelta: number };
export type ModifierGroup = { id: string; version: number; name: LocalText; min: number; max: number; modifiers: Modifier[] };
export type MenuItem = {
  id: string; version: number; categoryId: string; name: LocalText; price: number;
  color: string; imageUrl: string | null; stationId: string; active: boolean; is86d: boolean;
  variants: Variant[]; modifierGroupIds: string[]; sort: number;
};
export type Menu = { categories: Category[]; items: MenuItem[]; modifierGroups: ModifierGroup[] };
export type MenuItemInput = Omit<MenuItem, "id" | "version" | "sort" | "is86d" | "imageUrl" | "variants"> & {
  id?: string; variants: (Omit<Variant, "id"> & { id?: string })[];
};
export type ModifierGroupInput = Omit<ModifierGroup, "id" | "version" | "modifiers"> & {
  id?: string; modifiers: (Omit<Modifier, "id"> & { id?: string })[];
};
export type Move = "up" | "down";
```

- Produces, in `admin-api.ts`. `version` is required when `input.id` is set:

```ts
export interface SettingsApi { get(): Promise<Settings>; update(input: SettingsInput, version: number): Promise<Settings> }
export interface FloorApi {
  get(): Promise<Floor>;
  saveArea(input: { id?: string; name: LocalText }): Promise<Area>;
  deleteArea(id: string): Promise<void>;                 // IN_USE if it has tables
  moveArea(id: string, move: Move): Promise<void>;
  saveTable(input: { id?: string; areaId: string; name: string; seats: number; active: boolean }): Promise<Table>;
  moveTable(id: string, move: Move): Promise<void>;
}
export interface StationApi {
  list(): Promise<Station[]>;
  save(input: { id?: string; name: LocalText }): Promise<Station>;
  delete(id: string): Promise<void>;                     // IN_USE if items use it or it's the last station
}
export interface StaffApi {
  list(): Promise<StaffMember[]>;
  save(input: StaffInput): Promise<StaffMember>;
  resetPin(id: string, pin: string): Promise<void>;      // also clears a PIN lock
}
export interface MenuApi {
  get(): Promise<Menu>;
  saveCategory(input: { id?: string; name: LocalText; active: boolean }): Promise<Category>;
  deleteCategory(id: string): Promise<void>;             // IN_USE if it has items
  moveCategory(id: string, move: Move): Promise<void>;
  saveItem(input: MenuItemInput, version?: number): Promise<MenuItem>;
  moveItem(id: string, move: Move): Promise<void>;
  setItemImage(id: string, image: Blob | null): Promise<MenuItem>;  // ≤ 1 MB, image/jpeg|png|webp
  set86(ids: string[], is86d: boolean): Promise<void>;
  saveModifierGroup(input: ModifierGroupInput, version?: number): Promise<ModifierGroup>;
  deleteModifierGroup(id: string): Promise<void>;        // IN_USE if any item links it
}
```

  - `types.ts`: `ApiErrorReason` gains `"STALE_VERSION" | "NOT_ALLOWED" | "IN_USE" | "DUPLICATE" | "INVALID"`. `ApiError` gains an optional `field?: string` (constructor 4th arg) so a `DUPLICATE`/`INVALID` error can point at a form field.
  - `api-context.tsx`: `type Api = { auth: AuthApi; settings: SettingsApi; floor: FloorApi; stations: StationApi; staff: StaffApi; menu: MenuApi }`.
  - `AppProviders({ api?: Partial<Api>; auth?: AuthApi; realtime?; children })`. A missing service uses the mock in mock mode, or a "not wired" stub in real mode (same pattern as `createRealAuthApi`). Every service is wrapped with `withApiEvents`.
  - `renderWithProviders(ui, { auth?, api? })` and `renderWithProviders.wrapper({ children, auth?, api?, realtime? })`; defaults are the fakes with `latencyMs: 0`.
  - `validate.ts`, where every function returns `FieldErrors<T> = Partial<Record<keyof T | string, MessageKey>>` (empty object means valid):
    - `validateSettings(s: SettingsInput)`: `name.en` required; `panNumber` empty or exactly 9 digits (`admin.err.pan`); `kdsYellowMinutes` 1–120 and less than `kdsRedMinutes` (≤ 240) (`admin.err.kdsOrder`); `vatRateBp` and `serviceChargeRateBp` 0–10000.
    - `validateTable(t)`: `name` 1–20 chars; `seats` integer 1–50.
    - `validateStaff(s: StaffInput, isNew: boolean)`: `name` required. `pin`: required when `isNew`; when present, exactly 4 digits. For OWNER/MANAGER: `username` required (3–30 chars, `[a-z0-9._-]`), and `password` at least 8 chars when `isNew` or when present.
    - `validateItem(i: MenuItemInput)`: `name.en` required; `stationId` and `categoryId` required; `color` must be in `ITEM_COLORS`. With no variants, `price` must be > 0 (`admin.err.priceRequired`). With variants, every variant needs `name.en` and a price > 0; error keys are `variants.<index>.name` and `variants.<index>.price`.
    - `validateModifierGroup(g)`: `name.en` required; at least 1 modifier, each with `name.en`; `0 ≤ min ≤ max`, `max ≥ 1`, `max ≤ modifiers.length` (`admin.err.minMax`); `priceDelta` ≥ 0.
  - `colors.ts`: `ITEM_COLORS: readonly string[]` with 10 hex colors readable under white text (contrast ≥ 4.5:1), and `DEFAULT_ITEM_COLOR = ITEM_COLORS[0]`.

- [ ] **Step 1: Write failing tests** (`validate.test.ts`), one `test` per rule above. Each builds a valid base object, breaks one field and asserts the exact key. **Review Focus 3** is pinned here:

```ts
test("item without variants needs a base price", () => {
  expect(validateItem({ ...baseItem, price: 0, variants: [] })).toEqual({ price: "admin.err.priceRequired" });
});
test("item with variants ignores the base price but needs each variant priced", () => {
  expect(validateItem({ ...baseItem, price: 0, variants: [{ name: { en: "Half", ne: "" }, price: 15000 }] })).toEqual({});
  expect(validateItem({ ...baseItem, price: 0, variants: [{ name: { en: "Half", ne: "" }, price: 0 }] }))
    .toEqual({ "variants.0.price": "admin.err.priceRequired" });
});
```

Also: a test that every `ITEM_COLORS` entry has a contrast ratio ≥ 4.5 against `#ffffff` (small luminance helper in the test file).

Run → Expected: FAIL.
- [ ] **Step 2: Implement** the types, interfaces, validators and palette; extend `ApiError`, `Api`, `AppProviders` and `renderWithProviders`. For now, the new services in `AppProviders` default to "not wired" stubs in both modes; Task 4 swaps in the fakes.
- [ ] **Step 3: Run the whole suite** (`pnpm test`) → Expected: PASS. The Plan 1 tests pass unchanged.
- [ ] **Step 4: Add i18n keys** used by validation (`admin.err.required`, `admin.err.pan`, `admin.err.kdsOrder`, `admin.err.rate`, `admin.err.tableName`, `admin.err.seats`, `admin.err.pin`, `admin.err.username`, `admin.err.password`, `admin.err.priceRequired`, `admin.err.minMax`, `admin.err.color`) in EN and NE. The i18n parity test must stay green.
- [ ] **Step 5: Update the backend request list.** Append to "What the web needs from the backend developer" in `docs/superpowers/plans/2026-11-23-web-01-foundation.md`: "8. Admin services matching `web/src/lib/api/admin-api.ts` (settings, floor, stations, staff, menu), with `version` on settings, menu items and modifier groups, and error reasons STALE_VERSION, NOT_ALLOWED, IN_USE, DUPLICATE, INVALID with the field name."
- [ ] **Step 6: Commit** — `git commit -m "feat(web): admin types, API interfaces and validation"`

---

### Task 3: Shared mock store and fake StaffApi

**Files:**
- Create: `web/src/lib/api/mock/mock-store.ts`, `mock-staff-api.ts`, `mock-staff-api.test.ts`
- Modify: `web/src/lib/api/mock/mock-auth-api.ts` (read accounts from the store instead of `MOCK_ACCOUNTS`), `fixtures.ts` (becomes the seed only)

**Interfaces:**
- Consumes: `StaffApi`, `StaffInput`, `StaffMember`, `validateStaff` (Task 2); `ApiError`.
- Produces:
  - `type MockAccount = { user: User; active: boolean; pin: string; username: string | null; password: string | null }`
  - `type MockState = { auth: { sessionUserId; device; failedPins; languages }; accounts: MockAccount[]; settings: Settings; floor: Floor; stations: Station[]; menu: Menu; nextId: number }`
  - `loadMock(): MockState` / `saveMock(s: MockState): void` (key `localStorage["rm.mock"]`, seeded on first load) / `resetMockStore(): void` (re-exported from `mock-auth-api.ts` so Plan 1 imports keep working)
  - `mockId(s: MockState, prefix: string): string` → deterministic ids such as `"item-12"`
  - `runMock<T>(latencyMs: number, fn: (s: MockState) => T): Promise<T>` — the load → change → save wrapper that `mock-auth-api.ts` already uses, moved here
  - `currentMockUser(s: MockState): User` → throws `ApiError("unauthenticated")` when logged out
  - `createMockStaffApi(opts?: { latencyMs?: number }): StaffApi`
  - Seed: the 5 Plan 1 accounts; settings with spec defaults (name "Himalayan Kitchen" / "हिमालयन किचन", VAT registered, 13%, prices include VAT, service charge off at 10%, `ESTIMATE`, 80 mm, no rounding, `en`, KDS 10/20 min); one area "Main hall" with tables T1–T6 (4 seats); one station "Kitchen" (`भान्सा`) plus "Bar"; 3 categories, about 10 items (one with Half/Full variants, one linked to a "Spice level" modifier group with min 1 / max 1).

Fake-backend rules (they mirror what the server must enforce):
- Only OWNER/MANAGER may call `save`/`resetPin` (else `NOT_ALLOWED`).
- A MANAGER may not create or edit an OWNER/MANAGER account (`NOT_ALLOWED`).
- Nobody may change their own `role` or set themselves inactive (`NOT_ALLOWED`).
- The last active OWNER cannot be deactivated or demoted (`NOT_ALLOWED`).
- A duplicate `username` gives `DUPLICATE` with `field: "username"`. `validateStaff` failures give `INVALID` with the first failing field.
- `resetPin` sets the PIN and clears `failedPins` for that user.
- An inactive account cannot PIN-login or password-login (`INVALID_CREDENTIALS`) and is not returned by `listPinUsers`.

- [ ] **Step 1: Write failing tests** (`mock-staff-api.test.ts`). For each rule above, plus:
  - An owner creates waiter "Maya" with PIN `6666` → after sharing the device, `pinLogin(maya.id, "6666")` succeeds through `createMockAuthApi`.
  - Deactivating Gita → `listPinUsers()` doesn't include her and her PIN login fails.
  - Lock Gita (5 wrong PINs) → `list()` shows `pinLocked: true`; owner `resetPin(gita, "7777")` → login with `7777` works and `pinLocked` is false.
  - **Review Focus 4:** as manager Sita: `save({...sita, active:false})` → `NOT_ALLOWED`; `save({...sita, role:"WAITER"})` → `NOT_ALLOWED`; editing Ram (owner) → `NOT_ALLOWED`; as owner Ram, deactivating Ram when he is the only owner → `NOT_ALLOWED`.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement.** Move the store into `mock-store.ts` and point `mock-auth-api.ts` at it.
- [ ] **Step 3: Run the whole suite** → Expected: PASS, with all Plan 1 auth tests unchanged.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): shared mock store and fake StaffApi"`

---

### Task 4: Fake settings, floor, station and menu APIs

**Files:**
- Create: `web/src/lib/api/mock/mock-admin-api.ts`, `mock-admin-api.test.ts`
- Modify: `web/src/app/providers.tsx` (mock mode uses the fakes for all services)

**Interfaces:**
- Consumes: `runMock`, `loadMock`, `mockId`, `currentMockUser` (Task 3); interfaces and validators (Task 2); `mockRealtime` (Plan 1 Task 8).
- Produces: `createMockSettingsApi`, `createMockFloorApi`, `createMockStationApi`, `createMockMenuApi`, each `(opts?: { latencyMs?: number }) => <Interface>`.

Fake-backend rules:
- Every write requires an OWNER/MANAGER session: logged out → `unauthenticated`, other roles → `NOT_ALLOWED`. Reads need any session.
- Saving with a `version` that isn't current → `STALE_VERSION`. Each successful save increments `version`.
- Validators run on every save → `INVALID` with `field`.
- New entities get `sort` = max + 1 within their parent. `move` swaps `sort` with the neighbor and does nothing at the ends.
- Delete rules exactly as the interface comments say (`IN_USE`).
- Every write emits a realtime event through `mockRealtime.emit`: `settings.updated`, `table.updated`, `station.updated`, `menu.updated`, and `item.86d` (one per id) for `set86`.
- `setItemImage` rejects blobs over 1 MB or of other types (`INVALID`, field `image`) and stores a data URL.

- [ ] **Step 1: Write failing tests** covering:
  - Seed contents (settings defaults; T1–T6; stations Kitchen + Bar; the variant item and the modifier item exist).
  - Waiter `settings.update` → `NOT_ALLOWED`.
  - Owner updates the settings name → `version` +1. **A second update with the old version → `STALE_VERSION`.**
  - Zero tables: deactivating all tables is allowed.
  - `deleteArea` with tables → `IN_USE`; on an empty area → OK.
  - Deleting the station an item uses → `IN_USE`; deleting the last station → `IN_USE`.
  - `saveItem` for a new item → `sort` is last in its category. `moveItem("up")` on the first item → no change.
  - **Stale item save → `STALE_VERSION`** (load, save once, save again with the first version).
  - `set86([a, b], true)` → both `is86d`; `mockRealtime.emit` (spied) called with `{ type: "item.86d", id: a }` and `{ type: "item.86d", id: b }`.
  - A 2 MB blob → `INVALID`/`image`. A small PNG blob → `imageUrl` starts with `data:image/png`.
  - Deleting a modifier group linked to an item → `IN_USE`.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement** and wire the fakes into `AppProviders` and `renderWithProviders`.
- [ ] **Step 3: Run the whole suite** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): fake settings, floor, station and menu APIs"`

---

### Task 5: Admin hooks, shared form pieces and admin navigation

**Files:**
- Create: `web/src/lib/admin/use-admin.ts`, `use-admin.test.tsx`, `web/src/components/admin/admin-nav.tsx`, `field.tsx`, `local-text-fields.tsx`, `money-input.tsx`, `stale-notice.tsx`, `admin-pieces.test.tsx`, `web/src/app/(staff)/admin/layout.tsx`
- Modify: `web/src/app/(staff)/admin/page.tsx` (link cards instead of "Coming soon")
- shadcn: `pnpm dlx shadcn@latest add dialog select switch badge tabs`

**Interfaces:**
- Consumes: `useApi` (Api from Task 2); `localName`, `parseMoney`, `formatMoney` (Task 1).
- Produces:
  - Query hooks `useSettings()`, `useFloor()`, `useStations()`, `useStaff()`, `useMenu()` with keys `["settings"]`, `["floor"]`, `["stations"]`, `["staff"]`, `["menu"]`. Staff writes also invalidate `["pin-users"]`.
  - `useAdminSave<TArgs, TResult>(key: QueryKey, fn: (...args: TArgs) => Promise<TResult>)` → TanStack mutation that invalidates `key` on success **and on `STALE_VERSION`** (so the form receives fresh data).
  - `saveErrorMessage(err: unknown): MessageKey` → `STALE_VERSION` → `admin.stale`; `NOT_ALLOWED` → `admin.notAllowed`; `IN_USE` → `admin.inUse`; `DUPLICATE` → `admin.duplicate`; network → `admin.offline`; else `error.generic`.
  - `<Field label error? children />` — label, control, and an error line linked with `aria-describedby`.
  - `<LocalTextFields value={LocalText} onChange label errors? />` — two inputs labelled "Name (English)" and "Name (नेपाली)".
  - `<MoneyInput value={number | null} onChange(paisa: number | null) label error? />` — text input (`inputMode="decimal"`) that keeps the raw text while typing, calls `onChange(parseMoney(text))` on every change, and shows `formatMoney` under the field when valid.
  - `<StaleNotice />` — the `admin.stale` banner.
  - `<AdminNav />` — tab links to Settings, Floor & tables, Stations, Staff, Menu, Modifiers; the current tab is highlighted using `usePathname`. Scrolls horizontally inside itself on phones.
  - `/admin/` page: one large link card per sub-page, with a one-line description each.

- [ ] **Step 1: Write failing tests**
  - `use-admin.test.tsx`: `useMenu()` returns the seed; after `useAdminSave(["menu"], menu.saveItem)` succeeds, `useMenu()` shows the change. A `STALE_VERSION` failure also refetches. `saveErrorMessage` maps each reason.
  - `admin-pieces.test.tsx`: typing `1,250` in `MoneyInput` calls `onChange(125000)` and shows `Rs 1,250.00`; typing `12.345` calls `onChange(null)`. `Field` with an error is linked through `aria-describedby`. `AdminNav` marks `/admin/menu/` as current (`aria-current="page"`) when `testRouter.pathname` is that path.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement.** i18n keys for nav labels and page descriptions (`admin.nav.*`, `admin.desc.*`), `admin.stale`, `admin.notAllowed`, `admin.inUse`, `admin.duplicate`, `admin.offline`, `admin.save`, `admin.cancel`, `admin.add`, `admin.edit`, `admin.delete`, `admin.active`, `admin.inactive`, `admin.moveUp`, `admin.moveDown`, `admin.nameEn`, `admin.nameNe`.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): admin hooks, form pieces and navigation"`

---

### Task 6: Restaurant settings page

**Files:**
- Create: `web/src/app/(staff)/admin/settings/page.tsx`, `web/src/components/admin/settings-form.tsx`, `settings-form.test.tsx`

**Interfaces:**
- Consumes: `useSettings`, `useAdminSave`, `saveErrorMessage`, `Field`, `LocalTextFields`, `StaleNotice` (Task 5); `validateSettings` (Task 2); `formatPercent`/`parsePercent` (Task 1).
- Produces: `<SettingsForm settings={Settings} onSaved?() />`, which works on a local copy and saves through `settings.update(input, settings.version)`.

Sections:
1. **Restaurant:** name EN/NE, address, phone, PAN/VAT number.
2. **Tax:** VAT registered (switch); prices include VAT (switch); VAT rate % (shown only when VAT registered); service charge (switch) and its rate % (shown only when on).
3. **Bills:** receipt width 58/80 mm (two big toggle buttons); round total to rupee (switch); billing mode as a read-only badge, "Estimate (pilot)" or "Tax invoice", with a one-line explanation.
4. **Kitchen screen:** yellow after N minutes, red after N minutes.
5. **Language:** default language EN/NE.

Save stays disabled until something changes. Validation errors appear next to their fields. On `STALE_VERSION`, the form reloads the latest settings and shows `<StaleNotice />`.

- [ ] **Step 1: Write failing tests**
  - Loads the seed values (name "Himalayan Kitchen", VAT 13, service charge off and its rate hidden).
  - Turning on service charge shows its rate (10); saving persists `serviceChargeEnabled: true, serviceChargeRateBp: 1000` (read back through the fake).
  - PAN `12345` → `admin.err.pan` shown and no save call (spy). Yellow 20 / red 10 → `admin.err.kdsOrder`.
  - The billing mode badge shows the estimate text and has no editable control.
  - Stale: update the settings through the fake in between (`createMockSettingsApi().update(..., v)`), then save in the form → the `admin.stale` text is shown and the field shows the other edit's value.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement** the form and page, plus the i18n keys `settings.*`.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): restaurant settings page"`

---

### Task 7: Floor (areas and tables) and stations pages

**Files:**
- Create: `web/src/app/(staff)/admin/floor/page.tsx`, `web/src/components/admin/floor-editor.tsx`, `floor-editor.test.tsx`, `web/src/app/(staff)/admin/stations/page.tsx`, `web/src/components/admin/stations-editor.tsx`, `stations-editor.test.tsx`

**Interfaces:**
- Consumes: `useFloor`, `useStations`, `useAdminSave`, `saveErrorMessage`, `LocalTextFields`, `Field` (Task 5); `validateTable` (Task 2).
- Produces:
  - `<FloorEditor />`:
    - One card per area: area name, up/down, rename, delete (only when the area is empty).
    - Inside each card, a grid of table tiles (name, seats, inactive ones dimmed) with "Add table".
    - Tapping a tile opens a dialog: name, seats, area (select), active (switch), up/down.
    - "Add area" at the bottom.
    - Empty state: a "No tables. Counter-only restaurant" hint plus "Add area".
  - `<StationsEditor />` — list of stations with rename, up/down, delete (the error text shows when `IN_USE`), and "Add station".

- [ ] **Step 1: Write failing tests**
  - The floor shows "Main hall" with T1–T6.
  - Add table "T7" with 2 seats → appears in the grid.
  - A table with 0 seats → `admin.err.seats`, no save.
  - Deactivating T1 shows it dimmed with the `admin.inactive` badge.
  - Moving T2 up puts it first.
  - Deleting a non-empty area → its delete button isn't present. Deleting a new empty area works.
  - Stations: rename "Bar" to "Drinks"; deleting "Kitchen" (in use) shows the `admin.inUse` message.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement**, plus i18n keys `floor.*` and `stations.*`.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): floor and stations admin"`

---

### Task 8: Staff page

**Files:**
- Create: `web/src/app/(staff)/admin/staff/page.tsx`, `web/src/components/admin/staff-editor.tsx`, `staff-editor.test.tsx`

**Interfaces:**
- Consumes: `useStaff`, `useSession` (current user), `useAdminSave`, `saveErrorMessage`, `Field` (Tasks 5 and Plan 1); `validateStaff` (Task 2).
- Produces: `<StaffEditor />`:
  - A list grouped by role: name, role badge, "PIN locked" badge, inactive dimmed.
  - "Add staff" and tapping a row open a dialog: name; role select; username + password (only for OWNER/MANAGER); PIN (4-digit `inputMode="numeric"`, required on create); active switch.
  - Rows with `pinLocked` have a "Reset PIN" button that opens a small dialog for a new PIN.
  - Role options for a MANAGER are only CASHIER, WAITER and KITCHEN. Rows for OWNER/MANAGER accounts open read-only for a manager.
  - The current user's own role select and active switch are disabled, with the hint `staff.selfHint`.

- [ ] **Step 1: Write failing tests**
  - Owner adds waiter "Maya" with PIN 6666 → appears under Waiters. (Its PIN login is covered in Task 3.)
  - Adding a cashier without a PIN → `admin.err.pin`. Adding a manager without a username → `admin.err.username`. A duplicate username → `admin.duplicate` shown on the username field.
  - **Review Focus 4 (UI):** logged in as manager Sita, her own row has role and active disabled; Ram's (owner) row opens read-only; the role select offers exactly Cashier, Waiter and Kitchen.
  - Locked Gita shows "PIN locked"; resetting to 7777 removes the badge.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement**, plus i18n keys `staff.*`.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): staff admin"`

---

### Task 9: Menu page — categories, items, variants, photo and 86

**Files:**
- Create: `web/src/app/(staff)/admin/menu/page.tsx`, `web/src/components/admin/menu-editor.tsx`, `item-dialog.tsx`, `color-picker.tsx`, `menu-editor.test.tsx`, `item-dialog.test.tsx`

**Interfaces:**
- Consumes: `useMenu`, `useStations`, `useAdminSave`, `saveErrorMessage`, `LocalTextFields`, `MoneyInput`, `Field`, `StaleNotice` (Task 5); `validateItem`, `ITEM_COLORS` (Task 2); `localName`, `formatMoney` (Task 1); `useLanguage`.
- Produces:
  - `<MenuEditor />`:
    - Category tabs that scroll horizontally, plus "Add category". The active category has rename / up / down / active / delete (only when empty).
    - Below the tabs, a grid of item tiles in the item's color: name via `localName`, price (or "from Rs X" when there are variants), a photo thumbnail if set, an "86" badge, and inactive tiles dimmed.
    - Each tile has an **86 toggle** button reachable with one tap.
    - A "Select" mode for **bulk 86**: tap tiles to select, then "Mark out of stock" / "Back in stock".
    - "Add item" opens `<ItemDialog>`.
  - `<ItemDialog item={MenuItem | null} categoryId onClose />`. Fields:
    - names EN/NE, category, station
    - `<ColorPicker>` (the 10 swatches as radio buttons with accessible names "Color 1"…)
    - Price via `MoneyInput`, hidden when there are variants
    - Variants list (name EN/NE + price, add/remove)
    - Modifier groups as checkboxes
    - Active switch
    - Photo: file input `accept="image/jpeg,image/png,image/webp"` plus a remove button. The photo is saved right away through `setItemImage`; new items save first.
    - Saves with `saveItem(input, item?.version)`.
  - `<ColorPicker value onChange />`.

- [ ] **Step 1: Write failing tests**
  - `menu-editor.test.tsx`:
    - The seed's categories appear as tabs. Selecting the second tab shows its items.
    - The variant item's tile shows "from Rs …".
    - A one-tap 86 toggle shows the "86" badge.
    - Bulk: select 2 tiles → "Mark out of stock" → both show "86". `mockRealtime.emit` (spied) receives two `item.86d` events.
    - **Review Focus 5:** with language `ne` and an item whose `ne` name is blank, the tile shows the English name.
    - Deleting a category that has items: its delete button isn't present.
  - `item-dialog.test.tsx`:
    - Add "Chicken Momo" at Rs 250 in category 1, station Kitchen, color 3 → a new tile shows `Rs 250.00`.
    - Adding a variant hides the base price field. Saving a variant without a price → `admin.err.priceRequired` beside that variant.
    - Removing all variants shows the base price again.
    - Linking "Spice level" persists `modifierGroupIds`.
    - A 2 MB file → `admin.err.image` shown. A small PNG → the thumbnail appears.
    - **Review Focus 1:** open the item, change its price through the fake in between (`createMockMenuApi().saveItem(..., version)`), then save the dialog → `admin.stale` is shown, the dialog shows the other edit's price, and a second save (with the refreshed version) succeeds.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement**, plus i18n keys `menu.*` and `admin.err.image`.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): menu admin with variants, photos and 86"`

---

### Task 10: Modifier groups page

**Files:**
- Create: `web/src/app/(staff)/admin/modifiers/page.tsx`, `web/src/components/admin/modifier-editor.tsx`, `modifier-editor.test.tsx`

**Interfaces:**
- Consumes: `useMenu`, `useAdminSave`, `saveErrorMessage`, `LocalTextFields`, `MoneyInput`, `StaleNotice` (Task 5); `validateModifierGroup` (Task 2).
- Produces: `<ModifierEditor />`:
  - A list of groups with a "used by N items" count.
  - A dialog with: name EN/NE; "Customer must choose at least" (`min`) and "at most" (`max`) number steppers; a list of modifiers (name EN/NE, extra price via `MoneyInput`, add/remove); delete (only when unused).
  - A one-line preview of the rule, e.g. "Choose exactly 1" or "Choose up to 3" (`modifiers.rule.*` keys).

- [ ] **Step 1: Write failing tests**
  - The seed's "Spice level" shows "used by 1 item" and "Choose exactly 1".
  - Creating "Extras" with min 0, max 2 and two modifiers (Cheese +Rs 50, Egg +Rs 30) → listed with "Choose up to 2".
  - max 3 with 2 modifiers → `admin.err.minMax`.
  - The "Spice level" group has no delete button; a new unused group can be deleted.
  - A stale save shows `admin.stale`.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement**, plus i18n keys `modifiers.*`.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): modifier groups admin"`

---

### Task 11: End-to-end setup flow

**Files:**
- Create: `web/e2e/admin.spec.ts`
- Modify: `web/e2e/auth.spec.ts` only if a shared helper (`ownerLogin`) moves to `web/e2e/helpers.ts`

**Interfaces:**
- Consumes: everything above, through the browser only.

- [ ] **Step 1: Write the e2e spec**
  - **Owner sets up:** log in → `/admin/` → Settings: turn service charge on, save, reload, still on → Floor: add table "T7" → Stations: add "Tandoor" → Menu: add item "Butter Naan" (Rs 60, station Tandoor, color 2), mark it 86, see the badge → Staff: add waiter "Maya" PIN 6666 → Switch user → Maya appears in the PIN picker and her PIN logs her in to `/floor/`.
  - **Manager limits:** manager login → Staff: Ram's row is read-only; the role choices are Cashier, Waiter and Kitchen only.
  - **Phone layout:** on the `phone` project, `/admin/menu/` and the item dialog have no horizontal page scroll (reuse `noHorizontalScroll`). Save screenshots of the menu grid and the item dialog.
- [ ] **Step 2: Run** — `cd web && pnpm e2e` → Expected: all tests pass on both projects. Look at the phone screenshots.
- [ ] **Step 3: Full check** — `pnpm test && pnpm typecheck && pnpm lint && pnpm build` → Expected: all green.
- [ ] **Step 4: Commit** — `git commit -m "test(web): admin setup e2e"`
