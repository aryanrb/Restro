# Web Plan 1 — Foundation, Auth and App Shell Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Next.js web app that runs fully against a fake backend: owners and managers log in with a password, shared devices log staff in by PIN, each role lands on its own (placeholder) section, the UI switches between English and Nepali, and a connection indicator plus a realtime client are ready for later features. A final task switches it to the real Go API once the backend developer delivers the auth contract.

**Architecture:** Static-exported Next.js (App Router, client components). Screens never call the network directly. They call a web-side `AuthApi` interface with web-owned types. Two implementations exist: a fake one (in-browser, `localStorage`-backed) used for development and tests, and a real one built on the ConnectRPC TypeScript client generated from the backend developer's `.proto` files. TanStack Query holds server state. A WebSocket client turns "something changed" events into query refetches.

**Tech Stack:** Next.js (latest stable, App Router, `output: "export"`), React 19, TypeScript strict, Tailwind CSS v4 + shadcn/ui, TanStack Query v5, Vitest + Testing Library + jsdom, Playwright, pnpm 10, Node 24. Task 10 only: `@bufbuild/buf`, `@bufbuild/protoc-gen-es` v2, `@bufbuild/protobuf` v2, `@connectrpc/connect` v2, `@connectrpc/connect-web` v2.

**Spec:** `docs/superpowers/specs/2026-11-23-restaurant-mvp-design.md` (read §3, §3a, §5, §7 User/Session, §8.1, §8.10, §9).

## Scope: frontend only

The backend developer owns `proto/`, `api/`, `deploy/` and every server rule (sessions, PIN lockout, error reasons, WebSocket events). This plan creates and changes files **only under `web/`**, plus the initial git commit. Where the web needs something from the server, it is listed under "What the web needs from the backend developer" as a request, and the web code keeps the assumption in one place so it is easy to change.

## Plan series (web MVP)

This is plan 1 of 7. Each plan produces working, testable software and gets its own plan document when it starts.

1. **Foundation, auth, app shell** (this plan)
2. Settings & menu admin: restaurant profile, tax settings, areas/tables, stations, staff, menu/variants/modifiers, 86 toggle, money formatting (§8.2)
3. Waiter ordering + offline: table grid, cart, send, transfer/merge, service worker, IndexedDB outbox, Needs-attention list, manager-approval dialog (§8.3, §8.7, §9)
4. Kitchen display: station tickets, timers, item status, 86 from KDS, ready notifications (§8.4)
5. Billing & printing: bill/split, discounts, payments, QR display, print templates 58/80 mm, reprints, cancellation, counter pay-first flow (§8.5, §8.6, §10, §11)
6. Shifts: open/close, expected vs counted, QR reconciliation list (§8.8)
7. Owner dashboard, reports, exports, audit log browser (§8.7, §8.9)

## Global Constraints

- Free/open-source dependencies only; no paid SaaS. (spec §3)
- Single-tenant: no `restaurantId` anywhere. (spec §3)
- Files change only under `web/` (see Scope).
- TypeScript `strict: true`; no `any` in `web/src/` outside `web/src/gen`.
- Components and hooks import API types from `@/lib/api/types` only, never from `@/gen`. Only `web/src/lib/api/connect/` touches generated code.
- Every user-visible string goes through `t()` with both EN and NE text. (spec §8.10)
- No blocking spinner in staff flows; buttons show a pending state instead. (spec §2 anti-goals)
- Big touch targets: interactive controls on staff screens are at least 48×48 px. (product rule 3)
- Money arrives from the server as integer paisa; the web never computes tax. (spec §5)
- Static export: `next build` emits `web/out/`. No Next.js server features (no server actions, route handlers, middleware, `cookies()`, dynamic route segments). Use query strings instead of `[id]` segments.
- API base URL from `NEXT_PUBLIC_API_BASE` (default `/api`). `NEXT_PUBLIC_API_MODE=mock` selects the fake backend; dev default is `mock`.

## What the web needs from the backend developer

Send this list to the backend developer. Nothing in Tasks 1–9 waits on the answers; Task 10 does.

