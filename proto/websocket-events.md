# WebSocket event catalogue

Lives next to the `.proto` contract. The web app and the native mobile app both
depend on it. **Draft — agree with the frontends before the hub is built.**

## Transport

- One endpoint: `GET /api/ws` on the same origin as the Connect API (upgrade to WebSocket).
- Auth: the same session as Connect — web `rm_session` cookie, or mobile `Authorization: Bearer <token>`.
  An upgrade without a valid session is rejected with HTTP 401 before the socket opens.
- Server library: `coder/websocket`. One in-process hub per deployment (one process = one restaurant).
- **Server → client only.** No mutations over the socket. Every mutation is a Connect RPC.

## Message format

JSON text frames. This matches the web's assumption
(`web-01` request #6: `{"type": "order.updated", "id": "<uuid>", "version": 3}`):

```json
{ "type": "order.updated", "id": "0f8c…", "version": 7 }
```

- `type` (string, required) — the event name below.
- `id` (string, always present) — the primary entity id the change is about; `""` for broadcast-only events.
- `version` (int, optional) — the aggregate version after the change, when the entity has one.
  Every `order.updated` carries it (order commands and kitchen actions alike).

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
| `order.updated`    | order id            | create / add items / send / void / serve / transfer / merge (both orders) / guest count / kitchen start, ready, bump | waiter, cashier  |
| `ticket.created`   | order id            | items sent to a station (`SendItems`)                     | kitchen (station)|
| `item.ready`       | order item id       | KDS marks an item READY                                    | waiter           |
| `item.86d`         | menu item id        | an item is 86'd or un-86'd (`Set86`)                      | everyone         |
| `table.updated`    | table id            | table occupied/freed, seats/active change                 | waiter, cashier  |
| `menu.updated`     | — (broadcast)       | any menu/category/variant/modifier change                 | admin, waiter    |
| `settings.updated` | — (broadcast)       | settings saved                                            | admin            |
| `station.updated`  | station id          | station added/renamed/removed                             | admin, kitchen   |
| `invoice.issued`   | invoice id          | `IssueBill` / `CounterSale` issued (and paid) an invoice  | cashier, owner   |
| `customer.updated` | customer id         | credit account created/edited/limit/(de)activated; its balance changed (on-account bill issued or cancelled, repayment taken or voided) | cashier, owner |

### Inventory (InventoryService, recipes on MenuService)

| `type`             | `id` is…                 | Emitted when                                                                 | Primary audience |
|--------------------|--------------------------|------------------------------------------------------------------------------|------------------|
| `stock.updated`    | `"stock"`                | any stock movement was written (send, void before cooking, counter sale, waste, count post, tracking on) | admin inventory |
| `stock.low`        | inventory item id        | a tracked item's on hand crossed down to its low-stock level (once per crossing) | owner, manager (toast) |
| `stock.86suggest`  | menu item id             | a dish can no longer be made from what is on hand (once per crossing; never auto-86) | KDS, manager |
| `inventory.updated`| inventory item id        | a stock item was saved or tracking turned on/off (`version` = item version)  | admin            |
| `count.updated`    | count id, or `"lists"`   | a stock count started / counted / submitted / recount / posted / discarded; a count list saved | counters, manager |
| `waste.logged`     | waste entry id           | waste was logged                                                             | manager          |

Stock events from order commands and counter sales are sent only after the
request's transaction committed (server-side `inventory.EventsInterceptor`).

### Alerts (AlertService)

| `type`          | `id` is…  | When                                                                 | Who listens |
|-----------------|-----------|----------------------------------------------------------------------|-------------|
| `alert.created` | alert id  | an audited write fired a rule and opened a new alert (`version` = alert version) | owner, manager recipients |
| `alert.updated` | alert id  | a repeat firing grouped into the open alert, or it was resolved      | owner, manager recipients |

`settings.updated` with id `alerts` when the alert config changes. Like every event
these are broadcast to all sessions (see Scoping); the alert RPCs enforce who may read.

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
