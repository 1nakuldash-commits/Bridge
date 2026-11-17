//go:build windows

package installer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// IsElevated checks whether current process has administrative privileges.
func IsElevated() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

// SelfElevate relaunches the current executable with administrative privileges (UAC prompt).
func SelfElevate(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verbPtr, _ := windows.UTF16PtrFromString("runas")
	exePtr, _ := windows.UTF16PtrFromString(exe)
	cwdPtr, _ := windows.UTF16PtrFromString(filepath.Dir(exe))

	var argPtr *uint16
	if len(args) > 0 {
		argsStr := strings.Join(args, " ")
		argPtr, _ = windows.UTF16PtrFromString(argsStr)
	}

	err = windows.ShellExecute(0, verbPtr, exePtr, argPtr, cwdPtr, windows.SW_SHOWNORMAL)
	if err != nil {
		return fmt.Errorf("failed to elevate process: %w", err)
	}
	os.Exit(0)
	return nil
}

// OpenBrowser opens a URL in the user's default web browser.
func OpenBrowser(targetURL string) error {
	verbPtr, _ := windows.UTF16PtrFromString("open")
	urlPtr, _ := windows.UTF16PtrFromString(targetURL)
	return windows.ShellExecute(0, verbPtr, urlPtr, nil, nil, windows.SW_SHOWNORMAL)
}

// FindTailscale searches standard installation paths for tailscale.exe.
func FindTailscale() string {
	if p, err := exec.LookPath("tailscale.exe"); err == nil {
		return p
	}
	candidates := []string{
		`C:\Program Files\Tailscale\tailscale.exe`,
		`C:\Program Files (x86)\Tailscale\tailscale.exe`,
		`C:\Program Files\Tailscale IPN\tailscale.exe`,
		`C:\Program Files (x86)\Tailscale IPN\tailscale.exe`,
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Tailscale\tailscale.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Tailscale\tailscale.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Tailscale\tailscale.exe`),
	}
	for _, c := range candidates {
		if c != "" {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	return ""
}

// InstallTailscale downloads and installs Tailscale silently via winget, falling back to direct download.
func InstallTailscale() (string, error) {
	fmt.Println("[*] Downloading and installing Tailscale silently via winget...")
	cmd := exec.Command("winget", "install", "-e", "--id", "Tailscale.Tailscale", "--accept-package-agreements", "--accept-source-agreements", "--silent")
	out, err := cmd.CombinedOutput()
	if err == nil {
		for i := 0; i < 15; i++ {
			if path := FindTailscale(); path != "" {
				return path, nil
			}
			time.Sleep(1 * time.Second)
		}
	}

	// Fallback: Direct MSI / EXE download via PowerShell
	fmt.Println("[*] Winget unavailable or failed. Downloading official Tailscale installer directly...")
	psCmd := `
$installer = Join-Path $env:TEMP "tailscale-setup.exe"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
Invoke-WebRequest -Uri "https://pkgs.tailscale.com/stable/tailscale-setup-latest.exe" -OutFile $installer -UseBasicParsing
Start-Process -FilePath $installer -ArgumentList "/quiet" -Wait
Remove-Item $installer -Force -ErrorAction SilentlyContinue
`
	dlCmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psCmd)
	_ = dlCmd.Run()

	for i := 0; i < 15; i++ {
		if path := FindTailscale(); path != "" {
			return path, nil
		}
		time.Sleep(1 * time.Second)
	}

	return "", fmt.Errorf("tailscale installation attempt completed but binary not found: %w (%s)", err, string(out))
}

// GetTailscaleIP queries the local Tailscale IPv4 address.
// It prioritizes instantaneous (<1ms) interface detection to avoid subprocess spawns,
// and caches the result with a TTL.
func GetTailscaleIP() string {
	// 1. Fast in-process network interface check (instant, 0 process spawns)
	if ip := FindLocalTailscaleIP(); ip != "" {
		tsIPMu.Lock()
		cachedTSIP = ip
		lastTSCheck = time.Now()
		tsIPMu.Unlock()
		return ip
	}

	// 2. Check cached value
	tsIPMu.RLock()
	if cachedTSIP != "" && time.Since(lastTSCheck) < 30*time.Second {
		cached := cachedTSIP
		tsIPMu.RUnlock()
		return cached
	}
	tsIPMu.RUnlock()

	// 3. Fallback: Query tailscale CLI
	bin := FindTailscale()
	if bin == "" {
		return ""
	}
	cmd := exec.Command(bin, "ip", "-4")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(out))
	if strings.HasPrefix(ip, "100.") {
		tsIPMu.Lock()
		cachedTSIP = ip
		lastTSCheck = time.Now()
		tsIPMu.Unlock()
		return ip
	}
	return ""
}

// FindSumatraPDF searches for SumatraPDF.exe in standard locations.
func FindSumatraPDF() string {
	if p, err := exec.LookPath("SumatraPDF.exe"); err == nil {
		return p
	}
	candidates := []string{
		`C:\Program Files\SumatraPDF\SumatraPDF.exe`,
		`C:\Program Files (x86)\SumatraPDF\SumatraPDF.exe`,
		`C:\BridgeApp\SumatraPDF.exe`,
		filepath.Join(os.Getenv("LOCALAPPDATA"), `SumatraPDF\SumatraPDF.exe`),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// InstallSumatraPDF installs SumatraPDF silently via winget.
func InstallSumatraPDF() (string, error) {
	fmt.Println("[*] Installing SumatraPDF silently via winget...")
	cmd := exec.Command("winget", "install", "-e", "--id", "SumatraPDF.SumatraPDF", "--accept-package-agreements", "--accept-source-agreements", "--silent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("winget install SumatraPDF failed: %w (%s)", err, string(out))
	}

	for i := 0; i < 10; i++ {
		if path := FindSumatraPDF(); path != "" {
			return path, nil
		}
		time.Sleep(1 * time.Second)
	}
	return `C:\Program Files\SumatraPDF\SumatraPDF.exe`, nil
}

// GetAllDesktopDirs collects all valid desktop directories on the system.
func GetAllDesktopDirs() []string {
	desktopMap := make(map[string]bool)
	var dirs []string

	addDir := func(d string) {
		if strings.TrimSpace(d) == "" {
			return
		}
		clean := filepath.Clean(d)
		low := strings.ToLower(clean)
		if _, err := os.Stat(clean); err == nil && !desktopMap[low] {
			desktopMap[low] = true
			dirs = append(dirs, clean)
		}
	}

	// 1. Public Desktop
	addDir(`C:\Users\Public\Desktop`)

	// 2. Current user's desktop and OneDrive desktop
	if u := os.Getenv("USERPROFILE"); u != "" {
		addDir(filepath.Join(u, "Desktop"))
		addDir(filepath.Join(u, "OneDrive", "Desktop"))
	}

	// 3. User Shell Folders registry key
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`, registry.QUERY_VALUE)
	if err == nil {
		if val, _, err := k.GetStringValue("Desktop"); err == nil && val != "" {
			addDir(os.ExpandEnv(val))
		}
		k.Close()
	}

	// 4. Current user Shell Folders
	k2, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Explorer\Shell Folders`, registry.QUERY_VALUE)
	if err == nil {
		if val, _, err := k2.GetStringValue("Desktop"); err == nil && val != "" {
			addDir(os.ExpandEnv(val))
		}
		k2.Close()
	}

	// 5. Scan C:\Users for interactive user profiles
	entries, err := os.ReadDir(`C:\Users`)
	if err == nil {
		ignored := map[string]bool{
			"all users":    true,
			"default":      true,
			"default user": true,
			"public":       true,
		}
		for _, e := range entries {
			if e.IsDir() && !ignored[strings.ToLower(e.Name())] {
				userBase := filepath.Join(`C:\Users`, e.Name())
				addDir(filepath.Join(userBase, "Desktop"))
				addDir(filepath.Join(userBase, "OneDrive", "Desktop"))
			}
		}
	}

	return dirs
}

func escapeVBS(s string) string {
	return strings.ReplaceAll(s, `"`, `""`)
}

