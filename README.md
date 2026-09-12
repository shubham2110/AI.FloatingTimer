# Overlay Timer REST API

## Launcher and automatic updates (Windows x64)

Run `./build.ps1` to build both executables. Keep `FloatingTimerLauncher.exe` and
`OverlayTimer.exe` together in a writable folder, alongside your existing JSON
settings. Point desktop and startup shortcuts at **FloatingTimerLauncher.exe**.
Opening `OverlayTimer.exe` directly still works, but bypasses updates.

On every launch:

1. Verify and install any previously staged update before starting the timer.
2. Check the assets on the exact GitHub release tag
   [`latest`](https://github.com/shubham2110/AI.FloatingTimer/releases/tag/latest).
   Compare the `OverlayTimer.exe` asset's SHA-256 against the installed executable;
   replacing an asset under the same tag is detected without a version-number change.
3. Download changed content, verify its size, SHA-256 and Windows x64 executable
   header, install it as `OverlayTimer.exe`, and start it.
4. If checking/downloading is still running after two minutes, start the installed
   timer. The launcher remains alive in the background until the download finishes
   (each HTTP request has a 30-minute timeout). It leaves `OverlayTimer.pending.exe`
   and `update-pending.json` beside the app for the next launch. It does not restart
   or replace the active timer after this fallback.

Network errors, rate limits, missing assets and invalid downloads fall back to the
installed timer. Partial downloads never replace it. Updates retain
`OverlayTimer.previous.exe`; interrupted replacements recover from that backup.
If Windows refuses replacement because the timer is running, the update stays
pending: quit the timer and open the launcher again. Concurrent launchers are
serialized with an OS file lock, released automatically if the launcher crashes.
Errors are recorded in `launcher.log`.

The two-minute limit bounds waiting for the internet; Windows process startup and
local disk/antivirus operations can add time. An existing app is required for the
offline/timeout fallback, so distribute both executables initially. Settings, peer
data and activity files are preserved. The backup is also tried if Windows cannot
start the installed executable; this does not detect crashes after process startup.

For releases, upload both `OverlayTimer.exe` and `FloatingTimerLauncher.exe` with
the same names used locally. The updater selects only `OverlayTimer.exe` and ignores
the launcher asset when checking for timer updates.
The launcher requires GitHub's `sha256:` asset digest. It is updated manually;
publishing a new release alone does not update the launcher itself. No release is
published by the build script.

## Overview

The timer exposes an HTTP API on port `18081` by default (`timer_port` in configuration). The same executable serves the management UI on `18082` (`ui_discovery_port`). The ports must differ; both are reserved before services start. This reference uses only relative API paths so the UI is not tied to a particular machine.

The management UI obtains saved PC addresses from its own backend, then calls each active timer directly from the browser. Timer endpoints allow cross-origin requests with `Access-Control-Allow-Origin: *` and handle OPTIONS preflight requests.

> **Security:** The API currently has no authentication or encryption. Restrict both ports to trusted networks or place the timer behind an authenticated HTTPS proxy.

## Common request and response format

- Request method: `GET` for status and `POST` for commands
- JSON request content type, when a body is required: `application/json`
- Response content type: `application/json`
- Time values are integer seconds.

Example relative request from browser JavaScript:

```javascript ui-example.js
const response = await fetch("/reset", { method: "POST" });
const result = await response.json();
```

## Endpoints

### Read current status

```http api-request.http
GET /status
```

Returns a consistent snapshot of the current timer and alert state.

Success response (`200 OK`) while running:

```json api-response.json
{
  "remaining_seconds": 42,
  "is_running": true,
  "timer_status": "running",
  "time_up_visible": false,
  "seconds_until_time_up": 42
}
```

When paused with time remaining, `timer_status` is `"paused"` and `seconds_until_time_up` is `null`, because the countdown will not reach zero until resumed. When the remaining time is zero, `seconds_until_time_up` is `0`. `time_up_visible` independently reports whether the full-screen alert is currently displayed.

Browser JavaScript example:

```javascript ui-example.js
const response = await fetch("/status");
const timer = await response.json();
```

#### Status scenarios and UI decisions

In the table below, `99` represents any value greater than zero. These are common combinations the API can return; `/show` can additionally make the alert visible in any countdown state:

| Scenario | `is_running` | `remaining_seconds` | `seconds_until_time_up` | `time_up_visible` | `timer_status` | Suggested UI decision |
|---|---:|---:|---:|---:|---|---|
| Running countdown | `true` | `99` | `99` | `false` | `"running"` | Show the remaining time and an enabled **Pause** action. |
| Paused countdown | `false` | `99` | `null` | `false` | `"paused"` | Show the remaining time and an enabled **Play/Resume** action. Do not display an expected alert time. |
| Reset/cleared | `false` | `0` | `0` | `false` | `"paused"` | Show `00:00`; prompt the operator to set time before starting. |
| Time expired, alert showing | `true` | `0` | `0` | `true` | `"running"` | Show **TIME UP** and provide **Dismiss**, **Reset**, and **Set new time** actions. |
| Time expired, alert dismissed | `true` | `0` | `0` | `false` | `"running"` | Show `00:00` and indicate that the expired alert has been dismissed. Prompt for a new time or reset. |

The last scenario can occur after `/dismiss`, `/play` at zero, or `/set` with zero while the timer is running. A running timer at zero does not count below zero and does not repeatedly reopen the alert.

#### Field rules

The following rules define valid combinations:

1. `timer_status` is derived from `is_running`:
   - `is_running: true` always means `timer_status: "running"`.
   - `is_running: false` always means `timer_status: "paused"`.
2. When `remaining_seconds` is greater than zero:
   - If running, `seconds_until_time_up` equals `remaining_seconds`.
   - If paused, `seconds_until_time_up` is `null` because expiry is not approaching.
3. When `remaining_seconds` is zero, `seconds_until_time_up` is always `0`.
4. `time_up_visible` is independent of countdown state: `/show` can display the alert while paused or while time remains.
5. Reset produces the exact combination: `false`, `0`, `0`, `false`, `"paused"`.

#### Invalid or contradictory combinations

The UI should treat any response violating the rules above as invalid rather than attempting to assign it a new meaning. Examples include:

| Invalid combination | Reason |
|---|---|
| `is_running: true` with `timer_status: "paused"` | The two running-state fields contradict each other. |
| `is_running: false` with `timer_status: "running"` | The two running-state fields contradict each other. |
| Positive `remaining_seconds` with `seconds_until_time_up: 0` | A positive running timer must report the same positive value; a paused timer must report `null`. |
| Zero `remaining_seconds` with `seconds_until_time_up: null` or a positive value | At zero, the time until expiry is always zero. |
| Negative values in either seconds field | Timer values are never negative. |

Because commands and status polling can occur concurrently, a UI may very briefly observe a transitional snapshot while an alert is being dismissed. Poll again before treating one unusual response as a persistent error.

#### Recommended UI state selection

Use the fields in this priority order:

```javascript ui-status-decision.js
if (timer.time_up_visible) {
  showTimeUpState();
} else if (timer.remaining_seconds === 0 && !timer.is_running) {
  showResetState();
} else if (timer.remaining_seconds === 0) {
  showExpiredDismissedState();
} else if (timer.is_running) {
  showRunningState(timer.remaining_seconds);
} else {
  showPausedState(timer.remaining_seconds);
}
```

Do not use `timer_status` as a separate source of truth; it is a readable representation of `is_running`. Likewise, `seconds_until_time_up` is derived from the running state and remaining time.

### Set the exact remaining time

```http api-request.http
POST /set
Content-Type: application/json

{
  "seconds": 300
}
```

Sets the timer to exactly the supplied number of seconds. The value must be a non-negative integer. This operation dismisses the **TIME UP** screen but preserves the current running or paused state.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "remaining_seconds": 300
}
```

Invalid request response (`400 Bad Request`):

```json api-response.json
{
  "status": "error",
  "message": "seconds must be zero or greater"
}
```

The value may alternatively be supplied as a query parameter:

```text api-route.txt
POST /set?seconds=300
```

### Adjust the remaining time

```http api-request.http
POST /tweak
Content-Type: application/json

