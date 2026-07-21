[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Backup,
    [string]$DataRoot = "$env:ProgramData\dbgold",
    [switch]$ConfirmRestore
)

$ErrorActionPreference = 'Stop'
if (-not $ConfirmRestore) { throw 'Restore replaces current data. Re-run with -ConfirmRestore.' }
if (-not (Test-Path $Backup -PathType Container)) { throw "Backup snapshot not found: $Backup" }
$ServiceName = 'dbgold'
Stop-Service $ServiceName -ErrorAction SilentlyContinue
$safetyDir = Join-Path (Join-Path $DataRoot 'backups') ("pre-restore-{0}" -f [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))
New-Item -ItemType Directory -Path $safetyDir -Force | Out-Null
foreach ($item in @('data', 'uploads', 'config')) {
    $current = Join-Path $DataRoot $item
    if (Test-Path $current) { Move-Item $current (Join-Path $safetyDir $item) }
}
try {
    foreach ($item in @('data', 'uploads', 'config')) {
        $source = Join-Path $Backup $item
        if (Test-Path $source) { Copy-Item $source (Join-Path $DataRoot $item) -Recurse }
    }
    & icacls.exe $DataRoot /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-19:(OI)(CI)M' | Out-Null
    Start-Service $ServiceName
    $configFile = Join-Path $DataRoot 'config\dbgold.env'
    $portLine = Get-Content $configFile | Where-Object { $_ -match '^PORT=' } | Select-Object -First 1
    $port = [int]($portLine -replace '^PORT=', '')
    $ready = $false
    foreach ($attempt in 1..30) {
        try {
            if ((Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/api/health/ready" -TimeoutSec 3).StatusCode -eq 200) {
                $ready = $true
                break
            }
        } catch { Start-Sleep -Seconds 1 }
    }
    if (-not $ready) { throw 'The restored service failed its readiness check.' }
    Write-Host "Restore completed. Pre-restore data is retained at $safetyDir"
} catch {
    foreach ($item in @('data', 'uploads', 'config')) {
        $restored = Join-Path $DataRoot $item
        if (Test-Path $restored) { Move-Item $restored (Join-Path $safetyDir ("failed-" + $item)) }
        $original = Join-Path $safetyDir $item
        if (Test-Path $original) { Move-Item $original $restored }
    }
    Start-Service $ServiceName -ErrorAction SilentlyContinue
    throw
}
