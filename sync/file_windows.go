//go:build windows

package sync

import (
	"os"

	"golang.org/x/sys/windows"
)

// OpenSharedFileForRead opens a file for reading with full FILE_SHARE_READ,
// FILE_SHARE_WRITE, and FILE_SHARE_DELETE sharing flags.
// This ensures that Bridge never blocks Windows Explorer or other processes
// from deleting or renaming files while Bridge is hashing, uploading, or streaming them.
func OpenSharedFileForRead(path string) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	h, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(h), path), nil
}