{
  "amount": 60
}
```

Adds the supplied number of seconds to the current value. Use a negative value to subtract time. The result is clamped to zero and cannot become negative. This operation dismisses the **TIME UP** screen but does not change the running or paused state.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "remaining_seconds": 360
}
```

The value may alternatively be supplied as a query parameter:

```text api-route.txt
POST /tweak?amount=60
POST /tweak?amount=-30
```

### Start or resume

```http api-request.http
POST /play
```

Starts or resumes countdown and dismisses the **TIME UP** screen.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

If the remaining time is zero, `/play` dismisses the alert but the timer remains at zero. Set or add time before playing.

### Pause

```http api-request.http
POST /pause
```

Pauses countdown without changing the remaining time and dismisses the **TIME UP** screen.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

### Toggle running state

```http api-request.http
POST /toggle
```

Switches between running and paused and dismisses the **TIME UP** screen.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

For a UI, prefer `/play` and `/pause` because they are explicit and idempotent. Repeating `/toggle` can accidentally produce the wrong state.

### Reset to zero

```http api-request.http
POST /reset
```

Sets the timer to `00:00`, stops it, and dismisses the **TIME UP** screen. Resetting does not trigger a new alert.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

### Set overlay position

```http api-request.http
POST /position
Content-Type: application/json

{
  "x": 10,
  "y": 10
}
```