1. **Auth contract (`auth.proto`)** that lets the web:
   - read the current session in one call that does **not** fail when nobody is logged in: the user (id, name, role, language) or none, plus whether this device is a shared staff device and its label;
   - log in with username + password (owner/manager);
   - list staff for the PIN picker, and log in by user + PIN on a shared device;
   - log out (ending the session, not the device's shared status);
   - save the user's language preference.
2. **Shared-device security (suggestion, their decision):** with cloud hosting, PIN login open to the whole internet lets anyone guess 4-digit PINs. Suggest that PIN login only works on devices an owner/manager has marked as shared ("enroll device"), and that a PIN locks after a few wrong tries. If they agree, the web needs an "enroll this device" call. If not, the web drops it (see Task 6).
3. **Error reasons** the web can read from any error, at least: invalid credentials, PIN locked, device not enrolled. Later plans will need stale version, item 86'd, manager approval required/invalid, order closed, shift not open.
4. **URLs:** the Connect base URL, the health-check URL, and the WebSocket URL as seen from the browser. The web assumes `/api`, `/api/health`, `/api/ws` on the same origin.
5. **Sessions for the web:** an httpOnly cookie on the same origin (the web sends `credentials: "include"`).
6. **WebSocket message format.** The web assumes JSON text frames `{"type": "order.updated", "id": "<uuid>", "version": 3}`, with `version` optional.
7. **PIN length:** fixed 4 digits, or variable (then the staff list should include it per user).
8. **Admin services** matching `web/src/lib/api/admin-api.ts` (settings, floor, stations, staff, menu), with `version` on settings, menu items and modifier groups, and error reasons STALE_VERSION, NOT_ALLOWED, IN_USE, DUPLICATE, INVALID that say which field they refer to.

---

## Review Focus

1. **Cold start with the API unreachable:** a waiter reloads the app while the internet is down. Expected: the last known session is used and an "Offline" pill shows, instead of bouncing to the login page. Test in Task 5.
2. **Session expires mid-shift:** any call fails as unauthenticated. Expected: the cached session is cleared and the user goes to `/pin/` (shared device) or `/login/`, without a loop of error toasts. Test in Task 5.
3. **Wrong PIN, repeatedly:** Expected: the PIN pad clears with "Wrong PIN" each time; after lockout it shows "PIN locked, ask a manager" and stops submitting. Test in Task 6.
4. **Double tap on Log in, or a 5th digit typed fast:** Expected: exactly one request is sent. Test in Task 6.
5. **API server restart while tablets are open:** the socket drops. Expected: reconnect with backoff capped at 30 s, never a tight loop, and every query is refetched after reconnect. Test in Task 8.

---

## File structure (all under `web/`)

```
next.config.ts                          # output export, trailingSlash, images.unoptimized
vitest.config.mts
playwright.config.ts
buf.gen.yaml                            # Task 10
public/manifest.webmanifest, public/icons/icon-192.png, icon-512.png
e2e/auth.spec.ts
src/gen/                                # Task 10, generated from ../proto, committed
src/test/setup.ts                       # jest-dom, localStorage reset
src/test/render.tsx                     # renderWithProviders(ui, {api})
src/app/layout.tsx                      # fonts, <Providers>
src/app/providers.tsx                   # QueryClient, ApiProvider, I18nProvider, RealtimeProvider
src/app/page.tsx                        # entry redirect
src/app/login/page.tsx
src/app/pin/page.tsx
src/app/(staff)/layout.tsx              # AppShell
src/app/(staff)/{admin,cashier,floor,kitchen}/page.tsx
src/lib/api/config.ts                   # apiBase(), apiMode(), wsUrl(), healthUrl()
src/lib/api/types.ts                    # Role, Language, User, Device, Session, ApiError
src/lib/api/auth-api.ts                 # AuthApi interface
src/lib/api/api-events.ts               # withApiEvents()
src/lib/api/api-context.tsx             # ApiProvider, useApi()
src/lib/api/mock/fixtures.ts
src/lib/api/mock/mock-auth-api.ts       # createMockAuthApi(), resetMockStore()
src/lib/api/connect/connect-auth-api.ts # Task 10: createConnectAuthApi()
src/lib/api/connect/to-api-error.ts     # Task 10
src/lib/i18n/messages/en.ts, ne.ts
src/lib/i18n/i18n.tsx                   # I18nProvider, useT, useLanguage, format
src/lib/auth/roles.ts                   # Section, homeFor, canAccess
src/lib/auth/session.ts                 # useSession, session cache, entryPath, useAuthActions
src/lib/auth/device.ts                  # device label storage
src/lib/realtime/connectivity.ts
src/lib/realtime/client.ts              # RealtimeClient, backoffDelay
src/lib/realtime/realtime-provider.tsx
src/components/auth/password-login-form.tsx
src/components/auth/staff-picker.tsx
src/components/auth/pin-pad.tsx
src/components/shell/app-shell.tsx
src/components/shell/role-gate.tsx
src/components/shell/connection-pill.tsx
src/components/shell/language-toggle.tsx
```

---

### Task 1: Repository and Next.js scaffold

**Files:**
- Create: `.gitignore` (repo root, only if not present), `web/` (via create-next-app), `web/next.config.ts`, `web/vitest.config.mts`, `web/src/test/setup.ts`, `web/src/test/smoke.test.ts`

**Interfaces:**
- Produces: `pnpm test` (Vitest, jsdom), `pnpm build` → `web/out/`, path alias `@/*` → `web/src/*`.

- [ ] **Step 1: Git** — if the folder is not yet a git repo (or the backend developer has not created a shared one), run:

```bash
cd /home/thanatos/Work/restaurant-management
git init -b main
printf 'node_modules/\n.next/\nout/\n.env*.local\ntest-results/\nplaywright-report/\n' > .gitignore
git add .gitignore RESTAURANT_PLAN.md docs .claude/settings.json
git commit -m "docs: add plan, research, MVP spec and web plan 1"
```

If a shared repo already exists, clone it and work in its `web/` folder instead.

- [ ] **Step 2: Scaffold the app**

```bash
pnpm create next-app@latest web --ts --tailwind --eslint --app --src-dir --import-alias "@/*" --use-pnpm --yes
cd web && pnpm dlx shadcn@latest init -d
pnpm add @tanstack/react-query
pnpm add -D vitest @vitejs/plugin-react vite-tsconfig-paths jsdom @testing-library/react @testing-library/user-event @testing-library/jest-dom
```

Add scripts to `web/package.json`: `"test": "vitest run"`, `"test:watch": "vitest"`, `"typecheck": "tsc --noEmit"`.

- [ ] **Step 3: Configure static export** — `web/next.config.ts`: `output: "export"`, `trailingSlash: true`, `images: { unoptimized: true }`.

- [ ] **Step 4: Configure Vitest** — `vitest.config.mts`: plugins `react()` and `tsconfigPaths()`, `environment: "jsdom"`, `setupFiles: ["./src/test/setup.ts"]`, `include: ["src/**/*.test.{ts,tsx}"]`. `setup.ts` imports `@testing-library/jest-dom/vitest` and calls `localStorage.clear()` in `afterEach`.

- [ ] **Step 5: Smoke test**

```ts
// src/test/smoke.test.ts
import { expect, test } from "vitest";
test("jsdom is available", () => { expect(document.createElement("div")).toBeInstanceOf(HTMLElement); });
```

Run: `cd web && pnpm test` → Expected: 1 passed.

- [ ] **Step 6: Verify the static build** — Run: `cd web && pnpm build && test -f out/index.html && echo OK` → Expected: `OK`.

- [ ] **Step 7: Commit** — `git add web && git commit -m "chore(web): scaffold Next.js static-export app with Vitest"`

---

### Task 2: i18n (English / Nepali)

**Files:**
- Create: `web/src/lib/i18n/messages/en.ts`, `ne.ts`, `web/src/lib/i18n/i18n.tsx`, `i18n.test.tsx`, `web/src/components/shell/language-toggle.tsx`
- Modify: `web/src/app/layout.tsx` (fonts: Inter + `Noto_Sans_Devanagari` via `next/font/google` as CSS variables, Devanagari in the font stack)

**Interfaces:**
- Produces:
  - `en` (`as const` object, flat keys like `"login.title"`), `type MessageKey = keyof typeof en`, `ne: Record<MessageKey, string>` (a missing key is a compile error)
  - `format(template: string, vars?: Record<string, string | number>): string` — replaces `{name}` placeholders
  - `<I18nProvider>`; `useT(): (key: MessageKey, vars?) => string`
  - `useLanguage(): { language: Language; setLanguage(l: Language): void }` where `type Language = "en" | "ne"` (defined here in `i18n.tsx`, re-exported by `@/lib/api/types` in Task 3). Persists to `localStorage["rm.lang"]`, default `"en"`; keeps `<html lang>` in sync.
  - `<LanguageToggle onChange?: (l: Language) => void />` — one button showing "नेपाली" while English is active and "English" while Nepali is active

Keys this plan needs (each with Nepali text): `app.name`, `login.title`, `login.username`, `login.password`, `login.submit`, `login.invalid`, `login.deviceLabel`, `login.enrollDevice`, `login.useStaffPin`, `login.useOwnerLogin`, `pin.chooseStaff`, `pin.enterPin`, `pin.wrong`, `pin.locked`, `pin.back`, `shell.switchUser`, `shell.logout`, `conn.online`, `conn.offline`, `conn.connecting`, `section.admin`, `section.cashier`, `section.floor`, `section.kitchen`, `role.OWNER`, `role.MANAGER`, `role.CASHIER`, `role.WAITER`, `role.KITCHEN`, `common.comingSoon`, `error.generic`.

- [ ] **Step 1: Write failing tests** (`i18n.test.tsx`)
  - `t("login.title")` renders the English text by default.
  - Clicking `<LanguageToggle>` switches it to the `ne` value and stores `"ne"` in `localStorage["rm.lang"]`; remounting the provider keeps Nepali; `document.documentElement.lang === "ne"`.
  - `format("Hello {name}", { name: "Gita" })` → `"Hello Gita"`; an unknown placeholder is left as is.
  - Every `ne` value is non-empty and differs from `en`, except keys in `SAME_IN_BOTH = ["app.name"]`.

  Run: `pnpm test src/lib/i18n` → Expected: FAIL.
- [ ] **Step 2: Implement** messages, provider, toggle, fonts.
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): English/Nepali i18n with language toggle"`

