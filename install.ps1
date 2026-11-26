#Requires -RunAsAdministrator
<#
.SYNOPSIS
    Bridge System Setup & Symmetrical Installer.
.NOTES
    To build bridge.exe without console flash on boot/tray startup, compile with:
      go build -ldflags "-H windowsgui" -o bridge.exe .
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $false, HelpMessage = "Friendly name for this PC")]
    [string]$Name = $env:COMPUTERNAME,

    [Parameter(Mandatory = $false, HelpMessage = "Friendly name of the other PC")]
    [string]$PeerName = "",

    [Parameter(Mandatory = $false, HelpMessage = "Tailscale private IP of the other PC")]
    [string]$PeerIP = "",

    [Parameter(Mandatory = $false, HelpMessage = "Shared secret auth token")]
    [string]$Token = "bridge-secret-token"
)

$ErrorActionPreference = "Stop"

if (-not $PeerName -or $PeerName.Trim() -eq "") {
    if ($Name -ieq "Desktop 1") {
        $PeerName = "Desktop 2"
    } elseif ($Name -ieq "Desktop 2") {
        $PeerName = "Desktop 1"
    } else {
        $PeerName = "Remote PC"
    }
}

# 0. Administrator Check & Elevation
$currentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "[*] Requesting Administrator elevation..." -ForegroundColor Yellow
    Start-Process powershell.exe -Verb RunAs -ArgumentList ("-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`"")
    exit 0
}

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "   Bridge System Setup & Symmetrical Installer" -ForegroundColor Cyan
Write-Host "   PC Name: $Name ⇄ Peer: $PeerName" -ForegroundColor Yellow
Write-Host "==========================================================" -ForegroundColor Cyan

# Helper to find Tailscale binary
function Find-Tailscale {
    $cmd = Get-Command "tailscale.exe" -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $candidates = @(
        "$env:ProgramFiles\Tailscale\tailscale.exe",
        "${env:ProgramFiles(x86)}\Tailscale\tailscale.exe",
        "$env:LOCALAPPDATA\Tailscale\tailscale.exe"
    )
    foreach ($c in $candidates) {
        if (Test-Path $c) { return $c }
    }
    return $null
}

# Helper to find SumatraPDF binary
function Find-SumatraPDF {
    $candidates = @(
        "C:\BridgeApp\SumatraPDF.exe",
        "$env:ProgramFiles\SumatraPDF\SumatraPDF.exe",
        "${env:ProgramFiles(x86)}\SumatraPDF\SumatraPDF.exe",
        "$env:LOCALAPPDATA\SumatraPDF\SumatraPDF.exe"
    )
    foreach ($c in $candidates) {
        if ((Test-Path $c) -and ((Get-Item $c).Length -gt 1000000)) { return $c }
    }
    $cmd = Get-Command "SumatraPDF.exe" -ErrorAction SilentlyContinue
    if ($cmd -and (Test-Path $cmd.Source) -and ((Get-Item $cmd.Source).Length -gt 1000000)) { return $cmd.Source }
    return $null
}

# 1. Tailscale Check & Installation
Write-Host "`n[1/5] Checking Tailscale network..." -ForegroundColor Cyan
$tailscaleBin = Find-Tailscale
if (-not $tailscaleBin) {
    Write-Host "Tailscale not detected. Installing silently via winget..." -ForegroundColor Yellow
    winget install -e --id Tailscale.Tailscale --accept-package-agreements --accept-source-agreements --silent
    $tailscaleBin = Find-Tailscale
}
if ($tailscaleBin) {
    Write-Host "Tailscale located: $tailscaleBin" -ForegroundColor Green
    try {
        $myIP = & $tailscaleBin ip -4
        if ($myIP) {
            Write-Host "Local Tailscale IP: $myIP" -ForegroundColor Green
        }
    } catch {}

    # Auto-detect connected Tailscale peer if PeerIP is omitted
    if (-not $PeerIP -or $PeerIP.Trim() -eq "") {
        try {
            $tsJson = & $tailscaleBin status --json | ConvertFrom-Json
            if ($tsJson.Peer) {
                foreach ($prop in $tsJson.Peer.PSObject.Properties) {
                    $p = $prop.Value
                    $v4 = $p.TailscaleIPs | Where-Object { $_ -like "100.*" } | Select-Object -First 1
                    if ($v4) {
                        $PeerIP = $v4
                        $discoveredName = $p.HostName
                        try {
                            $h = Invoke-RestMethod -Uri "http://${v4}:8765/api/health" -Method Get -Headers @{ "X-Bridge-Probe" = "1" } -TimeoutSec 4 -ErrorAction SilentlyContinue
                            if ($h -and $h.node_name) {
                                $discoveredName = $h.node_name
                            }
                            if ($h -and $h.peer_name -and ($Name -eq $env:COMPUTERNAME -or -not $Name)) {
                                $Name = $h.peer_name
                            }
                        } catch {}

                        if (-not $PeerName -or $PeerName -eq "Remote PC" -or $PeerName -eq "Desktop 2" -or $PeerName -eq "Desktop 1") {
                            $PeerName = $discoveredName
                        }
                        Write-Host "[✓] Auto-detected connected Tailscale device: $PeerName ($PeerIP)" -ForegroundColor Green
                        break
                    }
                }
            }
        } catch {}
    }
}