Moves the timer overlay to an exact screen position in physical pixels. `x` is measured from the left of the primary screen and `y` from the top. Both values must be non-negative integers. The default position is near the top-left at `(10, 10)`.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "x": 10,
  "y": 10
}
```

### Enable or disable local dragging

```http api-request.http
POST /drag
Content-Type: application/json

{
  "enabled": true
}
```

When enabled, the operator at the timer machine can press and drag the timer overlay. Drag mode temporarily disables click-through behavior. Disable drag mode afterward to restore click-through:

```http api-request.http
POST /drag
Content-Type: application/json

{
  "enabled": false
}
```

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "drag_enabled": false
}
```

The same controls are available from the tray as **Enable dragging** and **Disable dragging (click-through)**. Drag mode is disabled by default.

### Dismiss the TIME UP screen

```http api-request.http
POST /dismiss
```

Hides the full-screen **TIME UP** alert without changing the remaining time or running state.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

## Typical UI operations

### Set five minutes and start

```javascript ui-example.js
await fetch("/set", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ seconds: 5 * 60 })
});
await fetch("/play", { method: "POST" });
```

### Add one minute

```javascript ui-example.js
await fetch("/tweak", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ amount: 60 })
});
```

### Stop and clear the timer

```javascript ui-example.js
await fetch("/reset", { method: "POST" });
```

### Dismiss only the alert

```javascript ui-example.js
await fetch("/dismiss", { method: "POST" });
```

## Operator UI and multiple machines

The frontend loads `GET /api/pcs` from its own manager, then uses each active PC's
`address` as the base URL. Status, set/play/pause, overlay controls, and numeric on-behalf
routes go directly to the timer host. The former `/api/pcs/{id}/{action}` proxy routes
have been removed. Local timer command activity is logged on the timer host.

```javascript
const pcs = await (await fetch('/api/pcs')).json();
const activePC = pcs.find(pc => !pc.inactive);
if (activePC) {
  const status = await (await fetch(activePC.address + '/status')).json();
  await fetch(activePC.address + '/set', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({seconds: 300})
  });
}
```

All machines must run the updated timer API for direct browser access. CORS allows
all origins, supported API methods, and request headers without credentials. The
manager's Content Security Policy permits HTTP/HTTPS connections to timer hosts.

