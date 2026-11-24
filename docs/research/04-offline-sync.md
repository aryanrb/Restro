# 04 — Offline order queue and sync

_Researched 2026-11-23. Scope: waiter phones keep taking orders when the internet drops, queue them locally and flush automatically on reconnect. Bills and invoice numbers need connectivity. No device-to-device sync._

## TL;DR

**Build it yourself; don't adopt a sync engine.** Use a **custom outbox in expo-sqlite** (or MMKV). Each queued command carries a **client-generated UUID that acts as an idempotency key**. Send commands through ordinary **tRPC mutations**, and use **TanStack Query** for server state (menu, tables, open orders), persisted so the app can cold-start offline. The server stays the only authority. Order items are **append-only**, and stale or conflicting commands are **rejected with a reason** that the waiter sees.

Sync engines (PowerSync, Zero, Electric, WatermelonDB, RxDB…) solve a harder problem than ours: general bidirectional replication of a data set. We only need a **write queue** plus a **read cache**. That takes about 300–500 lines of TypeScript you fully understand.

## 1. Comparison

Versions and activity were checked on npm and GitHub on 2026-11-23.

| Option | License / cost | Self-host footprint | Postgres fit | Expo / New Arch | Offline writes | Complexity for us | Verdict |
|---|---|---|---|---|---|---|---|
| **Custom outbox** (expo-sqlite 58.x or react-native-mmkv 4.x) + UUID idempotency + tRPC 11 | MIT, free | None. It's your existing API plus one `processed_commands` table | Native: Drizzle on the server | expo-sqlite is first-party and New Arch-ready (New Arch is mandatory since SDK 55) | Yes, by design | **Low.** You own all of it | **Recommended** |
| **TanStack Query v5 persisted mutations** (`persistQueryClient`, `setMutationDefaults`, `resumePausedMutations`, `onlineManager`) | MIT, free | None | Agnostic | Pure JS, works | Yes, but paused mutations are serialized blobs and need a `mutationFn` registered via `setMutationDefaults` to resume after an app restart. Ordering, inspection and partial failures are awkward | Low–medium | **Use for reads/cache.** Don't make it the source of truth for queued orders |
| **WatermelonDB** 0.28 | MIT, free | You write push/pull endpoints yourself | Manual | Weak: last stable release Apr 2025, New Arch issues, Expo needs a community plugin and patches. Rocket.Chat migrated off it in 2026 | Yes | Medium–high | **Avoid** (maintenance risk) |
| **PowerSync** (Open Edition) | Client SDKs Apache-2.0. Service is **FSL** (source-available, free incl. commercial, no competing service, becomes Apache after 2 years) | Extra service (Docker) + its own bucket storage (Mongo or Postgres) + Postgres logical replication | Excellent (logical replication, sync rules) | `@powersync/react-native` 2.x is GA and supports Expo dev builds | Yes (upload queue → your API) | Medium–high: sync rules, a second datastore, a JWT handshake | Strong, but overkill for the MVP. Best upgrade path if the needs grow |
| **ElectricSQL** 1.x (rewritten, GA Mar 2025, now under Neon) | Apache-2.0 | Extra Elixir service + logical replication | Excellent | Read sync only, via HTTP "shapes". RN works through TanStack DB | **No.** Writes go through your own API (you still build the outbox) | Medium | Not worth it. It solves reads, which we don't need |
| **Zero** 1.x (Rocicorp, GA Mar 2026) | Apache-2.0 | zero-cache service (SQLite replica) + logical replication + your mutator endpoint | Excellent | RN support exists but is less documented than web | Limited: Zero is designed as online-first with optimistic writes, not a durable offline queue across long outages or restarts | High: new query/mutator model, parallel to tRPC | Avoid for this use case |
| **Replicache** | Free / open-sourced, **maintenance mode** (no new features) | You implement push/pull | Manual | Needs a KV store adapter | Yes | Medium–high | Avoid (superseded by Zero) |
| **TinyBase** 7+/9.x | MIT | Its own sync (MergeableStore/CRDT over WebSocket). Has a Postgres persister but no relational sync | Poor (stores blobs/tables, not your Drizzle schema) | Expo SQLite persister works | Yes (CRDT) | Medium; a CRDT model doesn't fit server-authoritative orders | Avoid |
| **Legend-State** v3 | MIT | Sync plugins (Supabase, Keel, generic CRUD) | Via generic CRUD plugin | MMKV/AsyncStorage persistence | Yes (retry) | Low–medium, but **v3 is still beta** (3.0.0-beta.48; latest stable is 2.x) | Interesting, but beta-forever risk |
| **RxDB** 17 | Core Apache-2.0, but the **production SQLite storage for RN is a paid premium plugin** (free trial storage is "evaluation only") | Replication protocol endpoints | Via custom replication | Works with expo-sqlite (premium) | Yes | Medium–high | **Fails the "free" requirement** in practice |

