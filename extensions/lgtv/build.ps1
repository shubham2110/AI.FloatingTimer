# PowerShell Script to build lgtv_remote.exe
$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $ScriptDir

Write-Host "Tidying dependencies and downloading webostv..." -ForegroundColor Cyan
go mod tidy

Write-Host "Building lgtv_remote.exe..." -ForegroundColor Cyan
go build -o lgtv_remote.exe main.go

if (Test-Path "lgtv_remote.exe") {
    Write-Host "Successfully built ./extensions/lgtv/lgtv_remote.exe" -ForegroundColor Green
} else {
    Write-Host "Build failed." -ForegroundColor Red
}
