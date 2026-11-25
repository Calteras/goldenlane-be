# The two apps and their APIs

StreetFood OS has **two mobile apps** for two different people. They use **different backends**, so each
has its own API guide. Pick yours:

| | **Goldenlane** (customer app) | **Ansilum** (vendor / POS app) |
|---|---|---|
| **Who uses it** | A person who wants to buy food | The vendor, the cashier and the business owner who sell it |
| **What it does** | Find vendors, follow them on the map, order, pay, track the order, review | Open and close the working day, broadcast the position, take and fulfil orders, set the outlet up |
| **Backend** | The customer API (this repository) | The POS backend (`ansilum-be`) |
| **Staging address** | `https://staging-goldlane.calterras.com` | `https://api.ansilum.com/app/terrapos` |
| **Path prefix** | `/v1/…` | `/streetfood-vendor/…`, `/streetfood-orders/…`, `/integrations/streetfood/…`, `/analytics/streetfood/…` |
| **Sign-in** | Firebase Authentication (phone OTP). The Firebase ID token goes in `Authorization: Bearer` | The Ansilum session: `Authorization: Bearer <JWT>` plus `x-tenant-id` |
| **Position** | Reads it: `GET /v1/outlets/nearby` (polled every 15 s) | **Writes** it: `POST /streetfood-vendor/location/pings` (every `nextIntervalSeconds`) |
| **Orders** | Places them (`POST /v1/checkouts`), pays, follows the status, cancels before acceptance | Receives them, accepts or rejects, starts, marks ready, completes, cancels after acceptance |
| **Real time** | Order stream (server-sent events) and push (FCM) | Socket events `streetfood-order:placed`, `streetfood-order:updated`, `streetfood-session:changed` |
| **Money** | Pays online (merchant's own Xendit or Midtrans) or at pickup. Sees the platform fee before paying | Sees pay-online orders as already paid, collects cash for pay-at-pickup, owes the platform fee |
| **Feature switch** | none | `STREETFOOD_ENABLED=true` on the POS, otherwise every route answers 404 |
| **Guide** | [`MOBILE_API_CUSTOMER.md`](MOBILE_API_CUSTOMER.md) | [`MOBILE_API_VENDOR.md`](MOBILE_API_VENDOR.md) |
| **Contract** | [`api/openapi.yaml`](../api/openapi.yaml) | the POS's Swagger |

The story of how the two meet is in [`USER_JOURNEY.md`](USER_JOURNEY.md). The technical exchange between
the backends is in [`HOW_IT_WORKS.md`](HOW_IT_WORKS.md).

## The same moment, seen by both apps

| Moment | Vendor (Ansilum) calls | Customer (Goldenlane) calls |
|---|---|---|
| The cart opens for the day | `POST /streetfood-vendor/session/start` | next `GET /v1/outlets/nearby` lists it |
| The cart moves | `POST /streetfood-vendor/location/pings` | `GET /v1/outlets/nearby` or `GET /v1/outlets/{id}` (poll, 15 s) |
| The cart goes private | `PUT /streetfood-vendor/session/sharing` `HIDDEN` | the cart disappears from search |
| A customer orders | receives `streetfood-order:placed`; `GET /streetfood-orders?view=waiting` | `POST /v1/checkouts` |
| The vendor accepts | `POST /streetfood-orders/{id}/accept` | status `ACCEPTED`, push, stream event |
| The food is ready | `POST /streetfood-orders/{id}/ready` | status `READY`, push, stream event |
| The order is handed over | `POST /streetfood-orders/{id}/complete` | status `COMPLETED`, can `POST /v1/orders/{id}/review` |
| The day ends | `POST /streetfood-vendor/session/end` | the cart leaves the map |

## What each app must never do

| Customer app | Vendor app |
|---|---|
| Never call the POS backend or hold its address, keys or secrets | Never call the customer API or hold the link secret |
| Never compute the price of an order: the POS decides it | Never publish or edit positions by hand: the POS judges every reading |
| Never decide an online order is paid from the return link: read the order status | Never ignore `nextIntervalSeconds` and `acceptBy` |
| Never send another customer's data or call `/v1/admin` | Never call the signed platform endpoints (`integrations/streetfood/orders…`): server to server only |

## Where the server settings meet

Both apps depend on the same link between the backends:

| Setting | On | Must be |
|---|---|---|
| `POS_ALLOWED_HOSTS` | customer API | `api.ansilum.com` |
| `posBaseUrl` when registering a merchant | customer API (operator call) | `https://api.ansilum.com` (no `/app/terrapos`) |
| `platformUrl` when linking | POS (owner call) | `https://staging-goldlane.calterras.com` |
| `STREETFOOD_ALLOWED_PLATFORM_HOSTS` | POS | `staging-goldlane.calterras.com` |
| `FIREBASE_PROJECT_ID` | customer API | the Firebase project of the **Goldenlane** app (the Ansilum app does not use it) |
| `HTTP_CORS_ALLOW_ORIGINS` | customer API | browser origins only (native apps need none) |