---

### Task 3: Web-side API types, `AuthApi` interface and API events

**Files:**
- Create: `web/src/lib/api/config.ts`, `types.ts`, `auth-api.ts`, `api-events.ts`, `api-events.test.ts`, `api-context.tsx`, `web/.env.development` (`NEXT_PUBLIC_API_MODE=mock`)

**Interfaces:**
- Produces:
  - `config.ts`: `apiBase(): string` (env or `"/api"`), `apiMode(): "mock" | "real"`, `healthUrl(): string` (`${apiBase()}/health`), `wsUrl(loc: Location = window.location): string` (`ws`/`wss` by protocol + host + `${apiBase()}/ws`)
  - `types.ts`:
    - `type Role = "OWNER" | "MANAGER" | "CASHIER" | "WAITER" | "KITCHEN"`; re-export `Language`
    - `type User = { id: string; name: string; role: Role; language: Language }`
    - `type Device = { shared: boolean; label: string }`
    - `type Session = { user: User | null; device: Device }`
    - `type ApiErrorReason = "INVALID_CREDENTIALS" | "PIN_LOCKED" | "DEVICE_NOT_SHARED" | "UNKNOWN"`
    - `class ApiError extends Error { kind: "network" | "unauthenticated" | "rejected"; reason: ApiErrorReason }` (`reason` is `"UNKNOWN"` unless the server gives one)
  - `auth-api.ts`:

    ```ts
    export interface AuthApi {
      getSession(): Promise<Session>;                       // resolves with user: null when logged out
      passwordLogin(username: string, password: string): Promise<User>;
      enrollDevice(label: string): Promise<Device>;         // owner/manager only; see backend request #2
      listPinUsers(): Promise<User[]>;
      pinLogin(userId: string, pin: string): Promise<User>;
      logout(): Promise<void>;
      setLanguage(language: Language): Promise<User>;
    }
    ```
  - `api-events.ts`: `interface ApiEvents { onReachable(): void; onUnreachable(): void; onUnauthenticated(): void }`; `withApiEvents<T extends object>(api: T, events: ApiEvents): T` wraps every function-valued property
  - `api-context.tsx`: `type Api = { auth: AuthApi }`; `<ApiProvider api={Api}>`; `useApi(): Api`