# 2. SumatraPDF Check (for silent printing on both machines)
Write-Host "`n[2/5] Checking Silent Print Engine (SumatraPDF)..." -ForegroundColor Cyan
$sumatraBin = Find-SumatraPDF
if (-not $sumatraBin) {
    Write-Host "SumatraPDF not detected. Installing silently via winget..." -ForegroundColor Yellow
    try {
        winget install -e --id SumatraPDF.SumatraPDF --accept-package-agreements --accept-source-agreements --silent
        $sumatraBin = Find-SumatraPDF
    } catch {}
}
if (-not $sumatraBin) {
    Write-Host "Winget unavailable or failed; downloading standalone SumatraPDF portable package..." -ForegroundColor Yellow
    $destDir = "C:\BridgeApp"
    if (-not (Test-Path $destDir)) { New-Item -ItemType Directory -Force -Path $destDir | Out-Null }
    $tempZip = Join-Path $env:TEMP ("sumatra_" + [Guid]::NewGuid().ToString() + ".zip")
    $tempExt = Join-Path $env:TEMP ("sumatra_ext_" + [Guid]::NewGuid().ToString())
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $urls = @(
        "https://files.sumatrapdfreader.org/software/sumatrapdf/rel/3.5.2/SumatraPDF-3.5.2-64.zip",
        "https://www.sumatrapdfreader.org/dl/rel/3.5.2/SumatraPDF-3.5.2-64.zip",
        "https://files.sumatrapdfreader.org/software/sumatrapdf/rel/3.5.2/SumatraPDF-3.5.2.zip",
        "https://www.sumatrapdfreader.org/dl/rel/3.5.2/SumatraPDF-3.5.2.zip"
    )
    $wc = New-Object System.Net.WebClient
    $wc.Headers.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
    foreach ($u in $urls) {
        try {
            $wc.DownloadFile($u, $tempZip)
            if ((Test-Path $tempZip) -and ((Get-Item $tempZip).Length -gt 1000000)) { break }
        } catch {}
    }
    if (Test-Path $tempZip) {
        try {
            Expand-Archive -LiteralPath $tempZip -DestinationPath $tempExt -Force
            $exe = Get-ChildItem -Path $tempExt -Filter "SumatraPDF*.exe" -Recurse | Select-Object -First 1
            if ($exe) {
                Copy-Item -LiteralPath $exe.FullName -Destination "C:\BridgeApp\SumatraPDF.exe" -Force
                $sumatraBin = "C:\BridgeApp\SumatraPDF.exe"
            }
        } catch {}
        Remove-Item -LiteralPath $tempZip -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $tempExt -Recurse -Force -ErrorAction SilentlyContinue
    }
}
if ($sumatraBin) {
    Write-Host "SumatraPDF located: $sumatraBin" -ForegroundColor Green
} else {
    Write-Host "Warning: SumatraPDF could not be auto-installed. Silent printing will auto-provision on first job." -ForegroundColor Yellow
    $sumatraBin = "C:\BridgeApp\SumatraPDF.exe"
}
Remove-Item "C:\BridgeApp\SumatraPDF.lnk" -Force -ErrorAction SilentlyContinue

# 3. Create Required Directories
Write-Host "`n[3/5] Setting up directory structure..." -ForegroundColor Cyan
$dirsToCreate = @(
    "C:\BridgeApp",
    "C:\BridgeApp\icons",
    "C:\Bridge\Shared",
    "C:\Bridge\PrintQueue",
    "C:\Bridge\PrintFailed",
    "C:\Bridge\Logs"
)

foreach ($d in $dirsToCreate) {
    if (-not (Test-Path $d)) {
        New-Item -ItemType Directory -Path $d -Force | Out-Null
        Write-Host "Created directory: $d" -ForegroundColor Green
    }
}

# Decommission legacy print folders if present
Remove-Item "C:\Bridge\Print" -Recurse -Force -ErrorAction SilentlyContinue

