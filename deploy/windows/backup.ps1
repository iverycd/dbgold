[CmdletBinding()]
param(
    [string]$DataRoot = "$env:ProgramData\dbgold",
    [switch]$AlreadyStopped
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'dbgold'
$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
$wasRunning = $service -and $service.Status -eq 'Running'
if ($wasRunning -and -not $AlreadyStopped) { Stop-Service $ServiceName }
try {
    $backupDir = Join-Path $DataRoot 'backups'
    New-Item -ItemType Directory -Path $backupDir -Force | Out-Null
    $snapshot = Join-Path $backupDir ("dbgold-{0}" -f [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))
    New-Item -ItemType Directory -Path $snapshot | Out-Null
    foreach ($item in @('data', 'uploads', 'config')) {
        $source = Join-Path $DataRoot $item
        if (Test-Path $source) { Copy-Item $source (Join-Path $snapshot $item) -Recurse }
    }
    Write-Output $snapshot
} finally {
    if ($wasRunning -and -not $AlreadyStopped) { Start-Service $ServiceName }
}