- [ ] **Step 1: Write failing tests** (`api-events.test.ts`, wrapping a hand-written stub object)
  - Resolved call → `onReachable` once, others not; resolved value passed through.
  - Rejection with `ApiError` kind `"rejected"` → `onReachable` (the server answered); the same error is rethrown.
  - Kind `"network"` → `onUnreachable`, rethrown.
  - Kind `"unauthenticated"` → `onReachable` and `onUnauthenticated`, rethrown.
  - A non-`ApiError` rejection (e.g. `new TypeError("Failed to fetch")`) → treated as network: `onUnreachable`, rethrown.
  - `wsUrl({ protocol: "https:", host: "pos.example.com" } as Location)` → `"wss://pos.example.com/api/ws"`.

  Run: `pnpm test src/lib/api` → Expected: FAIL.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): AuthApi interface, API types and events wrapper"`

---

### Task 4: Fake backend (`createMockAuthApi`)

**Files:**
- Create: `web/src/lib/api/mock/fixtures.ts`, `mock-auth-api.ts`, `mock-auth-api.test.ts`

**Interfaces:**
- Consumes: `AuthApi`, `ApiError`, types (Task 3).
- Produces: `createMockAuthApi(opts?: { latencyMs?: number }): AuthApi` (default latency 150 ms, tests pass 0); `resetMockStore(): void`; `MOCK_USERS: User[]`. Later plans add `createMockMenuApi()` and so on next to it.

Fixtures (owner password `owner` / `owner123`, manager `manager` / `manager123`):

| name | role | PIN |
|---|---|---|
| Ram Shrestha | OWNER | 1111 |
| Sita Gurung | MANAGER | 2222 |
| Hari Thapa | CASHIER | 3333 |
| Gita Rai | WAITER | 4444 |
| Bikash Tamang | KITCHEN | 5555 |

State `{ sessionUserId, device: {shared, label}, failedPins: Record<userId, number>, languages: Record<userId, Language> }` lives in `localStorage["rm.mock"]` so it survives reloads. The device starts not shared. Five wrong PINs lock that user.

