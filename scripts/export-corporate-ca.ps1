# scripts/export-corporate-ca.ps1
# -----------------------------------------------------------------------------
# Export corporate TLS-interception CAs from the Windows trust store
# into secrets/corporate-ca-bundle.crt for use during `docker compose build`.
#
# Why this script exists:
#   Many corporate networks (SHL uses Netskope + Zscaler) intercept
#   outbound HTTPS to inspect traffic. The host trusts the corporate
#   root CA, but a fresh Alpine container in `docker build` doesn't —
#   so `go mod download` fails with `x509: certificate signed by
#   unknown authority` when reaching proxy.golang.org / sum.golang.org.
#
#   This script scans the Windows machine root cert store for known
#   interceptor names, exports matching certs as a PEM bundle to
#   secrets/corporate-ca-bundle.crt, and the Dockerfile (builder
#   stage) appends that bundle to the container's system trust before
#   the go mod step.
#
# Usage:
#   pwsh scripts/export-corporate-ca.ps1                 # auto-detect
#   pwsh scripts/export-corporate-ca.ps1 -Thumbprint <T> # force-include
#   pwsh scripts/export-corporate-ca.ps1 -Empty          # write empty file
#                                                         (devs on
#                                                         non-corp networks
#                                                         still need the
#                                                         file to exist)
# -----------------------------------------------------------------------------

[CmdletBinding()]
param(
    # Extra thumbprints to force-include even if they don't match the
    # interceptor name patterns below. Comma-separated or array.
    [string[]]$Thumbprint = @(),

    # Write an empty bundle file (for devs on networks without TLS
    # interception — Docker still needs the file to exist).
    [switch]$Empty
)

$ErrorActionPreference = 'Stop'

$outPath = Join-Path $PSScriptRoot '..\secrets\corporate-ca-bundle.crt'
$outDir  = Split-Path $outPath -Parent
if (-not (Test-Path $outDir)) { New-Item -Path $outDir -ItemType Directory | Out-Null }

if ($Empty) {
    Set-Content -Path $outPath -Encoding ASCII -Value '# Intentionally empty - dev not behind TLS interception.'
    Write-Host "Wrote empty bundle: $outPath" -ForegroundColor Green
    exit 0
}

# Patterns known to indicate corporate TLS interception.
# Add more as you encounter them on other corporate networks.
$interceptorPattern = 'Zscaler|Netskope|Forcepoint|Palo Alto|Bluecoat|Cisco Umbrella|McAfee|Symantec|Sophos|Check Point|Fortinet|Trend|SHL|Corporate|Proxy|MITM'

Write-Host "Scanning Windows root cert store for interceptors..." -ForegroundColor Cyan
$matched = Get-ChildItem Cert:\LocalMachine\Root |
    Where-Object { $_.Subject -match $interceptorPattern }

# Add any user-supplied thumbprints.
foreach ($t in $Thumbprint) {
    $cert = Get-Item "Cert:\LocalMachine\Root\$t" -ErrorAction SilentlyContinue
    if ($cert -and ($matched.Thumbprint -notcontains $cert.Thumbprint)) {
        $matched += $cert
    }
}

if ($matched.Count -eq 0) {
    Write-Host "No interceptor CAs found." -ForegroundColor Yellow
    Write-Host "If you're not behind a corporate proxy, run with -Empty." -ForegroundColor Yellow
    Write-Host "If you ARE but the pattern missed it, find your CA via:" -ForegroundColor Yellow
    Write-Host "  Get-ChildItem Cert:\LocalMachine\Root | Format-List Subject, Thumbprint"
    Write-Host "Then re-run with: -Thumbprint <THUMBPRINT>"
    exit 1
}

Write-Host ""
Write-Host "Found $($matched.Count) certificate(s):" -ForegroundColor Green
$matched | ForEach-Object { Write-Host "  - $($_.Subject)" }

# Write PEM bundle.
$lines = New-Object System.Collections.Generic.List[string]
foreach ($cert in $matched) {
    $lines.Add("# Subject: $($cert.Subject)") | Out-Null
    $lines.Add("# Thumbprint: $($cert.Thumbprint)") | Out-Null
    $lines.Add("# NotAfter: $($cert.NotAfter)")    | Out-Null
    $lines.Add('-----BEGIN CERTIFICATE-----')      | Out-Null
    $b64 = [Convert]::ToBase64String($cert.RawData)
    for ($i = 0; $i -lt $b64.Length; $i += 64) {
        $lines.Add($b64.Substring($i, [Math]::Min(64, $b64.Length - $i))) | Out-Null
    }
    $lines.Add('-----END CERTIFICATE-----') | Out-Null
    $lines.Add('') | Out-Null
}

Set-Content -Path $outPath -Encoding ASCII -Value $lines

Write-Host ""
Write-Host "Wrote $outPath ($((Get-Item $outPath).Length) bytes, $($matched.Count) certs)" -ForegroundColor Green

# Sanity check: gitignored?
$ignored = $false
try {
    $rel = Resolve-Path $outPath -Relative
    git check-ignore -q $rel 2>$null
    if ($LASTEXITCODE -eq 0) { $ignored = $true }
} catch {}
if ($ignored) {
    Write-Host "Bundle is gitignored (via secrets/* rule)." -ForegroundColor Green
} else {
    Write-Host "WARNING: bundle is NOT gitignored. Check .gitignore!" -ForegroundColor Red
    exit 2
}
