#!/usr/bin/env pwsh
# pg-local.ps1 - run a throwaway PostgreSQL for this repo, no admin required.
#
# Why this exists: installing PostgreSQL as a Windows service needs
# administrator rights. A small team machine often has none. The EnterpriseDB
# "binaries zip" is a plain zip: unpack it, initdb as your own user, run the
# postmaster as your own process. Nothing is installed, nothing is registered.
#
#   pwsh -File scripts/pg-local.ps1 -Action init     # download + unpack + initdb
#   pwsh -File scripts/pg-local.ps1 -Action start
#   pwsh -File scripts/pg-local.ps1 -Action status
#   pwsh -File scripts/pg-local.ps1 -Action stop
#   pwsh -File scripts/pg-local.ps1 -Action migrate  # apply db/migrations/*.sql
#   pwsh -File scripts/pg-local.ps1 -Action probe    # run db/probes/*.probe.sql
#
# The default port is 55432, not 5432: a throwaway instance should never collide
# with a real one. Override with -Port.
#
# NOT a deployment tool: no service, no password policy, no backup. See
# docs/postgres-local.md for the caveats.
[CmdletBinding()]
param(
    [ValidateSet('init', 'start', 'stop', 'status', 'migrate', 'probe', 'psql')]
    [string]$Action = 'status',
    [string]$PgHome = (Join-Path $env:USERPROFILE 'pgsql16'),
    [int]$Port = 55432,
    [string]$Database = 'gamebackend',
    [string]$Version = '16.4'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$bin = Join-Path $PgHome 'pgsql\bin'
$data = Join-Path $PgHome 'data'
$log = Join-Path $PgHome 'server.log'
$repo = Split-Path -Parent $PSScriptRoot

function Find-PgTool([string]$name) {
    $path = Join-Path $bin "$name.exe"
    if (-not (Test-Path $path)) {
        throw "$name not found at $path - run: pwsh -File scripts/pg-local.ps1 -Action init"
    }
    return $path
}

# psql writes NOTICEs and ERRORs to stderr. Under $ErrorActionPreference = 'Stop'
# PowerShell turns that into a terminating error even when psql succeeded, so the
# preference is relaxed around the call and the exit code is the only judge.
function Invoke-Psql([string[]]$arguments) {
    $psql = Find-PgTool 'psql'
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $output = & $psql -h 127.0.0.1 -p $Port -U postgres -v ON_ERROR_STOP=1 @arguments 2>&1
    $code = $LASTEXITCODE
    $ErrorActionPreference = $previous
    if ($code -ne 0) { throw "psql failed (exit $code): $($output | Out-String)" }
    return $output
}

switch ($Action) {
    'init' {
        New-Item -ItemType Directory -Force -Path $PgHome | Out-Null
        if (-not (Test-Path (Join-Path $bin 'initdb.exe'))) {
            $zip = Join-Path $env:TEMP "pg$Version-binaries.zip"
            if (-not (Test-Path $zip)) {
                $url = "https://get.enterprisedb.com/postgresql/postgresql-$Version-1-windows-x64-binaries.zip"
                Write-Host "downloading $url" -ForegroundColor Cyan
                & curl.exe -L --fail --retry 2 -o $zip $url
                if ($LASTEXITCODE -ne 0) { throw "download failed (exit $LASTEXITCODE)" }
            }
            Write-Host "unpacking to $PgHome" -ForegroundColor Cyan
            & tar.exe -xf $zip -C $PgHome
            if ($LASTEXITCODE -ne 0) { throw "unpack failed (exit $LASTEXITCODE)" }
        }
        if (-not (Test-Path (Join-Path $data 'PG_VERSION'))) {
            # trust auth on loopback only: this instance is for local development.
            & (Find-PgTool 'initdb') -D $data -U postgres -E UTF8 --locale=C `
                --auth-local=trust --auth-host=trust
            if ($LASTEXITCODE -ne 0) { throw "initdb failed (exit $LASTEXITCODE)" }
        }
        Write-Host "initialised: $data" -ForegroundColor Green
        Write-Host "next: pwsh -File scripts/pg-local.ps1 -Action start" -ForegroundColor Green
    }
    'start' {
        $postgres = Find-PgTool 'postgres'
        $already = Get-Process postgres -ErrorAction SilentlyContinue
        if ($already) { Write-Host "postgres is already running (pid $($already[0].Id))" -ForegroundColor Yellow; break }
        # Start-Process, not pg_ctl: pg_ctl leaves the postmaster attached to the
        # console, which means a wrapper script never returns.
        $proc = Start-Process -FilePath $postgres `
            -ArgumentList @('-D', $data, '-p', "$Port", '-c', 'listen_addresses=127.0.0.1') `
            -RedirectStandardOutput (Join-Path $PgHome 'pg.out.log') `
            -RedirectStandardError $log -PassThru -WindowStyle Hidden
        for ($i = 0; $i -lt 40; $i++) {
            Start-Sleep -Milliseconds 500
            & (Find-PgTool 'pg_isready') -h 127.0.0.1 -p $Port *> $null
            if ($LASTEXITCODE -eq 0) {
                Write-Host "ready on 127.0.0.1:$Port (pid $($proc.Id))" -ForegroundColor Green
                return
            }
        }
        throw "postgres did not become ready; see $log"
    }
    'stop' {
        & (Find-PgTool 'pg_ctl') -D $data -m fast stop
        Write-Host "stopped" -ForegroundColor Green
    }
    'status' {
        & (Find-PgTool 'pg_isready') -h 127.0.0.1 -p $Port
        "exit=$LASTEXITCODE"
        Invoke-Psql @('-d', $Database, '-tAc', 'select version()')
    }
    'migrate' {
        # A one-row-per-file ledger, not a real migration framework: enough to
        # make `migrate` idempotent for a dev box, and enough to be honest that
        # applying a file and recording it are two statements, not one.
        Invoke-Psql @('-d', $Database, '-c',
            'CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())')
        Get-ChildItem (Join-Path $repo 'db/migrations') -Filter '*.sql' | Sort-Object Name | ForEach-Object {
            $name = $_.Name
            $done = Invoke-Psql @('-d', $Database, '-tAc', "SELECT 1 FROM schema_migrations WHERE version = '$name'")
            if ($done) {
                Write-Host "skipping $name (already applied)" -ForegroundColor DarkGray
                return
            }
            Write-Host "applying $name" -ForegroundColor Cyan
            Invoke-Psql @('-d', $Database, '-f', $_.FullName)
            Invoke-Psql @('-d', $Database, '-c', "INSERT INTO schema_migrations (version) VALUES ('$name')")
        }
    }
    'probe' {
        Get-ChildItem (Join-Path $repo 'db/probes') -Filter '*.probe.sql' | Sort-Object Name | ForEach-Object {
            Write-Host "probing $($_.Name)" -ForegroundColor Cyan
            Invoke-Psql @('-d', $Database, '-t', '-q', '-f', $_.FullName) | ForEach-Object { "$_" }
        }
        Write-Host "all probes passed" -ForegroundColor Green
    }
    'psql' {
        & (Find-PgTool 'psql') -h 127.0.0.1 -p $Port -U postgres -d $Database
    }
}