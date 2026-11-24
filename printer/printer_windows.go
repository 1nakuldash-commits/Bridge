//go:build windows

package printer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	winspool              = syscall.NewLazyDLL("winspool.drv")
	procGetDefaultPrinter = winspool.NewProc("GetDefaultPrinterW")
	procEnumPrintersW     = winspool.NewProc("EnumPrintersW")
)

const (
	PRINTER_ENUM_LOCAL       = 0x00000002
	PRINTER_ENUM_CONNECTIONS = 0x00000004
)

type PRINTER_INFO_2W struct {
	ServerName         *uint16
	PrinterName        *uint16
	ShareName          *uint16
	PortName           *uint16
	DriverName         *uint16
	Comment            *uint16
	Location           *uint16
	DevMode            uintptr
	SepFile            *uint16
	PrintProcessor     *uint16
	Datatype           *uint16
	Parameters         *uint16
	SecurityDescriptor uintptr
	Attributes         uint32
	Priority           uint32
	DefaultPriority    uint32
	StartTime          uint32
	UntilTime          uint32
	Status             uint32
	CJobs              uint32
	AveragePPM         uint32
}

// enumPrintersWin32 queries all Windows printers directly via winspool.drv (< 1ms, 0 subprocesses).
func enumPrintersWin32() ([]WindowsPrinter, error) {
	flags := uintptr(PRINTER_ENUM_LOCAL | PRINTER_ENUM_CONNECTIONS)
	level := uintptr(2)

	var needed, returned uint32
	procEnumPrintersW.Call(
		flags,
		0,
		level,
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if needed == 0 {
		return nil, errors.New("no printers found or EnumPrinters buffer check failed")
	}

	buf := make([]byte, needed)
	r1, _, err := procEnumPrintersW.Call(
		flags,
		0,
		level,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r1 == 0 {
		return nil, err
	}

	defPrinter, _ := GetDefaultWindowsPrinter()
	count := int(returned)
	if count == 0 {
		return nil, errors.New("no printers returned from winspool")
	}

	p2Slice := unsafe.Slice((*PRINTER_INFO_2W)(unsafe.Pointer(&buf[0])), count)
	var list []WindowsPrinter
	seen := make(map[string]bool)

	for i := 0; i < count; i++ {
		pName := ""
		if p2Slice[i].PrinterName != nil {
			pName = windows.UTF16PtrToString(p2Slice[i].PrinterName)
		}
		if pName == "" {
			continue
		}
		low := strings.ToLower(strings.TrimSpace(pName))
		if seen[low] {
			continue
		}
		seen[low] = true

		portName := ""
		if p2Slice[i].PortName != nil {
			portName = windows.UTF16PtrToString(p2Slice[i].PortName)
		}

		list = append(list, WindowsPrinter{
			Name:     pName,
			PortName: portName,
			Default:  strings.EqualFold(pName, defPrinter),
		})
	}

	if len(list) > 0 {
		return list, nil
	}
	return nil, errors.New("empty printer list from winspool EnumPrinters")
}

// GetDefaultWindowsPrinter queries Windows for the default paper printer name.
func GetDefaultWindowsPrinter() (string, error) {
	var size uint32 = 0
	procGetDefaultPrinter.Call(0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return "", errors.New("no default printer configured")
	}

	buf := make([]uint16, size)
	r1, _, err := procGetDefaultPrinter.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r1 == 0 {
		return "", err
	}
	return syscall.UTF16ToString(buf), nil
}

var (
	lastVirtualNotice string
	lastAutoSelect    string
	noticeMu          sync.Mutex
)

// GetTargetPhysicalPrinter detects the true physical printer connected to this machine.
func GetTargetPhysicalPrinter() (string, error) {
	// 1. Check current Windows default printer
	defPrinter, _ := GetDefaultWindowsPrinter()

	// 2. Query all printers
	printers, err := queryWindowsPrinters()
	if err != nil && defPrinter != "" && !IsVirtualPrinterName(defPrinter, "") {
		return defPrinter, nil
	}

	// If default printer is valid physical printer, verify port and return it
	if defPrinter != "" {
		isVirtual := false
		for _, p := range printers {
			if strings.EqualFold(p.Name, defPrinter) {
				if IsVirtualPrinterName(p.Name, p.PortName) {
					isVirtual = true
				}
				break
			}
		}
		if !isVirtual && !IsVirtualPrinterName(defPrinter, "") {
			return defPrinter, nil
		}
		noticeMu.Lock()
		if lastVirtualNotice != defPrinter {
			lastVirtualNotice = defPrinter
			log.Printf("[Printer] Notice: Default printer '%s' is virtual. Searching for physical printer...", defPrinter)
		}
		noticeMu.Unlock()
	}

	// Default was virtual or empty: find first physical printer
	for _, p := range printers {
		if !IsVirtualPrinterName(p.Name, p.PortName) {
			noticeMu.Lock()
			if lastAutoSelect != p.Name {
				lastAutoSelect = p.Name
				log.Printf("[Printer] Auto-selected physical printer '%s' (port: %s)", p.Name, p.PortName)
			}
			noticeMu.Unlock()
			return p.Name, nil
		}
	}

	return "", errors.New("no physical printer connected on this computer")
}

func hasPhysicalPrinter(printers []WindowsPrinter) bool {
	for _, p := range printers {
		if !IsVirtualPrinterName(p.Name, p.PortName) {
			return true
		}
	}
	return false
}

var (
	cachedPrinters   []WindowsPrinter
	lastPrinterQuery time.Time
	printerQueryMu   sync.Mutex
)

// hideCmd configures Windows subprocess execution to be completely invisible.
func hideCmd(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: 0x08000000, // CREATE_NO_WINDOW
		}
	}
}

