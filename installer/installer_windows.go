//go:build windows

package installer

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"bridge/config"
	"bridge/printer"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	kernel32DLL = syscall.NewLazyDLL("kernel32.dll")
	user32DLL   = syscall.NewLazyDLL("user32.dll")

	procFindWindowW         = user32DLL.NewProc("FindWindowW")
	procShowWindow          = user32DLL.NewProc("ShowWindow")
	procSetForegroundWindow = user32DLL.NewProc("SetForegroundWindow")
	procSetConsoleTitleW    = kernel32DLL.NewProc("SetConsoleTitleW")
	procEnumWindows         = user32DLL.NewProc("EnumWindows")
	procGetWindowTextW      = user32DLL.NewProc("GetWindowTextW")

	interactiveMutex windows.Handle
	serviceMutex     windows.Handle
)

// hideCmd configures the command to run completely invisibly without spawning a console window.
func hideCmd(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: 0x08000000, // CREATE_NO_WINDOW
		}
	}
}

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

// formatElevatedArgs escapes each argument per Windows command-line rules so paths with spaces/quotes survive UAC relaunch.
func formatElevatedArgs(args []string) string {
	escaped := make([]string, len(args))
	for i, a := range args {
		escaped[i] = windows.EscapeArg(a)
	}
	return strings.Join(escaped, " ")
}

