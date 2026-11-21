#Requires -RunAsAdministrator
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
                            $h = Invoke-RestMethod -Uri "http://${v4}:8765/api/health" -Method Get -TimeoutSec 1 -ErrorAction SilentlyContinue
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
    # Create background engine shortcut in C:\BridgeApp\SumatraPDF.lnk
    try {
        $wscript = New-Object -ComObject WScript.Shell
        $sc = $wscript.CreateShortcut("C:\BridgeApp\SumatraPDF.lnk")
        $sc.TargetPath = $sumatraBin
        $sc.Description = "Bridge Silent Print Engine"
        $sc.Save()
    } catch {}
} else {
    Write-Host "Warning: SumatraPDF could not be auto-installed. Silent printing will auto-provision on first job." -ForegroundColor Yellow
    $sumatraBin = "C:\BridgeApp\SumatraPDF.exe"
}

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

# Stop existing task/process to allow clean file update
Stop-ScheduledTask -TaskName "BridgeSystemService" -ErrorAction SilentlyContinue | Out-Null
Get-Process -Name "bridge" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

# Locate and copy bridge.exe
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

if ($sourceBinary) {
    Copy-Item -Path $sourceBinary -Destination "C:\BridgeApp\bridge.exe" -Force
    Write-Host "Deployed binary to C:\BridgeApp\bridge.exe" -ForegroundColor Green
}

# Generate multi-device config.json
$peerClean = $PeerIP.Trim().TrimEnd('/')
if ($peerClean -ne "") {
    if (-not $peerClean.StartsWith("http://") -and -not $peerClean.StartsWith("https://")) {
        $peerUrl = "http://${peerClean}:8765"
    } else {
        $peerUrl = $peerClean
    }
} else {
    $peerUrl = ""
}

$peersList = @()
if ($peerClean -ne "" -and ($PeerName -ine $Name)) {
    $peersList += [ordered]@{
        "name"       = $PeerName
        "ip"         = $peerClean
        "url"        = $peerUrl
        "spool_port" = 9101
    }
}

$configObj = [ordered]@{
    "node_name"         = $Name
    "peer_name"         = $PeerName
    "peer_url"          = $peerUrl
    "peers"             = $peersList
    "configured"        = $true
    "listen_port"       = 8765
    "auth_token"        = $Token
    "shared_dir"        = "C:\Bridge\Shared"
    "print_dir"         = "C:\Bridge\Print"
    "sumatra_path"      = $sumatraBin
    "print_timeout_sec" = 45
    "state_file"        = "C:\BridgeApp\state.json"
    "print_queue_dir"   = "C:\Bridge\PrintQueue"
    "print_failed_dir"  = "C:\Bridge\PrintFailed"
    "print_sent_dir"    = "C:\Bridge\Print\Sent"
    "log_dir"           = "C:\Bridge\Logs"
}

$configJson = $configObj | ConvertTo-Json -Depth 5
$configPath = "C:\BridgeApp\config.json"
[System.IO.File]::WriteAllText($configPath, $configJson)
Write-Host "Generated config at $configPath" -ForegroundColor Green

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

# Remove legacy Session 0 scheduled task so it won't block Session 1 tray icon
Unregister-ScheduledTask -TaskName "BridgeSystemService" -Confirm:$false -ErrorAction SilentlyContinue | Out-Null

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

# Register ONLY HKCU Run key with -autostart flag for clean Session 1 tray startup
Set-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name "BridgeApp" -Value '"C:\BridgeApp\bridge.exe" -autostart' -Force

# Register Windows Virtual Printer for Ctrl+P Native Printing
$portName = 'Bridge_Port_9101'

# Protect Windows default printer mode
Set-ItemProperty -Path "HKCU:\Software\Microsoft\Windows NT\CurrentVersion\Windows" -Name "LegacyDefaultPrinterMode" -Value 1 -Type DWord -Force -ErrorAction SilentlyContinue

$existingDefault = $null
try {
    $existingDefault = (Get-CimInstance Win32_Printer -Filter "Default = True" -ErrorAction SilentlyContinue).Name
} catch {}

