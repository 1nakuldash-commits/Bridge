package tray

import (
	"sync"

	"bridge/config"
)

// TrayState represents the visual state of the taskbar status dot.
type TrayState int

const (
	StateConnected TrayState = iota // 🟢 Green Dot: Network online and ready
	StatePrinting                   // 🟡 Yellow Dot: Print transmission active
	StateSyncing                    // 🟡 Yellow Dot: File sync in progress
	StateCompleted                  // 🔵 Blue Dot: Operation completed
	StateOffline                    // ⚪ Gray Dot: All peers offline
)

// TrayCallbacks allows caller to attach actions to menu items.
type TrayCallbacks struct {
	OnOpenShared       func()
	OnPrintFile        func(filePath string)
	OnPrintFileToPeer  func(peerName string, filePath string)
	OnShowStatus       func() string
	OnResync           func()
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
