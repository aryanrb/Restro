# WebSocket event catalogue

Lives next to the `.proto` contract. The web app and the native mobile app both
depend on it. **Draft — agree with the frontends before the hub is built.**

## Transport

- One endpoint: `GET /api/ws` on the same origin as the Connect API (upgrade to WebSocket).
- Auth: the same session (web cookie / mobile bearer). Unauthenticated upgrades are rejected.
- Server library: `coder/websocket`. One in-process hub per deployment (one process = one restaurant).
- **Server → client only.** No mutations over the socket. Every mutation is a Connect RPC.

## Message format

JSON text frames. This matches the web's assumption
(`web-01` request #6: `{"type": "order.updated", "id": "<uuid>", "version": 3}`):

```json
{ "type": "order.updated", "id": "0f8c…", "version": 7 }
```

- `type` (string, required) — the event name below.
- `id` (string) — the primary entity id the change is about. Omitted for broadcast-only events.
- `version` (int, optional) — the aggregate version after the change, when the entity has one.

Events are **"something changed" pokes, not state.** Clients refetch the affected
query via Connect; a missed or out-of-order event can only delay state, never corrupt it.

## Client rules (same for web and mobile)

1. On every event: refetch the affected query (or `invalidate` the matching cache key).
2. On every (re)connect: flush the outbox first, then refetch open orders, tables, menu
   availability and kitchen tickets. Never trust replay for correctness.
3. Ignore an event whose `version` ≤ the version already cached for that `id`.
4. Reconnect with capped backoff `[1s, 2s, 5s, 15s, 30s]`; never a tight loop.

## Event names (MVP)

| `type`             | `id` is…            | Emitted when                                               | Primary audience |
|--------------------|---------------------|-----------------------------------------------------------|------------------|
| `order.updated`    | order id            | create / add items / void / transfer / merge / status flow| waiter, cashier  |
| `ticket.created`   | order id            | items sent to a station (`SendItems`)                     | kitchen (station)|
| `item.ready`       | order item id       | KDS marks an item READY                                    | waiter           |
| `item.86d`         | menu item id        | an item is 86'd or un-86'd (`Set86`)                      | everyone         |
| `table.updated`    | table id            | table occupied/freed, seats/active change                 | waiter, cashier  |
| `menu.updated`     | — (broadcast)       | any menu/category/variant/modifier change                 | admin, waiter    |
| `settings.updated` | — (broadcast)       | settings saved                                            | admin            |
| `station.updated`  | station id          | station added/renamed/removed                             | admin, kitchen   |
| `invoice.issued`   | invoice id          | `IssueBill` / `CounterSale` issued (and paid) an invoice  | cashier, owner   |

### Later plans (reserved names — do not reuse)

| `type`             | `id` is…   | Plan        |
|--------------------|------------|-------------|
| `invoice.issued`   | invoice id | billing      |
| `invoice.cancelled`| invoice id | billing      |
| `shift.updated`    | shift id   | shifts       |

## Scoping / rooms

MVP keeps it simple: broadcast every event to all connected clients; clients filter by
`type` and refetch what they care about. Per-station or per-role fan-out is an
optimisation (add only if peak-hour chatter measurably hurts), not a correctness need —
`ponytail:` broadcast-to-all, add station rooms if frame volume bites.
