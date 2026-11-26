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

// Stop marks the tray manager stopped and cancels pending debounce timers.
func (tm *TrayManager) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.stopped = true
	if tm.syncDebounceTimer != nil {
		tm.syncDebounceTimer.Stop()
		tm.syncDebounceTimer = nil
	}
	tm.syncDebounceCount = 0
	tm.syncDebounceToken++
}

// ShowBalloon displays a notification (stub on non-Windows, invokes test hook if present).
func (tm *TrayManager) ShowBalloon(title, message string) {
	tm.ShowNotification(title, message)
}

func (tm *TrayManager) modifyNotifyLocked() {}

func (tm *TrayManager) showBalloonLocked(title, message string, level NotifyLevel) {}

// OpenFilePicker stub.
func OpenFilePicker(title string) (string, error) {
	return "", nil
}

// HideConsole stub.
func HideConsole() {}

// HideConsoleIfDedicated stub.
func HideConsoleIfDedicated() {}

// SetClipboardText stub.
func SetClipboardText(text string) error {
	return nil
}
