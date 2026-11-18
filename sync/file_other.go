//go:build !windows

package sync

import (
	"os"
)

// OpenSharedFileForRead opens a file for reading (Unix/macOS fallback).
func OpenSharedFileForRead(path string) (*os.File, error) {
	return os.Open(path)
}
