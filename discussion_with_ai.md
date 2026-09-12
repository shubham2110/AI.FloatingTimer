# Discussion with AI: Merged Timer, UI, Discovery, and On-Behalf Architecture

## Date

2026-09-08

## Objective

Merge the overlay timer, management UI, LAN discovery, registry synchronization, and optional on-behalf timers into one executable. The source may remain split across `main.go` and `ui.go`; both files will be built together into a single executable.

## Confirmed decisions

### 1. Ports

- The local timer API defaults to port `18081`.
- The management UI and discovery API default to port `18082`.
- Both ports must be configurable in the application JSON configuration.
- The two listeners cannot use the same port on the same machine.

### 2. Deployment model

- Every machine runs the same single executable.
- The executable contains:
  - Local overlay timer
  - Timer REST API
  - Management web UI
  - Discovery API
  - Peer registry and synchronization
  - Optional on-behalf timers
  - Activity logging

### 3. Discovery range

- By default, discovery scans the local IPv4 `/24` subnet.
- Discovery ranges must be configurable as CIDR entries in the JSON configuration.
- Automatic discovery runs every five minutes by default.
- The interval must be configurable.
- The UI includes an on-demand **Discover** action.

### 4. Discovery delegation

- Full Raft consensus is not required.
- The reachable machine with the lowest IP address in the subnet is the discovery leader.
- Only the leader performs the periodic subnet scan.
- Other machines retrieve and merge the leader's known registry.
- If the leader is unavailable, the next-lowest reachable IP assumes discovery responsibility.
- A machine discovered on timer port `18081` can be queried on its UI/discovery port to retrieve the machines it already knows.
- Registries are merged so all reachable machines converge on the same machine list.

### 5. Machine identity

- A configurable friendly name is preferred.
- If no friendly name is configured, the Windows hostname is used.
- Discovery must verify an application identity endpoint rather than treating every open port as an overlay timer.

### 6. On-behalf timers

- On-behalf timer routes use clean numeric prefixes:
  - `GET /1/status`
  - `POST /1/set`
  - `POST /1/tweak`
  - `POST /1/play`
  - `POST /1/pause`
  - `POST /1/reset`
  - `POST /1/dismiss`
  - `POST /1/show`
  - `POST /1/hide`
- Additional timers use `/2/...`, `/3/...`, and so on.
- No `/timer/` or `/timers/` route prefix will be introduced.
- Unprefixed routes continue to operate on the machine's own local timer.
- Each on-behalf timer calculates and stores timer state on the host machine.
- Each on-behalf timer can invoke locally configured external commands to show and hide its TIME UP screen.
- Remote API callers cannot provide arbitrary command lines. They can only trigger commands already defined in local configuration.

### 7. Combined status response

- Unprefixed `GET /status` remains the main polling endpoint.
- It includes the local timer's existing status fields and also includes status details for every on-behalf timer.
- This allows the frontend to retrieve local and on-behalf status in one request.
- `GET /1/status`, `GET /2/status`, etc. remain available for querying one on-behalf timer directly.
- The combined-status behavior applies only to `/status`; other existing unprefixed endpoints retain their current local-timer behavior.

### 8. Show and hide TIME UP API

- Add explicit on-demand endpoints:
  - `POST /show`
  - `POST /hide`
- Add corresponding on-behalf endpoints:
  - `POST /1/show`
  - `POST /1/hide`
- Existing `POST /dismiss` remains backward compatible and behaves as a hide operation.
- Show/hide actions do not need to alter remaining time or running state unless separately requested.
- The frontend includes compact show/hide controls rather than large prominent buttons.

### 9. External EXE and PowerShell command execution

The Go application can run configured EXE or PowerShell commands through `os/exec`.

Execution conditions:

