[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateRange(1024, 65535)][int]$Port,
    [string]$DataRoot = "$env:ProgramData\dbgold"
)

$ErrorActionPreference = 'Stop'
$configFile = Join-Path $DataRoot 'config\dbgold.env'
if (-not (Test-Path $configFile)) { throw "Configuration not found: $configFile" }
$content = Get-Content $configFile -Raw
$oldPortLine = Get-Content $configFile | Where-Object { $_ -match '^PORT=' } | Select-Object -First 1
$oldPort = [int]($oldPortLine -replace '^PORT=', '')
if ($oldPort -eq $Port) {
    Write-Host "dbgold is already configured for port $Port"
    exit 0
}
if (Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue) {
    throw "Port $Port is already in use."
}
$content = $content -replace '(?m)^PORT=.*$', "PORT=$Port"
[IO.File]::WriteAllText($configFile, $content, [Text.UTF8Encoding]::new($false))
Restart-Service dbgold
foreach ($attempt in 1..30) {
    try {
        if ((Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port/api/health/ready" -TimeoutSec 3).StatusCode -eq 200) {
            Write-Host "dbgold is ready on 0.0.0.0:$Port"
            exit 0
        }
    } catch { Start-Sleep -Seconds 1 }
}
$content = (Get-Content $configFile -Raw) -replace '(?m)^PORT=.*$', "PORT=$oldPort"
[IO.File]::WriteAllText($configFile, $content, [Text.UTF8Encoding]::new($false))
Restart-Service dbgold -ErrorAction SilentlyContinue
throw 'dbgold failed its readiness check after changing the port.'
