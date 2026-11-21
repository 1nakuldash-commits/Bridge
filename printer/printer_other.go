//go:build !windows

package printer

import (
	"os"
	"os/exec"
	"path/filepath"
)

// GetDefaultWindowsPrinter stub for non-Windows platforms.
func GetDefaultWindowsPrinter() (string, error) {
	return "MockPrinter", nil
}

// GetTargetPhysicalPrinter stub for non-Windows platforms.
func GetTargetPhysicalPrinter() (string, error) {
	return "MockPrinter", nil
}

func hideCmd(cmd *exec.Cmd) {}

func findSumatraFromRegistry() string {
	return ""
}

// getDesktopDirsForShortcuts stub for non-Windows platforms.
func getDesktopDirsForShortcuts() []string {
	home, _ := os.UserHomeDir()
	if home != "" {
		return []string{filepath.Join(home, "Desktop")}
	}
	return []string{os.TempDir()}
}

// CreateShortcut creates a stub file for non-Windows platforms.
func CreateShortcut(targetPath, lnkPath, description, arguments string) error {
	_ = os.MkdirAll(filepath.Dir(lnkPath), 0755)
	return os.WriteFile(lnkPath, []byte(targetPath), 0644)
}

