//go:build !windows

package installer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsElevated stub.
func IsElevated() bool {
	return true
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

// UninstallAllVirtualPrinters stub.
func UninstallAllVirtualPrinters() error {
	return nil
}

// KillOtherBridgeProcesses stub.
func KillOtherBridgeProcesses() {}

// RegisterInteractiveStartup stub.
func RegisterInteractiveStartup() error {
	return nil
}

// UnregisterInteractiveStartup stub.
func UnregisterInteractiveStartup() {}
