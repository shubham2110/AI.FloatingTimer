# HTTP Request Audit

Audit scope: the Windows Timer Manager / overlay application, its embedded `ui.html`, peer billing and discovery traffic, and the separate launcher update check. The `extensions/lgtv` module was searched for outbound HTTP clients and does not issue GET or POST requests. All browser `fetch` calls use the single embedded-UI `request()` checker; all application Go `http.Client.Do` calls use `internal/httpcheck.Do`.

## Request inventory

**Status meaning:** `verified` means exercised by an automated test or a direct safe probe; `broken` means an observed live failure; `unverified` means existence was confirmed from source but end-to-end live behavior was not established. A registered route alone is not treated as proof that an installed peer supports it.

### Browser-originated requests

| Request ID | Method | Frontend caller | Request URL / base source | Backend handler | Purpose / expected response | Error handling / timeout | Status |
|---|---|---|---|---|---|---|---|
| FE-01 | GET | `loadPCList` | Same-origin `/api/pcs` | `pcsHandler` | JSON array of saved systems | Central JSON/content-type validation; 8 s default; surfaced in dashboard | Verified by UI tests |
| FE-02 | GET | `fetchHostStatus` | `{PC.address}/status`; address comes from saved/discovered PC registry | `registerTimerCommands` status handler in `main.go` or `onbehalf.go` | Timer status JSON | 4 s call-site abort; checker records timeout/network/HTTP/format errors; offline state shown | Verified by UI tests; live per-device availability varies |
| FE-03 | POST | `quick`, `toggleTimer`, `sendSpecialCommand` via `pcRequest` | `{PC.address}/{action}`; action is timer command (`play`, `pause`, `reset`, `dismiss`, `show`, `hide`, `tweak`, or `special`) | `registerTimerCommands`, `specialCommandHandler`, or `billingSessionLink` handler | JSON timer/command result | 4 s PC-request abort; request ID and PC ID attached; duplicate timer/special submissions guarded by existing per-PC pending state | Verified by UI tests; not comprehensively live-verified across all four devices |
| FE-04 | POST | Inline `startSessionForPC` | Same-origin `/api/sessions` | `sessionsHandler` → `startBillingSessionForDashboard` | Created open session JSON, HTTP 201 | Existing per-PC pending guard; no retry; request validation and mismatch checks; default 15 s | Automated start/UI tests pass; remote owning-PC start remains broken where peer route is absent |
| FE-05 | POST | `extendTimer` / Quick Add | Same-origin `/api/sessions/extend` | `extendSessionHandler` → `extendSessionForDashboard` | Extension result/session JSON | Existing per-PC pending guard; stable request ID makes extension idempotent; no automatic retry; default 15 s | Automated extension tests pass; PC-4 peer forwarding is broken live |
| FE-06 | POST | `distributeSystems` | Same-origin `/api/discovery/distribute` | `distributeHandler` → `distributePeers` | Per-peer distribution result JSON | 25 s checker timeout; errors returned per peer; no retry | Automated handler/UI coverage exists; live fleet distribution unverified |
| FE-07 | POST | `runDiscovery` | Same-origin `/api/discovery/run` | Discovery handler in `registerDiscoveryHandlers` | HTTP 202 `{status:"started"}` | 15 s default; duplicate scan gets 409 and is displayed | Source path verified; end-to-end live scan not covered by this task |
| FE-08 | GET | `refreshDiscoveryStatus` | Same-origin `/api/discovery/status` | Discovery handler in `registerDiscoveryHandlers` | JSON running/last scan state | 8 s default; visible error state | Source path verified |
| FE-09 | GET | `loadBillingReport` | Same-origin `/api/billing/report?date=...` | `billingReportHandler` | Consolidated report JSON | 8 s default; date is non-sensitive; errors displayed | Existing report tests pass; live multi-device completeness not verified here |
| FE-10 | POST | `syncBillingNow` | Same-origin `/api/billing/sync` | `billingSyncHandler` | Per-device sync status JSON | 25 s checker timeout, below the server's 30 s write timeout; individual peer failures remain visible; no retry in browser | Sync tests pass; actual remote peer route is broken as noted below |
| FE-11 | GET | `loadOpenBillingSessions`, `loadBilling`, `loadHistory` | Same-origin `/api/sessions` with optional status/search/payment filters | `sessionsHandler` | Session JSON array | 8 s default; errors shown in billing/history UI | Existing billing tests pass; live PC-4 history retrieval not separately exercised here |
| FE-12 | GET | `loadBilling` | Same-origin `/api/pricing` | Pricing handler in `registerBillingHandlers` | Pricing catalog JSON | 8 s default; errors shown | Source and billing tests verify response shape |
| FE-13 | GET | `loadBilling` | Same-origin `/api/billing/summary` | Summary handler in `registerBillingHandlers` | Billing status and summary JSON | 8 s default; errors shown | Source verified; PC-4 live summary returned HTTP 200 with billing disabled in prior probe |
| FE-14 | POST | `sessionAction` (`close`, `pay`, or other supported action) | Same-origin `/api/sessions/{escaped-session-id}/{action}` | `sessionActionHandler` → `updateSessionForDashboard` | Updated session JSON | No automatic retry; error returned to caller; request ID retained; 15 s default | Local route covered by billing tests; PC-4 remote close route is broken live |
| FE-15 | POST | `startBillingSession` (legacy/manual dialog) | Same-origin `/api/sessions` | `sessionsHandler` | Created open session JSON | Button disabled while pending; no automatic retry; 15 s default | Automated flow tests exist; PC-4 live route is broken |
| FE-16 | PUT | `setPCActive` | Same-origin `/api/pcs/{id}/active` | `pcHandler` | Updated PC JSON | 15 s default; error displayed | Source verified; outside GET/POST inventory |
| FE-17 | POST on create; PUT on edit | `savePC` | Same-origin `/api/pcs` on create; `/api/pcs/{id}` on edit | `pcsHandler` / `pcHandler` | Created/updated PC JSON | 15 s default; no automatic retry | Source verified |

