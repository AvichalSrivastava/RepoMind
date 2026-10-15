# scripts/run-dev.ps1 — start RepoMind locally with dev config.
#
# Usage (from repo root):
#   pwsh ./scripts/run-dev.ps1
#
# Load order (later wins, so .env overrides config.dev.env):
#   1. configs/config.dev.env   (committed template — SHL site, no secrets)
#   2. .env                     (gitignored — your real tokens)

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

function Import-DotEnv {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return }
    Write-Host "loading $Path" -ForegroundColor DarkGray
    Get-Content $Path | ForEach-Object {
        $line = $_.Trim()
        if (-not $line -or $line.StartsWith('#')) { return }
        $kv = $line -split '=', 2
        if ($kv.Count -ne 2) { return }
        $name  = $kv[0].Trim()
        $value = $kv[1].Trim().Trim('"').Trim("'")
        [Environment]::SetEnvironmentVariable($name, $value, 'Process')
    }
}

Import-DotEnv 'configs/config.dev.env'
Import-DotEnv '.env'

if (-not $env:REPOMIND_JIRA_API_TOKEN) {
    Write-Warning "REPOMIND_JIRA_API_TOKEN is empty. Create one at https://id.atlassian.com/manage-profile/security/api-tokens and put it in .env"
}

go run ./cmd/repomind