## Current API limitations

- Status is retrieved by polling `GET /status`; there are no push updates such as WebSocket or Server-Sent Events.
- There is no authentication or authorization.
- There is no HTTPS support in the application itself.
- Timer state is held in memory and is lost when the process exits.

An operator UI can poll `GET /status` periodically. A real-time push mechanism could be added later if polling is not sufficient.

## Merged application additions

Build the complete package on Windows, including the embedded `ui.html`:

```powershell
go build -ldflags="-H=windowsgui -s -w" -o OverlayTimer.exe .
```

No separate UI executable or deployed HTML file is needed. The JSON config, peer registry,
and activity files live beside the executable.

Alternatively, run `./build.ps1`. The `-s -w` flags strip debug information to avoid
malformed Windows executable headers with Go 1.25 and older MinGW/CGO toolchains.

| Route | Behavior |
|---|---|
| `GET /identity` | Application identity, friendly name, timer port, and UI port. |
| `POST /show` | Display TIME UP without changing time or running state. |
| `POST /hide` | Hide TIME UP; `/dismiss` remains an alias. |
| `GET /status` | Existing local fields plus `name` and an `on_behalf_of` array. |
| `GET /1/status` | Status for configured on-behalf timer 1. |
| `POST /1/{action}` | Set, tweak, play, pause, toggle, reset, show, hide, or dismiss timer 1. |

Other numeric IDs work the same way. Each on-behalf status includes `id` and `name` plus
the standard timer fields. External commands come only from local configuration and
remain asynchronous. Command ordering is not guaranteed. Each timer's activity CSV is
stored in its numeric subdirectory, with the same columns as the primary activity CSV.

### Management and registry behavior

- `GET /api/pcs` returns all saved non-deleted PCs, including their local `inactive` preference.
- `POST /api/pcs` adds a PC; `PUT/DELETE /api/pcs/{id}` edits/removes it. Add/edit accepts
  `name`, `address`, and optional `ui_port`; omission preserves the existing UI port.
- `PUT /api/pcs/{id}/active` with `{"active": false}` disables a PC. New and legacy PCs
  are active by default. Selection persists beside the executable in `overlay_ui_pcs.json`.
- Inactive PCs only appear in **Select active PCs**, not in the dashboard or detail list.
  They are not polled, commanded, probed by discovery, or included in distribution.
  Disabling cancels pending browser requests; commands already delivered cannot be undone.
- Status polls run from the browser about every two seconds; the saved list refreshes
  every 15 seconds. **Refresh** immediately reloads the list and fetches active status.
- **Discover** calls `POST /api/discovery/run`; progress is read with
  `GET /api/discovery/status`. Discover saves URLs locally without distributing them.
- **Distribute** calls `POST /api/discovery/distribute` with `{"ids":["selected-pc-id"]}`.
  The manager sends only those active records to the selected active machines' configured
  UI/discovery ports. Results report successes and per-destination errors. No request is
  made to inactive destinations. Receiving records never triggers another distribution.
- `GET/POST /api/discovery/peers` exports active records or imports received records.
  Peer imports preserve this manager's selection; remote inactive flags are never imported.

Automatic discovery is disabled by default and in the supplied configuration. Enable
it explicitly with `discovery.enabled: true`; `interval_seconds` defaults to 300. When
enabled, the existing lowest-reachable-IP leader logic is used. Distribution remains
an explicit button action even when automatic discovery is enabled.

Addresses are normalized registry keys. IDs remain stable locally, duplicate addresses
return HTTP 409, and metadata uses newest-edit-wins with a deterministic tie break.
Local deletion tombstones prevent old peer records from restoring removed addresses;
explicit Add PC can restore an address. Inactive selection is separate from shared
metadata and survives edits, imports, and restarts. Failed saves roll back memory.

Discovery still probes the configured timer port; differently configured remote timer
ports need manual registration or an existing peer record. A discovery observation
older than 15 minutes is marked stale, without deleting the record.
