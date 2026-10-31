# Code Cleanup Review

**Review date:** 2026-10-23  
**Scope:** Phase 10 read-only audit of the integrated dashboard, billing, timer, synchronization, build, configuration, and dependency surfaces.

No source, dependency, configuration, executable, or stored-data files were deleted or changed for this audit. The only new file is this review report.

## Evidence and reference checks

- Searched dashboard markup and JavaScript for old New Session panel IDs, direct event-handler names, and call sites.
- Checked route registration in `ui.go`, `billing.go`, `discovery.go`, and `distribution.go`; checked the embedded UI declaration in `ui.go`.
- Searched source and configuration files for admin-token settings, authorization headers, and token prompts.
- Checked direct module reachability with `go mod why` and dependency resolution with `go list -deps`.
- Checked tracked files and ignore rules for executable artifacts and local billing/configuration files.
- Go compile/test history catches unused imports; no dependency removal is recommended from this static audit.

## Safe refactoring recommendations

### 1. Coalesce consolidated-report refresh requests

- **Exact file:** `ui.html`
- **Function/component/section:** `loadBillingReport`, `loadBilling`, and startup polling.
- **Lines:** `loadBillingReport` around 2546-2585; `loadBilling` around 2729-2741; periodic call around 2833.
- **Why it may be redundant:** `loadBilling()` calls `loadBillingReport()`, while a separate 15-second interval also calls `loadBillingReport()`. A billing refresh and the timer can therefore issue duplicate report requests close together. There is no visible in-flight promise guard for report loads.
- **References/dependencies checked:** `loadBilling()` is called after billing actions and session changes; the periodic report call refreshes sync status; the report is also explicitly refreshed on date changes and after sync. These are distinct refresh triggers, so removing all but one trigger could leave data stale.
- **Potential impact:** Coalescing concurrent report reads can reduce repeated workbook reads and network requests without changing totals or the reporting API. An overly long interval could delay offline/pending status changes in the UI.
- **Recommendation:** **Refactor**, not remove the refresh behavior. Preserve date-change and post-sync refreshes; consider a shared in-flight request and a separately chosen status-refresh cadence.
- **Tests required:** Verify one request for overlapping refresh triggers; verify date changes, successful/failed sync, payment/session updates, and offline/pending-device status still refresh correctly.

## Approval-required removal proposals

### 2. Remove the orphaned single-panel billing form JavaScript

- **Exact file:** `ui.html`
- **Function/component/section:** `currentBillingSession`, `billingChanged`, `renderBillingForm`, `renderCurrentBill`, `loadCurrentBill`, `markCurrentPaid`, `closeCurrentSession`, and the JavaScript `startBillingSession`.
- **Lines:** State declaration around 2514; legacy function cluster around 2652-2805.
- **Why it appears obsolete:** The active page no longer contains the corresponding `sessionSystem`, `sessionCustomer`, `sessionDuration`, `sessionCategory`, `sessionPlayers`, `currentBill`, `currentPaidButton`, `currentCloseButton`, or `startSessionButton` elements. Search results show the legacy function names and fields only in this JavaScript cluster; no current markup event handler invokes them. Active session creation uses the independent per-PC card form (`cardSessionFormHTML` / `startPCBillingSession`) instead.
- **References/dependencies checked:** `renderBillingForm` is not called by active initialization or dashboard rendering. `loadCurrentBill` is only called from `renderBillingForm`. `markCurrentPaid` and `closeCurrentSession` have no active markup references. The global `loadBilling`, `loadHistory`, `renderPriceList`, `sessionAction`, inline card session-start path, and Go backend session handler are separate and actively referenced; they must remain.
- **Potential impact:** Removing this cluster would reduce dead client code. Risk is low for the current rendered UI, but code reachable through an external browser-console script or undocumented customization would cease to exist.
- **Recommendation:** **Remove only with explicit approval**, as requested. Do not remove the Go `startBillingSession` implementation or API; that is used by backend routes and tests.
- **Tests required:** Run all dashboard tests and add/retain assertions for the absence of old panel elements, independent per-PC session creation, payment/close actions, history, report, and pricing list; run Go billing tests and the Windows build.

### 3. Remove ignored one-off executable artifacts, if confirmed disposable

- **Exact files:** `OverlayTimer.exe~` and `OverlayTimer.verify.exe` in the repository root.
- **Function/component/section:** Local generated/build artifacts.
- **Lines:** Not applicable (binary files).
- **Why they may be obsolete:** Both are ignored executable files and neither filename has a source or build-script reference. `OverlayTimer.exe~` appears to be a backup from a prior replacement/build; `OverlayTimer.verify.exe` appears to be a verification build. The project’s supported build script creates `OverlayTimer.exe` and `FloatingTimerLauncher.exe`.
- **References/dependencies checked:** `rg` found no source references to these two filenames; `.gitignore` ignores them. The launcher does use `OverlayTimer.previous.exe` for rollback, and the README documents recovery behavior; that rollback binary is **not** proposed for deletion.
- **Potential impact:** Deleting these files could remove a local fallback or a binary useful for comparison/diagnosis. They are not tracked by Git, but they may still be user-maintained artifacts.
- **Recommendation:** **Retain until explicitly approved**; if approved as disposable, remove only these exact files after confirming they are not needed for rollback or investigation.
- **Tests required:** Rebuild with `build.ps1`; run the launcher’s update/rollback tests; confirm the intended deployed `OverlayTimer.exe` and `FloatingTimerLauncher.exe` remain available.

