//go:build !windows

package printer

import "os/exec"

// GetDefaultWindowsPrinter stub for non-Windows platforms.
func GetDefaultWindowsPrinter() (string, error) {
	return "MockPrinter", nil
}

// GetTargetPhysicalPrinter stub for non-Windows platforms.
func GetTargetPhysicalPrinter() (string, error) {
	return "MockPrinter", nil
}

func hideCmd(cmd *exec.Cmd) {}