func queryWindowsPrinters() ([]WindowsPrinter, error) {
	printerQueryMu.Lock()
	defer printerQueryMu.Unlock()

	// 30-second debounce cache prevents repeated registry/CIM queries on rapid health checks
	if len(cachedPrinters) > 0 && time.Since(lastPrinterQuery) < 30*time.Second {
		return cachedPrinters, nil
	}

	// 1. Direct Win32 API EnumPrinters query (< 1ms, 0 subprocesses)
	win32Printers, win32Err := enumPrintersWin32()
	if win32Err == nil && len(win32Printers) > 0 {
		cachedPrinters = win32Printers
		lastPrinterQuery = time.Now()
		return win32Printers, nil
	}

	// 2. Direct Registry query fallback
	printers, err := queryPrintersFromRegistry()
	if err == nil && len(printers) > 0 {
		cachedPrinters = printers
		lastPrinterQuery = time.Now()
		return printers, nil
	}

	// 3. CIM PowerShell query fallback (last resort)
	cimPrinters, cimErr := queryPrintersFromCim()
	if cimErr == nil && len(cimPrinters) > 0 {
		cachedPrinters = cimPrinters
		lastPrinterQuery = time.Now()
		return cimPrinters, nil
	}

	return printers, err
}

func findSumatraFromRegistry() string {
	keys := []struct {
		root registry.Key
		path string
		val  string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\SumatraPDF.exe`, ""},
		{registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\App Paths\SumatraPDF.exe`, ""},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\SumatraPDF.exe`, ""},
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\SumatraPDF`, "InstallLocation"},
		{registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Uninstall\SumatraPDF`, "InstallLocation"},
	}
	for _, k := range keys {
		if rk, err := registry.OpenKey(k.root, k.path, registry.QUERY_VALUE); err == nil {
			val, _, err := rk.GetStringValue(k.val)
			rk.Close()
			if err == nil && val != "" {
				clean := strings.Trim(strings.TrimSpace(val), `"`)
				if strings.HasSuffix(strings.ToLower(clean), ".exe") {
					if isValidSumatraExe(clean) {
						return clean
					}
				} else {
					candidate := filepath.Join(clean, "SumatraPDF.exe")
					if isValidSumatraExe(candidate) {
						return candidate
					}
				}
			}
		}
	}
	return ""
}

// getDesktopDirsForShortcuts returns all active user and public desktop paths on Windows.
func getDesktopDirsForShortcuts() []string {
	seen := make(map[string]bool)
	var dirs []string
	add := func(p string) {
		clean := filepath.Clean(strings.TrimSpace(p))
		if clean == "" {
			return
		}
		low := strings.ToLower(clean)
		if !seen[low] {
			seen[low] = true
			dirs = append(dirs, clean)
		}
	}

	if u := os.Getenv("USERPROFILE"); u != "" {
		add(filepath.Join(u, "Desktop"))
		add(filepath.Join(u, "OneDrive", "Desktop"))
	}
	add(`C:\Users\Public\Desktop`)

	if entries, err := os.ReadDir(`C:\Users`); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				name := strings.ToLower(e.Name())
				if name != "public" && name != "default" && name != "default user" && name != "all users" {
					add(filepath.Join(`C:\Users`, e.Name(), "Desktop"))
				}
			}
		}
	}
	return dirs
}