- [ ] **Step 1: Write failing tests** (`resetMockStore()` in `beforeEach`)
  - `getSession()` fresh → `{ user: null, device: { shared: false, label: "" } }`.
  - `passwordLogin("owner","owner123")` → role `"OWNER"`; `getSession().user.name === "Ram Shrestha"`.
  - Wrong password → rejects `ApiError` kind `"rejected"`, reason `"INVALID_CREDENTIALS"`.
  - `listPinUsers()` on a non-shared device → `"rejected"` / `"DEVICE_NOT_SHARED"`.
  - `enrollDevice("Counter PC")` while logged out → kind `"unauthenticated"`; as Gita (waiter) → `"rejected"` / `"UNKNOWN"`.
  - Owner + `enrollDevice("Counter PC")` → `getSession().device` equals `{ shared: true, label: "Counter PC" }`; `listPinUsers()` returns 5 users.
  - `pinLogin(gita.id, "4444")` → role `"WAITER"`.
  - `pinLogin(gita.id, "0000")` ×4 → `"INVALID_CREDENTIALS"` each; the 5th wrong try and every try after it (even `"4444"`) → `"PIN_LOCKED"`.
  - `logout()` → `user: null`, device still shared.
  - `setLanguage("ne")` → returned `language === "ne"` and kept in `getSession()` after logout + login.
  - `setLanguage` while logged out → kind `"unauthenticated"`.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): fake AuthApi backed by localStorage"`

---

### Task 5: Roles, session state and providers

**Files:**
- Create: `web/src/lib/auth/roles.ts`, `roles.test.ts`, `session.ts`, `session.test.tsx`, `device.ts`, `web/src/app/providers.tsx`, `web/src/test/render.tsx`
- Modify: `web/src/app/layout.tsx` (wrap children in `<Providers>`)

**Interfaces:**
- Consumes: `AuthApi`, `withApiEvents`, `ApiProvider`/`useApi`, `apiMode` (Task 3); `createMockAuthApi` (Task 4); `useLanguage` (Task 2).
- Produces:
  - `type Section = "admin" | "cashier" | "floor" | "kitchen"`
  - `homeFor(role: Role): string` — OWNER, MANAGER → `/admin/`; CASHIER → `/cashier/`; WAITER → `/floor/`; KITCHEN → `/kitchen/`
  - `canAccess(role: Role, section: Section): boolean` — admin: OWNER, MANAGER; cashier: OWNER, MANAGER, CASHIER; floor: OWNER, MANAGER, WAITER; kitchen: OWNER, MANAGER, KITCHEN
  - `type SessionState = { status: "loading" } | { status: "ready"; session: Session; offline: boolean }`
  - `useSession(): SessionState` — TanStack query key `["session"]` calling `auth.getSession()`. On success, caches the `Session` in `localStorage["rm.session"]`. On a network `ApiError`, returns the cached value with `offline: true`, or a logged-out session if nothing is cached. Applies `user.language` via `setLanguage` from `useLanguage` when it differs.
  - `entryPath(s: Session): string` — user → `homeFor(user.role)`; else shared device → `/pin/`; else `/login/`
  - `useAuthActions()` → TanStack mutations `{ passwordLogin, pinLogin, enrollDevice, logout, setLanguage }`, each invalidating `["session"]` on success
  - `getDeviceLabel(): string` / `setDeviceLabel(s: string): void` — `localStorage["rm.device"]`, default `""`
  - `<Providers>`: one `QueryClient` (`networkMode: "offlineFirst"`; `retry: (n, e) => e instanceof ApiError && e.kind === "network" && n < 2`); `ApiProvider` with `{ auth: withApiEvents(apiMode() === "mock" ? createMockAuthApi() : createRealAuthApi(), events) }`, where `createRealAuthApi` throws "real API not wired yet" until Task 10; `events.onUnauthenticated` removes `rm.session`, invalidates `["session"]` and `router.replace(entryPath(...))`; `I18nProvider`. `RealtimeProvider` is added in Task 8.
  - `renderWithProviders(ui, opts?: { api?: Partial<Api> })` for tests (defaults to the mock with latency 0)

- [ ] **Step 1: Write failing tests**

  `roles.test.ts`: table-driven over 5 roles × 4 sections against the matrix above; `homeFor` for all 5 roles.

  `session.test.tsx`:
  - Fresh mock → `ready`, `user: null`, `offline: false`; `entryPath` → `/login/`.
  - After `passwordLogin` as owner → role `"OWNER"`, `rm.session` stored, `entryPath` → `/admin/`.
  - **Review Focus 1:** `rm.session` holds Gita on a shared device, and `getSession` rejects with a network `ApiError` → `ready`, user Gita, `offline: true`; `entryPath` → `/floor/`.
  - **Review Focus 2:** logged in with a session cached; a later API call rejects as `"unauthenticated"` → `rm.session` removed and the mocked `useRouter().replace` called with `/pin/` (shared device).
  - Session user has `language: "ne"` → `useLanguage().language === "ne"`.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): roles, session state with offline cache, providers"`

---

### Task 6: Login screens — password, shared device, staff PIN

**Files:**
- Create: `web/src/components/auth/password-login-form.tsx`, `staff-picker.tsx`, `pin-pad.tsx`, `auth-screens.test.tsx`, `web/src/app/login/page.tsx`, `web/src/app/pin/page.tsx`, `web/src/app/page.tsx`
- shadcn: `pnpm dlx shadcn@latest add button input label card checkbox`

