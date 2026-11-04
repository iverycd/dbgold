# Exercise upgrade.ps1 against disposable directories. Service, HTTP and ACL
# commands are mocked so this also runs under PowerShell on macOS/Linux.
$ErrorActionPreference = 'Stop'
$work = Join-Path ([IO.Path]::GetTempPath()) ("dbgold-windows-upgrade-" + [guid]::NewGuid())
$package = Join-Path $work 'new package'
$utf8 = [Text.UTF8Encoding]::new($false)

function Write-FixtureFile($root, $name, $value) {
    $path = Join-Path $root $name
    [IO.Directory]::CreateDirectory((Split-Path -Parent $path)) | Out-Null
    [IO.File]::WriteAllText($path, $value, $utf8)
}

function Assert-Content($root, $name, $expected) {
    $path = Join-Path $root $name
    if ([IO.File]::ReadAllText($path) -cne $expected) { throw "Unexpected content at $path" }
}

function Get-Service { [CmdletBinding()] param($Name) return @{Status = 'Stopped'} }
function Stop-Service { [CmdletBinding()] param($Name) }
function Start-Service {
    [CmdletBinding()] param($Name)
    if ([IO.File]::ReadAllText((Join-Path $install 'VERSION')) -eq 'new') {
        Write-FixtureFile $data 'data/dbgold.db' 'migrated'
        if ($hadRuntime) {
            # Changes to operator-controlled Java during verification must survive a rollback.
            Write-FixtureFile $install 'runtime/site config' 'operator change'
        }
    }
}
function icacls.exe { }
function Invoke-WebRequest {
    param($Uri, [switch]$UseBasicParsing, $TimeoutSec)
    $version = [IO.File]::ReadAllText((Join-Path $install 'VERSION'))
    if ($failProbe -and $version -eq 'new') { return @{StatusCode = 503} }
    return @{StatusCode = 200}
}

try {
    New-Item -ItemType Directory $package -Force | Out-Null
    Copy-Item (Join-Path $PSScriptRoot '../windows/*.ps1') $package
    foreach ($name in @('dbgold.exe', 'VERSION', 'web/index.html', 'lib/dbgold-oscar-bridge.jar', 'lib/oscarJDBC8.jar')) {
        Write-FixtureFile $package $name 'new'
    }
    $manifest = foreach ($file in Get-ChildItem $package -File -Recurse) {
        $relative = $file.FullName.Substring($package.Length + 1).Replace('\', '/')
        $hash = (Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $relative"
    }
    [IO.File]::WriteAllLines((Join-Path $package 'manifest.sha256'), [string[]]$manifest, $utf8)
    foreach ($hadRuntime in @($false, $true)) {
        foreach ($failProbe in @($false, $true)) {
            $install = Join-Path $work "existing $hadRuntime $failProbe"
            $data = Join-Path $work "data $hadRuntime $failProbe"
            foreach ($name in @('dbgold.exe', 'VERSION', 'web/index.html', 'lib/oscarJDBC8.jar', 'upgrade.ps1')) {
                Write-FixtureFile $install $name 'old'
            }
            if ($hadRuntime) {
                Write-FixtureFile $install 'runtime/site config' 'original'
                Write-FixtureFile $install 'runtime/bin/java.exe' 'original java'
            }
            $config = "PORT=18089`nOSCAR_JAVA_BIN=C:/External Java/bin/java.exe`n"
            Write-FixtureFile $data 'config/dbgold.env' $config
            Write-FixtureFile $data 'data/dbgold.db' 'original'
            $failed = $false
            try {
                & (Join-Path $package 'upgrade.ps1') -InstallDir $install -DataRoot $data -ConfirmNoRunningTasks
            } catch {
                if (-not $failProbe -or $_.Exception.Message -notlike '*readiness check*') { throw }
                $failed = $true
            }
            if ($failed -ne $failProbe) { throw "Unexpected upgrade outcome for $install" }
            Assert-Content $data 'config/dbgold.env' $config
            if ($failProbe) {
                Assert-Content $data 'data/dbgold.db' 'original'
                Assert-Content $install 'upgrade.ps1' 'old'
                Assert-Content $install 'dbgold.exe' 'old'
            } else {
                Assert-Content $data 'data/dbgold.db' 'migrated'
                Assert-Content $install 'dbgold.exe' 'new'
            }
            if ($hadRuntime) {
                Assert-Content $install 'runtime/site config' 'operator change'
                Assert-Content $install 'runtime/bin/java.exe' 'original java'
            }
            elseif (Test-Path (Join-Path $install 'runtime')) { throw 'Upgrade unexpectedly created a JRE' }
        }
    }
    Write-Host 'Windows upgrade and rollback checks passed.'
} finally {
    if (Test-Path -LiteralPath $work) { Remove-Item -LiteralPath $work -Recurse -Force }
}