// CreateDesktopShortcut creates a Windows .lnk shortcut using VBScript.
func CreateDesktopShortcut(targetPath, lnkPath, description, arguments string) error {
	vbsContent := fmt.Sprintf(`Set oWS = WScript.CreateObject("WScript.Shell")
Set oLink = oWS.CreateShortcut("%s")
oLink.TargetPath = "%s"
oLink.Description = "%s"
oLink.Arguments = "%s"
oLink.Save
`, escapeVBS(lnkPath), escapeVBS(targetPath), escapeVBS(description), escapeVBS(arguments))

	tmpVbs := filepath.Join(os.TempDir(), fmt.Sprintf("mklink_%d.vbs", time.Now().UnixNano()))
	if err := os.WriteFile(tmpVbs, []byte(vbsContent), 0644); err != nil {
		return err
	}
	defer os.Remove(tmpVbs)

	cmd := exec.Command("cscript", "//nologo", tmpVbs)
	return cmd.Run()
}

// InstallVirtualPrinter registers a Windows RAW TCP port on 127.0.0.1:port and creates
// a virtual printer named "Print on <PeerName>".
func InstallVirtualPrinter(peerName string, port int) error {
	if port <= 0 {
		port = 9101
	}
	cleanPeer := SanitizeFileName(peerName)
	printerName := fmt.Sprintf("Print on %s", cleanPeer)
	portName := fmt.Sprintf("Bridge_Port_%d", port)

	psScript := fmt.Sprintf(`
$portName = '%s'
$portNumber = %d
$printerName = '%s'

# 1. Protect Windows default printer mode
Set-ItemProperty "HKCU:\Software\Microsoft\Windows NT\CurrentVersion\Windows" -Name "LegacyDefaultPrinterMode" -Value 1 -Type DWord -ErrorAction SilentlyContinue

# 2. Capture existing default printer before adding virtual printer
$existingDefault = $null
try {
    $existingDefault = (Get-CimInstance Win32_Printer -Filter "Default = True" -ErrorAction SilentlyContinue).Name
} catch {}

if (-not (Get-PrinterPort -Name $portName -ErrorAction SilentlyContinue)) {
    Add-PrinterPort -Name $portName -PrinterHostAddress '127.0.0.1' -PortNumber $portNumber -ErrorAction Stop
}

$preferredDrivers = @('Microsoft XPS Document Writer v4', 'Microsoft XPS Document Writer', 'Microsoft Software Printer Driver', 'Generic / Text Only')
$driver = $null
foreach ($d in $preferredDrivers) {
    if (Get-PrinterDriver -Name $d -ErrorAction SilentlyContinue) {
        $driver = $d
        break
    }
}
if (-not $driver) {
    $driver = (Get-PrinterDriver -ErrorAction SilentlyContinue | Where-Object { $_.Name -notlike '*PDF*' } | Select-Object -First 1).Name
}
if (-not $driver) {
    $driver = (Get-PrinterDriver -ErrorAction SilentlyContinue | Select-Object -First 1).Name
}
if (-not $driver) {
    throw "No printer drivers available"
}

if (-not (Get-Printer -Name $printerName -ErrorAction SilentlyContinue)) {
    Add-Printer -Name $printerName -DriverName $driver -PortName $portName -ErrorAction Stop
} else {
    Set-Printer -Name $printerName -PortName $portName -ErrorAction SilentlyContinue
}

# 3. Default Printer Selection:
# If current default is a REAL physical printer, keep it.
# If current default is a software/virtual printer (PDF, XPS, Fax, etc.) and no physical printer exists, set Bridge virtual printer as default.
$isPhysicalDefault = $false
if ($existingDefault) {
    $matched = Get-CimInstance Win32_Printer -Filter "Name = '$($existingDefault.Replace("'", "''"))'" -ErrorAction SilentlyContinue | Where-Object {
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
    }
    if ($matched) { $isPhysicalDefault = $true }
}

if ($isPhysicalDefault) {
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
    } else {
        (New-Object -ComObject WScript.Network).SetDefaultPrinter($printerName)
    }
}
`, portName, port, printerName)

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("virtual printer setup failed for %s on port %d: %w (%s)", printerName, port, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// UninstallAllVirtualPrinters removes all Bridge virtual printers and TCP ports.
func UninstallAllVirtualPrinters() error {
	psScript := `
Get-Printer -ErrorAction SilentlyContinue | Where-Object { 
    $_.PortName -like 'Bridge_Port_*' -or $_.PortName -eq 'Bridge_TCP_Port' -or $_.Name -like 'Print on *' 
} | Remove-Printer -ErrorAction SilentlyContinue

Get-PrinterPort -ErrorAction SilentlyContinue | Where-Object { 
    $_.Name -like 'Bridge_Port_*' -or $_.Name -eq 'Bridge_TCP_Port' 
} | Remove-PrinterPort -ErrorAction SilentlyContinue
`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	return cmd.Run()
}

// KillOtherBridgeProcesses terminates any other running bridge.exe instances
// while explicitly exempting the current process PID to avoid process suicide.
func KillOtherBridgeProcesses() {
	currPid := os.Getpid()
	filter := fmt.Sprintf("PID ne %d", currPid)
	_ = exec.Command("taskkill", "/F", "/FI", filter, "/IM", "bridge.exe").Run()
}

// RegisterInteractiveStartup ensures bridge.exe starts in Session 1 at user logon.
func RegisterInteractiveStartup() error {
	// 1. HKCU Run Key (Session 1 User)
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err == nil {
		_ = k.SetStringValue("BridgeApp", TargetExe)
		k.Close()
	}

	// 2. HKLM Run Key (Machine-wide fallback for any logged-in user)
	kAdmin, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err == nil {
		_ = kAdmin.SetStringValue("BridgeApp", TargetExe)
		kAdmin.Close()
	}

	// 3. User Startup Folder Shortcut
	startupDir := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	if _, err := os.Stat(startupDir); err == nil {
		_ = CreateDesktopShortcut(TargetExe, filepath.Join(startupDir, "BridgeApp.lnk"), "Bridge System Tray & Sync", "")
	}

	// 4. Common Startup Folder (All Users)
	commonStartup := `C:\ProgramData\Microsoft\Windows\Start Menu\Programs\Startup`
	if _, err := os.Stat(commonStartup); err == nil {
		_ = CreateDesktopShortcut(TargetExe, filepath.Join(commonStartup, "BridgeApp.lnk"), "Bridge System Tray & Sync", "")
	}

	return nil
}

// UnregisterInteractiveStartup removes startup entries.
func UnregisterInteractiveStartup() {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err == nil {
		_ = k.DeleteValue("BridgeApp")
		k.Close()
	}

	kAdmin, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err == nil {
		_ = kAdmin.DeleteValue("BridgeApp")
		kAdmin.Close()
	}

	startupLnk := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "BridgeApp.lnk")
	_ = os.Remove(startupLnk)

	commonStartupLnk := filepath.Join(`C:\ProgramData\Microsoft\Windows\Start Menu\Programs\Startup`, "BridgeApp.lnk")
	_ = os.Remove(commonStartupLnk)
}
