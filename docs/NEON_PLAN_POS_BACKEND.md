# Plan: let the POS backend's Neon database sleep (`ansilum-be`)

**Status: implemented in `ansilum-be`, November 2026, except WP6, WP7 and WP8 (below).** The reasoning, prices
and measurements behind it are in [`NEON_COST_STRATEGY.md`](NEON_COST_STRATEGY.md); the same work is done in the
customer API (its Status section), and this plan applies the same ideas to NestJS and Prisma.

## What was done

Measured on a throwaway Postgres 15 with the real app running (StreetFood and delivery workers on, nobody using it):

| | Before (`POS_BACKGROUND_MODE=poll`) | After (`awake`, the default) |
|---|---|---|
| Database transactions per minute, idle | about 400 | **0** statements in 60 s after start-up |

| Work package | Result |
|---|---|
| WP0 | `application_name=ansilum-be` is added to `DATABASE_URL` by `PrismaService` (seen in `pg_stat_activity`). `DIRECT_URL` needs `?application_name=ansilum-be-migrate` added by hand (documented in `docker/env/.secrets.env.example`). |
| WP1 | `releaseExpiredPromotions` (every 5 min) and `releaseForgottenTables` (hourly) run while other traffic has the database awake, at most every 5 and 60 minutes, and nightly. They are not purely nightly because a reserved promotion holds usage and budget. `expireTrials` is nightly only. |
| WP2 | `src/app/schedule/adaptive-loop.ts` (one timer that only moves earlier; ignores its own statements; backs off on rows that stay due), `work-signals.ts`, `background-mode.ts`. Unit tests in `adaptive-loop.spec.ts`. |
| WP3 | The StreetFood worker runs on a wake after commit (outbox write, new online order), at the next due retry, lease or order deadline, while awake (at most every `changeScanSeconds`), and nightly. It repeats at once only when an outbox batch was full. |
| WP4 | Same for the delivery worker. Wake after commit on webhook intake, operation create and `changed()` (every command is created with its outbox row). Retention runs nightly. |
| WP5 | `PosFulfillmentService.available()` keeps `true` for the life of the process. The POS app polls `GET /health` (no database) every 15 s and runs the full `capabilities` probe on start, resume, account change, after any failure, and while data is pending. |

Rollback: set `POS_BACKGROUND_MODE=poll` and the old fixed timers return. No migration is involved.

Not done, on purpose:

- **WP6** (tenant guard cache): it only affects requests from real use, which wake the database anyway, and it changes authorization semantics.
- **WP7** (vendor positions off Postgres): the plan says to do it last and after measuring the real bill.
- **WP8** (Redis): needed only with several `ansilum-be` instances. Wake signals are in-process, so a second instance still falls back to the awake and nightly runs.
- **Better Auth `secondaryStorage` session cache** (part of WP5): it needs `storeSessionInDatabase` and `verification.storeInDatabase` set, or sessions are lost on restart, and it is not needed once the probe stops hitting the database.

Not measured: the real Neon bill, and a long run with real traffic. The Flutter change was checked with unit tests only, not on a device.

## Contents