**Interfaces:**
- Consumes: `useSession`, `entryPath`, `homeFor`, `useAuthActions`, `getDeviceLabel`/`setDeviceLabel` (Task 5); `useT` (Task 2); `ApiError` (Task 3).
- Produces: routes `/` (renders nothing while loading, then `router.replace(entryPath)`), `/login/`, `/pin/`.
  - `<PasswordLoginForm />` — on owner/manager success, if the checkbox `login.enrollDevice` ("Use this device for staff PIN login") is ticked, the device label input (prefilled from `getDeviceLabel()`) is required, and it calls `enrollDevice(label)` before navigating to `homeFor(role)`. **If the backend developer declines device enrollment (request #2), delete the checkbox and the `enrollDevice` call. `/pin/` then shows whenever the user chooses "Staff PIN".**
  - `<StaffPicker users={User[]} onPick(user: User) />` — large name cards (≥48 px) with `t("role.<ROLE>")` under the name.
  - `<PinPad length={4} disabled={boolean} error={"wrong" | "locked" | null} onComplete(pin: string) />` — digits 0–9 (≥64 px) and backspace; calls `onComplete` once when `length` digits are entered and ignores further digits until reset; clears on a new `error`.
  - `/pin/`: picker → pad for the chosen user (with `pin.back`) → on success `router.replace(homeFor(role))`. Link `login.useOwnerLogin` → `/login/`. A device that isn't shared → `router.replace("/login/")`.

- [ ] **Step 1: Write failing tests** (`auth-screens.test.tsx`, mock API, mocked router)
  - Owner `owner`/`owner123` → `replace("/admin/")`.
  - Wrong password → `login.invalid` text visible, password field cleared, no navigation.
  - **Review Focus 4:** double-click Log in → a `vi.spyOn` on the mock's `passwordLogin` is called once; the button is disabled while pending.
  - Owner logs in with the checkbox ticked and label `Counter PC` → `getSession().device.shared === true`; `getDeviceLabel() === "Counter PC"`.
  - Shared device at `/pin/`: 5 staff listed; tap "Gita Rai", then 4,4,4,4 → `replace("/floor/")`.
  - **Review Focus 3:** `0000` → `pin.wrong` shown and pad empty; after the 5th wrong try → `pin.locked` shown, digit buttons disabled, no further `pinLogin` calls.
  - **Review Focus 4:** typing 4,4,4,4,4 quickly → `pinLogin` called once.
  - Not-shared device at `/pin/` → `replace("/login/")`.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Manual check** — `pnpm dev`, open `http://localhost:3000` at 390 px width and at desktop width. Log in as owner, mark the device shared, then PIN in as each role, in both languages. Nothing overflows horizontally.
- [ ] **Step 5: Commit** — `git commit -m "feat(web): password login, shared device and staff PIN login"`

---

### Task 7: App shell and role-gated sections

**Files:**
- Create: `web/src/components/shell/app-shell.tsx`, `role-gate.tsx`, `connection-pill.tsx`, `shell.test.tsx`, `web/src/app/(staff)/layout.tsx`, `web/src/app/(staff)/{admin,cashier,floor,kitchen}/page.tsx`

**Interfaces:**
- Consumes: `useSession`, `canAccess`, `homeFor`, `entryPath`, `useAuthActions` (Task 5); `LanguageToggle`, `useT` (Task 2).
- Produces:
  - `<RoleGate section={Section}>` — loading → nothing; no user → `replace(entryPath)`; no access → `replace(homeFor(role))`; else children
  - `<ConnectionPill status={"connecting" | "online" | "offline"} />` — presentational: green/amber/red dot plus `conn.*` text
  - `<AppShell>` — top bar with `app.name`, section title, user name, `<ConnectionPill>` (passes `"online"` in this task; Task 8 passes `useConnectivity()`), `<LanguageToggle>` (when logged in also calls the `setLanguage` mutation), and "Switch user" (shared device → `logout`, then `/pin/`) or "Log out" (→ `logout`, then `/login/`)
  - Section pages: `<RoleGate section="floor"><p>{t("common.comingSoon")}</p></RoleGate>`, and the same for the other three

- [ ] **Step 1: Write failing tests** (`shell.test.tsx`)
  - Waiter: `floor` gate renders children; `admin` gate → `replace("/floor/")`.
  - Kitchen user at `cashier` → `replace("/kitchen/")`; owner renders all four.
  - No user → `replace("/login/")`, or `/pin/` on a shared device.
  - Shared device: "Switch user" → `logout` called, `replace("/pin/")`; not shared: "Log out" → `replace("/login/")`.
  - Logged in, toggle language → mock `setLanguage("ne")` called; after logout and PIN login as the same user, the UI is in Nepali.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run tests** → Expected: PASS.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): app shell with role-gated sections"`

