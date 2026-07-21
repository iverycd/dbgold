[CmdletBinding()]
param(
    [string]$InstallDir = "$env:ProgramFiles\dbgold",
    [switch]$RemoveProgramFiles
)

$ErrorActionPreference = 'Stop'
$ServiceName = 'dbgold'
$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($service) {
    if ($service.Status -ne 'Stopped') { Stop-Service $ServiceName }
    & sc.exe delete $ServiceName | Out-Null
}
Get-NetFirewallRule -DisplayName 'dbgold (Domain and Private LAN)' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
if ($RemoveProgramFiles -and (Test-Path $InstallDir)) {
    Remove-Item $InstallDir -Recurse -Force
}
Write-Host 'dbgold service was removed. C:\ProgramData\dbgold was preserved.'
