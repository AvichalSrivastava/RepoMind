# scripts/docker-pull-images.ps1
# -----------------------------------------------------------------------------
# Pre-pull the external Docker images that `docker compose up` depends on.
#
# Why this script exists:
#   * On first `compose up`, Docker pulls these images on demand. That stalls
#     the boot for ~30s on a fast link, longer on coffee-shop wifi, and fails
#     completely offline. Running this once warms the local cache so compose
#     boots in seconds.
#   * Pinning image tags here next to the ones in deploy/docker-compose.yml
#     gives a single place to bump versions; the script's `Test` step catches
#     drift between the two locations.
#
# Image choices (kept in sync with deploy/docker-compose.yml):
#   postgres:16-alpine
#     * PG 16 — current production line; mature LISTEN/NOTIFY and
#       data-modifying CTEs used by the outbox dispatcher (M9/M10).
#     * Alpine base — ~80 MB vs ~140 MB for debian-slim. We use no
#       extensions that require glibc, so Alpine is the right trade.
#   migrate/migrate:v4.17.1
#     * Same tool the Makefile's `migrate-up` target uses, so dev-host
#       and compose stack apply migrations identically.
#     * Version pinned (not `:latest`) for reproducible builds.
#
# Usage:
#   pwsh scripts/docker-pull-images.ps1          # pull both
#   pwsh scripts/docker-pull-images.ps1 -Verify  # also assert tags match compose
# -----------------------------------------------------------------------------

[CmdletBinding()]
param(
    # When set, also re-read deploy/docker-compose.yml and fail if any
    # image tag here drifted from the compose file.
    [switch]$Verify
)

$ErrorActionPreference = 'Stop'

# Source-of-truth image list. Mirror deploy/docker-compose.yml.
$images = @(
    [pscustomobject]@{ Name = 'postgres';        Tag = '16-alpine' }
    [pscustomobject]@{ Name = 'migrate/migrate'; Tag = 'v4.17.1'   }
)

function Assert-DockerRunning {
    try {
        docker info --format '{{.ServerVersion}}' *> $null
        if ($LASTEXITCODE -ne 0) { throw "docker info exit $LASTEXITCODE" }
    } catch {
        Write-Host "ERROR: Docker daemon not reachable. Start Docker Desktop and retry." -ForegroundColor Red
        exit 2
    }
}

function Get-ComposeImages {
    $composePath = Join-Path $PSScriptRoot '..\deploy\docker-compose.yml'
    if (-not (Test-Path $composePath)) {
        throw "compose file not found at $composePath"
    }
    Get-Content $composePath |
        Select-String -Pattern '^\s+image:\s*(\S+)' |
        ForEach-Object { $_.Matches.Groups[1].Value }
}

function Test-TagDrift {
    $declared = $images | ForEach-Object { "$($_.Name):$($_.Tag)" }
    $compose  = Get-ComposeImages
    $drift = $false
    foreach ($d in $declared) {
        if ($compose -notcontains $d) {
            Write-Host "DRIFT: '$d' declared in script but not in compose" -ForegroundColor Yellow
            $drift = $true
        }
    }
    foreach ($c in $compose) {
        if ($declared -notcontains $c) {
            Write-Host "DRIFT: '$c' in compose but not declared in script" -ForegroundColor Yellow
            $drift = $true
        }
    }
    if ($drift) {
        Write-Host "Resolve the drift (update either this script or the compose file)." -ForegroundColor Red
        exit 3
    }
    Write-Host "tag verification OK (script and compose agree)" -ForegroundColor Green
}

function Pull-Image {
    param([string]$ref)

    Write-Host ""
    Write-Host "pulling $ref ..." -ForegroundColor Cyan
    docker pull $ref
    if ($LASTEXITCODE -ne 0) {
        Write-Host "FAILED to pull $ref (exit $LASTEXITCODE)" -ForegroundColor Red
        exit 1
    }
}

function Show-ImageSummary {
    Write-Host ""
    Write-Host "=== local cache summary ===" -ForegroundColor Cyan
    $refs = $images | ForEach-Object { "$($_.Name):$($_.Tag)" }
    docker image ls --format 'table {{.Repository}}:{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}' |
        Select-String -Pattern ('^(REPOSITORY|' + (($refs | ForEach-Object { [regex]::Escape($_) }) -join '|') + ')')
}

# -----------------------------------------------------------------------------
# main
# -----------------------------------------------------------------------------

Assert-DockerRunning

if ($Verify) { Test-TagDrift }

foreach ($img in $images) {
    Pull-Image -ref "$($img.Name):$($img.Tag)"
}

Show-ImageSummary

Write-Host ""
Write-Host "Done. Next: 'make docker-up' (or 'docker compose -f deploy/docker-compose.yml --env-file .env --env-file .env.secrets up')." -ForegroundColor Green
