//go:build windows

package main

import (
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

const attachParentProcess = ^uint32(0) // ATTACH_PARENT_PROCESS ((DWORD)-1)

var (
	consoleKernel32        = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole      = consoleKernel32.NewProc("AttachConsole")
	procAllocConsole       = consoleKernel32.NewProc("AllocConsole")
	procGetConsoleWindow   = consoleKernel32.NewProc("GetConsoleWindow")
	procSetConsoleOutputCP = consoleKernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = consoleKernel32.NewProc("SetConsoleCP")

	ensureConsoleOnce sync.Once
)

func isValidStdHandle(stdID uint32) bool {
	h, err := windows.GetStdHandle(stdID)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return false
	}
	ft, err := windows.GetFileType(h)
	if err != nil || ft == windows.FILE_TYPE_UNKNOWN {
		return false
	}
	return true
}

func isRedirectedStdHandle(stdID uint32) bool {
	h, err := windows.GetStdHandle(stdID)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return false
	}
	ft, err := windows.GetFileType(h)
	return err == nil && (ft == windows.FILE_TYPE_PIPE || ft == windows.FILE_TYPE_DISK)
}

func openConsoleHandle(name string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		p,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
}

func enableVirtualTerminal(h windows.Handle) {
	if h == 0 || h == windows.InvalidHandle {
		return
	}
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err == nil {
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}

// EnsureConsole attaches to the parent console or allocates a new console for interactive/CLI
// execution when compiled as a Windows GUI-subsystem binary (-H windowsgui).
// It configures UTF-8 code pages (65001), enables virtual terminal processing, and rebinds
// os.Stdin, os.Stdout, and os.Stderr to CONIN$/CONOUT$ when needed without clobbering redirected pipes.
func EnsureConsole() {
	ensureConsoleOnce.Do(func() {
		stdinValid := isValidStdHandle(windows.STD_INPUT_HANDLE)
		stdoutValid := isValidStdHandle(windows.STD_OUTPUT_HANDLE)
		stderrValid := isValidStdHandle(windows.STD_ERROR_HANDLE)
		stdoutRedirected := isRedirectedStdHandle(windows.STD_OUTPUT_HANDLE)
		stderrRedirected := isRedirectedStdHandle(windows.STD_ERROR_HANDLE)

		attached := false
		allocated := false

		hwnd, _, _ := procGetConsoleWindow.Call()
		if hwnd == 0 {
			r, _, _ := procAttachConsole.Call(uintptr(attachParentProcess))
			if r != 0 {
				attached = true
			} else if !(stdoutRedirected && stderrRedirected) {
				rAlloc, _, _ := procAllocConsole.Call()
				if rAlloc != 0 {
					allocated = true
				}
			}
		} else {
			attached = true
		}

		if !attached && !allocated {
			return
		}

		_, _, _ = procSetConsoleOutputCP.Call(65001)
		_, _, _ = procSetConsoleCP.Call(65001)

		if (allocated || !stdinValid) && !isRedirectedStdHandle(windows.STD_INPUT_HANDLE) {
			if hIn, err := openConsoleHandle("CONIN$", windows.GENERIC_READ|windows.GENERIC_WRITE); err == nil && hIn != windows.InvalidHandle {
				_ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, hIn)
				os.Stdin = os.NewFile(uintptr(hIn), "CONIN$")
			}
		}

		if (allocated || !stdoutValid) && !stdoutRedirected {
			if hOut, err := openConsoleHandle("CONOUT$", windows.GENERIC_READ|windows.GENERIC_WRITE); err == nil && hOut != windows.InvalidHandle {
				_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, hOut)
				os.Stdout = os.NewFile(uintptr(hOut), "CONOUT$")
				enableVirtualTerminal(hOut)
			}
		} else if hOut, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil {
			enableVirtualTerminal(hOut)
		}

		if (allocated || !stderrValid) && !stderrRedirected {
			if hErr, err := openConsoleHandle("CONOUT$", windows.GENERIC_READ|windows.GENERIC_WRITE); err == nil && hErr != windows.InvalidHandle {
				_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, hErr)
				os.Stderr = os.NewFile(uintptr(hErr), "CONOUT$")
				enableVirtualTerminal(hErr)
			}
		} else if hErr, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE); err == nil {
			enableVirtualTerminal(hErr)
		}
	})
}