## Audited items to retain (no cleanup proposed)

### Active session-creation UI and backend

- **Exact files:** `ui.html`, `billing.go`.
- **Function/component/section:** Per-PC `cardSessionFormHTML` / `startPCBillingSession`; backend `startBillingSession`, local/remote session routes.
- **Lines:** `ui.html` around 1820-2017; `billing.go` around 938-974 and route registration around 2027-2046.
- **Why retain:** These are the active inline session workflow and the backend timer/billing persistence path. The standalone form markup is gone, but the backend routine with a similar name remains necessary.
- **References/dependencies checked:** Card markup and UI tests reference the inline workflow; registered HTTP routes, remote-device routing, and Go tests reference the backend.
- **Potential impact:** Removing either would break per-PC session creation or persistence.
- **Recommendation:** **Retain.**
- **Tests required:** Existing UI session-start tests, Go billing/session tests, and Windows build.

### Admin-token compatibility cleanup

- **Exact file:** `ui.html`
- **Function/component/section:** Startup removal of old browser-stored billing tokens.
- **Lines:** Around 2824-2828.
- **Why it may look obsolete:** Billing authorization and token prompts have been removed; the only current code reference to `billing-token` clears stale values.
- **References/dependencies checked:** No current backend token configuration, authorization check, bearer header, or prompt was found. Historical mentions in `CHANGELOG.md` and `PROJECT_AUDIT.md` describe previous behavior, not active enforcement.
- **Potential impact:** Removing the cleanup would leave old tokens stored in browser storage. It does not authorize current requests.
- **Recommendation:** **Retain** as one-time compatibility cleanup. Historical documentation should remain.
- **Tests required:** If changed, verify old stored keys are cleared and billing requests continue to work without token prompts.

### API handlers and synchronization interfaces

- **Exact files:** `billing.go`, `billing_sync.go`, `ui.go`, `discovery.go`, `distribution.go`, `onbehalf.go`.
- **Function/component/section:** API route registration and handlers.
- **Lines:** `ui.go` around 55-62; `billing.go` around 2027-2046; per-handler lines in `billing.go`, `discovery.go`, and `distribution.go`.
- **Why they may look unused:** Some endpoints are not buttons in the primary dashboard.
- **References/dependencies checked:** Routes are registered in production muxes and used by the embedded UI, other PCs’ synchronization clients, timer hosts, the launcher, or tests. Internal routes implement cross-device session forwarding, synchronization export/acknowledgement, and timer billing links.
- **Potential impact:** Removing a seemingly hidden route can break remote session operations, record synchronization, timer extensions, or API compatibility.
- **Recommendation:** **Retain** unless a route-specific consumer audit and compatibility plan support removal.
- **Tests required:** Relevant route/handler, sync, remote-routing, timer-extension, and API regression tests.

### Embedded assets, dependencies, and local data

- **Exact files:** `ui.go`, `go.mod`, `go.sum`, `billing.xlsx`, `billing_device_id.txt`, `billing-sync-status.json`, `overlay_timer_config.json`, `overlay_ui_pcs.json`, `billing-backups/`.
- **Function/component/section:** Embedded UI; Go runtime/build dependencies; persisted billing, device identity, sync status, configuration, registry, and backups.
- **Lines:** `ui.go` embed declaration around 263; module declarations in `go.mod`; runtime files have no source line ranges.
- **Why they may look unused:** Runtime data/configuration files are ignored by Git; some dependencies are transitive or used only by Windows-specific packages.
- **References/dependencies checked:** `ui.go` embeds `ui.html`; `go mod why` and `go list -deps` show the direct Fyne, Excelize, and `x/sys` modules are reachable from the app or launcher. Storage and configuration paths are referenced by the billing, registry, and startup code. Build tags scope Windows code and tests.
- **Potential impact:** Removing embedded assets breaks the UI; removing dependencies may break compilation; deleting local data/configuration/backups can destroy billing history, stable identity, sync status, or user settings.
- **Recommendation:** **Retain all.** No dependency removal is justified by this audit.
- **Tests required:** If any module is later approved for removal, use `go mod tidy`, run all Go tests, `go vet`, and both Windows executable builds. Data/config changes require a separately approved backup and migration plan.

## Approval requested

Please identify the specific items you approve for removal or refactoring. No proposed source or artifact removal has been performed. Approval for one item will not be applied to the others.