// SelfElevate relaunches the current executable with administrative privileges (UAC prompt).
func SelfElevate(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verbPtr, _ := windows.UTF16PtrFromString("runas")
	exePtr, _ := windows.UTF16PtrFromString(exe)

	cwd := filepath.Dir(exe)
	for _, a := range args {
		if strings.Contains(a, "-uninstall") {
			cwd = `C:\`
			break
		}
	}
	cwdPtr, _ := windows.UTF16PtrFromString(cwd)

	var argPtr *uint16
	if len(args) > 0 {
		argsStr := formatElevatedArgs(args)
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
	hideCmd(cmd)
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
	hideCmd(dlCmd)
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
	hideCmd(cmd)
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

// FindSumatraPDF searches for SumatraPDF.exe across standard system locations.
func FindSumatraPDF() string {
	return printer.FindSumatraPDF()
}

// InstallSumatraPDF installs standalone SumatraPDF silently and verifies the executable.
func InstallSumatraPDF() (string, error) {
	fmt.Println("[*] Auto-provisioning standalone Silent Print Engine (SumatraPDF)...")
	path, err := printer.EnsureSumatraPDF()
	if err != nil {
		// Secondary fallback: winget if available
		fmt.Printf("[*] Direct standalone download returned (%v); trying winget fallback...\n", err)
		cmd := exec.Command("winget", "install", "-e", "--id", "SumatraPDF.SumatraPDF", "--accept-package-agreements", "--accept-source-agreements", "--silent")
		hideCmd(cmd)
		_, _ = cmd.CombinedOutput()
		for i := 0; i < 5; i++ {
			if found := printer.FindSumatraPDF(); found != "" {
				removeSumatraShortcuts()
				return found, nil
			}
			time.Sleep(1 * time.Second)
		}
		return "", fmt.Errorf("failed to install SumatraPDF: %w", err)
	}

	removeSumatraShortcuts()
	return path, nil
}

func expandDesktopRegistryPath(val string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return ""
	}
	if exp, err := registry.ExpandString(val); err == nil && strings.TrimSpace(exp) != "" {
		val = exp
	}
	return os.ExpandEnv(val)
}

func isExistingDir(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	info, err := os.Stat(filepath.Clean(p))
	return err == nil && info.IsDir()
}

func readRegistryDesktopDir(subkey string) string {
	k, err := registry.OpenKey(registry.CURRENT_USER, subkey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	val, _, err := k.GetStringValue("Desktop")
	if err != nil || strings.TrimSpace(val) == "" {
		return ""
	}
	expanded := filepath.Clean(expandDesktopRegistryPath(val))
	if isExistingDir(expanded) {
		return expanded
	}
	return ""
}

func collectAllDesktopDirs() []string {
	desktopMap := make(map[string]bool)
	var dirs []string

	addDir := func(d string) {
		if strings.TrimSpace(d) == "" {
			return
		}
		clean := filepath.Clean(d)
		low := strings.ToLower(clean)
		if isExistingDir(clean) && !desktopMap[low] {
			desktopMap[low] = true
			dirs = append(dirs, clean)
		}
	}

	// 1. User Shell Folders & Shell Folders registry keys
	addDir(readRegistryDesktopDir(`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`))
	addDir(readRegistryDesktopDir(`Software\Microsoft\Windows\CurrentVersion\Explorer\Shell Folders`))

	// 2. Current user's desktop and OneDrive desktop
	if u := os.Getenv("USERPROFILE"); u != "" {
		addDir(filepath.Join(u, "Desktop"))
		addDir(filepath.Join(u, "OneDrive", "Desktop"))
	}

	// 3. Public Desktop
	addDir(`C:\Users\Public\Desktop`)

	// 4. Scan C:\Users for interactive user profiles
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

// GetPrimaryDesktopDir resolves the single active Windows Desktop directory for the current user.
func GetPrimaryDesktopDir() string {
	if d := readRegistryDesktopDir(`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`); d != "" {
		return d
	}
	if d := readRegistryDesktopDir(`Software\Microsoft\Windows\CurrentVersion\Explorer\Shell Folders`); d != "" {
		return d
	}
	if u := os.Getenv("USERPROFILE"); u != "" {
		for _, cand := range []string{
			filepath.Join(u, "Desktop"),
			filepath.Join(u, "OneDrive", "Desktop"),
		} {
			clean := filepath.Clean(cand)
			if isExistingDir(clean) {
				return clean
			}
		}
	}
	if all := collectAllDesktopDirs(); len(all) > 0 {
		return all[0]
	}
	return ""
}

// GetAllDesktopDirs collects all valid desktop directories on the system, placing GetPrimaryDesktopDir() at index 0.
func GetAllDesktopDirs() []string {
	primary := GetPrimaryDesktopDir()
	rest := collectAllDesktopDirs()

	desktopMap := make(map[string]bool)
	var dirs []string
	addDir := func(d string) {
		if strings.TrimSpace(d) == "" {
			return
		}
		clean := filepath.Clean(d)
		low := strings.ToLower(clean)
		if isExistingDir(clean) && !desktopMap[low] {
			desktopMap[low] = true
			dirs = append(dirs, clean)
		}
	}

	if primary != "" {
		addDir(primary)
	}
	for _, d := range rest {
		addDir(d)
	}
	return dirs
}

func escapeVBS(s string) string {
	return strings.ReplaceAll(s, `"`, `""`)
}

// CreateDesktopShortcut creates a Windows .lnk shortcut using VBScript.
func CreateDesktopShortcut(targetPath, lnkPath, description, arguments string) error {
	// Fast-path: If shortcut already exists on disk, avoid spawning wscript
	if fi, err := os.Stat(lnkPath); err == nil && fi.Size() > 0 {
		return nil
	}

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

	cmd := exec.Command("wscript", "//B", "//nologo", tmpVbs)
	hideCmd(cmd)
	return cmd.Run()
}

// InstallVirtualPrinter registers a Windows RAW TCP port on 127.0.0.1:port and creates
// a virtual printer named "Print on <PeerName>".
func InstallVirtualPrinter(peerName string, port int) error {
	if strings.TrimSpace(peerName) == "" {
		return nil
	}
	cleanPeer := SanitizeFileName(peerName)
	if cleanPeer == "" {
		return nil
	}

	localNode := ""
	if cfg, err := config.LoadConfig(TargetConfig); err == nil && cfg.NodeName != "" {
		localNode = cfg.NodeName
	}
	if config.IsSelfTarget(cleanPeer, "", "", localNode, 0) {
		return nil // Strictly never install virtual printer for self device
	}

	if port <= 0 {
		port = 9101
	}
	printerName := fmt.Sprintf("Print on %s", cleanPeer)
	portName := fmt.Sprintf("Bridge_Port_%d", port)
	const printerAttrWorkOffline = 0x00000400

	// In-Process Fast Registry Check (0 subprocesses, <1ms):
	// Verify printer exists with matching port, WorkOffline is disabled, and TCP port has SNMP disabled.
	printerClean := false
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Print\Printers\`+printerName, registry.QUERY_VALUE)
	if err == nil {
		portVal, _, portErr := k.GetStringValue("Port")
		attrs, _, attrErr := k.GetIntegerValue("Attributes")
		k.Close()

		if portErr == nil && strings.EqualFold(strings.TrimSpace(portVal), portName) &&
			attrErr == nil && (attrs&printerAttrWorkOffline) == 0 {
			pk, pkErr := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\`+portName, registry.QUERY_VALUE)
			if pkErr == nil {
				snmpVal, _, snmpErr := pk.GetIntegerValue("SNMP Enabled")
				portNum, _, numErr := pk.GetIntegerValue("PortNumber")
				pk.Close()
				if snmpErr == nil && snmpVal == 0 && numErr == nil && int(portNum) == port {
					printerClean = true
				}
			}
		}
	}
	if printerClean {
		return nil // Already installed and cleanly configured (SNMP=0, WorkOffline=false)
	}

	if !IsElevated() {
		return fmt.Errorf("administrator privileges required to configure virtual printer %q on port %d", printerName, port)
	}

	psPortName := strings.ReplaceAll(portName, `'`, `''`)
	psPrinterName := strings.ReplaceAll(printerName, `'`, `''`)

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
    Add-PrinterPort -Name $portName -PrinterHostAddress '127.0.0.1' -PortNumber $portNumber -SNMP 0 -ErrorAction Stop
}
try {
    Set-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\$portName" -Name "SNMP Enabled" -Value 0 -Type DWord -ErrorAction SilentlyContinue
} catch {}

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
    # If XPS driver was removed in Windows optional features, enable Printing-XPSServices-Features
    Start-Process -FilePath "dism.exe" -ArgumentList "/Online /Enable-Feature /FeatureName:Printing-XPSServices-Features /NoRestart /Quiet" -Wait -WindowStyle Hidden -ErrorAction SilentlyContinue
    foreach ($d in $preferredDrivers) {
        if (Get-PrinterDriver -Name $d -ErrorAction SilentlyContinue) {
            $driver = $d
            break
        }
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
    if (Get-PrinterDriver -Name 'Microsoft Print To PDF' -ErrorAction SilentlyContinue) {
        $driver = 'Microsoft Print To PDF'
    }
}
if (-not $driver) {
    throw "A compatible virtual printer driver (Microsoft XPS Document Writer or Microsoft Print To PDF) is required for native remote printing but is not installed."
}

if (-not (Get-Printer -Name $printerName -ErrorAction SilentlyContinue)) {
    Add-Printer -Name $printerName -DriverName $driver -PortName $portName -ErrorAction Stop
} else {
    Set-Printer -Name $printerName -PortName $portName -DriverName $driver -ErrorAction SilentlyContinue
}

# Ensure WorkOffline is always disabled and clear stuck error print jobs
try {
    $pObj = Get-CimInstance Win32_Printer -Filter "Name = '$($printerName.Replace("'", "''"))'" -ErrorAction SilentlyContinue
    if ($pObj -and $pObj.WorkOffline) {
        $pObj.WorkOffline = $false
        Set-CimInstance -InputObject $pObj -ErrorAction SilentlyContinue
    }
    Get-PrintJob -PrinterName $printerName -ErrorAction SilentlyContinue | Where-Object { $_.JobStatus -match 'Error|Blocked|Offline|Paused' } | Remove-PrintJob -ErrorAction SilentlyContinue
} catch {}

# Configure virtual printer default paper size to match regional standard (A4 or Letter)
try {
    $geo = (Get-Culture).Name
    $defPaper = if ($geo -match 'en-US|en-CA') { 'Letter' } else { 'A4' }
    Set-PrintConfiguration -PrinterName $printerName -PaperSize $defPaper -ErrorAction SilentlyContinue
} catch {}

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
`, psPortName, port, psPrinterName)

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	hideCmd(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("virtual printer setup failed for %s on port %d: %w (%s)", printerName, port, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveVirtualPrinter uninstalls a specific Bridge virtual printer if present.
func RemoveVirtualPrinter(peerName string) error {
	cleanPeer := SanitizeFileName(peerName)
	printerName := fmt.Sprintf("Print on %s", cleanPeer)

	psScript := fmt.Sprintf(`
$pName = '%s'
Get-Printer -Name $pName -ErrorAction SilentlyContinue | Remove-Printer -ErrorAction SilentlyContinue
Get-CimInstance Win32_Printer -Filter "Name = '$($pName.Replace("'", "''"))'" -ErrorAction SilentlyContinue | Remove-CimInstance -ErrorAction SilentlyContinue
`, strings.ReplaceAll(printerName, `'`, `''`))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	hideCmd(cmd)
	return cmd.Run()
}

// PruneObsoleteVirtualPrinters removes any virtual printers and TCP ports that do not match the active peers.
func PruneObsoleteVirtualPrinters(activePeers []config.PeerDevice) error {
	var validPrinters []string
	var validPorts []string

	for _, p := range activePeers {
		clean := SanitizeFileName(p.Name)
		validPrinters = append(validPrinters, fmt.Sprintf("'%s'", strings.ReplaceAll(fmt.Sprintf("Print on %s", clean), `'`, `''`)))
		port := p.SpoolPort
		if port <= 0 {
			port = 9101
		}
		validPorts = append(validPorts, fmt.Sprintf("'Bridge_Port_%d'", port))
	}

	printersArray := strings.Join(validPrinters, ", ")
	portsArray := strings.Join(validPorts, ", ")

	psScript := fmt.Sprintf(`
$validPrinters = @( %s )
$validPorts = @( %s )

# 1. Remove obsolete Bridge virtual printers that are not in validPrinters
Get-Printer -ErrorAction SilentlyContinue | Where-Object {
    ($_.Name -like 'Print on *' -or $_.PortName -like 'Bridge_Port_*' -or $_.PortName -eq 'Bridge_TCP_Port') -and ($_.Name -notin $validPrinters)
} | ForEach-Object {
    $pName = $_.Name
    Get-PrintJob -PrinterName $pName -ErrorAction SilentlyContinue | Remove-PrintJob -ErrorAction SilentlyContinue
    Remove-Printer -Name $pName -ErrorAction SilentlyContinue
    try {
        Get-CimInstance Win32_Printer -Filter "Name = '$($pName.Replace("'", "''"))'" -ErrorAction SilentlyContinue | Remove-CimInstance -ErrorAction SilentlyContinue
    } catch {}
}

# 2. Remove obsolete Bridge TCP ports that are not in validPorts
Get-PrinterPort -ErrorAction SilentlyContinue | Where-Object {
    ($_.Name -like 'Bridge_Port_*' -or $_.Name -eq 'Bridge_TCP_Port') -and ($_.Name -notin $validPorts)
} | ForEach-Object {
    Remove-PrinterPort -Name $_.Name -ErrorAction SilentlyContinue
}
`, printersArray, portsArray)

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	hideCmd(cmd)
	return cmd.Run()
}

// UninstallAllVirtualPrinters removes all Bridge virtual printers and TCP ports.
func UninstallAllVirtualPrinters() error {
	psScript := `
# 1. Clear any stuck print jobs on Bridge virtual printers
Get-Printer -ErrorAction SilentlyContinue | Where-Object { 
    $_.PortName -like 'Bridge_Port_*' -or $_.PortName -eq 'Bridge_TCP_Port' -or $_.Name -like 'Print on *' -or $_.Name -like '*Bridge*'
} | ForEach-Object {
    $pName = $_.Name
    Get-PrintJob -PrinterName $pName -ErrorAction SilentlyContinue | Remove-PrintJob -ErrorAction SilentlyContinue
}

# 2. Reset default printer if set to a Bridge virtual printer
$currDef = $null
try { $currDef = (Get-CimInstance Win32_Printer -Filter "Default = True" -ErrorAction SilentlyContinue).Name } catch {}
if ($currDef -and ($currDef -like 'Print on *' -or $currDef -like '*Bridge*')) {
    $fallback = (Get-CimInstance Win32_Printer -ErrorAction SilentlyContinue | Where-Object {
        $_.Name -notlike 'Print on *' -and
        $_.Name -notlike '*Bridge*' -and
        $_.PortName -notlike 'Bridge_*' -and
        $_.PortName -ne 'Bridge_TCP_Port'
    } | Select-Object -First 1).Name
    if ($fallback) {
        (New-Object -ComObject WScript.Network).SetDefaultPrinter($fallback)
    }
}

# 3. Remove all Bridge virtual printers via standard cmdlet and WMI CIM
Get-Printer -ErrorAction SilentlyContinue | Where-Object { 
    $_.PortName -like 'Bridge_Port_*' -or $_.PortName -eq 'Bridge_TCP_Port' -or $_.Name -like 'Print on *' -or $_.Name -like '*Bridge*'
} | Remove-Printer -ErrorAction SilentlyContinue

Get-CimInstance Win32_Printer -ErrorAction SilentlyContinue | Where-Object {
    $_.PortName -like 'Bridge_Port_*' -or $_.PortName -eq 'Bridge_TCP_Port' -or $_.Name -like 'Print on *' -or $_.Name -like '*Bridge*'
} | ForEach-Object {
    Remove-CimInstance -InputObject $_ -ErrorAction SilentlyContinue
}

# 4. Restart Spooler to flush port bindings, then remove all Bridge TCP ports
Restart-Service spooler -Force -ErrorAction SilentlyContinue
Get-PrinterPort -ErrorAction SilentlyContinue | Where-Object { 
    $_.Name -like 'Bridge_Port_*' -or $_.Name -eq 'Bridge_TCP_Port' 
} | Remove-PrinterPort -ErrorAction SilentlyContinue
`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	hideCmd(cmd)
	return cmd.Run()
}

// KillOtherBridgeProcesses terminates any other running bridge.exe instances
// while explicitly exempting the current process PID to avoid process suicide.
func KillOtherBridgeProcesses() {
	currPid := os.Getpid()
	filter := fmt.Sprintf("PID ne %d", currPid)
	cmd := exec.Command("taskkill", "/F", "/FI", filter, "/IM", "bridge.exe")
	hideCmd(cmd)
	_ = cmd.Run()
}

// AcquireInteractiveLock attempts to acquire a named mutex ensuring only one interactive dashboard/setup terminal runs.
// If another instance is already running, it brings that instance's console window to the foreground and returns false.
func AcquireInteractiveLock() bool {
	if interactiveMutex != 0 {
		BringDashboardToFront()
		return false
	}
	mutexName, _ := windows.UTF16PtrFromString("Local\\BridgeApp_Interactive_Mutex")
	h, err := windows.CreateMutex(nil, false, mutexName)
	if err == windows.ERROR_ALREADY_EXISTS {
		BringDashboardToFront()
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return false
	}
	if err != nil && h == 0 {
		return true // Fallback: allow launch if mutex creation fails
	}
	interactiveMutex = h
	return true
}

// ReleaseInteractiveLock releases the interactive dashboard mutex.
func ReleaseInteractiveLock() {
	if interactiveMutex != 0 {
		_ = windows.CloseHandle(interactiveMutex)
		interactiveMutex = 0
	}
}

// BringDashboardToFront finds the running dashboard console window and brings it to the foreground.
// It uses both direct title lookup and EnumWindows fallback to locate windows across conhost, Windows Terminal, and PowerShell.
func BringDashboardToFront() {
	var hwnd uintptr

	// 1. Exact title check
	titlePtr, _ := windows.UTF16PtrFromString("Bridge Multi-Device Dashboard")
	hwnd, _, _ = procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))

	if hwnd == 0 {
		setupTitlePtr, _ := windows.UTF16PtrFromString("Bridge Multi-Device Setup")
		hwnd, _, _ = procFindWindowW.Call(0, uintptr(unsafe.Pointer(setupTitlePtr)))
	}

	// 2. Fuzzy window title match (handles Windows Terminal, PowerShell, or shells appending suffix)
	if hwnd == 0 {
		cb := syscall.NewCallback(func(h uintptr, lparam uintptr) uintptr {
			var buf [512]uint16
			r, _, _ := procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 512)
			if r > 0 {
				title := windows.UTF16ToString(buf[:r])
				if strings.Contains(title, "Bridge Multi-Device Dashboard") || strings.Contains(title, "Bridge Multi-Device Setup") {
					hwnd = h
					return 0 // Stop enumeration
				}
			}
			return 1 // Continue enumeration
		})
		procEnumWindows.Call(cb, 0)
	}

	if hwnd != 0 {
		procShowWindow.Call(hwnd, 9) // SW_RESTORE
		procSetForegroundWindow.Call(hwnd)
	}
}