---

### Task 8: Connectivity and realtime client

**Files:**
- Create: `web/src/lib/realtime/connectivity.ts`, `connectivity.test.ts`, `client.ts`, `client.test.ts`, `realtime-provider.tsx`, `realtime-provider.test.tsx`
- Modify: `web/src/app/providers.tsx` (add `RealtimeProvider`; route `ApiEvents.onReachable/onUnreachable` to the connectivity store), `web/src/components/shell/app-shell.tsx` (pass `useConnectivity()` to `ConnectionPill`)

**Interfaces:**
- Consumes: `wsUrl`, `healthUrl`, `apiMode` (Task 3); `useSession` (Task 5).
- Produces:
  - `type ConnStatus = "connecting" | "online" | "offline"`
  - `createConnectivity(now: () => number = Date.now)` → `{ setSocketOpen(open: boolean): void; markApiSuccess(): void; markApiFailure(): void; getStatus(): ConnStatus; subscribe(fn: () => void): () => void }`. Rule: `online` if the socket is open, or there has been an API success and no failure after it; `offline` if the socket is closed and a failure came after the last success; `connecting` before any signal. Staleness alone never makes it `offline`; the heartbeat produces failures.
  - `useConnectivity(): ConnStatus` (`useSyncExternalStore`)
  - `type RealtimeEvent = { type: string; id: string; version?: number }`
  - `interface WebSocketLike { onopen: (() => void) | null; onclose: (() => void) | null; onmessage: ((e: { data: unknown }) => void) | null; close(): void }`
  - `backoffDelay(attempt: number): number` → `[1000, 2000, 5000, 15000, 30000][Math.min(attempt, 4)]`
  - `class RealtimeClient { constructor(opts: { url: string; createSocket(url: string): WebSocketLike; onEvent(e: RealtimeEvent): void; onOpen(isReconnect: boolean): void; onClose(): void }); start(): void; stop(): void }` — reconnects using `backoffDelay`; resets the attempt count on open; ignores frames that aren't a valid `RealtimeEvent` JSON; `stop()` cancels a pending reconnect
  - `useRealtimeEvent(type: string, handler: (e: RealtimeEvent) => void): void` — for later plans (e.g. `item.ready` toasts)
  - `<RealtimeProvider>` — runs only while a user is logged in; uses `wsUrl()`; on every event and on every reconnect calls `queryClient.invalidateQueries()`; while the socket is closed, fetches `healthUrl()` every 10 000 ms and reports the result to connectivity. In mock mode, uses an in-memory socket that opens at once and exports `mockRealtime.emit(event: RealtimeEvent)` for tests and later plans.

- [ ] **Step 1: Write failing tests**

  `connectivity.test.ts` (injected clock):
  - Initial → `connecting`; `setSocketOpen(true)` → `online`.
  - Socket closed, `markApiSuccess()` at t=0 → `online` at t=19 999 and at t=25 000.
  - `markApiSuccess()` then `markApiFailure()` with the socket closed → `offline`; then `setSocketOpen(true)` → `online`.
  - `subscribe` listener fires on each status change, not on no-op updates.

  `client.test.ts` (fake sockets, `vi.useFakeTimers()`):
  - `backoffDelay` for attempts 0..6 → `1000, 2000, 5000, 15000, 30000, 30000, 30000`.
  - **Review Focus 5:** the socket closes 10 times without opening → every reconnect delay is between 1000 and 30 000 ms; after the 5th failure, at most 2 new sockets are created in the next 60 s.
  - Open → `onOpen(false)`; close and reopen → `onOpen(true)`, and the next failure waits 1000 ms again.
  - `'{"type":"order.updated","id":"a","version":2}'` → `onEvent` with that object; `"garbage"` → no call, no throw.
  - `stop()` with a reconnect pending → no new socket.

  `realtime-provider.test.tsx` (mock mode):
  - Logged in: `mockRealtime.emit({ type: "table.updated", id: "t1" })` → spy on `queryClient.invalidateQueries` called; a `useRealtimeEvent("table.updated", h)` handler receives the event.
  - Logged out → no socket created.

  Run → Expected: FAIL.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run tests** → Expected: PASS (whole suite green).
- [ ] **Step 4: Commit** — `git commit -m "feat(web): connectivity status and realtime client"`

---

### Task 9: Installable PWA shell and end-to-end smoke test

**Files:**
- Create: `web/public/manifest.webmanifest`, `web/public/icons/icon-192.png`, `icon-512.png`, `web/playwright.config.ts`, `web/e2e/auth.spec.ts`
- Modify: `web/src/app/layout.tsx` (`metadata.manifest`, `viewport.themeColor`), `web/package.json` (`"e2e": "playwright test"`)

