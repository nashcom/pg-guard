# pg-guard -- build.ps1 -- builds the deployable binaries for both
# platforms into bin/, matching what windows/start-pg-guard.cmd
# (bin/pg-guard.exe) and docker/docker-compose.yml (bin/pg-guard-linux)
# expect.

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$src = Join-Path $root "src"
$bin = Join-Path $root "bin"

New-Item -ItemType Directory -Force -Path $bin | Out-Null

Push-Location $src
try {
    Write-Host "building bin/pg-guard.exe (windows/amd64)..."
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    go build -buildvcs=false -ldflags="-s -w" -o (Join-Path $bin "pg-guard.exe") .

    Write-Host "building bin/pg-guard-linux (linux/amd64)..."
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    go build -buildvcs=false -ldflags="-s -w" -o (Join-Path $bin "pg-guard-linux") .
}
finally {
    Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}

Write-Host "done."
Get-ChildItem (Join-Path $bin "pg-guard.exe"), (Join-Path $bin "pg-guard-linux") | Select-Object Name, Length, LastWriteTime
