[CmdletBinding()]
param(
    [ValidateRange(1024, 65535)][int]$Port = 18089,
    [string]$InstallDir = "$env:ProgramFiles\dbgold",
    [string]$DataRoot = "$env:ProgramData\dbgold"
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'dbgold'
$SourceDir = Split-Path -Parent $MyInvocation.MyCommand.Path

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run install.ps1 from an elevated PowerShell window.'
}
if (-not [Environment]::Is64BitOperatingSystem) { throw 'Windows x64 is required.' }
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) { throw "Service $ServiceName already exists. Use upgrade.ps1." }
if (Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue) {
    throw "Port $Port is already in use. Choose another value with -Port."
}
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

$ConfigDir = Join-Path $DataRoot 'config'
foreach ($dir in @($InstallDir, $DataRoot, $ConfigDir, (Join-Path $DataRoot 'data'), (Join-Path $DataRoot 'uploads'), (Join-Path $DataRoot 'logs'), (Join-Path $DataRoot 'backups'))) {
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
}
Copy-Item (Join-Path $SourceDir 'dbgold.exe') (Join-Path $InstallDir 'dbgold.exe') -Force
Copy-Item (Join-Path $SourceDir 'VERSION') (Join-Path $InstallDir 'VERSION') -Force
if (Test-Path (Join-Path $InstallDir 'web')) { Remove-Item (Join-Path $InstallDir 'web') -Recurse -Force }
Copy-Item (Join-Path $SourceDir 'web') (Join-Path $InstallDir 'web') -Recurse
foreach ($script in @('backup.ps1', 'restore.ps1', 'upgrade.ps1', 'set-port.ps1', 'uninstall.ps1')) {
    Copy-Item (Join-Path $SourceDir $script) (Join-Path $InstallDir $script) -Force
}

$ConfigFile = Join-Path $ConfigDir 'dbgold.env'
if (-not (Test-Path $ConfigFile)) {
    function New-RandomHex([int]$ByteCount) {
        $bytes = New-Object byte[] $ByteCount
        $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
        try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
        return -join ($bytes | ForEach-Object { $_.ToString('x2') })
    }
    $jwt = New-RandomHex 32
    $adminPassword = 'Db!' + (New-RandomHex 10)
    $configText = Get-Content (Join-Path $SourceDir 'dbgold.env.example') -Raw
    $configText = $configText.Replace('__GENERATED_BY_INSTALLER__', $jwt)
    $configText = $configText -replace '(?m)^ADMIN_PASS=.*$', "ADMIN_PASS=$adminPassword"
    $configText = $configText -replace '(?m)^PORT=.*$', "PORT=$Port"
    $configText = $configText -replace '(?m)^STATIC_DIR=.*$', ('STATIC_DIR="' + ($InstallDir -replace '\\','/') + '/web"')
    $configText = $configText -replace '(?m)^SQLITE_PATH=.*$', ('SQLITE_PATH=' + ($DataRoot -replace '\\','/') + '/data/dbgold.db')
    $configText = $configText -replace '(?m)^UPLOAD_DIR=.*$', ('UPLOAD_DIR=' + ($DataRoot -replace '\\','/') + '/uploads')
    $configText = $configText -replace '(?m)^LOG_DIR=.*$', ('LOG_DIR=' + ($DataRoot -replace '\\','/') + '/logs')
    [IO.File]::WriteAllText($ConfigFile, $configText, [Text.UTF8Encoding]::new($false))
    Write-Host "Initial administrator: admin"
    Write-Host "Initial administrator password: $adminPassword"
    Write-Host 'Store this password securely; it will not be printed again.'
}

& icacls.exe $DataRoot /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-19:(OI)(CI)M' | Out-Null
$binaryPath = '"' + (Join-Path $InstallDir 'dbgold.exe') + '" serve --config "' + $ConfigFile + '"'
& sc.exe create $ServiceName binPath= $binaryPath start= delayed-auto obj= 'NT AUTHORITY\LocalService' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Failed to create the dbgold Windows service.' }
& sc.exe description $ServiceName 'dbgold database migration service' | Out-Null
& sc.exe failure $ServiceName reset= 86400 actions= 'restart/5000/restart/15000/restart/60000' | Out-Null

$firewallName = 'dbgold (Domain and Private LAN)'
Get-NetFirewallRule -DisplayName $firewallName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName $firewallName -Direction Inbound -Action Allow -Profile Domain,Private -RemoteAddress LocalSubnet -Protocol TCP -Program (Join-Path $InstallDir 'dbgold.exe') | Out-Null

Start-Service $ServiceName
$healthURL = "http://127.0.0.1:$Port/api/health/ready"
foreach ($attempt in 1..30) {
    try {
        $result = Invoke-WebRequest -UseBasicParsing -Uri $healthURL -TimeoutSec 3
        if ($result.StatusCode -eq 200) {
            Write-Host "dbgold is ready on 0.0.0.0:$Port"
            exit 0
        }
    } catch { Start-Sleep -Seconds 1 }
}
Get-Content (Join-Path $DataRoot 'logs\*.log') -Tail 100 -ErrorAction SilentlyContinue
throw 'dbgold failed its readiness check.'
