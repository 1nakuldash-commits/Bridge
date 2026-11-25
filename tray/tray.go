package tray

import (
	"fmt"
	"sync"
	"time"

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

const flashHoldDuration = 4 * time.Second

// TrayCallbacks allows caller to attach actions to menu items.
type TrayCallbacks struct {
	OnOpenShared      func()
	OnPrintFile       func(filePath string)
	OnPrintFileToPeer func(peerName string, filePath string)
	OnShowStatus      func() string
	OnResync          func()
	OnCopyIP          func() string
	OnOpenLogs        func()
	OnExit            func()
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
	menuPeers    []config.PeerDevice
	isBusy       bool
	flashUntil   time.Time
	flashSeq     uint64
	stopped      bool
}

func computeBaselineStatus(nodeName string, peers []config.PeerDevice) (TrayState, string) {
	onlineCount := 0
	for _, p := range peers {
		if p.Online {
			onlineCount++
		}
	}

	total := len(peers)
	if total == 0 || onlineCount == 0 {
		if total == 0 {
			return StateDisconnected, fmt.Sprintf("Bridge: %s (No other device connected)", nodeName)
		}
		return StateDisconnected, fmt.Sprintf("Bridge: %s (0/%d devices connected - Disconnected)", nodeName, total)
	}
	if onlineCount < total {
		return StatePartial, fmt.Sprintf("Bridge: %s (%d/%d devices connected)", nodeName, onlineCount, total)
	}
	return StateConnected, fmt.Sprintf("Bridge: %s (All %d devices connected & ready)", nodeName, total)
}

// SetState updates the dot color and tooltip.
func (tm *TrayManager) SetState(state TrayState, tooltip string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped {
		return
	}
	tm.currentState = state
	tm.currentTip = tooltip
	tm.modifyNotifyLocked()
}

// UpdateNames updates the friendly names shown in menu items and tooltips (backward compatibility).
func (tm *TrayManager) UpdateNames(nodeName, peerName string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.nodeName = nodeName
	tm.peerName = peerName
}

// UpdateNetworkStatus updates multi-device status, tooltip, and dot color in real-time.
// It always refreshes cached node/peer metadata, while preserving active busy or completion flash states.
func (tm *TrayManager) UpdateNetworkStatus(nodeName string, peers []config.PeerDevice) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.nodeName = nodeName
	tm.peers = make([]config.PeerDevice, len(peers))
	copy(tm.peers, peers)
	if len(peers) > 0 && peers[0].Name != "" {
		tm.peerName = peers[0].Name
	} else if len(peers) == 0 {
		tm.peerName = ""
	}

	if tm.isBusy || time.Now().Before(tm.flashUntil) {
		return
	}

	tm.applyBaselineLocked()
}

func (tm *TrayManager) applyBaselineLocked() {
	state, tip := computeBaselineStatus(tm.nodeName, tm.peers)
	if tm.currentState != state || tm.currentTip != tip {
		tm.currentState = state
		tm.currentTip = tip
		tm.modifyNotifyLocked()
	}
}

func (tm *TrayManager) setBusyState(state TrayState, tooltip string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped {
		return
	}
	tm.isBusy = true
	tm.flashUntil = time.Time{}
	tm.flashSeq++
	tm.currentState = state
	tm.currentTip = tooltip
	tm.modifyNotifyLocked()
}

// SetPrintingState displays the loading printer icon during print spooling/transmission.
func (tm *TrayManager) SetPrintingState(tooltip string) {
	tm.setBusyState(StatePrinting, tooltip)
}

// SetSyncingState displays the loading cloud icon during file upload/synchronization.
func (tm *TrayManager) SetSyncingState(tooltip string) {
	tm.setBusyState(StateSyncing, tooltip)
}

func (tm *TrayManager) flashCompleteState(state TrayState, title, message string) {
	tm.mu.Lock()
	if tm.stopped {
		tm.mu.Unlock()
		return
	}
	tm.isBusy = false
	tm.flashSeq++
	seq := tm.flashSeq
	tm.flashUntil = time.Now().Add(flashHoldDuration)
	tm.currentState = state
	tm.currentTip = message
	tm.showBalloonLocked(title, message)
	tm.mu.Unlock()

	go func(expectedSeq uint64) {
		time.Sleep(flashHoldDuration)
		tm.restoreNetworkStatusIfSeq(expectedSeq)
	}(seq)
}

// FlashPrintComplete displays the blue printer icon for 4 seconds, then reverts to baseline network status.
func (tm *TrayManager) FlashPrintComplete(title, message string) {
	tm.flashCompleteState(StatePrintCompleted, title, message)
}

// FlashSyncComplete displays the blue cloud icon for 4 seconds, then reverts to baseline network status.
func (tm *TrayManager) FlashSyncComplete(title, message string) {
	tm.flashCompleteState(StateSyncCompleted, title, message)
}

// FlashComplete backward-compatible alias for print completion.
func (tm *TrayManager) FlashComplete(title, message string) {
	tm.FlashPrintComplete(title, message)
}

// RestoreNetworkStatus clears any active busy/flash state and restores the baseline printer icon immediately.
func (tm *TrayManager) RestoreNetworkStatus() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped {
		return
	}
	tm.isBusy = false
	tm.flashUntil = time.Time{}
	tm.flashSeq++
	tm.applyBaselineLocked()
}

func (tm *TrayManager) restoreNetworkStatusIfSeq(expectedSeq uint64) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped || tm.flashSeq != expectedSeq || tm.isBusy {
		return
	}
	tm.flashUntil = time.Time{}
	tm.applyBaselineLocked()
}