if (-not (Get-PrinterPort -Name $portName -ErrorAction SilentlyContinue)) {
    Add-PrinterPort -Name $portName -PrinterHostAddress '127.0.0.1' -PortNumber 9101 -ErrorAction SilentlyContinue
}
$preferredDrivers = @('Microsoft XPS Document Writer v4', 'Microsoft XPS Document Writer', 'OpenXPS Document Writer')
$driver = $null
foreach ($d in $preferredDrivers) {
    if (Get-PrinterDriver -Name $d -ErrorAction SilentlyContinue) {
        $driver = $d
        break
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
    throw "Microsoft XPS Document Writer driver is required for native remote printing but is not installed. Enable 'Printing-XPSServices-Features' in Windows Features."
}
$printerName = "Print on $PeerName"
if ($driver -and $PeerName -and ($PeerName -ine $Name)) {
    Get-Printer -ErrorAction SilentlyContinue | Where-Object { $_.PortName -eq $portName -and $_.Name -ne $printerName } | Remove-Printer -ErrorAction SilentlyContinue
    if (-not (Get-Printer -Name $printerName -ErrorAction SilentlyContinue)) {
        Add-Printer -Name $printerName -DriverName $driver -PortName $portName -ErrorAction SilentlyContinue
    }
    Write-Host "Virtual Printer '$printerName' registered on port 9101 for Ctrl+P printing." -ForegroundColor Green
}

# Remove any virtual printer for THIS device if it was previously created
$selfPrinterNames = @("Print on $Name", "Print on $env:COMPUTERNAME", "Print on Desktop 1", "Print on Unit 1")
foreach ($spn in $selfPrinterNames) {
    if ($spn -ne $printerName) {
        Get-Printer -Name $spn -ErrorAction SilentlyContinue | Remove-Printer -ErrorAction SilentlyContinue
    }
}

# Restore or protect physical default printer
if ($existingDefault -and -not ($existingDefault -like "Print on *")) {
    (New-Object -ComObject WScript.Network).SetDefaultPrinter($existingDefault)
} else {
    $physical = Get-CimInstance Win32_Printer -ErrorAction SilentlyContinue | Where-Object {
        $_.PortName -notlike 'Bridge_Port_*' -and
        $_.PortName -ne 'Bridge_TCP_Port' -and
        $_.PortName -ne 'PORTPROMPT:' -and
        $_.PortName -ne 'nul:' -and
        $_.Name -notlike 'Print on *' -and
        $_.Name -notlike '*PDF*' -and
        $_.Name -notlike '*XPS*' -and
        $_.Name -notlike '*AnyDesk*' -and
        $_.Name -notlike '*Fax*' -and
        $_.Name -notlike '*OneNote*'
    } | Select-Object -First 1
    if ($physical) {
        (New-Object -ComObject WScript.Network).SetDefaultPrinter($physical.Name)
    } elseif ($printerName) {
        (New-Object -ComObject WScript.Network).SetDefaultPrinter($printerName)
    }
}

# Create Desktop Shortcuts (Public, User, and OneDrive) - ONLY ONE shared folder shortcut
$wscript = New-Object -ComObject WScript.Shell
$desktopDirs = @([Environment]::GetFolderPath("Desktop"), "C:\Users\Public\Desktop")
if ($env:USERPROFILE) {
    $desktopDirs += (Join-Path $env:USERPROFILE "OneDrive\Desktop")
}
Get-ChildItem "C:\Users" -Directory -ErrorAction SilentlyContinue | ForEach-Object {
    $desktopDirs += (Join-Path $_.FullName "Desktop")
    $desktopDirs += (Join-Path $_.FullName "OneDrive\Desktop")
}
$desktopDirs = $desktopDirs | Select-Object -Unique

foreach ($d in $desktopDirs) {
    if (Test-Path $d) {
        # Remove legacy shortcuts and any drop-to-print shortcuts
        Remove-Item (Join-Path $d "Bridge Print Folder.lnk") -ErrorAction SilentlyContinue
        Get-ChildItem $d -Filter "* - Drop to Print.lnk" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
        Get-ChildItem $d -Filter "* - Shared Folder.lnk" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue

        $s1 = $wscript.CreateShortcut((Join-Path $d "Bridge Shared Folder.lnk"))
        $s1.TargetPath = "C:\Bridge\Shared"
        $s1.Description = "Bridge Multi-Device Two-Way Shared Folder (Global Mesh)"
        $s1.Save()

        if ($sumatraBin -and (Test-Path $sumatraBin)) {
            $s2 = $wscript.CreateShortcut((Join-Path $d "SumatraPDF.lnk"))
            $s2.TargetPath = $sumatraBin
            $s2.Description = "Bridge Silent Print Engine (SumatraPDF)"
            $s2.Save()
        }
    }
}

# Create Windows Explorer "Send To" shortcut
$sendToDir = Join-Path $env:APPDATA "Microsoft\Windows\SendTo"
if (Test-Path $sendToDir) {
    $sendToShortcut = $wscript.CreateShortcut((Join-Path $sendToDir "Print on $PeerName.lnk"))
    $sendToShortcut.TargetPath = "C:\BridgeApp\bridge.exe"
    $sendToShortcut.Arguments = "-send-print -target `"$PeerName`""
    $sendToShortcut.Description = "Send file to print on $PeerName"
    $sendToShortcut.Save()
}

Write-Host "Canonical Desktop shortcut created ('Bridge Shared Folder')." -ForegroundColor Green
Write-Host "Direct Printing ready: Select 'Print on $PeerName' in any print dialog (Ctrl+P)." -ForegroundColor Green
Write-Host "SendTo right-click shortcut created ('Print on $PeerName')." -ForegroundColor Green

# Launch bridge.exe in current interactive session with tray icon (Session 1)
Start-Process "C:\BridgeApp\bridge.exe" -ArgumentList "-autostart"

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
