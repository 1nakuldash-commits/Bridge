//go:build !windows

package main

// EnsureConsole is a no-op on non-Windows platforms where standard streams are already attached.
func EnsureConsole() {}