// CreateShortcut creates a Windows .lnk shortcut using PowerShell COM WScript.Shell.
func CreateShortcut(targetPath, lnkPath, description, arguments string) error {
	_ = os.MkdirAll(filepath.Dir(lnkPath), 0755)
	if fi, err := os.Stat(lnkPath); err == nil && fi.Size() > 0 {
		return nil
	}

	// First try PowerShell WScript.Shell COM object (guaranteed on Windows 10 & 11)
	psScript := fmt.Sprintf(`$ws = New-Object -ComObject WScript.Shell; $s = $ws.CreateShortcut('%s'); $s.TargetPath = '%s'; $s.Description = '%s'; $s.Arguments = '%s'; $s.Save()`,
		strings.ReplaceAll(lnkPath, `'`, `''`),
		strings.ReplaceAll(targetPath, `'`, `''`),
		strings.ReplaceAll(description, `'`, `''`),
		strings.ReplaceAll(arguments, `'`, `''`))

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	hideCmd(cmd)
	out, err := cmd.CombinedOutput()
	_ = out

	// Progressive wait for Windows Shell to flush shortcut to disk
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(lnkPath); err == nil && fi.Size() > 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	if fi, err := os.Stat(lnkPath); err == nil && fi.Size() > 0 {
		return nil
	}
	return fmt.Errorf("failed to create shortcut at %s: err=%v out=%s", lnkPath, err, string(out))
}

func queryPrintersFromRegistry() ([]WindowsPrinter, error) {
	defPrinter, _ := GetDefaultWindowsPrinter()
	seen := make(map[string]bool)
	var list []WindowsPrinter

	// 1. Check machine-wide printers in HKLM
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Print\Printers`, registry.READ); err == nil {
		if names, err := k.ReadSubKeyNames(-1); err == nil {
			for _, name := range names {
				low := strings.ToLower(strings.TrimSpace(name))
				if seen[low] {
					continue
				}
				seen[low] = true
				subK, err := registry.OpenKey(k, name, registry.READ)
				port := ""
				if err == nil {
					port, _, _ = subK.GetStringValue("Port")
					subK.Close()
				}
				list = append(list, WindowsPrinter{
					Name:     name,
					PortName: port,
					Default:  strings.EqualFold(name, defPrinter),
				})
			}
		}
		k.Close()
	}

	// 2. Check user-level network and redirected printers in HKCU Devices
	if kUser, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows NT\CurrentVersion\Devices`, registry.READ); err == nil {
		if valNames, err := kUser.ReadValueNames(-1); err == nil {
			for _, name := range valNames {
				low := strings.ToLower(strings.TrimSpace(name))
				if seen[low] {
					continue
				}
				seen[low] = true
				val, _, err := kUser.GetStringValue(name)
				port := ""
				if err == nil {
					parts := strings.Split(val, ",")
					if len(parts) >= 2 {
						port = strings.TrimSpace(parts[1])
					}
				}
				list = append(list, WindowsPrinter{
					Name:     name,
					PortName: port,
					Default:  strings.EqualFold(name, defPrinter),
				})
			}
		}
		kUser.Close()
	}

	if len(list) > 0 {
		return list, nil
	}
	return nil, errors.New("no printers found in registry")
}

func queryPrintersFromCim() ([]WindowsPrinter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", "@(Get-CimInstance Win32_Printer -ErrorAction SilentlyContinue | Select-Object Name, PortName, Default) | ConvertTo-Json -Compress")
	hideCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return nil, errors.New("no printers returned from CIM")
	}

	var raw []WindowsPrinter
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		var single WindowsPrinter
		if err2 := json.Unmarshal([]byte(trimmed), &single); err2 == nil && single.Name != "" {
			raw = append(raw, single)
		} else {
			return nil, err
		}
	}

	return raw, nil
}
