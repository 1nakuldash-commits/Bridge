//go:build !windows

package installer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"bridge/config"
)

// IsElevated stub.
func IsElevated() bool {
	return true
}

func hideCmd(cmd *exec.Cmd) {}

func formatElevatedArgs(args []string) string {
	return strings.Join(args, " ")
}

// SelfElevate stub.
func SelfElevate(args []string) error {
	return nil
}

// OpenBrowser stub.
func OpenBrowser(targetURL string) error {
	return exec.Command("open", targetURL).Start()
}

// FindTailscale stub.
func FindTailscale() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	candidates := []string{
		"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		"/usr/local/bin/tailscale",
		"/opt/homebrew/bin/tailscale",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// InstallTailscale stub.
func InstallTailscale() (string, error) {
	return "", errors.New("automated winget install only supported on Windows")
}

// GetTailscaleIP stub.
func GetTailscaleIP() string {
	if ip := FindLocalTailscaleIP(); ip != "" {
		return ip
	}
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
		return ip
	}
	return ""
}

// FindSumatraPDF stub.
func FindSumatraPDF() string {
	return ""
}

// InstallSumatraPDF stub.
func InstallSumatraPDF() (string, error) {
	return "", nil
}

// GetPrimaryDesktopDir returns the primary desktop directory for non-Windows platforms.
func GetPrimaryDesktopDir() string {
	dirs := GetAllDesktopDirs()
	if len(dirs) > 0 && dirs[0] != "" {
		return dirs[0]
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		return filepath.Join(home, "Desktop")
	}
	return os.TempDir()
}

// GetAllDesktopDirs stub.
func GetAllDesktopDirs() []string {
	home, _ := os.UserHomeDir()
	if home != "" {
		return []string{filepath.Join(home, "Desktop")}
	}
	return []string{os.TempDir()}
}

// CreateDesktopShortcut stub.
func CreateDesktopShortcut(targetPath, lnkPath, description, arguments string) error {
	return os.WriteFile(lnkPath, []byte(targetPath), 0644)
}

// InstallVirtualPrinter stub.
func InstallVirtualPrinter(peerName string, port int) error {
	return nil
}

// IsVirtualPrinterRegistered stub.
func IsVirtualPrinterRegistered(peerName string) bool {
	return false
}

// CheckVirtualPrinters stub.
func CheckVirtualPrinters(peers []config.PeerDevice) map[string]bool {
	return make(map[string]bool)
}

// RemoveVirtualPrinter stub.
func RemoveVirtualPrinter(peerName string) error {
	return nil
}

// PruneObsoleteVirtualPrinters stub.
func PruneObsoleteVirtualPrinters(activePeers []config.PeerDevice) error {
	return nil
}

// UninstallAllVirtualPrinters stub.
func UninstallAllVirtualPrinters() error {
	return nil
}

// KillOtherBridgeProcesses stub.
func KillOtherBridgeProcesses() {}

func startBridgeBackgroundService() error {
	cmd := exec.Command(TargetExe, "-autostart")
	return cmd.Start()
}

func endAndDeleteTask(taskName string) {}

func scheduleSelfDeleteAppDir(appDir string, pid int) {}

// RegisterInteractiveStartup stub.
func RegisterInteractiveStartup() error {
	return nil
}

// UnregisterInteractiveStartup stub.
func UnregisterInteractiveStartup() {}

var (
	mockInteractiveHeld bool
	mockServiceHeld     bool
	mockMu              sync.Mutex
)

// AcquireInteractiveLock stub with simulated state tracking for testing.
func AcquireInteractiveLock() bool {
	mockMu.Lock()
	defer mockMu.Unlock()
	if mockInteractiveHeld {
		BringDashboardToFront()
		return false
	}
	mockInteractiveHeld = true
	return true
}

// ReleaseInteractiveLock stub.
func ReleaseInteractiveLock() {
	mockMu.Lock()
	defer mockMu.Unlock()
	mockInteractiveHeld = false
}

// BringDashboardToFront stub.
func BringDashboardToFront() {}

// SetDashboardConsoleTitle stub.
func SetDashboardConsoleTitle(title string) {}

// AcquireServiceLock stub with simulated state tracking for testing.
func AcquireServiceLock() bool {
	mockMu.Lock()
	defer mockMu.Unlock()
	if mockServiceHeld {
		return false
	}
	mockServiceHeld = true
	return true
}

// ReleaseServiceLock stub.
func ReleaseServiceLock() {
	mockMu.Lock()
	defer mockMu.Unlock()
	mockServiceHeld = false
}
