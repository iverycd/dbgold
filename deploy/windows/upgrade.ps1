[CmdletBinding()]
param(
    [string]$InstallDir = "$env:ProgramFiles\dbgold",
    [string]$DataRoot = "$env:ProgramData\dbgold",
    [switch]$ConfirmNoRunningTasks
)

$ErrorActionPreference = 'Stop'
if (-not $ConfirmNoRunningTasks) { throw 'Confirm that no migration task is running with -ConfirmNoRunningTasks.' }
$SourceDir = Split-Path -Parent $MyInvocation.MyCommand.Path
foreach ($required in @('dbgold.exe', 'VERSION', 'web\index.html')) {
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
Stop-Service $ServiceName
$backupFile = $null
$programBackup = $null

try {
    $backupFile = & (Join-Path $SourceDir 'backup.ps1') -DataRoot $DataRoot -AlreadyStopped
    $programBackup = Join-Path (Join-Path $DataRoot 'backups') ("program-{0}" -f [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))
    New-Item -ItemType Directory -Path $programBackup -Force | Out-Null
    Copy-Item (Join-Path $InstallDir 'dbgold.exe') $programBackup
    Copy-Item (Join-Path $InstallDir 'VERSION') $programBackup -ErrorAction SilentlyContinue
    Copy-Item (Join-Path $InstallDir 'web') $programBackup -Recurse

    Copy-Item (Join-Path $SourceDir 'dbgold.exe') (Join-Path $InstallDir 'dbgold.exe') -Force
    Copy-Item (Join-Path $SourceDir 'VERSION') (Join-Path $InstallDir 'VERSION') -Force
    Remove-Item (Join-Path $InstallDir 'web') -Recurse -Force
    Copy-Item (Join-Path $SourceDir 'web') (Join-Path $InstallDir 'web') -Recurse
    foreach ($script in @('backup.ps1', 'restore.ps1', 'upgrade.ps1', 'set-port.ps1', 'uninstall.ps1')) {
        Copy-Item (Join-Path $SourceDir $script) (Join-Path $InstallDir $script) -Force
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
    if ($programBackup -and (Test-Path (Join-Path $programBackup 'dbgold.exe'))) {
        Copy-Item (Join-Path $programBackup 'dbgold.exe') (Join-Path $InstallDir 'dbgold.exe') -Force
        Copy-Item (Join-Path $programBackup 'VERSION') (Join-Path $InstallDir 'VERSION') -Force -ErrorAction SilentlyContinue
        if (Test-Path (Join-Path $InstallDir 'web')) { Remove-Item (Join-Path $InstallDir 'web') -Recurse -Force }
        Copy-Item (Join-Path $programBackup 'web') (Join-Path $InstallDir 'web') -Recurse
    }
    if ($backupFile -and (Test-Path $backupFile -PathType Container)) {
        & (Join-Path $SourceDir 'restore.ps1') -Backup $backupFile -DataRoot $DataRoot -ConfirmRestore
    } else {
        Start-Service $ServiceName -ErrorAction SilentlyContinue
    }
    throw
}
