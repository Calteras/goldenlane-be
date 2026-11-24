# How the POS, this API and the tracking engine exchange information

This explains what each system knows, who may call whom, what is sent, what happens when something
goes wrong, and how a vendor's phone ends up as a dot on a customer's map. Setup is in the
[README](../README.md). The formal wire contract is
`../../ansilum-be/docs/streetfood-os/INTEGRATION_CONTRACT.md`; this document is the story around it.

## Contents

1. [The cast](#1-the-cast)
2. [The rule that keeps it simple: one owner per fact](#2-the-rule-that-keeps-it-simple-one-owner-per-fact)
3. [The link and the signature](#3-the-link-and-the-signature)
4. [Staying in step: push, with pull as the safety net](#4-staying-in-step-push-with-pull-as-the-safety-net)
5. [The tracking engine, end to end](#5-the-tracking-engine-end-to-end)
6. [An order, end to end](#6-an-order-end-to-end)
7. [Online payment](#7-online-payment)
8. [How the platform's fees are collected](#8-how-the-platforms-fees-are-collected)
9. [What can go wrong and how it heals](#9-what-can-go-wrong-and-how-it-heals)
10. [Where to find it in the code](#10-where-to-find-it-in-the-code)

## 1. The cast

| Who | What it is | Talks to |
|---|---|---|
| **The POS** (`ansilum-be`) | NestJS and PostgreSQL. The merchant's system of record: outlets, hours, products, prices, stock, the vendor's working day, GPS trails, the cashier's order list, sales, and the merchant's subscription. | The cashier and vendor apps, and this API |
| **This API** | Go and its own PostgreSQL. Everything customer-facing: accounts, search, orders and payments from the customer's side, push, reviews, the fee ledger. | The customer app, the POS, the payment gateways, Firebase |
| **The cashier tablet and vendor phone** | The merchant's staff apps. They use the POS API only. | The POS |
| **The customer app** | Mobile app (not built yet; the API is ready for it). It uses this API only. | This API, Firebase |
| **The payment gateways** | Xendit or Midtrans, each merchant's own account. | This API |
| **The tracking engine** | Not a separate server. It is a set of rules inside the POS (validating and throttling GPS) plus a set of rules inside this API (turning positions into search results). Section 5. | |

Two things never happen: the customer app never calls the POS, and the vendor's phone never calls this
API. The two backends also never read each other's databases. Everything crosses as a signed HTTP
message.

```
 cashier / vendor staff                                              customer
        │                                                               │
        ▼                                                               ▼
 ┌─────────────────┐   1. events (POS → API): what changed      ┌────────────────────┐
 │       POS       │ ─────────────────────────────────────────▶ │   This API (Go)    │
 │    ansilum-be   │   2. reads (API → POS): outlets, menus     │                    │
 │                 │ ◀───────────────────────────────────────── │                    │
 │                 │   3. orders (API → POS): price, place,     │                    │
 │                 │ ◀───────────────────────────────────────── │                    │
 │                 │      cancel, status                        │                    │
 │                 │   4. fees (POS → API): reserve, report     │                    │
 │                 │ ─────────────────────────────────────────▶ │                    │
 └─────────────────┘                                            └─────────┬──────────┘
                                                                          │ 5. charge, refund,
                                                                          │    status (API → gateway)
                                                                          ▼    callbacks (gateway → API)
                                                                  Xendit / Midtrans
```

## 2. The rule that keeps it simple: one owner per fact

| Fact | Owner | The other side holds |
|---|---|---|
| Outlet name, address, hours, status, prep time, location mode | POS | a copy, replaced when the POS says it changed |
| Menu, prices, availability, tax and service charge | POS | a copy of the menu, used only to show it and to give friendly early errors |
| Whether a vendor is out today, and where | POS | a copy of the working day and the latest published position |
| The price of an order | **POS only** (the API never prices) | the price the POS answered |
| The merchant's view of an order and its status | POS | a mirror of the status, updated by events |
| The raw GPS trail, distances, route reports | POS only | nothing |
| Customer accounts, devices, reviews, ratings | This API only | nothing |
| Whether an online payment succeeded | The gateway (this API reads it) | nothing |
| Platform fees owed, and whether they were collected | This API (the ledger) | the POS adds reserved fees to the subscription charge and reports the payment |

Because the POS owns the shared facts, this API's copy can always be thrown away and rebuilt from the
POS. That is what makes the lost-message cases in section 9 harmless.

## 3. The link and the signature

### Creating the link

```
 business owner          POS                               operator              This API
      │  POST /link  ─────▶ │                                  │                      │
      │  {platformUrl}      │ makes keyId (sfk_…) and          │                      │
      │                     │ secret (sfs_…), stores the       │                      │
      │ ◀──── keyId, secret │ secret encrypted, shows it ONCE  │                      │
      │       (once)        │                                  │                      │
      │  publish outlets ──▶│                                  │                      │
      │                     │                    POST /v1/admin/merchants             │
      │                     │                    {name, posTenantId, keyId, secret,   │
      │                     │                     posBaseUrl}  ───────────────────────▶│
      │                     │ ◀── signed GET /outlets (does the secret work?) ─────────│
      │                     │ ──── answer ───────────────────────────────────────────▶│
      │                     │                                  stores the secret       │
      │                     │                                  encrypted, loads outlets│
      │                     │                                  and menus at once       │
```

- One link per business. The `keyId` identifies it; the `secret` is shared by both sides.
- The POS stores the secret AES-256-GCM encrypted with `STREETFOOD_ENCRYPTION_KEY`, bound to the
  tenant. This API stores it the same way with `ENCRYPTION_KEY`, bound to the merchant. Neither key
  is ever sent anywhere.
- This API checks the credentials against the POS **before** storing anything, so a typo cannot
  create a half-working merchant.
- Which POS addresses are acceptable is restricted (`POS_ALLOWED_HOSTS`, `https` only, `localhost`
  in development), so the platform cannot be pointed at internal addresses. The POS applies the same
  kind of rule to the platform's address (`STREETFOOD_ALLOWED_PLATFORM_HOSTS`).

### Every call in both directions is signed

```
X-SF-Key-Id:     sfk_<24 hex>
X-SF-Timestamp:  <unix seconds>
X-SF-Signature:  hex( HMAC-SHA256( secret, canonical ) )

canonical = timestamp + "\n" + METHOD + "\n" + path(+query) + "\n" + sha256hex(body)
```

- The path is the path **without** the host, so a signature for one endpoint cannot be replayed on
  another. The body is covered byte for byte (empty for `GET`).
- A request more than 5 minutes off the receiver's clock is refused, which limits replay.
- A body larger than 1 MB is refused.
- This API answers every authentication failure with the same `401`. The POS answers an unknown key,
  a revoked link and a wrong signature identically (`SIGNATURE_INVALID`), and reports a stale
  timestamp separately (`TIMESTAMP_SKEW`). Nobody can probe which key ids exist.
- The tenant or merchant is always decided by the key, never by anything in the body.
- Both sides use the same scheme, and the code is checked against test vectors produced by the other
  implementation, so a signature made in TypeScript verifies in Go and the reverse.

## 4. Staying in step: push, with pull as the safety net

The POS tells this API about changes (push). This API occasionally asks the POS what is true (pull).
Either would be enough most of the time; together they are robust.

### 4.1 Push: POS to API

```
 something changes in the POS (menu edit, vendor opens the day, position, order status…)
        │ in the SAME database transaction as the change
        ▼
 outbox row  ────────────────────────────────────────────────┐
        │                                                      │ commit, or nothing at all
        ▼
 worker (every 5 s): lease up to 20 waiting events per link
        │ POST {platformUrl}/v1/integrations/pos/events   (signed batch)
        ▼
 This API: verify signature ─▶ store each event in the inbox ─▶ answer 204
                                      │ (a duplicate event id is ignored)
                                      ▼
                        worker (every second while idle): apply events
```

Why it is built this way:

- **The event exists if and only if the change committed.** The POS writes the outbox row in the
  transaction that made the change, so there is no "changed but never announced" and no "announced
  but rolled back".
- **The platform answers `204` once the event is stored, not once it is applied.** The POS gets a
  fast answer, and a crash between receiving and applying loses nothing: the stored event is applied
  after the restart.
- **Delivery is at least once and unordered.** The platform ignores an event id it has seen
  (`(key id, event id)` is unique), and applies a change only if it is newer than what it holds
  (versions for outlet, menu and order events, timestamps for working days and positions). A test
  shuffles real events and checks that the result is the same.
- **Retries.** If this API is down or slow, the POS retries after 5 s, 15 s, 1 min, then every 5 min,
  for up to 24 hours. This API retries an event it could not apply after 5 s, 15 s, 1 min, then every
  5 min, up to 12 attempts.
- **How the POS notices changes.** Besides the events written by actions (orders, the working day,
  positions), it compares a fingerprint of each published outlet and its menu every
  `STREETFOOD_CHANGE_SCAN_SECONDS` (15 s). That way no existing service had to be modified to
  announce changes, and many edits within one interval become one event. It also re-announces every
  outlet and menu every 6 hours.

The events:

| Event | Sent when | What this API does |
|---|---|---|
| `outlet.updated` | the outlet's state changed (name, hours, status, prep time, location mode) | replaces its copy if the version is newer |
| `catalog.changed` | the menu or prices changed | fetches the outlet's snapshot from the POS and replaces the stored menu (the event says *that* it changed, the snapshot says *what it is*) |
| `outlet.unpublished` | the owner stopped publishing the outlet | hides it |
| `outlet.session.changed` | a cart or kiosk opened or closed its day, or changed what it shares | stores the working day: active or not, sharing mode, the chosen spot or the place the day is held at |
| `outlet.location.updated` | a new vendor position (section 5) | stores it if it is newer than the current one |
| `outlet.location.cleared` | the day ended or the vendor hid itself | removes the position |
| `order.status.changed` | an order's status changed at the POS | applies it if its version is newer, queues one push per status, wakes any open order stream |
| `billing.fees.settled` | a subscription payment that included platform fees was recorded | marks those fees collected (section 8) |
| `link.revoked` | the owner revoked the link | disables the merchant and hides its outlets |

An event type this API does not know is ignored, so the POS can add new ones without breaking it.

### 4.2 Pull: API from POS

Every 15 minutes (`INTEGRATION_RECONCILE_INTERVAL`), and once when a merchant is registered, this API
calls the POS for each merchant:

1. `GET /app/terrapos/integrations/streetfood/outlets` returns every published outlet with its state,
   its working day and its position, plus `generatedAt` (the POS clock).
2. For each outlet whose menu is missing or older than 6 hours, it calls
   `GET …/outlets/{id}/snapshot` for the full menu.
3. Outlets it holds that the POS no longer lists are hidden.

`generatedAt` matters: the answer is ordered against events using the POS's own clock, never the
platform's, so a slow reply cannot overwrite something newer that arrived by event. Only POS times are
ever compared with POS times.

Pull is why a lost event, a platform outage or a new deployment never needs manual repair.

## 5. The tracking engine, end to end

The goal: a customer sees a cart where it really is, only while it is working, only as precisely as the
vendor chose, and never a stale or bogus dot.

### 5.1 The vendor's working day

A mobile outlet (location mode `MOBILE`) is **not on the map until its day is open**. The vendor's
phone drives the day through the POS (`/app/terrapos/streetfood-vendor/…`):

| Call | Meaning |
|---|---|
| `POST session/start` | open the day, at a saved spot (`spotId`) or at the phone's position, with a sharing choice |
| `POST session/relocate` | the cart moved to another place for the rest of the day |
| `PUT session/sharing` | change what customers see: `LIVE`, `SPOT` or `HIDDEN` |
| `POST session/end` | close the day |
| `POST location/pings` | send GPS readings (below) |
| `GET/POST/PATCH/DELETE spots` | the places the vendor regularly works from |

Only one day can be open per outlet. A day nobody closed is closed automatically after 18 hours.

What the vendor shares is the vendor's decision:

| Sharing | Customers see | The phone still sends GPS to the POS? |
|---|---|---|
| `LIVE` | the live dot, as it moves | yes, and it is published |
| `SPOT` | the place the day is held at (a named spot, or the position the day was opened at). The exact GPS is never published | yes, for the owner's reports only |
| `HIDDEN` | nothing | yes, for the owner's reports only |

Fixed kiosks have no live position: customers simply see the address coordinates.

### 5.2 A GPS reading, step by step

```
 vendor phone ──(every 15–120 s, batched when offline)──▶ POS  POST location/pings
                                                           │
                          1. judge every reading on its own (one bad reading never
                             throws away the rest of a queued batch)
                          2. store all accepted readings in the trail (owner's reports)
                          3. update the outlet's "current position" from the newest
                             trusted reading
                          4. decide whether to publish it (the throttle)
                          5. answer the phone with how often to send next
                                                           │
                                                           ▼
                                         outbox: outlet.location.updated
```

**Judging a reading.** Each reading in a batch (up to 100) is classified:

- *Rejected outright*: impossible coordinates (out of range, or exactly 0,0), a time more than
  5 minutes in the future or more than 24 hours old, an accuracy worse than 2,000 m or negative, an
  impossible speed or heading.
- *Accepted but flagged*, kept for the owner's audit trail and **never used for the live position**:
  accuracy worse than 100 m (`LOW_ACCURACY`), the phone says the location is mocked (`MOCK`), the
  jump from the previous trusted position implies more than 30 m/s (`IMPLAUSIBLE_SPEED`; jumps under
  100 m are treated as GPS jitter and ignored), or the reading is not newer than the previous trusted
  one (`LATE`: a phone catching up out of order).
- *Trusted*: everything else. The newest trusted reading becomes the outlet's current position, and
  "moving" is set when the reported speed is above 0.8 m/s, or, when the phone reports no speed,
  when it has moved more than 15 m.

The answer tells the phone how many readings were accepted and flagged, and **how long to wait before
sending the next** (`nextIntervalSeconds`): 15 s while moving and 60 s while parked when sharing live;
30 s and 120 s when nobody is watching live (spot or hidden). A parked cart saves battery, a moving one
stays fresh.

**The throttle: when does a reading become an event?** The platform does not need every reading. The
POS publishes the current position when **both** hold: at least 15 seconds since it last published,
and the vendor has moved at least 20 m since the published position. A parked vendor is announced at
least every 90 seconds (a heartbeat) so the platform can tell "parked" from "gone". Waiting position
events for the same outlet are **replaced**, not queued behind each other, so a slow platform never
receives a backlog of old positions, and the queue stays small however fast a vendor moves.

The throttle is evaluated when a reading arrives. A reading that arrives too soon after a publication
is stored but not announced, and is not retried later by itself; the next reading carries the news.
That is why the phone is told how often to send.

**What is published.** For `LIVE`: `{mode: "LIVE", latitude, longitude, accuracyM, headingDeg,
speedMps, moving, recordedAt, sessionId}`. For `SPOT`: the spot's coordinates and no GPS-derived
fields (mode `SPOT`). For `HIDDEN`: nothing. When the day ends or sharing becomes hidden, an
`outlet.location.cleared` event removes the position.

### 5.3 What this API does with it

```
 outlet.session.changed ──▶ outlets.session_* columns   (active, id, sharing, spot / anchor)
 outlet.location.updated ─▶ outlets.cur_* columns        (only if recordedAt is newer than the
                                                          current position AND newer than the last
                                                          "cleared")
 outlet.location.cleared ─▶ clears cur_*, remembers when
            │
            ▼
   recompute "where do customers find it": outlets.pos_lat / pos_lng / pos_mode   (indexed)
```

The rules for "where customers find an outlet" are one function (`Outlet.DerivePosition`) that mirrors
how the POS presents it:

- A fixed outlet is at its address.
- A mobile outlet is nowhere unless its day is open.
- With `HIDDEN` it is nowhere.
- With `LIVE` and a position, it is at that position (mode `LIVE`).
- Otherwise, if the day has a place (a saved spot, or the position the day was opened at), it is
  there (mode `SPOT`).

Only the newest reading wins: an older position that arrives late (retries, reordering) is ignored,
and a position older than the moment the day was cleared never brings the vendor back.

This is also why the POS sends the working day's `anchor` (the place the day is held at) even when
the day was opened at a raw position rather than a saved spot. Without it, a vendor sharing "spot
only" would have had no coordinates on the platform side at all.

### 5.4 What customers see

`GET /v1/outlets/nearby?lat=…&lng=…&radius_m=…` finds published outlets by distance using
`earthdistance` and a GiST index on `pos_lat`, `pos_lng`. Each outlet carries:

```json
"location": { "mode": "LIVE", "latitude": -6.2010, "longitude": 106.8001,
              "accuracyM": 8, "moving": true,
              "recordedAt": "2026-11-21T07:10:02Z", "stale": false },
"isOpenNow": true, "acceptingOrders": true
```

Freshness is judged on the customer's request, not stored:

| Age of a live position | Behaviour |
|---|---|
| up to 3 minutes | shown normally |
| over 3 minutes | shown with `stale: true`, so the app says "last seen" |
| over 10 minutes | left out of search results |

Fields that do not apply are left out (a spot has no accuracy or `recordedAt`). Fixed places and
spots never go stale. `isOpenNow` and `acceptingOrders` are computed from the working
day, the weekly hours and the outlet's status, with the same rules as the POS, so a mobile vendor with
no open day is never "open".

The customer app **polls** `nearby` (and `outlets/{id}`) to move the dot. There is no live stream of
positions yet; the server-sent stream in this API is for orders only.

### 5.5 One position's journey, with time

```
t = 0.0 s   vendor phone sends a reading; the POS judges and stores it
t = 0.0 s   POS: ≥15 s since the last publication and moved ≥20 m  → writes the outbox event
t ≤ 5 s     POS worker picks it up and POSTs the signed batch
t + ~50 ms  this API verifies, stores it in the inbox, answers 204
t ≤ 1 s     this API's worker applies it; pos_lat / pos_lng are updated
            → the next GET /v1/outlets/nearby shows the new position
```

In practice a customer sees a moving cart a few seconds behind the vendor, updated at most every
15 seconds, and only when the cart actually moved.

### 5.6 What stays in the POS

The raw trail, the distance travelled each day, per-spot and per-day reports, and the audit flags are
never sent to this API. The POS owner reads them under `/app/terrapos/analytics/streetfood/…`. Raw
trails are deleted after 90 days (`STREETFOOD_LOCATION_RETENTION_DAYS`).

## 6. An order, end to end

### 6.1 Pay at pickup

```
 customer ─POST /v1/checkouts──▶ This API                                POS                  cashier
   Idempotency-Key             1. check the cart against the stored
                                  menu (friendly early errors)
                               2. store the order CREATED (unique per customer + key)
                               3. ─ signed POST /orders ─────────────▶ 4. price it (the only price
                                    {platformOrderId, items,              authority), check the outlet is
                                     expectedTotalMinor, payment}         open, store it PENDING_ACCEPTANCE
                               ◀──────────── pricing, acceptBy ───────  ─ socket / push ───────────▶ new order
                               5. store the POS's answer                                           accepts
 customer ◀── order, PENDING ──                                       ◀──────── accept ───────────────┘
                                                                      6. status ACCEPTED, version 2
                               ◀── event order.status.changed ─────── (outbox)
                               7. apply if version is newer; queue ONE
                                  push per (order, status); wake streams
 customer ◀── push + SSE ─────  …start (stock is consumed), ready, complete (the sale is recorded)
```

- **Idempotency, twice.** The customer's `Idempotency-Key` makes a repeated checkout return the same
  order. The `platformOrderId` makes a repeated submission to the POS return the same POS order. A
  lost answer, a timeout, or a retry can never create two orders.
- **The POS prices; this API never does.** The customer's app may send `expectedTotalMinor`; if the
  price moved, the POS answers `PRICE_CHANGED` with the new total and the customer is asked again.
- **The first submit happens inside the checkout request** so the customer usually gets the POS's
  answer at once. If the POS is slow or down, a worker keeps trying (2 s, 5 s, 15 s, 30 s). After
  2 minutes it asks the POS whether it has the order after all (the answer may have been lost). If
  not, the order fails with `MERCHANT_UNREACHABLE`.
- **Status updates are versioned.** Each POS order carries a version that rises with every change; a
  status is applied only if its version is newer, so late or repeated events change nothing. If an
  event is lost, a worker re-reads open orders from the POS when nothing has been heard about them for
  2 minutes.
- **Cancel.** A customer can cancel only before the merchant accepts. Afterwards only the merchant can.
  If the customer cancels while the order is being sent, the order is taken back at the POS.
- **If the merchant never answers**, the POS expires the order at its `acceptBy` time (5 minutes by
  default) and the customer is told.
- **Limits** keep one account from flooding merchants: 5 open orders, 20 checkouts an hour.

What the POS does on its side: staff see the order at once (sockets and push), stock is consumed when
the order is started, and a sale (`source = STREETFOOD`) is recorded when it is completed, so the
merchant's reports include online orders.

## 7. Online payment

The customer's money goes **straight to the merchant's own gateway account**. The platform never
holds it.

```
 customer ─checkout, payment ONLINE─▶ This API ──signed POST /orders/quote──▶ POS prices it (stores nothing)
                                       │ ◀──────── pricing ────────────────────
                                       │ stores the order UNPAID, 15-minute deadline
                                       │ ── create invoice for  total + 2.5% ─▶ gateway (merchant's account)
 customer ◀── payUrl ──────────────────┤ ◀──────────── page ───────────────────
 customer pays on the gateway's page ──────────────────────────────────────────▶ gateway
                                       │ ◀── callback (authenticated, then IGNORED for its content)
                                       │ ── ask the gateway for the payment's real state ─▶
                                       │ ◀──────────── PAID ───────────────────
                                       │ order UNPAID → CREATED; then, as in 6.1 but marked PREPAID:
                                       │ ── signed POST /orders  payment: PREPAID, reference ─▶ POS
```

Points that matter:

- **The POS prices before anyone is charged.** The quote runs the same checks and prices as a real
  order, so a customer is never asked to pay for an order the merchant could not take.
- **A callback is a hint, not a fact.** It is checked (Xendit's callback token, Midtrans's signature),
  and then the API asks the gateway for the payment's state and uses that answer. A forged or stale
  callback changes nothing. A poller also asks about open payments, so a lost callback only delays.
- **A paid order goes to the POS with the price the customer paid for.** If prices moved in between,
  the POS refuses and the payment is refunded.
- **Failure leaves the order `UNPAID` and retryable** until the deadline, then it expires and the
  payment page is closed. The customer is told at each step.
- **Refunds are automatic** when a paid order will not happen: rejected, cancelled or expired by the
  merchant, refused by the POS, never delivered, paid after it was closed, or paid twice. A stable key
  stops a refund being paid out twice. If a gateway cannot refund (Midtrans cannot refund bank
  transfers by API), the payment is marked and the customer is told the merchant will return the
  money by hand.
- The platform's fee is added on top for the customer (2.5%) and owed by the merchant (2.5%). The
  merchant's sale in the POS records exactly the order total; the fee is never sent to the POS as part
  of the sale.

## 8. How the platform's fees are collected

Fees are recorded here, and collected by the POS as part of the merchant's **automatic monthly
subscription charge**.

```
 order completed ──▶ this API's ledger:  online order: customer fee + merchant fee
                                         pay-at-pickup order: merchant fee (rate fixed when ordered)

 a renewal is due (POS, ~24 h before the period ends, card on file):
   POS ─signed POST /v1/integrations/pos/fees/reserve {billingRef}─▶ this API sets the open fees aside
   POS ◀── lines and total ─────────────────────────────────────── for that billing period
   POS creates the renewal order: plan price + fees, and charges the saved card ONCE (Midtrans)
   payment recorded ──▶ in the SAME database transaction the POS writes a billing.fees.settled event
   POS worker ─── event ───▶ this API marks exactly those entries collected, against the invoice number
```

- **Reserve is repeatable.** The same period always gets the same entries and total, however often
  it is asked and whatever accrues meanwhile; a period already collected gets nothing more. Entries
  reserved but never collected are released after 21 days.
- **Fees never cost a merchant their subscription.** They ride on the first attempt only. If that
  card charge is declined, the second attempt charges the plan alone and the fees carry over. If this
  API cannot be reached, the plan is charged alone.
- **The "paid" report is retried for 14 days** (other events: 24 hours), because losing it would let
  the same fees be charged again.
- The invoice lists the plan and the fee lines apart and totals to what was charged. The owner can
  see what is owed at `GET /app/terrapos/integrations/streetfood/fees` (the POS asks this API with a
  signed `GET /v1/integrations/pos/fees/pending`, which reserves nothing).
- A business with several owners is billed to one account, the one the POS already uses to decide its
  employees' plan.
- Merchants without an automatic-renewal card are not charged fees by this mechanism; the operator
  settles them by hand (`POST /v1/admin/merchants/{id}/fees/settle`).

## 9. What can go wrong and how it heals

| What happens | What the customer or merchant notices | How it heals |
|---|---|---|
| This API is down for an hour | Nothing on the POS. Customers cannot use the app | The POS keeps events in its outbox and retries for 24 hours. On recovery the events arrive, and reconciliation (every 15 min) repairs anything missed. Positions are not queued: only the newest is kept |
| The POS is down | Customers still see outlets (the last known copy). New orders fail with `MERCHANT_UNREACHABLE` after 2 minutes | Orders are retried for 2 minutes. Reconciliation resumes when the POS is back |
| An event is lost or arrives twice or out of order | Nothing | Duplicates are ignored, older changes are ignored, and reconciliation re-reads the truth |
| The vendor's phone is offline | The dot freezes, then shows "last seen" after 3 minutes and disappears after 10 | Queued readings are sent when the phone is back (up to 24 hours old are accepted); only readings newer than the current position move the dot |
| The vendor forgets to close the day | The cart stays on the map | The POS closes the day after 18 hours and publishes the clear |
| A spoofed or wild GPS reading | Nothing | Flagged readings are kept for the owner but never move the dot |
| A callback from a gateway is lost | The payment page shows paid, the order moves on a little late | A poller asks the gateway about open payments |
| A paid order cannot go through | The customer is told, and the money returns | Automatic refund; if the gateway cannot, the merchant is told to refund by hand |
| The clock of one machine is wrong | Requests are refused (`401`) when more than 5 minutes off | Fix the clock. Business times (positions, orders) always use the sender's own timestamps and versions |
| The platform's copy is wrong for any reason | Wrong menu or hours | Never edit it by hand: reconciliation rebuilds it from the POS |

## 10. Where to find it in the code

| Concern | POS (`ansilum-be/src/domain/streetfood/`) | This API |
|---|---|---|
| Signing | `streetfood-signing.ts`, `streetfood-signature.guard.ts` | `pkg/signing`, `internal/usecase/integration` (`authenticate`) |
| Link and registration | `streetfood-link.service.ts` | `internal/usecase/merchant` |
| Outbox and delivery | `streetfood-outbox.service.ts`, `streetfood-worker.service.ts` | |
| Receiving events | | `internal/usecase/integration` (`Receive`, `Process`, `apply`) |
| Reconciliation | `streetfood-platform.controller.ts` (the reads) | `internal/usecase/integration` (`ReconcileLink`) |
| Working day and GPS | `streetfood-session.service.ts`, `streetfood-geo.ts`, `streetfood-vendor.controller.ts` | `internal/entity/outlet.go` (`DerivePosition`, freshness), `internal/repository/persistent/outlets.go` |
| Search | | `internal/usecase/discovery`, `internal/repository/persistent/discovery.go` |
| Orders | `streetfood-orders.service.ts`, `streetfood-orders.controller.ts`, `streetfood-staff.controller.ts` | `internal/usecase/ordering`, `internal/repository/webapi/pos` |
| Payments and refunds | | `internal/usecase/payment`, `internal/repository/webapi/gateway` |
| Fee collection | `streetfood-billing.service.ts`, `../subscription/subscription.service.ts`, `../subscription/platform-fees.ts` | `internal/repository/persistent/fees.go`, `internal/usecase/payment` |

Each rule above has a test that fails if the rule is broken; the Go tests include a replay of real POS
events captured in `internal/testutil/testdata/pos/`.