## 2. Recommended MVP approach

### Principles
1. **Commands, not state sync.** The phone queues *intents* such as `addItems`, `voidItem` and `moveTable`. It never pushes table rows.
2. **The server is the authority.** It validates price, availability (86'd items) and order status at apply time.
3. **Idempotency.** Every command has a `commandId` (UUID v7, generated on the device). The server records processed IDs, so replays are no-ops that return the original result.
4. **Strict FIFO per device.** The outbox drains one command at a time, in order, so `addItems` never arrives after its own `voidItem`.
5. **Append-only order lines.** Adding items creates new rows. You never edit a sent quantity: changes are a void (negative/void row) plus a new line.

### Data flow sketch

```
Waiter taps "Send"
  │
  ▼
[expo-sqlite outbox]  INSERT {commandId, type, payload, orderClientId,
  │                           baseVersion, createdAt, status='pending', attempts}
  │  UI shows items immediately as "pending" (grey clock icon)
  ▼
Flusher (runs on: app start, enqueue, reconnect, every 15 s while pending>0)
  │  takes the oldest 'pending' → trpc.orders.command.mutate(cmd)
  ▼
API (Hono/Fastify + tRPC)
  BEGIN
    INSERT INTO processed_commands(command_id) ON CONFLICT DO NOTHING
      → if conflict: return stored result (duplicate replay)
    validate (menu item active? order open? version ok?)
    apply (insert order_items with server-side price snapshot, bump orders.version)
    store result in processed_commands
  COMMIT
  emit Socket.IO "order.updated" {orderId, version} to room restaurant:{id}
  │
  ▼
Client result handling
  ok        → delete outbox row, merge server order into TanStack cache
  rejected  → mark 'rejected' + reason, show red banner/toast with action
  network/5xx/timeout → keep 'pending', backoff (1s,2s,5s,15s,30s cap)
  4xx (validation/auth) → 'rejected' (never retry forever)
```

**Offline creation of new orders.** The client creates the order with its own `orderClientId` (UUID) as the stable key, and the server stores that ID (it can be the PK, or a unique column alongside it). Later commands in the queue reference the same ID, so you never need to swap temp IDs for server IDs.

**Reads.** Use TanStack Query with `@tanstack/query-async-storage-persister` (or an MMKV-backed persister). That way menu, tables and open orders survive restarts and cold starts while offline. Set `networkMode: 'offlineFirst'` for queries.

**What is blocked offline.** Closing a bill, issuing an invoice number, payments, and editing other waiters' orders. Disable those buttons with the explanation "Needs connection".

**Kitchen display.** In the MVP, items reach the kitchen only after the server accepts them. The waiter UI must clearly show "Queued, not yet in kitchen". If the kitchen tablet is on the same Wi-Fi that lost internet, it is offline too. That's an operational fallback (paper tickets), not a software one.

### Minimal schema additions (server)
- `orders.version int` (optimistic concurrency) and `orders.client_id uuid unique`
- `order_items`: `client_id uuid unique`, `unit_price_snapshot`, `status` (`queued|sent|preparing|ready|served|void`), `added_by`, `void_reason`
- `processed_commands(command_id uuid pk, device_id, result jsonb, created_at)`, pruned after about 7 days
- `menu_items.available bool` and `menu_items.updated_at`

## 3. Conflict rules

| Scenario | Rule | UX |
|---|---|---|
| Two waiters add items to the same table's order at once | Accept both. Adds are append-only and commute, so `addItems` **doesn't check `baseVersion`** | Both see the merged order through the socket push |
| Two waiters edit the same thing (void, change qty, move table, apply discount) | Destructive or structural commands carry `baseVersion`. If `orders.version` ≠ `baseVersion`, **reject** with `STALE_VERSION` | "Order changed by Maria. Review and retry" button reloads the order |
| Item 86'd while the order sat in the queue | Server checks availability at apply time and rejects **only those lines** (partial accept). The rest of the command is applied | Red line "Salmon unavailable, tell guest". Push notification/vibration to the waiter |
| Price changed while queued | **Server price at apply time wins** and is snapshotted onto the line. Optionally accept the client price if the change happened less than N minutes ago (decide later; default is the server price) | Show the price diff on the line if it differs from what was displayed |
| Item already sent to kitchen | Lines with status ≥ `sent` cannot be edited. Only `voidItem` with a reason is allowed (may require manager role), and it notifies the kitchen | "Already in kitchen. Request void?" |
| Order closed/paid while a queued add was pending | Reject with `ORDER_CLOSED` | "Table already billed. Create new order for table 5?" (one tap re-queues on a new order) |
| Duplicate submit (retry after timeout, double tap) | Idempotency key → server returns the original result | Invisible |
| Long outage (e.g. >30 min) | Commands older than a threshold still apply, but are flagged for manager review | "Queued 42 min ago" label in the queue |

General rule: **never silently drop or alter a waiter's command.** Every rejection lands in a visible "Needs attention" list on the device until the waiter acknowledges it.

## 4. Connectivity detection and reconnect consistency

**Detection.** Combine signals; don't trust one.
- `@react-native-community/netinfo` (v12) is fine for *fast negative* signals (radio off, airplane mode). Its `isInternetReachable` is unreliable on Android: it uses native validation and ignores your `reachabilityUrl` unless `useNativeReachability: false`, VPNs can report true, and `null` means unknown.
- **Source of truth = can we reach *our* API.** Treat "online" as: Socket.IO connected **or** the last API call / `GET /health` (every 10–15 s while disconnected, with backoff) succeeded within 20 s.
- Wire it into TanStack Query with `onlineManager.setEventListener(...)` so queries and mutations pause and resume on *your* signal, not NetInfo's alone.
- Banner states: **Online**, **Offline: N orders queued** (amber), **Syncing N…**, **N need attention** (red).

**Socket.IO on reconnect.**
- Socket.IO's `connectionStateRecovery` replays missed packets only within a short window (default 2 min). It depends on the adapter (the in-memory adapter is fine for one instance per restaurant) and isn't guaranteed. **Treat it as an optimization, never as correctness.**
- On every `connect` event where `socket.recovered === false`: (1) flush the outbox first, (2) then `queryClient.invalidateQueries()` for open orders, tables, menu availability and kitchen tickets. Optionally call `orders.changedSince({ sinceVersion | sinceTimestamp })` to keep the refetch cheap.
- Socket events carry only `{entity, id, version}` ("poke" pattern). Clients refetch or patch through tRPC, so a missed event can never corrupt state, only delay it.
- Ignore events whose `version` ≤ the cached version (handles out-of-order delivery).
- Mutations go over **HTTP tRPC, not socket emits**. You get request/response semantics, status codes and idempotency for free.

## 5. Risks

| Risk | Mitigation | Severity |
|---|---|---|
| Waiter thinks a queued order reached the kitchen | Explicit per-line "Queued" status, loud banner, count badge, optional vibration on flush | **High** (operational) |
| Restaurant Wi-Fi down means the kitchen display is also down | Out of software scope. Document a paper fallback; consider phone hotspot/4G failover on the router | High |
| App killed or phone dies with a queue | SQLite outbox is durable. Flush on next start. Lost only if the phone is lost | Medium |
| Clock skew on devices | Server timestamps are authoritative. Client `createdAt` is informational only | Low |
| Queue poisoned by one permanently failing command blocking FIFO | 4xx → mark rejected and move on. Retry only network/5xx errors. Manual "discard" in the attention list | Medium |
| `processed_commands` growth | Prune after 7 days (cron) | Low |
| Scope creep toward "full offline POS" | Keep billing online-only, as decided. Revisit PowerSync if offline billing becomes a requirement | Medium |

## 6. Confidence

| Claim | Confidence |
|---|---|
| Custom outbox + idempotency is the simplest maintainable option for this scope | **High** |
| TanStack Query persisted mutations work, but are awkward as the primary durable queue | Medium-high (based on docs, GitHub discussions and community reports) |
| PowerSync Open Edition is free for commercial self-hosting under FSL | Medium-high (vendor blog/legal pages; have someone read the FSL text) |
| WatermelonDB maintenance and New Arch risk | Medium-high (npm release dates, changelog, Rocket.Chat migration) |
| Zero is not suited to long offline write queues | Medium (status docs don't address offline writes explicitly; design is online-first) |
| RxDB production RN storage is premium | High (RxDB docs) |
| Legend-State v3 still beta | High (npm dist-tags show 3.0.0-beta.48, latest = 2.1.15) |
| NetInfo `isInternetReachable` unreliable on Android | Medium-high (open GitHub issues) |

## 7. Sources

- TanStack Query offline example & persistQueryClient: https://tanstack.com/query/v4/docs/framework/react/examples/offline , https://tanstack.com/query/latest/docs/framework/react/plugins/persistQueryClient
- `setMutationDefaults` / resume discussion: https://github.com/TanStack/query/discussions/7355
- RN offline with TanStack Query (community): https://dev.to/fedorish/react-native-offline-first-with-tanstack-query-1pe5
- PowerSync Open Edition & licensing: https://www.powersync.com/blog/powersync-open-edition-release , https://www.powersync.com/legal/overview , https://www.powersync.com/blog/powersync-supports-fair-source , https://github.com/powersync-ja/self-host-demo
- ElectricSQL 1.0 & TanStack DB: https://electric-sql.com/blog/2025/03/17/electricsql-1.0-released , https://tanstack.com/blog/electric-partnership , https://neon.com/blog/tanstack-db-and-electricsql
- Zero: https://zero.rocicorp.dev/docs/open-source , https://zero.rocicorp.dev/docs/status , https://zero.rocicorp.dev/docs/self-host , https://www.infoq.com/news/2026/06/zero-version-1
- Replicache maintenance mode: https://replicache.dev/
- WatermelonDB: https://github.com/Nozbe/WatermelonDB/blob/master/CHANGELOG.md , https://dev.to/fasthedeveloper/watermelondb-expo-sdk-54-the-complete-mobile-offline-first-setup-guide-that-actually-works-5he5 , https://github.com/RocketChat/Rocket.Chat.ReactNative/pull/7407
- TinyBase persisters: https://tinybase.org/api/persisters/ , https://expo.dev/blog/how-to-synchronize-reactive-local-first-apps-with-tinybase
- Legend-State sync: https://legendapp.com/open-source/state/v3/sync/persist-sync/ , https://expo.dev/blog/offline-first-apps-with-expo-and-legend-state
- RxDB SQLite storage / premium: https://rxdb.info/rx-storage-sqlite.html , https://rxdb.info/premium/
- Expo local-first guide & New Architecture: https://docs.expo.dev/guides/local-first/ , https://docs.expo.dev/guides/new-architecture , https://expo.dev/changelog/sdk-55
- NetInfo reachability caveats: https://github.com/react-native-netinfo/react-native-netinfo , https://github.com/react-native-netinfo/react-native-netinfo/issues/745
- Socket.IO connection state recovery: https://socket.io/docs/v4/connection-state-recovery/ , https://socket.io/docs/v4/tutorial/step-6
