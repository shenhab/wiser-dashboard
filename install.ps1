# Installs or upgrades Wiser Dashboard on Windows, from the latest release.
#
#   irm https://raw.githubusercontent.com/shenhab/wiser-dashboard/main/install.ps1 | iex
#
# Options, set before running it:  $env:WISER_VERSION = "v1.3.1"   (instead of the latest)
#   $env:WISER_PORT = "8080"   $env:WISER_HUB = "192.168.1.50"   $env:WISER_UNINSTALL = "1"
#   $env:WISER_SECRET = "..."  (the hub secret; otherwise the dashboard asks for it)
#
# The download is checked against the release's SHA-256 checksums (Windows
# PowerShell has no Ed25519 to check their signature; the dashboard checks the
# signature of every later update itself).
#
# Installs for your user in %LOCALAPPDATA%\wiser-dashboard, starts it minimized
# at login, allows it through the firewall on private networks (when run as
# administrator; otherwise Windows asks on first start), and checks it runs.
# Running it again upgrades in place, keeping settings, history and pairings.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # downloads are many times faster without the progress bar
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = 'shenhab/wiser-dashboard'
$Dir = Join-Path $env:LOCALAPPDATA 'wiser-dashboard'
$Exe = Join-Path $Dir 'wiser-dashboard.exe'
$Shortcut = Join-Path ([Environment]::GetFolderPath('Startup')) 'Wiser Dashboard.lnk'
$State = Join-Path $env:APPDATA 'wiser-dashboard'
$Port = if ($env:WISER_PORT) { [int]$env:WISER_PORT } else { 8080 }
$RuleName = 'Wiser Dashboard'

function Say($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Fail($msg) { Write-Host "error: $msg" -ForegroundColor Red; throw $msg }

function Stop-Dashboard {
    Get-Process -Name 'wiser-dashboard' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2 # the Matter bridge stops by itself once the dashboard is gone
}

function Test-Dashboard {
    try { Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 "http://127.0.0.1:$Port/api/state" | Out-Null; $true } catch { $false }
}

$IsAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

# ---------------------------------------------------------------- uninstall
if ($env:WISER_UNINSTALL -eq '1') {
    Say 'Removing Wiser Dashboard'
    Stop-Dashboard
    Remove-Item -Force -ErrorAction SilentlyContinue $Shortcut
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $Dir
    if ($IsAdmin) { Remove-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue }
    Say "Removed. Settings and history are kept in $State (delete that folder to remove them too)."
    return
}

# ---------------------------------------------------------------- which build
$CpuArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$Arch = switch ($CpuArch) { 'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { Fail "no build for $CpuArch Windows" } }

$Version = $env:WISER_VERSION
if (-not $Version) {
    # The releases/latest page redirects to the newest tag; unlike GitHub's API
    # it isn't limited to 60 requests an hour per address.
    try {
        $Req = [Net.HttpWebRequest]::Create("https://github.com/$Repo/releases/latest")
        $Req.AllowAutoRedirect = $false
        $Res = $Req.GetResponse()
        $Version = ($Res.Headers['Location'] -split '/tag/')[-1]
        $Res.Close()
    } catch { $Version = $null }
    if (-not $Version -or $Version -match '/') {
        try { $Version = (Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$Repo/releases/latest").tag_name }
        catch { Fail "couldn't find the latest release (try again, or set `$env:WISER_VERSION)" }
    }
}
if (-not $Version.StartsWith('v')) { $Version = "v$Version" }

$Name = "wiser-dashboard-$Version-windows-$Arch"
$Url = "https://github.com/$Repo/releases/download/$Version"
$Tmp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid())
New-Item -ItemType Directory -Path $Tmp | Out-Null
try {
    Say "Downloading Wiser Dashboard $Version for windows-$Arch"
    Invoke-WebRequest -UseBasicParsing "$Url/$Name.zip" -OutFile "$Tmp\$Name.zip"
    Invoke-WebRequest -UseBasicParsing "$Url/SHA256SUMS.txt" -OutFile "$Tmp\SHA256SUMS.txt"
    $Line = Get-Content "$Tmp\SHA256SUMS.txt" | Where-Object { $_ -match " $([regex]::Escape("$Name.zip"))$" } | Select-Object -First 1
    if (-not $Line) { Fail "$Name.zip isn't listed in the release's checksums" }
    $Want = ($Line -split '\s+')[0]
    $Got = (Get-FileHash -Algorithm SHA256 "$Tmp\$Name.zip").Hash
    if ($Got -ne $Want) { Fail 'checksum mismatch: the download is damaged or was tampered with' }
    Say 'Checksum verified'
    Expand-Archive -Path "$Tmp\$Name.zip" -DestinationPath $Tmp -Force
    $Pkg = Join-Path $Tmp $Name
    & "$Pkg\wiser-dashboard.exe" -version | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "the downloaded program doesn't run on this system" }

    # ------------------------------------------------------------ install
    Stop-Dashboard
    if (Test-Dashboard) { Fail "something is still answering on port $Port, probably a copy started another way: close it, then run this again" }

    New-Item -ItemType Directory -Force -Path $Dir, $State | Out-Null
    Copy-Item -Force "$Pkg\wiser-dashboard.exe", "$Pkg\README.md" $Dir

    # A secret given here goes into the dashboard's own config, never onto a command line.
    $Config = Join-Path $State 'config.json'
    if ($env:WISER_SECRET) {
        if (Test-Path $Config) { Write-Warning 'already set up: WISER_SECRET ignored; change the secret on the dashboard''s setup page' }
        else { @{ secret = $env:WISER_SECRET } | ConvertTo-Json | Set-Content -Encoding ascii $Config }
    }

    $Args_ = "-listen :$Port"
    if ($env:WISER_HUB) { $Args_ += " -hub $($env:WISER_HUB)" }

    # Start at login, minimized.
    $Shell = New-Object -ComObject WScript.Shell
    $Link = $Shell.CreateShortcut($Shortcut)
    $Link.TargetPath = $Exe
    $Link.Arguments = $Args_
    $Link.WorkingDirectory = $Dir
    $Link.WindowStyle = 7
    $Link.Description = 'Wiser Dashboard'
    $Link.Save()

    if ($IsAdmin) {
        Remove-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue
        New-NetFirewallRule -DisplayName $RuleName -Program $Exe -Direction Inbound -Action Allow -Profile Private, Domain | Out-Null
        Say 'Firewall: allowed on private networks'
    } else {
        Write-Host '    Windows will ask whether to allow it on networks: choose Private networks.'
    }

    Start-Process -FilePath $Exe -ArgumentList $Args_ -WorkingDirectory $Dir -WindowStyle Minimized

    # ------------------------------------------------------------ check it really runs
    Say 'Waiting for the dashboard to start'
    $ok = $false
    for ($i = 0; $i -lt 40; $i++) { if (Test-Dashboard) { $ok = $true; break }; Start-Sleep -Seconds 1 }
    if (-not $ok) { Fail "the dashboard didn't start within 40 seconds; run $Exe in a terminal to see why" }

    $Ip = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -in 'Dhcp', 'Manual' -and $_.IPAddress -notlike '169.254.*' } | Select-Object -First 1).IPAddress
    Write-Host ''
    Say "Wiser Dashboard $Version is installed and running."
    Write-Host "    Open:   http://localhost:$Port$(if ($Ip) { "   or from other devices: http://${Ip}:$Port" })"
    Write-Host '    It starts (minimized) when you log in.'
    Write-Host '    Upgrade: run the same command again. Remove: set $env:WISER_UNINSTALL = "1" and run it again.'
}
finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $Tmp
}