**Interfaces:**
- Produces: `pnpm e2e` — Playwright `webServer` runs `NEXT_PUBLIC_API_MODE=mock pnpm build && npx serve out -l 4173`; projects Desktop Chrome and Pixel 7.

Manifest: `name` "Restaurant POS", `short_name` "POS", `display` "standalone", `start_url` "/", theme/background colours matching the shell, two placeholder icons (replace with the restaurant logo later). The service worker comes in Plan 3, with the offline outbox.

- [ ] **Step 1: Write the e2e spec** (`e2e/auth.spec.ts`)
  - Fresh browser → `/login/`.
  - Owner `owner`/`owner123`, "Use this device…" ticked, label `Counter PC` → `/admin/` shows "Ram Shrestha".
  - "Switch user" → `/pin/`; "Gita Rai", PIN 4444 → `/floor/`.
  - `page.goto("/admin/")` as Gita → ends on `/floor/`.
  - Toggle language → section title shows the `ne` text of `section.floor`; reload → still Nepali.
  - Hari Thapa, wrong PIN ×5 → locked message visible.
- [ ] **Step 2: Run** — `cd web && pnpm add -D @playwright/test serve && pnpm exec playwright install chromium && pnpm e2e` → Expected: all pass on both projects.
- [ ] **Step 3: Full check** — `pnpm test && pnpm typecheck && pnpm lint && pnpm build && test -f out/manifest.webmanifest && echo OK` → Expected: all green, `OK`.
- [ ] **Step 4: Commit** — `git commit -m "feat(web): PWA manifest and auth e2e smoke test"`

---

### Task 10: Real `AuthApi` over the backend's Connect contract

**Blocked until** the backend developer has committed `proto/` with the auth service (request #1). Tasks 1–9 don't depend on it.

**Files:**
- Create: `web/buf.gen.yaml`, `web/src/gen/**` (generated, committed), `web/src/lib/api/connect/to-api-error.ts`, `to-api-error.test.ts`, `connect-auth-api.ts`, `connect-auth-api.test.ts`
- Modify: `web/package.json` (`gen` script), `web/src/app/providers.tsx` (`createRealAuthApi` → `createConnectAuthApi(transport)`)

**Interfaces:**
- Consumes: the backend's generated auth service and error-detail types (names come from their `.proto`); `AuthApi`, `ApiError`, types (Task 3).
- Produces:
  - `toApiError(err: unknown): ApiError` — Connect `Unauthenticated` with no reason → kind `"unauthenticated"`; `Unavailable`, `DeadlineExceeded`, `Unknown` and non-Connect `TypeError` → `"network"`; everything else → `"rejected"`. `reason` is mapped from the backend's error-detail reason to `ApiErrorReason` (`"UNKNOWN"` when absent). Note: a wrong-password `Unauthenticated` **with** a reason is `"rejected"`, so a failed login doesn't trigger the session-expired redirect.
  - `createConnectAuthApi(transport: Transport): AuthApi` — maps each `AuthApi` method to the backend RPC, converts their messages to the web types (`Role`/`Language` enums → string unions) and wraps every error with `toApiError`
  - Transport: `createConnectTransport({ baseUrl: apiBase(), fetch: (i, init) => fetch(i, { ...init, credentials: "include" }) })`

- [ ] **Step 1: Codegen**

```bash
cd web && pnpm add @bufbuild/protobuf @connectrpc/connect @connectrpc/connect-web && pnpm add -D @bufbuild/buf @bufbuild/protoc-gen-es
```

`web/buf.gen.yaml`:

```yaml
version: v2
plugins:
  - local: protoc-gen-es
    out: src/gen
    include_imports: true
    opt: target=ts
```

Script `"gen": "buf generate ../proto"`. Run `pnpm gen` → `src/gen/` contains the auth service.

- [ ] **Step 2: Write failing tests**
  - `to-api-error.test.ts`: one case per mapping rule above, using real `ConnectError` instances with and without the backend's error detail.
  - `connect-auth-api.test.ts`: `createRouterTransport` implementing the backend's auth service with canned responses → each `AuthApi` method returns the web-typed value (e.g. backend waiter role → `"WAITER"`, Nepali language enum → `"ne"`, logged-out session → `user: null`); a handler that throws a PIN-locked error → `ApiError` `"rejected"` / `"PIN_LOCKED"`.

  Run → Expected: FAIL.
- [ ] **Step 3: Implement** both files and swap `providers.tsx` to the real API when `apiMode() === "real"`.
- [ ] **Step 4: Run** — `pnpm test && pnpm typecheck` → Expected: PASS. The Task 4–8 tests still pass unchanged against the mock.
- [ ] **Step 5: Integration check** — with the backend's dev stack running and serving the web origin through its proxy, set `NEXT_PUBLIC_API_MODE=real` and repeat the Task 6 manual check against seeded users.
- [ ] **Step 6: Commit** — `git commit -m "feat(web): real AuthApi over generated Connect client"`