- Commands run under the same Windows account, desktop session, and permission level as the merged application.
- If the app starts through Task Scheduler as an interactive user, commands can display UI in that user's desktop session.
- If the app runs as a Windows Service in Session 0, interactive UI commands generally cannot appear on the logged-in user's desktop.
- The configured account must have permission to read and execute the target files.
- PowerShell execution policy may affect `.ps1` files. A configured command may explicitly use an appropriate invocation such as `powershell.exe -NoProfile -ExecutionPolicy Bypass -File ...` if the administrator accepts that policy.
- The application should capture command exit status and output in activity logs.
- Commands should use an execution timeout so a stuck child process cannot block timer processing indefinitely.

### 10. Activity records

- The host machine's primary UI activity remains in:
  - `./overlay_ui_activity.csv`
- Each on-behalf timer stores activity in its own folder:
  - `./1/overlay_ui_activity.csv`
  - `./2/overlay_ui_activity.csv`
  - `./3/overlay_ui_activity.csv`
- Directories are created as needed.
- CSV files are append-only.
- Existing columns must not be reordered or removed, preserving backward compatibility.
- Future fields should be appended as new columns or represented through existing payload/detail columns.

### 11. Source and build layout

- `main.go` and `ui.go` may remain separate source files for maintainability.
- They are compiled together into one executable by building the package/directory, not by building only one file.
- Example development build:

```powershell build-merged.ps1
go build -o OverlayTimer.exe .
```

- Example GUI build without a console window:

```powershell build-merged-gui.ps1
go build -ldflags="-H=windowsgui" -o OverlayTimer.exe .
```

## Proposed configuration shape

The exact schema may be refined during implementation while retaining sensible defaults.

```json overlay_timer_config.example.json
{
  "friendly_name": "Gaming PC 1",
  "timer_port": 18081,
  "ui_discovery_port": 18082,
  "discovery": {
    "enabled": true,
    "interval_seconds": 300,
    "cidr_ranges": [
      "192.168.1.0/24"
    ],
    "connect_timeout_milliseconds": 350,
    "maximum_concurrency": 32
  },
  "on_behalf_of": [
    {
      "id": "1",
      "name": "Station 1",
      "show_command": [
        "C:\\TimerHooks\\show-timeout.exe",
        "--station",
        "1"
      ],
      "hide_command": [
        "C:\\TimerHooks\\hide-timeout.exe",
        "--station",
        "1"
      ],
      "command_timeout_seconds": 10
    },
    {
      "id": "2",
      "name": "Station 2",
      "show_command": [
        "powershell.exe",
        "-NoProfile",
        "-File",
        "C:\\TimerHooks\\show.ps1",
        "-Station",
        "2"
      ],
      "hide_command": [
        "powershell.exe",
        "-NoProfile",
        "-File",
        "C:\\TimerHooks\\hide.ps1",
        "-Station",
        "2"
      ],
      "command_timeout_seconds": 10
    }
  ]
}
```

Using command arrays instead of one shell command string avoids ambiguous quoting and avoids invoking a command shell unnecessarily.

## Security considerations

- Ports `18081` and `18082` currently have no authentication or encryption.
- Discovery and management access must be restricted to trusted LANs through Windows Firewall or an authenticated reverse proxy.
- Peer-provided addresses and registry entries must be validated before storage or use.
- Discovery should scan only configured local CIDRs and enforce safe host-count limits.
- The manager must never accept arbitrary executable paths or command strings from remote API requests.
- Peer registry synchronization needs loop prevention, deduplication, timestamps, and stale-peer handling.

## Implementation direction

1. Refactor local timer state and handlers so local and on-behalf timers share reusable logic.
2. Merge UI server startup into the primary application lifecycle.
3. Add configurable ports and identity endpoints.
4. Add on-demand local show/hide endpoints.
5. Add on-behalf timer state, routes, external command hooks, and per-agent activity folders.
6. Extend `/status` with on-behalf statuses.
7. Add persistent peer registry and peer-list API.
8. Add `/24`/CIDR discovery with bounded concurrency and on-demand triggering.
9. Add lowest-IP discovery-leader selection and registry synchronization.
10. Update the frontend for discovered physical machines and on-behalf agents.
11. Preserve the individual-PC detail screen and the all-PC consolidated dashboard.
