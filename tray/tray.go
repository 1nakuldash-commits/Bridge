package tray

import (
	"sync"

	"bridge/config"
)

// TrayState represents the visual state of the taskbar status dot.
type TrayState int

const (
	StateDisconnected   TrayState = iota // 🖨️❌ Printer with cross line (0 devices connected)
	StatePartial                         // 🖨️⚪ Gray printer (1 or more devices connected, cross removed)
	StateConnected                       // 🖨️🟢 Green printer (all devices connected)
	StatePrinting                        // 🖨️🟡 Printer with loading progress
	StatePrintCompleted                  // 🖨️🔵 Printer blue (print completed)
	StateSyncing                         // ☁️🟡 Cloud with loading progress (uploading / syncing)
	StateSyncCompleted                   // ☁️🔵 Cloud blue (upload completed)

	// Backward-compatible aliases
	StateOffline   = StateDisconnected
	StateCompleted = StatePrintCompleted
)

// TrayCallbacks allows caller to attach actions to menu items.
type TrayCallbacks struct {
	OnOpenShared       func()
	OnPrintFile        func(filePath string)
	OnPrintFileToPeer  func(peerName string, filePath string)
	OnShowStatus       func() string
	OnResync           func()
	OnCopyIP           func() string
	OnOpenLogs         func()
	OnExit             func()
}

// TrayManager manages the life-cycle of the multifunctional system tray icon.
type TrayManager struct {
	mu           sync.Mutex
	hwnd         uintptr
	icons        IconPaths
	hIcons       map[TrayState]uintptr
	currentState TrayState
	currentTip   string
	callbacks    TrayCallbacks
	nodeName     string
	peerName     string
	peers        []config.PeerDevice
	stopped      bool
}
