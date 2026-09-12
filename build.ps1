$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
    # Strip debug information to avoid malformed PE headers with Go 1.25 / older MinGW.
    go build -ldflags='-H=windowsgui -s -w' -o OverlayTimer.exe .
    if ($LASTEXITCODE -ne 0) { throw "Build failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
}
