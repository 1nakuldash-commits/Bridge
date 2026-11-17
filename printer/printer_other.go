//go:build !windows

package printer

// GetDefaultWindowsPrinter stub for non-Windows platforms.
func GetDefaultWindowsPrinter() (string, error) {
	return "MockPrinter", nil
}

// GetTargetPhysicalPrinter stub for non-Windows platforms.
func GetTargetPhysicalPrinter() (string, error) {
	return "MockPrinter", nil
}