All other frontend API requests found by search route through the same `request()` helper. The checker assigns a unique request ID, sends `X-Request-ID`, attaches `X-Device-ID` when known, records host/path/method/feature/device/status/duration, retains only the most recent 100 entries, and never records request bodies or response bodies. On localhost only, `?requestMonitor=1` enables structured browser-console records; `globalThis.__requestMonitor` exposes totals, success/failure counts, 404/5xx counts, average duration, recent records, and filtering by method/device/error/status.

### Go-originated requests

| Request ID | Method | Go caller | Request URL / base source | Remote handler | Purpose / response validation | Timeout / errors | Status |
|---|---|---|---|---|---|---|---|
| GO-01 | GET | `uiNodeReachable` | `http://{host}:{uiPort}/api/node` | `registerDiscoveryHandlers` node route | JSON node identity; only HTTP 200 counts reachable | Discovery context plus shared 4 s HTTP client; failures mean unreachable | Verified by safe peer probes |
| GO-02 | GET | `probeTimer` | `http://{host}:{timerPort}/identity` | Timer `/identity` handler | JSON identity validated for service and ports | Discovery context plus shared 4 s client | Source validation verified; per-host live result varies |
| GO-03 | GET | `pullPeersURL` / `pullPeersFromPC` | `peerEndpoint`: `http://{host}:{uiPort}/api/discovery/peers` | `discoveryPeersHandler` | JSON registered-PC list merged by stable registry rules | Shared 4 s client; read/decode failures do not merge data | Source verified; live fleet not fully checked |
| GO-04 | POST | `distributePeers` | `peerEndpoint`: `http://{host}:{uiPort}/api/discovery/peers` | `discoveryPeersHandler` | JSON PC list; HTTP 200 required | Request context bounded to 20 s; per-peer errors returned | Automated paths covered; live unverified |
| GO-05 | GET | `getTimerStatus` | `{PC.address}/status` | Timer or on-behalf status handler | JSON `TimerStatus`, including valid `running` or `paused` state | Shared 4 s client; malformed/missing state is an error | Automated behavior verified; live status endpoint varies |
| GO-06 | POST | `timerActionWithRemaining` | `{PC.address}/{action}` | Timer command handler | JSON status and optional remaining seconds; status must be `success` | Shared 4 s client; non-2xx and invalid JSON fail | Automated timer tests pass |
| GO-07 | POST / DELETE | `updateBillingSessionLink` | `{PC.address}/billing-session` | Timer billing-link handler | JSON session/manager link; 2xx required | Shared 4 s client; errors propagate; DELETE is outside GET/POST scope | Source path verified; live old peers may not implement it |
| GO-08 | POST | `callBillingPeer` session start | `http://{registered host}:{UI port}/internal/api/local/sessions` | `localSessionHandler` | `StartSessionRequest` JSON; response session must match owning PC/address/device | Shared 4 s client; no retries; failures mapped to 502 at dashboard boundary | **Broken live:** PC-1/3/4 returned 404 in safe route probes; PC-2 timed out in latest probe |
| GO-09 | POST | `callBillingPeer` extension | `http://{registered host}:{UI port}/internal/api/local/sessions/extend` | `localSessionHandler` extension branch | `ExtendSessionRequest` with session/device/PC identity, seconds/minutes, request ID; extension response JSON | Shared 4 s client; stable request ID; no automatic retry | **Broken live:** PC-1/3/4 returned 404 in latest probes; PC-2 timed out |
| GO-10 | POST | `callBillingPeer` close/update | `http://{registered host}:{UI port}/internal/api/local/sessions/{escaped-session-id}/{action}` | `localSessionHandler` session update branch | Session identity + action payload; updated session JSON; same session is updated | Shared 4 s client; no retries; upstream error preserved and surfaced as 502 | **Broken live:** PC-1/3/4 returned 404 in latest probes; PC-2 timed out |
| GO-11 | GET | `syncBillingDevices` | `http://{registered host}:{UI port}/internal/api/billing/sync/pending` | `billingSyncPendingHandler` | Export containing stable device ID and pending session records | Shared 4 s client; failed peer remains pending/incomplete | **Broken live:** PC-1/3/4 returned 404 in latest probes; PC-2 timed out |
| GO-12 | POST | `syncBillingDevices` | `http://{registered host}:{UI port}/internal/api/billing/sync/ack` | `billingSyncAckHandler` | Ack of exported session IDs/update versions; acknowledged count validated | Shared 4 s client; idempotent versioned acknowledgement; no retry within one request | Automated idempotency tests pass; live peer route unavailable as above |
| GO-13 | POST | `main.go` overlay billing-link extension | Configured `ManagerEndpoint` from the local billing session link | Management `/internal/api/sessions/extend` route | `ExtendSessionRequest`; JSON result validated | Shared 4 s client; failure does not report success | Source/tests verify contract; live remote endpoint not verified |
| GO-14 | POST | `main.go` legacy extension forwarding | `http://{peer host}/internal/api/sessions/extend` on registered UI port | `internalExtensionHandler` | `ExtendSessionRequest`; JSON response validated | Shared 4 s client; peer errors retained | Source path verified; live fallback not exercised |
| GO-15 | GET | Launcher updater `updater.check` | Configured release endpoint (default GitHub latest release API) | GitHub Releases API | Release JSON decoded and assets/digest verified before update | 30 min updater client timeout; errors abort update | Launcher unit tests pass; external release reachability not tested in this audit |

