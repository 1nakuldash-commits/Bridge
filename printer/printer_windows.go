//go:build windows

package printer

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

var (
	winspool              = syscall.NewLazyDLL("winspool.drv")
	procGetDefaultPrinter = winspool.NewProc("GetDefaultPrinterW")
)

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
		log.Printf("[Printer] Notice: Default printer '%s' is virtual. Searching for physical printer...", defPrinter)
	}

	// Default was virtual or empty: find first physical printer
	for _, p := range printers {
		if !IsVirtualPrinterName(p.Name, p.PortName) {
			log.Printf("[Printer] Auto-selected physical printer '%s' (port: %s)", p.Name, p.PortName)
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

func queryWindowsPrinters() ([]WindowsPrinter, error) {
	printers, err := queryPrintersFromRegistry()
	if err == nil && hasPhysicalPrinter(printers) {
		return printers, nil
	}

	cimPrinters, cimErr := queryPrintersFromCim()
	if cimErr == nil && len(cimPrinters) > 0 {
		return cimPrinters, nil
	}

	return printers, err
}

func queryPrintersFromRegistry() ([]WindowsPrinter, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Print\Printers`, registry.READ)
	if err != nil {
		return nil, err
	}
	defer k.Close()

	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}

	defPrinter, _ := GetDefaultWindowsPrinter()
	var list []WindowsPrinter
	for _, name := range names {
		subK, err := registry.OpenKey(k, name, registry.READ)
		if err != nil {
			continue
		}
		port, _, _ := subK.GetStringValue("Port")
		subK.Close()

		list = append(list, WindowsPrinter{
			Name:     name,
			PortName: port,
			Default:  strings.EqualFold(name, defPrinter),
		})
	}
	return list, nil
}

func queryPrintersFromCim() ([]WindowsPrinter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", "@(Get-CimInstance Win32_Printer -ErrorAction SilentlyContinue | Select-Object Name, PortName, Default) | ConvertTo-Json -Compress")
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
