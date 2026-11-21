//go:build !windows

package tray

import (
	"bridge/config"
)

// NewTrayManager non-Windows stub.
func NewTrayManager(iconDir, nodeName, peerName string, cb TrayCallbacks) (*TrayManager, error) {
	return &TrayManager{
		currentState: StateOffline,
		currentTip:   "Bridge (Headless/Non-Windows)",
		callbacks:    cb,
		nodeName:     nodeName,
		peerName:     peerName,
		peers:        make([]config.PeerDevice, 0),
	}, nil
}

// Start stub.
func (tm *TrayManager) Start() error {
	return nil
}

// Stop stub.
func (tm *TrayManager) Stop() {}

// SetState stub.
func (tm *TrayManager) SetState(state TrayState, tooltip string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.currentState = state
	tm.currentTip = tooltip
}

// ShowBalloon stub.
func (tm *TrayManager) ShowBalloon(title, message string) {}

// UpdateNames stub.
func (tm *TrayManager) UpdateNames(nodeName, peerName string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.nodeName = nodeName
	tm.peerName = peerName
}

// UpdateNetworkStatus stub.
func (tm *TrayManager) UpdateNetworkStatus(nodeName string, peers []config.PeerDevice) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.nodeName = nodeName
	tm.peers = peers
}

// FlashComplete stub.
func (tm *TrayManager) FlashComplete(title, message string) {}

// FlashPrintComplete stub.
func (tm *TrayManager) FlashPrintComplete(title, message string) {}

// FlashSyncComplete stub.
func (tm *TrayManager) FlashSyncComplete(title, message string) {}

// SetPrintingState stub.
func (tm *TrayManager) SetPrintingState(tooltip string) {}

// SetSyncingState stub.
func (tm *TrayManager) SetSyncingState(tooltip string) {}

// RestoreNetworkStatus stub.
func (tm *TrayManager) RestoreNetworkStatus() {}

// OpenFilePicker stub.
func OpenFilePicker(title string) (string, error) {
	return "", nil
}

// HideConsole stub.
func HideConsole() {}

// HideConsoleIfDedicated stub.
func HideConsoleIfDedicated() {}