# Stop existing tasks/processes to allow clean file update
schtasks /End /TN "BridgeAppLogon" 2>$null | Out-Null
schtasks /End /TN "BridgeSystemService" 2>$null | Out-Null
Stop-ScheduledTask -TaskName "BridgeAppLogon" -ErrorAction SilentlyContinue | Out-Null
Stop-ScheduledTask -TaskName "BridgeSystemService" -ErrorAction SilentlyContinue | Out-Null
Get-Process -Name "bridge" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 300

# Locate and copy bridge.exe (or build with -ldflags "-H windowsgui" if Go toolchain is present)
$binarySourceCandidates = @(
    "$PSScriptRoot\bridge.exe",
    "$PSScriptRoot\dist\bridge.exe",
    ".\bridge.exe",
    ".\dist\bridge.exe"
)
$sourceBinary = $null
foreach ($b in $binarySourceCandidates) {
    if (Test-Path $b) {
        $sourceBinary = (Resolve-Path $b).Path
        break
    }
}

if (-not $sourceBinary) {
    $goCmd = Get-Command "go.exe" -ErrorAction SilentlyContinue
    if ($goCmd -and (Test-Path (Join-Path $PSScriptRoot "go.mod"))) {
        Write-Host "Building bridge.exe with -ldflags `"-H windowsgui`"..." -ForegroundColor Yellow
        Push-Location $PSScriptRoot
        try {
            & $goCmd.Source build -ldflags "-H windowsgui" -o "$PSScriptRoot\bridge.exe" .
            if (Test-Path "$PSScriptRoot\bridge.exe") {
                $sourceBinary = "$PSScriptRoot\bridge.exe"
            }
        } finally {
            Pop-Location
        }
    } else {
        Write-Host "Warning: bridge.exe not found. Build with: go build -ldflags `"-H windowsgui`" -o bridge.exe ." -ForegroundColor Yellow
    }
}

if ($sourceBinary) {
    Copy-Item -Path $sourceBinary -Destination "C:\BridgeApp\bridge.exe" -Force
    Write-Host "Deployed binary to C:\BridgeApp\bridge.exe" -ForegroundColor Green
}

# Load existing config.json on upgrade to preserve node_name, auth_token, peers, and ignored_peers
$configPath = "C:\BridgeApp\config.json"
$existingCfg = $null
if (Test-Path $configPath) {
    try {
        $existingCfg = Get-Content -LiteralPath $configPath -Raw -ErrorAction Stop | ConvertFrom-Json
        Write-Host "Existing config detected at $configPath; preserving saved peers and settings." -ForegroundColor Cyan
    } catch {
        $existingCfg = $null
    }
}

if ($existingCfg) {
    if (-not $PSBoundParameters.ContainsKey('Name') -and $existingCfg.node_name -and -not [string]::IsNullOrWhiteSpace($existingCfg.node_name)) {
        $Name = [string]$existingCfg.node_name
    }
    if (-not $PSBoundParameters.ContainsKey('Token') -and $existingCfg.auth_token -and -not [string]::IsNullOrWhiteSpace($existingCfg.auth_token)) {
        $Token = [string]$existingCfg.auth_token
    }
}

# Build merged multi-device peers list (preserving existing peers and spool ports)
$peersList = @()
$usedPorts = @{ 9100 = $true; 8765 = $true }
$seenUrls = @{}
$seenIps = @{}

if ($existingCfg -and $existingCfg.peers) {
    foreach ($ep in $existingCfg.peers) {
        if (-not $ep) { continue }
        $epName = ([string]$ep.name).Trim()
        $epIp = ([string]$ep.ip).Trim()
        $epUrl = ([string]$ep.url).Trim().TrimEnd('/')
        if (-not $epUrl -and $epIp) { $epUrl = "http://${epIp}:8765" }
        if (-not $epIp -and $epUrl) {
            try { $epIp = ([Uri]$epUrl).Host } catch {}
        }
        if (-not $epIp -and -not $epUrl) { continue }
        if ($epName -ieq $Name) { continue }

        $portNum = 0
        if ($ep.spool_port) { $portNum = [int]$ep.spool_port }
        if ($portNum -le 0 -or $usedPorts.ContainsKey($portNum)) {
            $nextP = 9101
            while ($usedPorts.ContainsKey($nextP)) { $nextP++ }
            $portNum = $nextP
        }
        $usedPorts[$portNum] = $true
        if ($epUrl) { $seenUrls[$epUrl.ToLowerInvariant()] = $true }
        if ($epIp) { $seenIps[$epIp] = $true }

        $entry = [ordered]@{
            "name"       = $epName
            "ip"         = $epIp
            "url"        = $epUrl
            "spool_port" = $portNum
        }
        if ($ep.custom_name) { $entry["custom_name"] = $true }
        $peersList += $entry
    }
}