// SetDashboardConsoleTitle sets the console window title so it can be located and brought to front.
func SetDashboardConsoleTitle(title string) {
	titlePtr, _ := windows.UTF16PtrFromString(title)
	procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(titlePtr)))
}

// AcquireServiceLock ensures only one background service runs.
// It checks the Global namespace first, with fallback to Local namespace for unprivileged standard user accounts.
func AcquireServiceLock() bool {
	if serviceMutex != 0 {
		return false
	}
	mutexName, _ := windows.UTF16PtrFromString("Global\\BridgeApp_Service_Mutex")
	h, err := windows.CreateMutex(nil, false, mutexName)
	if err == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return false
	}
	if err != nil && h == 0 {
		// Fallback for non-admin tokens without SeCreateGlobalPrivilege
		localName, _ := windows.UTF16PtrFromString("Local\\BridgeApp_Service_Mutex")
		h, err = windows.CreateMutex(nil, false, localName)
		if err == windows.ERROR_ALREADY_EXISTS {
			if h != 0 {
				_ = windows.CloseHandle(h)
			}
			return false
		}
		if err != nil && h == 0 {
			return true
		}
	}
	serviceMutex = h
	return true
}

// ReleaseServiceLock releases the background service mutex.
func ReleaseServiceLock() {
	if serviceMutex != 0 {
		_ = windows.CloseHandle(serviceMutex)
		serviceMutex = 0
	}
}