1. [Goal and how we will know](#1-goal-and-how-we-will-know)
2. [What keeps the database awake today](#2-what-keeps-the-database-awake-today)
3. [The approach, in one paragraph](#3-the-approach-in-one-paragraph)
4. [Work packages](#4-work-packages)
5. [Order, effort and what each step saves](#5-order-effort-and-what-each-step-saves)
6. [Rollout and rollback](#6-rollout-and-rollback)
7. [Tests](#7-tests)
8. [Decisions needed before starting](#8-decisions-needed-before-starting)
9. [What not to do](#9-what-not-to-do)

---

## 1. Goal and how we will know

**Goal:** with nobody using the apps, the POS backend sends the database no statements except one
nightly job, so Neon suspends after 5 idle minutes. While people use the apps, the database is touched
only by real work (a sale, a login, a menu edit), not by a timer or by the apps' own polling.

| Check | Today (expected, to be measured first) | Target |
|---|---|---|
| Statements per minute, nobody using the apps | a few (a `*/5` job, a `*/15` job, workers if their flags are on) | none, except the nightly run |
| Longest awake streak at night | the whole night | under 10 minutes |
| A POS device left open on the sales screen | 4 probes a minute, each reading the session | 0 statements per probe |
| StreetFood worker flag on, no orders | a statement every 5 s | none |
| Neon "compute active time", 7 days | measure (step 0) | under 40% on a normal week, near 0 at night |

---

## 2. What keeps the database awake today

Found by reading the code in November 2026. Confirm each with the measurements in section 4, WP0.

| # | Source | File | Interval | Effect |
|---|---|---|---|---|
| 1 | `releaseExpiredPromotions` | `src/domain/transaction/reservation.cron.ts` | `*/5 * * * *` | One query every 5 minutes: **by itself keeps the database on 24/7** |
| 2 | `expireTrials` | `src/domain/trust/trust.cron.ts` | `*/15 * * * *` | A wake every 15 minutes (a third of the month) |
| 3 | `releaseForgottenTables` | `src/domain/transaction/reservation.cron.ts` | hourly at :15 | A wake every hour (8%) |
| 4 | StreetFood worker tick | `src/domain/streetfood/streetfood-worker.service.ts` | every 5 s when `STREETFOOD_WORKERS_ENABLED=true`: `expireDue`, `closeStale`, `dispatch` | Constant floor |
| 5 | StreetFood change scan | same file, `scan()` | every `STREETFOOD_CHANGE_SCAN_SECONDS` (15 s), per published outlet: builds its state and catalog and compares fingerprints | Constant floor, and the heaviest of the four |
| 6 | StreetFood hourly scrub and purge | same file | hourly | A wake every hour |
| 7 | Delivery worker | `src/domain/food-delivery/delivery-worker.service.ts` | every 1 s when its worker flag is on: claim, commands, operations, notifications | Constant floor |
| 8 | POS app probe | `lib/src/presentation/connectivity/pos_connectivity_controller.dart` (Flutter) to `src/domain/auth/pos/sync/pos-sync.controller.ts` | every 15 s per open POS device | Each probe reads the session (Better Auth `cookieCache` is off, about 2 statements) and checks whether the fulfillment tables exist (`fulfillment.available()`): **every open device keeps the database awake for the whole business day** |
| 9 | Per request | `TenantGuard`, Better Auth | on every authenticated request | Membership, subscription and session lookups each time |
| 10 | Vendor pings | `streetfood-session.service.ts`, `ingestPings` | one call per vendor every 15 to 120 s | Real work, but frequent: inserts, a live-position upsert, session counters, maybe an outbox row |

Already good, do not redo: the email worker sleeps until work is due and sweeps while the database is
awake (`src/app/email/email-schedule.ts`), and the nightly jobs run at 00:00 Asia/Jakarta
(`src/app/schedule/nightly.ts`).

---

## 3. The approach, in one paragraph

Replace "check every N seconds" with **"run when work is created, when a known due time arrives, and
otherwise at the night (or while the database is already awake)"**. The POS backend already has the
pieces: `afterCommit()` (`src/app/prisma/after-commit.ts`), the Prisma `query` event, and
`shouldSweepWhileAwake` (email worker). Work packages 2 to 4 reuse them for the other workers. Then stop
the clients' own polling from reaching the database (WP5, WP6) with a session and guard cache, and, last,
take the vendor position stream off Postgres (WP7). Redis is optional and only needed when the backend
runs more than one instance (WP8).

---

## 4. Work packages

Each package says what to change, how, how to check it, and the risk.

### WP0. Measure first (hours, no code)

1. Add `application_name=ansilum-be` to the Prisma connection string (`DATABASE_URL`). Check it appears in
   `pg_stat_activity`. Do the same for `DIRECT_URL` with `application_name=ansilum-be-migrate`.
2. Record Neon's Monitoring for 7 days: "compute active time" and "CU over time".
3. In the Neon SQL editor, after a few hours of uptime:

```sql
SELECT calls, round(total_exec_time::numeric / 1000, 1) AS total_seconds,
       left(regexp_replace(query, '\s+', ' ', 'g'), 110) AS query
FROM pg_stat_statements ORDER BY calls DESC LIMIT 20;
```

4. Write the numbers at the top of this file. Everything below is judged against them.

### WP1. The three crons (a day)

| Job | Why it exists | Change |
|---|---|---|
| `releaseExpiredPromotions` (`*/5`) | A promotion reserved on an open sale expires; the row stays `RESERVED` until released | First check where a reserved promotion's expiry matters: if usage counting and the checkout already ignore a reservation past `expiresAt`, run the release **nightly** (and while awake, WP2). If something blocks until the row is released, release those rows **on read** at that point (the same read already loads them), and run the job nightly as the record |
| `expireTrials` (`*/15`) | Records the end of a trial. The code comment says entitlements already stop at `expiresAt` | Move to the nightly schedule. Entitlement checks keep using `expiresAt`, so nothing user-visible changes. Confirm with one test |
| `releaseForgottenTables` (hourly :15) | Frees a table nobody cleared after `FORGOTTEN_TABLE_HOURS` | Nightly, plus "while awake" (WP2). A table is freed at the next night at the latest, or sooner when anyone else is using the database |

How: change the `@Cron(...)` expressions to `NIGHTLY_CRON` with `{ timeZone: NIGHTLY_TIME_ZONE }` (as the
other jobs do). **Risk:** a delay of up to a day in releasing a promotion or a table. Decision 1 in
section 8.

### WP2. A reusable "run while awake" helper (a day)

Extract the idea in `shouldSweepWhileAwake` and the email worker's use of the Prisma `query` event into
`src/app/schedule/while-awake.ts`:

- `whileAwake(name, run, { gapMs })` runs `run()` when another statement just ran (the database is
  already awake) and the last run was at least `gapMs` ago, never because of its own statements.
- `wakeAfterCommit(name)`: a hook the workers call from `afterCommit()` when they enqueue work, so work
  created by this process is picked up within a second.
- `nextDue(name, query)`: one query for the earliest scheduled item, so a worker sleeps until a retry or
  deadline instead of polling.

Reuse in WP1 (the crons), WP3 and WP4. **Risk:** a bug here silently delays work; the nightly run is the
net. Unit-test it as `email-schedule.ts` is.

### WP3. StreetFood worker (2 to 3 days)

Split the 5-second `tick()` into jobs with their own triggers:

| Job | Today | New trigger |
|---|---|---|
| `orders.expireDue()` | every 5 s | At order creation, schedule a timer for its `acceptBy` (in memory, restored at start from `SELECT min("acceptBy") … WHERE status = 'PENDING_ACCEPTANCE'`). Plus nightly. An order is also expired **on read** when it is loaded after `acceptBy` (the guard in `expireDue` already compares against now) |
| `sessions.closeStale()` | every 5 s | Nightly, and when a session is read. A day open 18 hours is rare; one night of delay is fine |
| `dispatch()` (outbox to the customer API) | every 5 s | After commit (`afterCommit` in `StreetfoodOutboxService` when it writes a row), plus `nextDue` for retry backoffs, plus the awake sweep |
| `scan()` (menu, hours and outlet fingerprints) | every 15 s per outlet | Run from the code that changes an outlet, its hours or its menu (a call next to where the outbox row would be written, after commit), plus a nightly full scan as the net. Keep the fingerprint comparison, so repeated triggers publish once. If touching those services is unwanted now, run the scan "while awake" with a 1 to 5 minute gap |
| `scrubPersonalData()`, `purgePings()` | hourly | Nightly |

**Risk:** order acceptance windows (default 5 minutes) must still expire on time for the customer. The
`acceptBy` timer plus the on-read check covers it; test it. Rollback: the flag in section 6.

### WP4. Delivery worker (1 to 2 days)

Same pattern as WP3 for `DeliveryWorker.tick()` (claim, commands, operations, notifications): wake after
commit when a webhook row, command or notification is written (`afterCommit`), `nextDue` for retries,
awake sweep, nightly net. It is off unless `FOOD_DELIVERY_WORKERS_ENABLED=true`, so do it when that is
turned on.

### WP5. The POS probe and the session (2 to 4 days)

Two parts. Do both.

1. **Server:** make `GET /auth/pos/sync/capabilities` cost no statements.
   - Cache `fulfillment.available()` for the life of the process once it is `true`.
   - Cache the session behind `PosSessionGuard`: use Better Auth's `secondaryStorage` (version 1.7.10
     supports it) with an in-process store for one instance (or Redis, WP8), instead of
     `cookieCache` (the audit flagged a cookie cache as a security trade-off: revocation would lag). With a storage
     the session is deleted from it on sign-out and revoke, so **revocation stays immediate**.
2. **Flutter (`ansilum-flutter`):** probe a database-free route (`/app/terrapos/health`, already there)
   every 15 seconds, and call `capabilities` only at start, on resume and after a failed sync. The
   connectivity answer ("can I reach the POS?") is the same.

Result: an open POS device sends no statements while idle. **Risk:** session cache and revocation
(decision 3 in section 8).

### WP6. Tenant and subscription checks (1 to 2 days)

`TenantGuard` runs membership and subscription lookups on every authenticated request. Cache them per
`(user, tenant)` for 30 to 60 seconds in process, invalidated by the subscription and membership events
the code already emits (`SUBSCRIPTION_EVENTS`), and by the TTL. **Risk:** a removed member or a lapsed
plan keeps access for up to the TTL (decision 4).

### WP7. Vendor positions off Postgres (1 to 2 weeks, the largest change)

`ingestPings` writes to Postgres for every batch. Split the data by what it is:

- **Live position and distance counters:** keep the newest trusted reading and the session counters in
  memory (Redis with a TTL of 10 minutes when there is more than one instance). Send
  `outlet.location.updated` to the customer API directly from there. The contract already says positions
  are "only the newest is kept", never queued, so they do not need an outbox row.
- **GPS trail rows** (owner's route reports): append to a **SQLite file** on a persistent volume and
  flush to `outlet_location_pings` every 15 minutes, at `session/end` and on shutdown.
- **Session start, end, relocate, sharing:** stay transactional in Postgres. They are rare and part of the
  audit trail.

What a crash can lose: up to one flush interval of trail rows (never money, orders or the working day).
Decision 6. Do this after WP1 to WP6 and re-measure first: during a vendor's working day the database is
awake anyway because of the orders, so the gain is smaller than the idle fixes.

### WP8. Redis, only if there is more than one instance (1 to 2 days)

Same Redis as the customer API can serve both (different database index, or a key prefix). Uses in the
POS backend: Better Auth `secondaryStorage`, cross-instance wake-ups for WP2 to WP4, and the live
position of WP7. One instance does not need it: use process memory. Setup of the shared Redis is in the
customer API README, section "Redis".

---

## 5. Order, effort and what each step saves

| Order | Package | Effort | What it removes | Depends on |
|---|---|---|---|---|
| 1 | WP0 measure | hours | nothing, it sets the baseline | none |
| 2 | WP1 crons | 1 day | The 5-minute, 15-minute and hourly wake-ups. **This alone lets Neon suspend at night** if no worker flag is on | decision 1 |
| 3 | WP2 helper | 1 day | nothing yet, it is the tool | none |
| 4 | WP3 StreetFood worker | 2 to 3 days | The 5 s and 15 s floor while the flag is on | WP2 |
| 5 | WP5 probe and session | 2 to 4 days | The business-day floor from open POS devices | decision 3 |
| 6 | WP6 tenant checks | 1 to 2 days | Per-request lookups | decision 4 |
| 7 | WP4 delivery worker | 1 to 2 days | The 1 s floor, if that flag is on | WP2 |
| 8 | WP7 positions | 1 to 2 weeks | The vendor's recurring writes | measure after 2 to 6 |
| 9 | WP8 Redis | 1 to 2 days | Needed only with several instances | none |

Expected result, at the 0.25 CU minimum on the Launch plan ($0.106 per CU-hour, compute only; see
`NEON_COST_STRATEGY.md` section 8):

| After | Database awake | Compute per month |
|---|---|---|
| Today, a `*/5` job | around the clock | about 182 CU-hours, **about $19** |
| WP1 only | the business day, plus a wake a night | roughly 75 CU-hours, about $8 |
| WP1 to WP6 | only when someone really does something | roughly 25 to 50 CU-hours, about $2.50 to $5 |
| plus WP7 | writes batched every 15 minutes | roughly 15 to 40 CU-hours, about $1.50 to $4 |

These are estimates for planning, not a quote. Re-measure after each step.

---

## 6. Rollout and rollback

- Ship each package behind a setting so it can be switched back without a deploy of code:
  `POS_BACKGROUND_MODE=awake` (new behaviour) or `poll` (the current timers). Default `awake` on staging
  first, then production after a week of comparing Neon's active time.
- One package per release, so a regression points at one change.
- Keep the old `@Interval` and `@Cron` bodies reachable under `poll` for one release, then remove them.
- Watch after each release: the statements-per-minute query (WP0), Neon active time, and these business
  signals: a StreetFood order that was not accepted in time becomes `EXPIRED` within a minute of
  `acceptBy`; a position event reaches the customer API within 20 seconds; promotions and tables are
  released by the next morning.

---

## 7. Tests

Add with each package (unit tests where there is logic, one integration test per trigger):

- **WP2:** the helper never runs because of its own statements, runs once per burst, and honours the
  gap (model it on `email-schedule.spec.ts`).
- **WP3:** an order expires at `acceptBy` with no tick running; an order read after `acceptBy` is expired;
  an outbox row written in a transaction is dispatched after commit and not after rollback; a menu edit
  publishes `catalog.changed` once.
- **WP5:** `capabilities` with a warm cache issues no statement (count them with the Prisma `query`
  event); a revoked session is refused on the next call.
- **WP6:** a removed member loses access within the TTL, and at once on the removal event.
- **WP7:** a crash between flushes loses at most the buffered trail rows; positions still reach the customer
  API; `session/end` flushes.

---

## 8. Decisions needed before starting

1. How late may a **promotion reservation** or a **forgotten table** be released? (Proposed: next morning, or sooner while the database is awake.)
2. Is a cold start of a few hundred milliseconds on the first request of the day acceptable?
3. How long may a **revoked POS session** stay usable? (Proposed: zero, with Better Auth `secondaryStorage`; the cache TTL with a process-local cache.)
4. How long may a **removed member or a lapsed plan** keep access? (Proposed: 60 seconds, and at once on the event.)
5. Are the StreetFood and delivery worker flags on in production now? It decides how urgent WP3 and WP4 are.
6. How much GPS trail may be lost in a crash (WP7)? (Proposed: 15 minutes of trail rows.)
7. Will the POS backend run more than one instance soon? (Decides WP8.)
8. Which environments get the `awake` mode first? (Proposed: staging, then production after a week.)

---

## 9. What not to do

- Do not move orders, sales, payments, subscriptions or the outbox rows to Redis or SQLite. They stay in
  Postgres, written in the same transaction as today.
- Do not enable Neon logical replication or CDC to feed a stream: a connected subscriber keeps the compute
  active ([Neon docs](https://neon.com/docs/guides/logical-replication-neon)).
- Do not shorten `STREETFOOD_CHANGE_SCAN_SECONDS` or add more timers "to be safe". A safety net belongs
  at the night, or while the database is already awake.
- Do not cache anything that grants access without a TTL and an invalidation.
- Do not turn the Neon suspend timeout off: the savings in this plan depend on it.