$peerClean = $PeerIP.Trim().TrimEnd('/')
if ($peerClean -ne "") {
    if (-not $peerClean.StartsWith("http://") -and -not $peerClean.StartsWith("https://")) {
        $peerUrl = "http://${peerClean}:8765"
        $peerIpOnly = $peerClean
    } else {
        $peerUrl = $peerClean
        try { $peerIpOnly = ([Uri]$peerClean).Host } catch { $peerIpOnly = $peerClean }
    }
    $alreadyTracked = ($peerUrl -and $seenUrls.ContainsKey($peerUrl.ToLowerInvariant())) -or ($peerIpOnly -and $seenIps.ContainsKey($peerIpOnly))
    if (-not $alreadyTracked -and ($PeerName -ine $Name)) {
        $nextP = 9101
        while ($usedPorts.ContainsKey($nextP)) { $nextP++ }
        $usedPorts[$nextP] = $true
        $peersList += [ordered]@{
            "name"       = $PeerName
            "ip"         = $peerIpOnly
            "url"        = $peerUrl
            "spool_port" = $nextP
        }
    }
}

$primaryPeerName = ""
$primaryPeerUrl = ""
if ($peersList.Count -gt 0) {
    $primaryPeerName = $peersList[0].name
    $primaryPeerUrl = $peersList[0].url
}

$ignoredPeersList = @()
if ($existingCfg -and $existingCfg.ignored_peers) {
    $ignoredPeersList = @($existingCfg.ignored_peers)
}

$configObj = [ordered]@{
    "node_name"         = $Name
    "peer_name"         = $primaryPeerName
    "peer_url"          = $primaryPeerUrl
    "peers"             = $peersList
    "ignored_peers"     = $ignoredPeersList
    "configured"        = $true
    "listen_port"       = 8765
    "auth_token"        = $Token
    "shared_dir"        = "C:\Bridge\Shared"
    "print_dir"         = "C:\Bridge\Print"
    "sumatra_path"      = $sumatraBin
    "print_timeout_sec" = 120
    "state_file"        = "C:\BridgeApp\state.json"
    "print_queue_dir"   = "C:\Bridge\PrintQueue"
    "print_failed_dir"  = "C:\Bridge\PrintFailed"
    "print_sent_dir"    = "C:\Bridge\Print\Sent"
    "log_dir"           = "C:\Bridge\Logs"
}

$configJson = $configObj | ConvertTo-Json -Depth 6
[System.IO.File]::WriteAllText($configPath, $configJson)
Write-Host "Saved config at $configPath (Peers: $($peersList.Count))" -ForegroundColor Green

# 4. Windows Firewall Inbound Rule
Write-Host "`n[4/5] Configuring Windows Firewall..." -ForegroundColor Cyan
$ruleName = "BridgeSystemService_8765"
Remove-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue | Out-Null
New-NetFirewallRule -DisplayName $ruleName `
    -Direction Inbound `
    -LocalPort "8765","9100-9120" `
    -Protocol TCP `
    -Action Allow `
    -Profile Any `
    -Description "Allow Bridge Service sync, multi-spooler ports (9100-9120), and print communication over Tailscale" | Out-Null
Write-Host "Firewall inbound rule added for TCP ports 8765 and 9100-9120." -ForegroundColor Green

# 5. Register Interactive Startup & Launch (Session 1 Taskbar Tray Dot)
Write-Host "`n[5/5] Registering Interactive Startup, Virtual Printer & Shortcuts..." -ForegroundColor Cyan

# Remove legacy Session 0 scheduled task and prior logon task before re-registering
Unregister-ScheduledTask -TaskName "BridgeSystemService" -Confirm:$false -ErrorAction SilentlyContinue | Out-Null
Unregister-ScheduledTask -TaskName "BridgeAppLogon" -Confirm:$false -ErrorAction SilentlyContinue | Out-Null