const bridgeRegSubkey = `Software\BridgeApp`

func isBridgeLogonTaskXMLCurrent(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	clean := strings.ToLower(string(bytes.ReplaceAll(raw, []byte{0}, nil)))
	return strings.Contains(clean, strings.ToLower(TargetExe)) &&
		strings.Contains(clean, "-autostart") &&
		strings.Contains(clean, "<logontrigger>") &&
		strings.Contains(clean, "<runlevel>highestavailable</runlevel>") &&
		strings.Contains(clean, "<logontype>interactivetoken</logontype>") &&
		strings.Contains(clean, "<disallowstartifonbatteries>false</disallowstartifonbatteries>") &&
		strings.Contains(clean, "<stopifgoingonbatteries>false</stopifgoingonbatteries>") &&
		strings.Contains(clean, "<executiontimelimit>pt0s</executiontimelimit>")
}

func isBridgeLogonTaskCurrent() bool {
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	taskXMLPath := filepath.Join(sysRoot, "System32", "Tasks", LogonTaskName)
	raw, err := os.ReadFile(taskXMLPath)
	if err != nil {
		return false
	}
	return isBridgeLogonTaskXMLCurrent(raw)
}

func buildRegisterLogonTaskScript(exePath, taskName string) string {
	escExe := strings.ReplaceAll(exePath, "'", "''")
	escTask := strings.ReplaceAll(taskName, "'", "''")
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$exe = '%s'
$taskName = '%s'
$user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
if ([string]::IsNullOrWhiteSpace($user)) {
    $user = "$env:USERDOMAIN\$env:USERNAME"
}
$action = New-ScheduledTaskAction -Execute $exe -Argument '-autostart'
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force -ErrorAction Stop | Out-Null
`, escExe, escTask)
}

func removeHKCURunBridgeEntries() {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()

	if valNames, err := k.ReadValueNames(-1); err == nil {
		for _, vn := range valNames {
			if strings.Contains(strings.ToLower(vn), "bridge") {
				_ = k.DeleteValue(vn)
				continue
			}
			if val, _, err := k.GetStringValue(vn); err == nil && strings.Contains(strings.ToLower(val), "bridge.exe") {
				_ = k.DeleteValue(vn)
			}
		}
	}
}

func setStartupModeMarker(mode string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, bridgeRegSubkey, registry.SET_VALUE)
	if err == nil {
		_ = k.SetStringValue("StartupMode", mode)
		k.Close()
	}
}

func clearStartupModeMarker() {
	_ = registry.DeleteKey(registry.CURRENT_USER, bridgeRegSubkey)
}

func hasBridgeLogonMarker() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, bridgeRegSubkey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	val, _, err := k.GetStringValue("StartupMode")
	return err == nil && strings.EqualFold(strings.TrimSpace(val), LogonTaskName)
}

// RegisterInteractiveStartup ensures bridge.exe starts silently in Session 1 at user logon.
// When elevated, it registers the interactive elevated logon task BridgeAppLogon (configured to
// start on battery, stay running on battery, and have no execution time limit) and removes
// HKCU\Run\BridgeApp to avoid dual-launch races, falling back cleanly to HKCU\Run if
// Register-ScheduledTask fails. Non-elevated calls stay fast and in-process (<1ms).
func RegisterInteractiveStartup() error {
	if IsElevated() {
		if isBridgeLogonTaskCurrent() {
			removeHKCURunBridgeEntries()
			setStartupModeMarker(LogonTaskName)
			return nil
		}

		psScript := buildRegisterLogonTaskScript(TargetExe, LogonTaskName)
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
		hideCmd(cmd)
		if err := cmd.Run(); err == nil {
			removeHKCURunBridgeEntries()
			setStartupModeMarker(LogonTaskName)
			return nil
		}
		// Fall back to HKCU\Run if Register-ScheduledTask fails or is blocked by policy
		clearStartupModeMarker()
	} else if hasBridgeLogonMarker() {
		// Elevated logon task is already registered; keep HKCU\Run clean so two instances never race
		removeHKCURunBridgeEntries()
		return nil
	}

	runCmd := fmt.Sprintf(`"%s" -autostart`, TargetExe)

	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if valNames, err := k.ReadValueNames(-1); err == nil {
		for _, vn := range valNames {
			if vn == "BridgeApp" {
				continue
			}
			if strings.Contains(strings.ToLower(vn), "bridge") {
				_ = k.DeleteValue(vn)
				continue
			}
			if val, _, err := k.GetStringValue(vn); err == nil && strings.Contains(strings.ToLower(val), "bridge.exe") {
				_ = k.DeleteValue(vn)
			}
		}
	}

	if existing, _, err := k.GetStringValue("BridgeApp"); err == nil && strings.EqualFold(existing, runCmd) {
		return nil
	}
	return k.SetStringValue("BridgeApp", runCmd)
}

// UnregisterInteractiveStartup cleanly removes startup entries across all possible locations.
func UnregisterInteractiveStartup() {
	clearStartupModeMarker()

	// 1. In-process HKCU Run Key clean
	removeHKCURunBridgeEntries()

	// 2. In-process HKLM Run Key clean (if process is elevated)
	kAdmin, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err == nil {
		if valNames, err := kAdmin.ReadValueNames(-1); err == nil {
			for _, vn := range valNames {
				if strings.Contains(strings.ToLower(vn), "bridge") {
					_ = kAdmin.DeleteValue(vn)
					continue
				}
				if val, _, err := kAdmin.GetStringValue(vn); err == nil && strings.Contains(strings.ToLower(val), "bridge.exe") {
					_ = kAdmin.DeleteValue(vn)
				}
			}
		}
		kAdmin.Close()
	}
	kAdmin32, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err == nil {
		if valNames, err := kAdmin32.ReadValueNames(-1); err == nil {
			for _, vn := range valNames {
				if strings.Contains(strings.ToLower(vn), "bridge") {
					_ = kAdmin32.DeleteValue(vn)
					continue
				}
				if val, _, err := kAdmin32.GetStringValue(vn); err == nil && strings.Contains(strings.ToLower(val), "bridge.exe") {
					_ = kAdmin32.DeleteValue(vn)
				}
			}
		}
		kAdmin32.Close()
	}

	// 3. User & Common Startup Folder Shortcuts (purge any shortcuts pointing to bridge.exe across all users)
	startupDirs := []string{
		filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup"),
		`C:\ProgramData\Microsoft\Windows\Start Menu\Programs\Startup`,
	}
	if uDirs, err := os.ReadDir(`C:\Users`); err == nil {
		for _, u := range uDirs {
			if u.IsDir() {
				startupDirs = append(startupDirs, filepath.Join(`C:\Users`, u.Name(), "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup"))
			}
		}
	}
	for _, sd := range startupDirs {
		if sd != "" {
			if entries, err := os.ReadDir(sd); err == nil {
				for _, e := range entries {
					low := strings.ToLower(e.Name())
					if strings.Contains(low, "bridge") && strings.HasSuffix(low, ".lnk") {
						_ = os.Remove(filepath.Join(sd, e.Name()))
					}
				}
			}
		}
	}

	// 4. Exhaustive PowerShell cleanup across all Run keys, HKEY_USERS hives, and Task Scheduler
	purgeScript := `
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
                    Remove-ItemProperty -Path $rk -Name $prop.Name -Force -ErrorAction SilentlyContinue
                }
            }
        }
    }
}
Get-ScheduledTask -ErrorAction SilentlyContinue | Where-Object { 
    $_.TaskName -like "*bridge*" -or ($_.Actions -and ($_.Actions.Execute -like "*bridge.exe*"))
} | Unregister-ScheduledTask -Confirm:$false -ErrorAction SilentlyContinue
`
	psCmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", purgeScript)
	hideCmd(psCmd)
	_ = psCmd.Run()

	// 5. Explicit Task Scheduler removals (Interactive Logon task + Legacy Session 0 task)
	for _, tn := range []string{LogonTaskName, TaskName} {
		cmdSch := exec.Command("schtasks", "/Delete", "/TN", tn, "/F")
		hideCmd(cmdSch)
		_ = cmdSch.Run()
	}
}
