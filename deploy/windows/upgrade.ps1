[CmdletBinding()]
param(
    [string]$InstallDir = "$env:ProgramFiles\dbgold",
    [string]$DataRoot = "$env:ProgramData\dbgold",
    [switch]$ConfirmNoRunningTasks
)

$ErrorActionPreference = 'Stop'
if (-not $ConfirmNoRunningTasks) { throw 'Confirm that no migration task is running with -ConfirmNoRunningTasks.' }
$SourceDir = Split-Path -Parent $MyInvocation.MyCommand.Path
foreach ($required in @('dbgold.exe', 'VERSION', 'web\index.html', 'lib\dbgold-oscar-bridge.jar', 'lib\oscarJDBC8.jar')) {
    if (-not (Test-Path (Join-Path $SourceDir $required))) { throw "Release file is missing: $required" }
}
if (-not (Test-Path (Join-Path $SourceDir 'manifest.sha256'))) { throw 'Release checksum manifest is missing.' }
foreach ($line in Get-Content (Join-Path $SourceDir 'manifest.sha256')) {
    if ($line -notmatch '^([0-9a-fA-F]{64})\s+(.+)$') { throw "Invalid checksum line: $line" }
    $expected = $Matches[1].ToLowerInvariant()
    $relativePath = $Matches[2]
    $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $SourceDir $relativePath)).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum mismatch: $relativePath" }
}

$ServiceName = 'dbgold'
if (-not (Test-Path -LiteralPath (Join-Path $InstallDir 'dbgold.exe') -PathType Leaf) -or
    -not (Test-Path -LiteralPath (Join-Path $InstallDir 'web') -PathType Container)) {
    throw "Existing dbgold installation not found at $InstallDir"
}
Stop-Service $ServiceName
$backupFile = $null
$programBackup = $null
$backupReady = $false
# Both a legacy bundled JRE and a user-provided runtime/ stay in place.
$programItems = @('dbgold.exe', 'VERSION', 'web', 'lib', 'backup.ps1', 'restore.ps1', 'upgrade.ps1', 'set-port.ps1', 'uninstall.ps1')

try {
    $backupFile = & (Join-Path $SourceDir 'backup.ps1') -DataRoot $DataRoot -AlreadyStopped
    $programBackup = Join-Path (Join-Path $DataRoot 'backups') ("program-{0}" -f [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))
    New-Item -ItemType Directory -Path $programBackup | Out-Null
    foreach ($item in $programItems) {
        $existing = Join-Path $InstallDir $item
        if (Test-Path -LiteralPath $existing) {
            Copy-Item -LiteralPath $existing -Destination (Join-Path $programBackup $item) -Recurse
        }
    }
    $backupReady = $true
    foreach ($item in $programItems) {
        $destination = Join-Path $InstallDir $item
        if (Test-Path -LiteralPath $destination) { Remove-Item -LiteralPath $destination -Recurse -Force }
        Copy-Item -LiteralPath (Join-Path $SourceDir $item) -Destination $destination -Recurse
    }
    Start-Service $ServiceName
    $configFile = Join-Path $DataRoot 'config\dbgold.env'
    $portLine = Get-Content $configFile | Where-Object { $_ -match '^PORT=' } | Select-Object -First 1
    $port = [int]($portLine -replace '^PORT=', '')
    foreach ($attempt in 1..30) {
        try {
            if ((Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/api/health/ready" -TimeoutSec 3).StatusCode -eq 200) {
                Write-Host "Upgrade completed. Cold backup: $backupFile"
                exit 0
            }
        } catch { Start-Sleep -Seconds 1 }
    }
    throw 'The upgraded service failed its readiness check.'
} catch {
    Stop-Service $ServiceName -ErrorAction SilentlyContinue
    if ($backupReady) {
        foreach ($item in $programItems) {
            $saved = Join-Path $programBackup $item
            $destination = Join-Path $InstallDir $item
            if (Test-Path -LiteralPath $destination) { Remove-Item -LiteralPath $destination -Recurse -Force }
            if (Test-Path -LiteralPath $saved) { Copy-Item -LiteralPath $saved -Destination $destination -Recurse }
        }
        & (Join-Path $SourceDir 'restore.ps1') -Backup $backupFile -DataRoot $DataRoot -ConfirmRestore
    } else {
        Start-Service $ServiceName -ErrorAction SilentlyContinue
    }
    throw
}