# Clean up legacy duplicate startup registrations (HKLM, WOW6432Node, User Startup, Common Startup, and HKEY_USERS)
$runKeys = @(
    "HKLM:\Software\Microsoft\Windows\CurrentVersion\Run",
    "HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Run",
    "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
)
Get-ChildItem "Registry::HKEY_USERS" -ErrorAction SilentlyContinue | ForEach-Object {
    $runKeys += "$($_.PSPath)\Software\Microsoft\Windows\CurrentVersion\Run"
}
foreach ($rk in $runKeys) {
    if (Test-Path $rk) {
        $item = Get-ItemProperty -Path $rk -ErrorAction SilentlyContinue
        if ($item) {
            foreach ($prop in $item.PSObject.Properties) {
                if ($prop.Name -like "*bridge*" -or ($prop.Value -is [string] -and $prop.Value -like "*bridge.exe*")) {
                    Remove-ItemProperty -Path $rk -Name $prop.Name -Force -ErrorAction SilentlyContinue | Out-Null
                }
            }
        }
    }
}
$legacyStartupDirs = @(
    (Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\Startup"),
    "C:\ProgramData\Microsoft\Windows\Start Menu\Programs\Startup"
)
Get-ChildItem "C:\Users" -Directory -ErrorAction SilentlyContinue | ForEach-Object {
    $legacyStartupDirs += (Join-Path $_.FullName "AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup")
}
foreach ($sd in $legacyStartupDirs) {
    if (Test-Path $sd) {
        Get-ChildItem $sd -Filter "*.lnk" -ErrorAction SilentlyContinue | ForEach-Object {
            try {
                $target = (New-Object -ComObject WScript.Shell).CreateShortcut($_.FullName).TargetPath
                if ($target -like "*bridge.exe*" -or $_.Name -like "*bridge*") {
                    Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue | Out-Null
                }
            } catch {
                if ($_.Name -like "*bridge*") {
                    Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue | Out-Null
                }
            }
        }
    }
}

# Register Interactive Elevated Logon task (Session 1 + Admin rights for virtual printers), falling back to HKCU Run
$logonTaskOk = $false
try {
    $exe = "C:\BridgeApp\bridge.exe"
    $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
    if ([string]::IsNullOrWhiteSpace($user)) {
        $user = "$env:USERDOMAIN\$env:USERNAME"
    }
    $action = New-ScheduledTaskAction -Execute $exe -Argument '-autostart'
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
    $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -MultipleInstances IgnoreNew
    Register-ScheduledTask -TaskName "BridgeAppLogon" -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force -ErrorAction Stop | Out-Null
    $logonTaskOk = $true
    Remove-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name "BridgeApp" -Force -ErrorAction SilentlyContinue | Out-Null
    if (-not (Test-Path "HKCU:\Software\BridgeApp")) { New-Item -Path "HKCU:\Software\BridgeApp" -Force | Out-Null }
    Set-ItemProperty -Path "HKCU:\Software\BridgeApp" -Name "StartupMode" -Value "BridgeAppLogon" -Force -ErrorAction SilentlyContinue
} catch {}
if (-not $logonTaskOk) {
    Remove-ItemProperty -Path "HKCU:\Software\BridgeApp" -Name "StartupMode" -Force -ErrorAction SilentlyContinue | Out-Null
    Set-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name "BridgeApp" -Value '"C:\BridgeApp\bridge.exe" -autostart' -Force
}

# Register Windows Virtual Printers for Ctrl+P Native Printing (all peers)
function Get-SanitizedBridgeName {
    param([string]$RawName)
    $res = [string]$RawName
    foreach ($ch in @('\', '/', ':', '*', '?', '"', '<', '>', '|')) {
        $res = $res.Replace($ch, '_')
    }
    $res = $res.Trim()
    if ([string]::IsNullOrWhiteSpace($res)) { $res = "Remote PC" }
    return $res
}

function Test-BridgeVirtualPrinter {
    param($p)
    if (-not $p) { return $true }
    $n = ([string]$p.Name).ToLowerInvariant().Trim()
    $pt = ([string]$p.PortName).ToLowerInvariant().Trim()
    if ($n.StartsWith('print on ')) { return $true }
    if ($pt.Contains('bridge_tcp_port') -or $pt.Contains('bridge_port_') -or $pt.Contains('bridge - ')) { return $true }
    if ($pt -eq 'portprompt:' -or $pt -eq 'nul:' -or $pt.StartsWith('file:')) { return $true }
    $keywords = @(
        'print to pdf', 'pdf creator', 'pdfcreator', 'pdf24', 'win2pdf', 'clawpdf',
        'adobe pdf', 'foxit', 'cutepdf', 'bullzip', 'primopdf', 'dopdf', 'novapdf',
        'nitro pdf', 'soda pdf', 'xps document writer', 'openxps',
        'generic / text only', 'generic', 'text only',
        'anydesk', 'fax', 'onenote', 'snagit', 'virtual printer'
    )
    foreach ($kw in $keywords) {
        if ($n.Contains($kw)) { return $true }
    }
    return $false
}

# Protect Windows default printer mode
Set-ItemProperty -Path "HKCU:\Software\Microsoft\Windows NT\CurrentVersion\Windows" -Name "LegacyDefaultPrinterMode" -Value 1 -Type DWord -Force -ErrorAction SilentlyContinue

$existingDefault = $null
try {
    $existingDefault = (Get-CimInstance Win32_Printer -Filter "Default = True" -ErrorAction SilentlyContinue).Name
} catch {}

$preferredDrivers = @('Microsoft Print To PDF', 'Microsoft XPS Document Writer v4', 'Microsoft XPS Document Writer', 'OpenXPS Document Writer')
$driver = $null
foreach ($d in $preferredDrivers) {
    if (Get-PrinterDriver -Name $d -ErrorAction SilentlyContinue) {
        $driver = $d
        break
    }
}
if (-not $driver) {
    try { Add-PrinterDriver -Name 'Microsoft Print To PDF' -ErrorAction SilentlyContinue } catch {}
    if (Get-PrinterDriver -Name 'Microsoft Print To PDF' -ErrorAction SilentlyContinue) {
        $driver = 'Microsoft Print To PDF'
    }
}
if (-not $driver) {
    try { Add-PrinterDriver -Name 'Microsoft XPS Document Writer v4' -ErrorAction SilentlyContinue } catch {}
    if (Get-PrinterDriver -Name 'Microsoft XPS Document Writer v4' -ErrorAction SilentlyContinue) {
        $driver = 'Microsoft XPS Document Writer v4'
    }
}
if (-not $driver) {
    try { Add-PrinterDriver -Name 'Microsoft XPS Document Writer' -ErrorAction SilentlyContinue } catch {}
    if (Get-PrinterDriver -Name 'Microsoft XPS Document Writer' -ErrorAction SilentlyContinue) {
        $driver = 'Microsoft XPS Document Writer'
    }
}
if (-not $driver) {
    # Attempt installing XPS driver INF directly from driver store / INF repository
    $infCandidates = @(
        "$env:SystemRoot\System32\spool\tools\Microsoft XPS Document Writer\prnms001.inf",
        "$env:SystemRoot\INF\ntprint.inf"
    )
    foreach ($inf in $infCandidates) {
        if (Test-Path $inf) {
            try { pnputil.exe /add-driver $inf /install | Out-Null } catch {}
            try { Add-PrinterDriver -Name 'Microsoft XPS Document Writer v4' -ErrorAction SilentlyContinue } catch {}
            if (Get-PrinterDriver -Name 'Microsoft XPS Document Writer v4' -ErrorAction SilentlyContinue) {
                $driver = 'Microsoft XPS Document Writer v4'
                break
            }
        }
    }
}
if (-not $driver) {
    throw "Microsoft XPS Document Writer (or Microsoft Print To PDF) driver is required for native remote printing but is not installed. Enable 'Printing-XPSServices-Features' in Windows Features."
}

$registeredPrinters = @()
foreach ($peer in $peersList) {
    if (-not $peer.name -or ($peer.name -ieq $Name)) { continue }
    $sanitizedPeer = Get-SanitizedBridgeName -RawName $peer.name
    $spoolPort = if ($peer.spool_port -and [int]$peer.spool_port -gt 0) { [int]$peer.spool_port } else { 9101 }
    $portName = "Bridge - $sanitizedPeer ($spoolPort)"
    $printerName = "Print on $sanitizedPeer"

    if (-not (Get-PrinterPort -Name $portName -ErrorAction SilentlyContinue)) {
        Add-PrinterPort -Name $portName -PrinterHostAddress '127.0.0.1' -PortNumber $spoolPort -ErrorAction Stop
    }
    try {
        Set-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\$portName" -Name "SNMP Enabled" -Value 0 -Type DWord -ErrorAction SilentlyContinue
        $wmiPort = Get-CimInstance Win32_TCPIPPrinterPort -Filter "Name = '$($portName.Replace("'", "''"))'" -ErrorAction SilentlyContinue
        if ($wmiPort) {
            Set-CimInstance -InputObject $wmiPort -Property @{ SNMPEnabled = $false } -ErrorAction SilentlyContinue
        }
    } catch {}

    if (-not (Get-Printer -Name $printerName -ErrorAction SilentlyContinue)) {
        Add-Printer -Name $printerName -DriverName $driver -PortName $portName -ErrorAction Stop
    } else {
        Set-Printer -Name $printerName -DriverName $driver -PortName $portName -ErrorAction Stop
    }

    $verified = Get-Printer -Name $printerName -ErrorAction Stop
    if (-not $verified) {
        throw "Virtual Printer '$printerName' verification failed after creation."
    }

    # Clean up legacy Bridge_Port_<spoolPort> once migrated
    $legacyPort = "Bridge_Port_$spoolPort"
    if ($legacyPort -ne $portName -and (Get-PrinterPort -Name $legacyPort -ErrorAction SilentlyContinue)) {
        $inUse = Get-Printer -ErrorAction SilentlyContinue | Where-Object { $_.PortName -eq $legacyPort }
        if (-not $inUse) {
            Remove-PrinterPort -Name $legacyPort -ErrorAction SilentlyContinue
        }
    }

    try {
        $pObj = Get-CimInstance Win32_Printer -Filter "Name = '$($printerName.Replace("'", "''"))'" -ErrorAction SilentlyContinue
        if ($pObj -and $pObj.WorkOffline) {
            $pObj.WorkOffline = $false
            Set-CimInstance -InputObject $pObj -ErrorAction SilentlyContinue
        }
        Get-PrintJob -PrinterName $printerName -ErrorAction SilentlyContinue | Where-Object { $_.JobStatus -match 'Error|Blocked|Offline|Paused' } | Remove-PrintJob -ErrorAction SilentlyContinue
    } catch {}

    $registeredPrinters += $printerName
    Write-Host "Virtual Printer '$printerName' registered on port '$portName' (TCP $spoolPort) for Ctrl+P printing." -ForegroundColor Green
}

# Remove any virtual printer for THIS device if it was previously created
$selfPrinterNames = @("Print on $Name", "Print on $env:COMPUTERNAME", "Print on Desktop 1", "Print on Unit 1")
foreach ($spn in $selfPrinterNames) {
    if ($registeredPrinters -notcontains $spn) {
        Get-Printer -Name $spn -ErrorAction SilentlyContinue | Remove-Printer -ErrorAction SilentlyContinue
    }
}

# Restore or protect physical default printer using synced virtual printer filter
$isPhysicalDefault = $false
if ($existingDefault) {
    $defObj = Get-CimInstance Win32_Printer -Filter "Name = '$($existingDefault.Replace("'", "''"))'" -ErrorAction SilentlyContinue
    if ($defObj -and -not (Test-BridgeVirtualPrinter $defObj)) {
        $isPhysicalDefault = $true
    }
}
if ($isPhysicalDefault) {
    (New-Object -ComObject WScript.Network).SetDefaultPrinter($existingDefault)
} else {
    $physical = Get-CimInstance Win32_Printer -ErrorAction SilentlyContinue | Where-Object {
        -not (Test-BridgeVirtualPrinter $_)
    } | Select-Object -First 1
    if ($physical) {
        (New-Object -ComObject WScript.Network).SetDefaultPrinter($physical.Name)
    } elseif ($registeredPrinters.Count -gt 0) {
        (New-Object -ComObject WScript.Network).SetDefaultPrinter($registeredPrinters[0])
    }
}

# Create Desktop Shortcut ONLY ONCE in the active user Desktop, cleaning duplicates and SumatraPDF.lnk everywhere
$wscript = New-Object -ComObject WScript.Shell
$primaryDesktop = $null
$desktopCandidates = @()
try {
    $usfDesktop = (Get-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders" -Name "Desktop" -ErrorAction SilentlyContinue).Desktop
    if ($usfDesktop) { $desktopCandidates += [Environment]::ExpandEnvironmentVariables($usfDesktop) }
} catch {}
try {
    $sfDesktop = (Get-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Explorer\Shell Folders" -Name "Desktop" -ErrorAction SilentlyContinue).Desktop
    if ($sfDesktop) { $desktopCandidates += [Environment]::ExpandEnvironmentVariables($sfDesktop) }
} catch {}
$desktopCandidates += [Environment]::GetFolderPath("Desktop")
if ($env:USERPROFILE) {
    $desktopCandidates += (Join-Path $env:USERPROFILE "Desktop")
    $desktopCandidates += (Join-Path $env:USERPROFILE "OneDrive\Desktop")
}
foreach ($cand in $desktopCandidates) {
    if ($cand -and (Test-Path $cand)) {
        $primaryDesktop = [IO.Path]::GetFullPath($cand).TrimEnd('\')
        break
    }
}

$rawDesktopDirs = @()
if ($primaryDesktop) { $rawDesktopDirs += $primaryDesktop }
$rawDesktopDirs += @([Environment]::GetFolderPath("Desktop"), "C:\Users\Public\Desktop")
if ($env:USERPROFILE) {
    $rawDesktopDirs += (Join-Path $env:USERPROFILE "Desktop")
    $rawDesktopDirs += (Join-Path $env:USERPROFILE "OneDrive\Desktop")
}
Get-ChildItem "C:\Users" -Directory -ErrorAction SilentlyContinue | ForEach-Object {
    $rawDesktopDirs += (Join-Path $_.FullName "Desktop")
    $rawDesktopDirs += (Join-Path $_.FullName "OneDrive\Desktop")
}
$seenDesktops = @{}
$desktopDirs = @()
foreach ($rd in $rawDesktopDirs) {
    if ($rd -and (Test-Path $rd)) {
        $norm = [IO.Path]::GetFullPath($rd).TrimEnd('\')
        $low = $norm.ToLowerInvariant()
        if (-not $seenDesktops.ContainsKey($low)) {
            $seenDesktops[$low] = $true
            $desktopDirs += $norm
        }
    }
}

foreach ($d in $desktopDirs) {
    # Remove legacy shortcuts, drop-to-print shortcuts, and unwanted SumatraPDF shortcuts
    Remove-Item (Join-Path $d "Bridge Print Folder.lnk") -Force -ErrorAction SilentlyContinue
    Remove-Item (Join-Path $d "SumatraPDF.lnk") -Force -ErrorAction SilentlyContinue
    Get-ChildItem $d -Filter "* - Drop to Print.lnk" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
    Get-ChildItem $d -Filter "* - Shared Folder.lnk" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
    if ($primaryDesktop -and ($d -ine $primaryDesktop)) {
        Remove-Item (Join-Path $d "Bridge Shared Folder.lnk") -Force -ErrorAction SilentlyContinue
    }
}

if ($primaryDesktop) {
    $s1 = $wscript.CreateShortcut((Join-Path $primaryDesktop "Bridge Shared Folder.lnk"))
    $s1.TargetPath = "C:\Bridge\Shared"
    $s1.Description = "Bridge Multi-Device Two-Way Shared Folder (Global Mesh)"
    $s1.Save()
}

# Create Windows Explorer "Send To" shortcuts for all paired peers
$sendToDir = Join-Path $env:APPDATA "Microsoft\Windows\SendTo"
if (Test-Path $sendToDir) {
    foreach ($peer in $peersList) {
        if (-not $peer.name -or ($peer.name -ieq $Name)) { continue }
        $sanitizedPeer = Get-SanitizedBridgeName -RawName $peer.name
        $sendToShortcut = $wscript.CreateShortcut((Join-Path $sendToDir "Print on $sanitizedPeer.lnk"))
        $sendToShortcut.TargetPath = "C:\BridgeApp\bridge.exe"
        $sendToShortcut.Arguments = "-send-print -target `"$sanitizedPeer`""
        $sendToShortcut.Description = "Send file to print on $sanitizedPeer"
        $sendToShortcut.Save()
        Write-Host "SendTo right-click shortcut created ('Print on $sanitizedPeer')." -ForegroundColor Green
    }
}

Write-Host "Canonical Desktop shortcut created ('Bridge Shared Folder')." -ForegroundColor Green
if ($registeredPrinters.Count -gt 0) {
    Write-Host "Direct Printing ready: Select any 'Print on <Peer>' in Ctrl+P." -ForegroundColor Green
}

# Launch bridge.exe in current interactive session silently (preferring elevated BridgeAppLogon task)
$launchedViaTask = $false
if ($logonTaskOk) {
    try {
        schtasks /Run /TN "BridgeAppLogon" 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) { $launchedViaTask = $true }
    } catch {}
}
if (-not $launchedViaTask) {
    Start-Process "C:\BridgeApp\bridge.exe" -ArgumentList "-autostart" -WindowStyle Hidden
}

# 6. Post-Install Verification Report
Start-Sleep -Seconds 2
Write-Host "`n================ Verification Report ================" -ForegroundColor Cyan
$healthCheck = "FAIL"
try {
    $response = Invoke-RestMethod -Uri "http://127.0.0.1:8765/api/health" -Method Get -TimeoutSec 3 -ErrorAction Stop
    if ($response -and $response.status -eq "healthy") {
        $healthCheck = "PASS"
    }
} catch {
    $healthCheck = "FAIL ($($_.Exception.Message))"
}
Write-Host ("{0,-35} : [{1}]" -f "Health API (GET /api/health)", $healthCheck) -ForegroundColor $(if ($healthCheck -eq "PASS") { "Green" } else { "Red" })
Write-Host "=====================================================" -ForegroundColor Cyan
