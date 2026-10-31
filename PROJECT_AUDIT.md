# AI.FloatingTimer — Paradise Gaming Dashboard: Comprehensive Project Audit (Phase 1)

**Audit Date:** 2026-10-23  
**Auditor:** Antigravity Agent  
**Repository Branch:** `feat/quick-timer-extension`  
**Host Environment:** Windows 11 x86_64, Go 1.25, Node.js v22  

---

## 1. Project Overview & Architecture

**AI.FloatingTimer** is a dual-purpose gaming center management application designed for gaming cafes (specifically configured for **Paradise Gaming**). It combines:
1. A **Desktop Floating Overlay & Countdown Timer** running locally on each gaming PC/console station.
2. A **Web-Based Management Dashboard** serving system operators, tracking live countdowns, managing customer sessions, computing bills, and generating centralized daily revenue reports.

### Process Topology & Port Bindings

The application runs two distinct internal HTTP services and one optional launcher supervisor:

```
+---------------------------------------------------------------------------------------+
| FloatingTimerLauncher.exe (Supervisor, cmd/launcher/main.go)                          |
|   ↳ Launches, supervises, and safely hot-swaps: OverlayTimer.exe                      |
+---------------------------------------------------------------------------------------+
        |
        v
+---------------------------------------------------------------------------------------+
| OverlayTimer.exe (PID 24800, main.go)                                                 |
|                                                                                       |
|   [Port 18081] - Timer & Desktop Overlay Server (main.go)                             |
|     • Endpoints: /status, /play, /pause, /toggle, /reset, /dismiss, /show, /hide,     |
|                  /set, /tweak, /billing-session, /special                             |
|     • Engine: Fyne GUI floating widget + fullscreen "TIME UP" red alert window         |
|     • Automatic process killer (e.g. TekkenGame-Win64-Shipping.exe) on expiry        |
|                                                                                       |
|   [Port 18082] - Management & Central Billing Server (ui.go, billing.go)              |
|     • Serves single-page UI via `//go:embed ui.html` at `/`                           |
|     • Registry APIs: /api/pcs, /api/pcs/{id} (overlay_ui_pcs.json)                    |
|     • Billing APIs: /api/pricing, /api/sessions, /api/sessions/extend,                |
|                     /api/billing/summary, /api/billing/report, /api/billing/sync      |
|     • Peer Sync APIs: /internal/api/billing/sync/pending, /internal/api/billing/sync/ack|
|     • Local Persistence: billing.xlsx + pre-write backups in billing-backups/         |
+---------------------------------------------------------------------------------------+
```

---

## 2. File Inventory & Responsibilities

| File Path | Language / Type | Primary Responsibilities |
| --- | --- | --- |
| [main.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/main.go) | Go (`main`) | Application entry point; Fyne UI initialization (floating timer pill & fullscreen `TIME UP` window); configuration parser (`overlay_timer_config.json`); Port 18081 timer HTTP server; countdown goroutine (`runCountdown`); Win32 API window control. |
| [ui.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/ui.go) | Go (`main`) | Port 18082 web management server; `//go:embed ui.html` directive; static HTML route; `/api/pcs` REST handlers; activity CSV logger (`overlay_ui_activity.csv`); HTTP security headers (CSP, nosniff, DENY). |
| [ui.html](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/ui.html) | HTML/CSS/JS | Single-page operator dashboard; responsive PC card grid; live pricing estimator; per-card session forms; timer extension triggers (+15m, +30m, +1h); consolidated daily report view; device sync health table; session history. |
| [billing.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/billing.go) | Go (`main`) | `BillingStore` implementation using Excel (`billing.xlsx`) via `excelize/v2`; atomic temporary file replacement and pre-write backups; session lifecycle handlers; `/api/sessions/extend`; peer sync client calls (`callBillingPeer`). |
| [billing_sync.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/billing_sync.go) | Go (`main`) | Multi-device sync algorithms; persistent device ID (`billing_device_id.txt`); `pendingSync()` exporter; `mergeSyncedSessions()` (idempotent upsert by `(device_id, session_id)`); `acknowledgeSync()`; `report()` in `Asia/Kolkata`. |
| [pricing.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/pricing.go) | Go (`main`) | Authoritative rate catalog for Paradise Gaming: PC Lite, PC Pro, PS5, Racing Lite, Racing Pro across 15, 30, and 60 minutes for 1–4 players. |
| [registry.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/registry.go) | Go (`main`) | `PCStore` maintaining `overlay_ui_pcs.json`; loads, normalizes, and stores device addresses, friendly names, stable IDs, last-seen timestamps, and tombstoned deletions. |
| [discovery.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/discovery.go) | Go (`main`) | Background network discovery scheduler; scans CIDR blocks or local `/24` on port 18081 to find peer timers. |
| [distribution.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/distribution.go) | Go (`main`) | Peer registry distributor; sends active PC list across peer managers via `POST /api/discovery/distribute`. |
| [special_commands.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/special_commands.go) | Go (`main`) | Configurable external command runner; spawns subprocesses/scripts from timer triggers without blocking timer operations. |
| [onbehalf.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/onbehalf.go) | Go (`main`) | On-behalf secondary timer support (e.g., host port 18081 with `/1` path for console stations). |
| [timer.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/timer.go) | Go (`main`) | Duration parsing, timestamp formatting, and countdown arithmetic helpers. |
| [startup.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/startup.go) | Go (`main`) | Windows registry startup autorun helper (`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`). |
| [cors.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/cors.go) | Go (`main`) | Cross-Origin Resource Sharing middleware for LAN requests. |
| [cmd/launcher/main.go](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/cmd/launcher/main.go) | Go (`main`) | `FloatingTimerLauncher.exe`; supervisor process for launching, verifying SHA-256 sums, and hot-swapping `OverlayTimer.exe`. |
| [build.ps1](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/build.ps1) | PowerShell | Official build script; strips debug symbols (`-s -w`) and compiles `OverlayTimer.exe` and `FloatingTimerLauncher.exe`. |
| [ui_polling_test.cjs](file:///c:/Users/sarva/Desktop/AI.FloatingTimer/ui_polling_test.cjs) | Node.js Test | 27-suite comprehensive headless integration test harness validating dashboard UI behaviors, polling coalesce, and card isolation. |

---

## 3. Data Flows & Runtime Mappings

### 3.1 Device Identification & Discovery
1. **Device Identity:** Every physical PC generates a stable device ID (e.g. `PC-d36d37...` or configured `billing.device_id`) persisted in `billing_device_id.txt`.
2. **Registry:** Devices are cataloged in `overlay_ui_pcs.json` with a local unique ID, friendly name, IP address, timer port (18081), and UI port (18082).
3. **Discovery:** When enabled, `startDiscoveryScheduler()` periodically probes CIDRs on port 18081 using lightweight `/status` probes to register newly connected peers.

### 3.2 Session Creation, Timer Start & Termination
1. **Operator Action:** The operator selects customer name, category, player count, and duration directly on the target PC card.
2. **Creation Request:** The browser submits `POST /api/sessions`.
3. **Backend Execution:**
   - Resolves owning device via registry (`pc_address`).
   - Generates unique session ID (e.g., `S-20261023-xxxx`).
   - Sends HTTP command to start countdown timer on target PC (`18081/set` or `18081/tweak`).
   - Inserts session row and payment record into local Excel workbook (`billing.xlsx`).
   - Immediately returns the session record.
4. **UI Update:** The PC card hides the new-session form and reveals active session details (Customer, Duration, Bill, Paid/Due, Mark Paid, Close).
5. **Termination:** Clicking **Close Session** (`POST /api/sessions/{id}/close`) sets `status = "closed"`, records `ended_at`, stops the timer, and re-enables the new-session form on the card.

### 3.3 Timer Controls & Duration Extensions
1. **Play/Pause/Reset/Dismiss:** Direct commands route to `http://<target-ip>:18081/{action}`.
2. **Extensions (+15m, +30m, +1h):**
   - Routes through `POST /api/sessions/extend` with `seconds`, `session_id`, and `pc_id`.
   - Backend extends the existing active session in `billing.xlsx` (`additional_minutes` incremented, `final_amount` updated per pricing rules).
   - Simultaneously tweaks the running timer by exact seconds.
   - Preserves customer identity without spawning a duplicate session or billing row.

### 3.4 Local Persistence & Historical Storage
- **Primary Store:** Microsoft Excel workbook `billing.xlsx` with three worksheets:
  1. `Sessions`: Columns for Date, Time, Customer, System, Players, Start Time, Purchased Minutes, Amount Paid. Extra metadata (IDs, pricing snapshot, payment timestamps) is stored in cell comments.
  2. `Payments`: Individual transaction entries.
  3. `Daily Summary`: Aggregated daily figures.
- **Atomic Reliability:** Before every workbook write:
  1. Pre-write backup is created in `billing-backups/billing-YYYY-MM-DD-HHMMSS.xlsx`.
  2. Workbook is saved to `billing.xlsx.tmp`.
  3. Atomic OS rename replaces `billing.xlsx`.

### 3.5 Central Dashboard Updates & Multi-Device Synchronization
1. **Polling:** The dashboard polls timer status once per host (coalescing multiple timers/on-behalf agents on the same host into one HTTP request).
2. **Periodic Sync:** Every 30 seconds (and on clicking **Sync PCs**):
   - Hub calls `GET /internal/api/billing/sync/pending` on each registered PC.
   - Remote PC exports all sessions where `SyncedUpdatedAt != UpdatedAt`.
   - Hub executes `mergeSyncedSessions()`: merges records by composite key `(device_id, session_id)`, updating rows only if the incoming record is newer.
   - Hub calls `POST /internal/api/billing/sync/ack` on the peer with acknowledged IDs and timestamps.
   - Peer sets `SyncedUpdatedAt = UpdatedAt`.
3. **Consolidated Daily Report:**
   - Querying `GET /api/billing/report?date=YYYY-MM-DD` groups sessions strictly by `started_at` in timezone `Asia/Kolkata`.
   - Distinctly calculates `Revenue` (actual collected payments) vs `Outstanding` (unpaid balances).
   - Flags report as **incomplete** if any registered gaming PC is offline, reporting sync issues, or holding unacknowledged records.

### 3.6 Authentication Architecture
- **Status:** The legacy "Billing Admin Token" system has been **completely removed**.
- **Historical Flow:** In earlier revisions, `ui.html` intercepted authorization failures and called `prompt("Enter the billing admin token")`, storing tokens in `sessionStorage("billing-token")`.
- **Current Flow:** All APIs (`/api/billing/*`, `/api/sessions/*`, etc.) are completely unauthenticated and accessible on the local private network without credentials. No admin login or token prompts exist.

---

## 4. Current Process & Executable Audit

- **Active Process Serving Port 18082:**
  - **Executable:** `C:\Users\sarva\Desktop\AI.FloatingTimer\OverlayTimer.exe`
  - **PID:** `24800`
  - **Start Time:** 23-10-2026 01:14:02 PM
- **Active Process Serving Port 18081:**
  - Shared with PID `24800` (dual HTTP listeners in `main.go` and `ui.go`).
- **Embedded Asset Verification:**
  - `ui.go` uses `//go:embed ui.html`.
  - Binary `OverlayTimer.exe` embeds the latest dashboard UI (version compiled at 13:13:55).

---

## 5. Architectural Risks, Duplications & Known Limitations

1. **Obsolete Code in `ui.html` (Safe Cleanup Candidate):**
   - The central "New Session" and "Current Bill" panels were removed from the HTML layout.
   - However, legacy helper functions (`renderBillingForm`, `billingChanged`, `renderCurrentBill`, `loadCurrentBill`, `markCurrentPaid`, `closeCurrentSession`, `startBillingSession`) and CSS rules (`.workbench`, `.form-grid`, `.billing-summary`) remain dormant in `ui.html`.
   - *Risk:* Zero functional risk, but clutters the frontend script.
2. **Remote Peer PC Binaries:**
   - Remote gaming PCs (`PC-1` at `192.168.1.4`, `PC-2` at `192.168.1.5`, `PC-3` at `192.168.1.13`, `PC-4` at `192.168.1.7`) are currently running older executable builds that lack `/internal/api/billing/sync/pending` and have billing disabled (`"enabled": false`).
   - *Impact:* The dashboard correctly marks these devices as `Sync issue (HTTP 404)` in the consolidated report without crashing. They must receive the updated binary and have billing enabled locally.
3. **Concurrency & Excel Storage:**
   - In-process mutex serialization (`s.mu.Lock()`) prevents concurrent write corruption within a single binary.
   - If an external program (e.g. Microsoft Excel) locks `billing.xlsx`, writes will fail gracefully and restore from `.tmp`.

---

## 6. Build and Run Commands

### Compiling Binaries
```powershell
powershell.exe -ExecutionPolicy Bypass -File .\build.ps1
```

### Running Test Suites
```powershell
# Frontend Integration Tests (27 suites)
node ui_polling_test.cjs

# Backend Go Test Suites
go test -count=1 ./...
```

### Starting the Application
```powershell
# Via launcher
.\FloatingTimerLauncher.exe

# Or directly
Start-Process -FilePath ".\OverlayTimer.exe"
```

---

## 7. Proposed Implementation Sequence for Subsequent Phases

1. **Phase 2 — UI Layout & Polish Review:** Validate operator feedback on the per-PC card layout, badge contrast, and multi-column grid scalability across displays.
2. **Phase 3 — Field Validation & Edge Cases:** Verify session start validation, edge cases with empty customer names, and price calculation responsiveness on network lag.
3. **Phase 4 & 5 — Historical Report Drilldowns:** Confirm daily summary report exports, Excel download verification (`/api/billing/download`), and date-picker range inspections.
4. **Phase 6 — Peer PC Deployment:** Guide copying the updated `OverlayTimer.exe` to peer gaming PCs (`PC-01` through `PC-04`) and enabling billing in their local configs.
5. **Phase 10 — Safe Destructive Cleanup (Upon Explicit Approval):** Remove the identified obsolete JavaScript functions and unused workbench CSS classes from `ui.html`.
