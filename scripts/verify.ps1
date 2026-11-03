#!/usr/bin/env pwsh
# verify — the single gate for this repo.
#
#   scripts\verify.cmd          # Windows PowerShell 5.1: gofmt + vet + build + test + contract
#   scripts\verify.cmd -Race    # same, with the race detector
#   pwsh -File scripts/verify.ps1 [ -Race ]   # PowerShell 7 machines
#
# Everything it runs is also documented in .harness/rules/60-commands.md.
[CmdletBinding()]
param([switch]$Race)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Continue'

Set-Location (Split-Path -Parent $PSScriptRoot)

# Go caches belong in TEMP, never in the working tree: the module cache is
# large, and a checkout on a small volume must stay buildable.
$cacheRoot = Join-Path ([System.IO.Path]::GetTempPath()) 'gamebackend'
$env:GOCACHE = Join-Path $cacheRoot 'go-build'
$env:GOMODCACHE = Join-Path $cacheRoot 'go-mod'
$env:GOPATH = Join-Path $cacheRoot 'gopath'
$env:GOTMPDIR = Join-Path $cacheRoot 'tmp'
# A checkout inside a git worktree can defeat `go`'s VCS stamping; a build
# failure for that reason is not worth debugging.
$env:GOFLAGS = '-buildvcs=false'
foreach ($dir in @($env:GOCACHE, $env:GOMODCACHE, $env:GOPATH, $env:GOTMPDIR)) {
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
}

$failures = New-Object System.Collections.Generic.List[string]

Write-Host "==> gofmt" -ForegroundColor Cyan
$unformatted = & gofmt -l .
if ($LASTEXITCODE -ne 0) {
    $failures.Add('gofmt (command failed)')
} elseif ($unformatted) {
    $unformatted | ForEach-Object { Write-Host "    needs gofmt: $_" -ForegroundColor Yellow }
    $failures.Add('gofmt')
}

Write-Host "==> go vet" -ForegroundColor Cyan
& go vet ./...
if ($LASTEXITCODE -ne 0) { $failures.Add('go vet') }

Write-Host "==> go build" -ForegroundColor Cyan
& go build ./...
if ($LASTEXITCODE -ne 0) { $failures.Add('go build') }

# The same two gates CI runs. Installed by the operator, not by this script: a gate that
# silently downloads a tool is a gate that changes what it checks on its own.
Write-Host "==> staticcheck" -ForegroundColor Cyan
if (Get-Command staticcheck -ErrorAction SilentlyContinue) {
    & staticcheck ./...
    if ($LASTEXITCODE -ne 0) { $failures.Add('staticcheck') }
} else {
    Write-Host "    staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@v0.8.2); skipped" -ForegroundColor Yellow
}

Write-Host "==> govulncheck" -ForegroundColor Cyan
if (Get-Command govulncheck -ErrorAction SilentlyContinue) {
    & govulncheck ./...
    if ($LASTEXITCODE -ne 0) { $failures.Add('govulncheck') }
} else {
    Write-Host "    govulncheck not installed (go install golang.org/x/vuln/cmd/govulncheck@v1.9.0); skipped" -ForegroundColor Yellow
}

if ($Race -and -not (Get-Command $(if ($env:CC) { $env:CC } else { 'gcc' }) -ErrorAction SilentlyContinue)) {
    # -race needs cgo, hence a C toolchain. A missing compiler is an
    # environment gap, not a code defect: skip loudly instead of failing red.
    Write-Host "==> go test -race: SKIPPED (no C compiler; set CC= or install gcc/mingw)" -ForegroundColor Yellow
    $Race = $false
}

$coverProfile = Join-Path $cacheRoot 'cover.out'
if ($Race) {
    Write-Host "==> go test -race" -ForegroundColor Cyan
    & go test -race "-coverprofile=$coverProfile" ./...
} else {
    Write-Host "==> go test" -ForegroundColor Cyan
    & go test "-coverprofile=$coverProfile" ./...
}
if ($LASTEXITCODE -ne 0) { $failures.Add('go test') }

# The coverage floor lives in one place (scripts/coverage-check.sh) so Windows and CI cannot
# disagree about the rule. Needs `sh` (Git for Windows provides it); without it the check is
# reported as skipped rather than silently passing.
Write-Host "==> coverage floor" -ForegroundColor Cyan
if (Get-Command sh -ErrorAction SilentlyContinue) {
    if ($env:BACKEND_TEST_DATABASE_URL) {
        & sh scripts/coverage-check.sh $coverProfile
    } else {
        Write-Host "    no BACKEND_TEST_DATABASE_URL: reporting only (CI enforces the floor)" -ForegroundColor Yellow
        $env:COVERAGE_MIN = '0'
        & sh scripts/coverage-check.sh $coverProfile
        Remove-Item Env:COVERAGE_MIN -ErrorAction SilentlyContinue
    }
    if ($LASTEXITCODE -ne 0) { $failures.Add('coverage') }
} else {
    Write-Host "    sh (Git for Windows) not found; coverage floor skipped" -ForegroundColor Yellow
}

Write-Host "==> contract (apigen -check)" -ForegroundColor Cyan
& go run ./tools/apigen -check
if ($LASTEXITCODE -ne 0) {
    Write-Host "    regenerate with: go run ./tools/apigen" -ForegroundColor Yellow
    $failures.Add('contract')
}

Write-Host "==> docs (links, index counts, md/html mirrors)" -ForegroundColor Cyan
if (Get-Command sh -ErrorAction SilentlyContinue) {
    & sh scripts/docs-check.sh
    if ($LASTEXITCODE -ne 0) { $failures.Add('docs') }
} else {
    Write-Host "    sh (Git for Windows) not found; docs check skipped" -ForegroundColor Yellow
}

Write-Host ""
if ($failures.Count -gt 0) {
    Write-Host ("VERIFY FAILED: " + ($failures -join ', ')) -ForegroundColor Red
    exit 1
}
Write-Host "VERIFY OK" -ForegroundColor Green
exit 0