The central Go HTTP checker injects `X-Request-ID`, adds `X-Parent-Request-ID` when a peer request is caused by an incoming dashboard request, adds device attribution when available, and records UTC timestamp, method, path (query omitted), target host/port, feature, device ID, response status, outcome, and duration. It does not read or log request/response bodies or credentials. The shared JSON decoder rejects malformed JSON and unexpected content types; individual operations validate their required fields/status. It does not retry requests.

## Backend route inventory

Management server (`ui.go`, `registerDiscoveryHandlers`, `registerBillingHandlers`):

| Route | Methods | Handler |
|---|---|---|
| `/` | GET | `servePage` (embedded `ui.html`) |
| `/api/health` | GET | `healthHandler` (read-only service status) |
| `/api/pcs` | GET, POST | `pcsHandler` |
| `/api/pcs/{id}` | PUT, DELETE | `pcHandler` → `handleRecord` |
| `/api/pcs/{id}/active` | PUT | `pcHandler` |
| `/api/node` | GET | `registerDiscoveryHandlers` node handler |
| `/api/discovery/status` | GET | Discovery status handler |
| `/api/discovery/run` | POST | Discovery run handler |
| `/api/discovery/peers` | GET, POST | `discoveryPeersHandler` |
| `/api/discovery/distribute` | POST | `distributeHandler` |
| `/api/pricing` | GET | Pricing handler |
| `/api/billing/summary` | GET | Billing summary handler |
| `/api/billing/report` | GET | `billingReportHandler` |
| `/api/billing/download` | GET | `downloadBilling` |
| `/api/billing/sync` | POST | `billingSyncHandler` |
| `/api/sessions` | GET, POST | `sessionsHandler` |
| `/api/sessions/extend` | POST | `extendSessionHandler` |
| `/api/sessions/{id}/{action}` | POST | `sessionActionHandler` → `updateSessionForDashboard` |
| `/internal/api/local/sessions` | POST | `localSessionHandler` |
| `/internal/api/local/sessions/extend` | POST | `localSessionHandler` extension branch |
| `/internal/api/local/sessions/{id}/{action}` | POST | `localSessionHandler` |
| `/internal/api/sessions/extend` | POST | `internalExtensionHandler` |
| `/internal/api/billing/sync/pending` | GET | `billingSyncPendingHandler` |
| `/internal/api/billing/sync/ack` | POST | `billingSyncAckHandler` |

