$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
    # Strip debug information to avoid malformed PE headers with Go 1.25 / older MinGW.
    go build -v -gcflags="all=-N -l" -ldflags="-s -w -H windowsgui" -o OverlayTimer.exe .
    if ($LASTEXITCODE -ne 0) { throw "Build failed (exit $LASTEXITCODE)" }
    go build -v -gcflags="all=-N -l" -ldflags="-s -w -H windowsgui" -o FloatingTimerLauncher.exe ./cmd/launcher
    if ($LASTEXITCODE -ne 0) { throw "Launcher build failed (exit $LASTEXITCODE)" }
    Write-Host 'Built OverlayTimer.exe and FloatingTimerLauncher.exe. Run the launcher.'
} finally {
    Pop-Location
}




