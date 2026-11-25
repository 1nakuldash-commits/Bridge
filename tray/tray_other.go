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

// Stop marks the tray manager stopped.
func (tm *TrayManager) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.stopped = true
}

// ShowBalloon stub.
func (tm *TrayManager) ShowBalloon(title, message string) {}

func (tm *TrayManager) modifyNotifyLocked() {}

func (tm *TrayManager) showBalloonLocked(title, message string) {}

// OpenFilePicker stub.
func OpenFilePicker(title string) (string, error) {
	return "", nil
}

// HideConsole stub.
func HideConsole() {}

// HideConsoleIfDedicated stub.
func HideConsoleIfDedicated() {}