Timer server (`main.go`, `timer.go`, `onbehalf.go`, `special_commands.go`):

| Route family | Methods | Handler |
|---|---|---|
| `/identity` | GET | Timer identity |
| `/status` | GET | Timer status |
| `/{set,tweak,play,pause,toggle,reset,show,hide,dismiss}` | POST | `registerTimerCommands` |
| `/billing-session` | POST, DELETE | Billing session-link handler |
| `/{id}/status` and `/{id}/{timer-action}` | GET / POST respectively | On-behalf timer status/commands |
| `/{special-command-prefix}/special` | POST | `specialCommandHandler` |
| `/position` | POST | Overlay window position |
| `/drag` | POST | Overlay window drag state |

Go `ServeMux` matching uses the longest registered route; the frontend/handler route comparison in this document is source-derived. No automatic static source scanner is available to infer all dynamically registered timer prefixes. Exact runtime route matching is logged as `r.Pattern`; unmatched routes are logged as `<unmatched>`.

## Request logging and diagnostic access

- Every incoming management-server and timer-server request, including 404 and method-mismatch responses, passes through middleware that logs the actual written status, request ID, method, escaped path without query, device header (if supplied), matched `ServeMux` pattern, and duration. CORS `OPTIONS` preflight is intentionally handled before the mux and therefore logs route `<unmatched>` with its actual 204 response.
- `GET /api/health` returns only `{status:"ok", service:"overlay-timer"}`; it does not inspect or modify billing state.
- The dashboard request history is memory-only and capped at 100 entries. Use the developer console on localhost with `?requestMonitor=1`; normal LAN clients do not get a diagnostic panel or diagnostic endpoint with internal state.
- HTTP CORS behavior remains unchanged. No request body, customer name, payment data, authorization header, or secret is logged by the checker.

## PC-4 incident findings (observed probes; no session mutations)

The current source registers the exact routes/methods used for start, extension, close, and pending sync. Safe diagnostic POST bodies were empty objects; close used a nonexistent audit-only session ID. Therefore, no customer session or billing record was created, extended, or closed by these probes.

| Device | Start POST | Extend POST | Close POST | Pending-sync GET | Evidence |
|---|---:|---:|---:|---:|---|
| PC-1 (`192.168.1.5:18082`) | 404 | 404 | 404 | 404 | Latest route probes |
| PC-2 (`192.168.1.6:18082`) | timeout | timeout | timeout | timeout | Latest route probes (3 s each) |
| PC-3 (`192.168.1.13:18082`) | 404 | 404 | 404 | 404 | Latest route probes |
| PC-4 (`192.168.1.7:18082`) | 404 | 404 | 404 | 404 | Latest route probes |

Start request payload is the existing `StartSessionRequest` structure (`pc_id`, `category`, `customer_name`, `players`, `duration_minutes`, optional `started_at`); the audit probe sent `{}` only. Extension payload uses the existing `ExtendSessionRequest` fields (`session_id`, `pc_id`, `pc_address`, `minutes` or `seconds`, `request_id`, optional `tweak_timer`); the audit probe sent `{}` only. Close payload contains `pc_id`, `pc_address`, `device_id`, `minutes`, and `amount`; the audit probe used a nonexistent audit-only session ID and `{}`. Sync pending is GET with no body; sync acknowledgement posts a stable device ID and versioned session ID/update-time pairs. Since the live probes intentionally did not submit real operations, their bodies contained no customer or billing values. The request shown in the user's actual close error did not include its body, so the live device/session identifiers in that particular payload cannot be independently confirmed from the error text.

For each 404 probe, the remote matched handler is **unknown/not reached**: the plain Go `404 page not found` is the default not-found response. The expected current-source handler is `localSessionHandler` for start, extension, and close, and `billingSyncPendingHandler` for sync.

The plain `404 page not found` responses indicate these running peer services do not match the current source route table (or an alternate service/build is serving the port); no URL prefix, method, or port error was found in the current caller construction. The PC-4 billing summary endpoint has separately returned HTTP 200 with `enabled=false`. The exact remote executable/version cannot be identified through the exposed legacy APIs. A local build cannot alter those remote processes. Install/restart the current executable and enable billing on each owning PC, preserving its device ID and workbook, then verify the routes and perform real start/extension/close/sync regression tests.

## Known coverage limits

- Four-PC route probes are reachability/route checks only, not real transaction tests. No valid customer payload was sent.
- PC-2 was unreachable during this probe window. Previously collected evidence reported billing disabled on all four peers; recheck each after deployment.
- The current version cannot remotely inspect process executable identity or version on peer machines.
- No route-protection/security redesign or retry policy change was made. Billing/session POSTs are not automatically retried.
