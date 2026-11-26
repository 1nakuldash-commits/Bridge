package tray

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bridge/config"
)

// TrayState represents the visual state of the taskbar status dot.
type TrayState int

const (
	StateDisconnected   TrayState = iota // Printer with cross line (0 devices connected)
	StatePartial                         // Gray printer (1 or more devices connected, cross removed)
	StateConnected                       // Green printer (all devices connected)
	StatePrinting                        // Printer with loading progress
	StatePrintCompleted                  // Printer blue (print completed)
	StateSyncing                         // Cloud with loading progress (uploading / syncing)
	StateSyncCompleted                   // Cloud blue (upload completed)

	// Backward-compatible aliases
	StateOffline   = StateDisconnected
	StateCompleted = StatePrintCompleted
)

// NotifyLevel specifies the severity icon for taskbar balloon notifications.
type NotifyLevel int

const (
	NotifyInfo NotifyLevel = iota
	NotifyWarning
	NotifyError
)

const (
	flashHoldDuration          = 4 * time.Second
	defaultSyncDebounceWindow  = 2 * time.Second
	defaultSyncMaxDebounceWait = 6 * time.Second
)

const (
	CmdHeader = 1000 + iota
	CmdQueuedPrints
	CmdOpenShared
	CmdOpenDashboard
	CmdPairDevice
	CmdPrintFileDefault
	CmdPrintTip
	CmdStatus
	CmdResync
	CmdCopyIP
	CmdOpenLogs
	CmdExit
	CmdPrintFilePeerBase     = 2000
	CmdConnectDiscoveredBase = 3000
)

// DiscoveredDevice represents an unlinked Bridge node discovered on the network.
type DiscoveredDevice struct {
	Name string
	IP   string
	URL  string
	Port int
}

// TrayCallbacks allows caller to attach actions to menu items.
type TrayCallbacks struct {
	OnOpenShared        func()
	OnOpenDashboard     func()
	OnPairDevice        func()
	OnConnectDiscovered func(device DiscoveredDevice)
	OnPrintFile         func(filePath string)
	OnPrintFileToPeer   func(peerName string, filePath string)
	OnQueueOfflinePrint func(peerName string, filePath string)
	OnShowStatus        func() string
	OnResync            func()
	OnCopyIP            func() string
	OnOpenLogs          func()
	OnExit              func()
}

type menuItemSpec struct {
	id        int
	text      string
	disabled  bool
	separator bool
}

// TrayManager manages the life-cycle of the multifunctional system tray icon.
type TrayManager struct {
	mu              sync.Mutex
	hwnd            uintptr
	icons           IconPaths
	hIcons          map[TrayState]uintptr
	currentState    TrayState
	currentTip      string
	callbacks       TrayCallbacks
	nodeName        string
	peerName        string
	peers           []config.PeerDevice
	menuPeers       []config.PeerDevice
	discoveredPeers []DiscoveredDevice
	menuDiscovered  []DiscoveredDevice
	queuedPrints    int
	isBusy          bool
	flashUntil      time.Time
	flashSeq        uint64
	stopped         bool

	// Sync debounce state
	syncDebounceWindow   time.Duration
	syncMaxDebounceWait  time.Duration
	syncDebounceTimer    *time.Timer
	syncDebounceToken    uint64
	syncDebounceCount    int
	syncDebounceLastItem string
	syncDebounceDeadline time.Time

	// Optional hooks for testing without Win32 UI
	onBalloon    func(title, message string, level NotifyLevel)
	alertFn      func(title, message string)
	confirmFn    func(title, message string) bool
	filePickerFn func(title string) (string, error)
}

// InferNotifyLevel infers notification severity from the notification title.
func InferNotifyLevel(title string) NotifyLevel {
	lower := strings.ToLower(strings.TrimSpace(title))
	switch {
	case strings.Contains(lower, "error"), strings.Contains(lower, "failed"):
		return NotifyError
	case strings.Contains(lower, "warning"),
		strings.Contains(lower, "offline"),
		strings.Contains(lower, "unavailable"),
		strings.Contains(lower, "no peer"):
		return NotifyWarning
	default:
		return NotifyInfo
	}
}

// FormatPeerMenuLabel returns a uniform menu item label for a peer device regardless of peer count.
func FormatPeerMenuLabel(p config.PeerDevice) string {
	statusTag := "Offline"
	if p.Online {
		if p.IsPrinterKnownMissing() {
			statusTag = "Online - No Printer"
		} else {
			statusTag = "Online"
		}
	}
	return fmt.Sprintf("Print on %s (%s)...", p.Name, statusTag)
}

// FormatStatusText normalizes tabs and wide column padding so status dialogs align cleanly in proportional fonts.
func FormatStatusText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "Bridge Multi-Device Mesh is active."
	}
	lines := strings.Split(strings.ReplaceAll(trimmed, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.ReplaceAll(line, "\t", "  ")
		// Preserve up to 2 spaces of leading indentation, collapse wide internal column padding.
		indent := ""
		body := line
		if strings.HasPrefix(line, "  ") {
			indent = "  "
			body = strings.TrimLeft(line, " ")
		} else {
			body = strings.TrimLeft(line, " ")
		}
		for strings.Contains(body, "   ") {
			body = strings.ReplaceAll(body, "   ", "  ")
		}
		body = strings.TrimRight(body, " ")
		if body == "" {
			out = append(out, "")
		} else {
			out = append(out, indent+body)
		}
	}
	return strings.Join(out, "\n")
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

// SetDiscoveredPeers updates pending unlinked Bridge devices shown near the top of the tray menu.
func (tm *TrayManager) SetDiscoveredPeers(devices []DiscoveredDevice) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.discoveredPeers = make([]DiscoveredDevice, len(devices))
	copy(tm.discoveredPeers, devices)
}

// GetDiscoveredPeers returns a copy of currently pending discovered devices.
func (tm *TrayManager) GetDiscoveredPeers() []DiscoveredDevice {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	res := make([]DiscoveredDevice, len(tm.discoveredPeers))
	copy(res, tm.discoveredPeers)
	return res
}

// SetQueuedPrintCount updates the offline queued print job count displayed in the tray menu.
func (tm *TrayManager) SetQueuedPrintCount(count int) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if count < 0 {
		count = 0
	}
	tm.queuedPrints = count
}

// GetQueuedPrintCount returns the cached offline queued print job count.
func (tm *TrayManager) GetQueuedPrintCount() int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.queuedPrints
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
	level := InferNotifyLevel(title)
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
	tm.deliverBalloonLocked(title, message, level)
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

// FlashSyncCompleteDebounced aggregates rapid sync completion notifications within a ~2-second window
// into a single summary notification (e.g. "Synced N files") instead of spamming multiple balloons.
func (tm *TrayManager) FlashSyncCompleteDebounced(countOrFile string) {
	tm.mu.Lock()
	if tm.stopped {
		tm.mu.Unlock()
		return
	}

	window := tm.syncDebounceWindow
	if window <= 0 {
		window = defaultSyncDebounceWindow
	}
	maxWait := tm.syncMaxDebounceWait
	if maxWait <= 0 {
		maxWait = defaultSyncMaxDebounceWait
	}

	now := time.Now()
	if tm.syncDebounceCount == 0 {
		tm.syncDebounceDeadline = now.Add(maxWait)
	}

	trimmed := strings.TrimSpace(countOrFile)
	if n, err := strconv.Atoi(trimmed); err == nil && n > 0 {
		tm.syncDebounceCount += n
	} else {
		tm.syncDebounceCount++
		if trimmed != "" {
			tm.syncDebounceLastItem = filepath.Base(trimmed)
		}
	}

	// Reflect sync completion icon immediately while aggregating the balloon notification.
	tm.isBusy = false
	tm.currentState = StateSyncCompleted
	if tm.syncDebounceCount == 1 && tm.syncDebounceLastItem != "" {
		tm.currentTip = fmt.Sprintf("Synced %s", tm.syncDebounceLastItem)
	} else {
		tm.currentTip = fmt.Sprintf("Synced %d files", tm.syncDebounceCount)
	}
	tm.modifyNotifyLocked()

	if tm.syncDebounceTimer != nil {
		tm.syncDebounceTimer.Stop()
	}
	tm.syncDebounceToken++
	token := tm.syncDebounceToken

	delay := window
	if remaining := time.Until(tm.syncDebounceDeadline); remaining < delay {
		delay = remaining
	}
	if delay < time.Millisecond {
		delay = time.Millisecond
	}

	tm.syncDebounceTimer = time.AfterFunc(delay, func() {
		tm.flushSyncDebounce(token)
	})
	tm.mu.Unlock()
}

func (tm *TrayManager) flushSyncDebounce(token uint64) {
	tm.mu.Lock()
	if tm.stopped || tm.syncDebounceToken != token || tm.syncDebounceCount == 0 {
		tm.mu.Unlock()
		return
	}
	count := tm.syncDebounceCount
	lastItem := tm.syncDebounceLastItem
	tm.syncDebounceCount = 0
	tm.syncDebounceLastItem = ""
	tm.syncDebounceTimer = nil
	tm.syncDebounceDeadline = time.Time{}
	tm.mu.Unlock()

	var msg string
	if count == 1 && lastItem != "" {
		msg = fmt.Sprintf("Synced %s", lastItem)
	} else if count == 1 {
		msg = "Synced 1 file"
	} else {
		msg = fmt.Sprintf("Synced %d files", count)
	}
	tm.FlashSyncComplete("Sync Complete", msg)
}

// FlashComplete backward-compatible alias for print completion.
func (tm *TrayManager) FlashComplete(title, message string) {
	tm.FlashPrintComplete(title, message)
}

// ShowNotification displays a native taskbar notification, automatically inferring severity from title.
func (tm *TrayManager) ShowNotification(title, message string) {
	tm.ShowNotificationLevel(title, message, InferNotifyLevel(title))
}

// ShowNotificationLevel displays a native taskbar notification with an explicit severity level.
func (tm *TrayManager) ShowNotificationLevel(title, message string, level NotifyLevel) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.deliverBalloonLocked(title, message, level)
}

func (tm *TrayManager) deliverBalloonLocked(title, message string, level NotifyLevel) {
	if tm.stopped {
		return
	}
	if tm.onBalloon != nil {
		tm.onBalloon(title, message, level)
	}
	tm.showBalloonLocked(title, message, level)
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

func (tm *TrayManager) lookupPeer(targetPeer string) (config.PeerDevice, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	clonePeer := func(p config.PeerDevice) config.PeerDevice {
		if p.PrinterReady != nil {
			ready := *p.PrinterReady
			p.PrinterReady = &ready
		}
		return p
	}
	for _, p := range tm.menuPeers {
		if strings.EqualFold(p.Name, targetPeer) {
			return clonePeer(p), true
		}
	}
	for _, p := range tm.peers {
		if strings.EqualFold(p.Name, targetPeer) {
			return clonePeer(p), true
		}
	}
	return config.PeerDevice{}, false
}

func (tm *TrayManager) isPeerOffline(targetPeer string) bool {
	p, ok := tm.lookupPeer(targetPeer)
	return ok && !p.Online
}

func (tm *TrayManager) buildDefaultStatusText() string {
	tm.mu.Lock()
	nodeName := tm.nodeName
	peers := make([]config.PeerDevice, len(tm.peers))
	copy(peers, tm.peers)
	queued := tm.queuedPrints
	discovered := len(tm.discoveredPeers)
	tm.mu.Unlock()

	onlineCount := 0
	for _, p := range peers {
		if p.Online {
			onlineCount++
		}
	}

	var b strings.Builder
	b.WriteString("Bridge Multi-Device Mesh Status\n\n")
	b.WriteString(fmt.Sprintf("This Device: %s\n", nodeName))
	if queued > 0 {
		b.WriteString(fmt.Sprintf("Queued Prints: %d (waiting for offline peer)\n", queued))
	}
	if discovered > 0 {
		b.WriteString(fmt.Sprintf("Discovered Unpaired Devices: %d\n", discovered))
	}
	b.WriteString(fmt.Sprintf("\nConnected Devices (%d/%d Online):\n", onlineCount, len(peers)))
	if len(peers) == 0 {
		b.WriteString("  (Waiting for other devices to connect)\n")
	} else {
		for _, p := range peers {
			state := "Offline"
			if p.Online {
				state = "Online"
			}
			b.WriteString(fmt.Sprintf("  - %s (%s) | IP: %s | Port: %d\n", p.Name, state, p.IP, p.SpoolPort))
		}
	}
	return b.String()
}

func (tm *TrayManager) snapshotAndBuildMenuItems() []menuItemSpec {
	tm.mu.Lock()
	nodeName := tm.nodeName
	tm.menuPeers = make([]config.PeerDevice, len(tm.peers))
	copy(tm.menuPeers, tm.peers)
	peers := make([]config.PeerDevice, len(tm.menuPeers))
	copy(peers, tm.menuPeers)

	tm.menuDiscovered = make([]DiscoveredDevice, len(tm.discoveredPeers))
	copy(tm.menuDiscovered, tm.discoveredPeers)
	discovered := make([]DiscoveredDevice, len(tm.menuDiscovered))
	copy(discovered, tm.menuDiscovered)

	fallbackPeer := tm.peerName
	queuedPrints := tm.queuedPrints
	tm.mu.Unlock()

	onlineCount := 0
	for _, p := range peers {
		if p.Online {
			onlineCount++
		}
	}

	var items []menuItemSpec

	// 1. Header with dynamic online count
	headerText := fmt.Sprintf("Bridge: %s (%d/%d Online)", nodeName, onlineCount, len(peers))
	if len(peers) == 0 {
		headerText = fmt.Sprintf("Bridge: %s (Standby)", nodeName)
	}
	items = append(items, menuItemSpec{id: CmdHeader, text: headerText, disabled: true})
	if queuedPrints > 0 {
		items = append(items, menuItemSpec{
			id:       CmdQueuedPrints,
			text:     fmt.Sprintf("Queued Prints (%d)", queuedPrints),
			disabled: true,
		})
	}
	items = append(items, menuItemSpec{separator: true})

	// 2. Pending discovered devices near the top
	if len(discovered) > 0 {
		for i, d := range discovered {
			if i >= 20 {
				break
			}
			label := strings.TrimSpace(d.Name)
			if label == "" {
				label = strings.TrimSpace(d.IP)
			}
			if label == "" {
				label = strings.TrimSpace(d.URL)
			}
			items = append(items, menuItemSpec{
				id:   CmdConnectDiscoveredBase + i,
				text: fmt.Sprintf("Connect New Device: %s...", label),
			})
		}
		items = append(items, menuItemSpec{separator: true})
	}

	// 3. Primary Navigation & Pairing
	items = append(items, menuItemSpec{id: CmdOpenShared, text: "Open Shared Folder"})
	items = append(items, menuItemSpec{id: CmdOpenDashboard, text: "Open Control Dashboard"})
	items = append(items, menuItemSpec{id: CmdPairDevice, text: "Pair New Device..."})
	items = append(items, menuItemSpec{separator: true})

	// 4. Print Actions: Uniform across 1 or N peers
	if len(peers) == 0 {
		target := fallbackPeer
		if target == "" {
			target = "Remote PC"
		}
		items = append(items, menuItemSpec{
			id:   CmdPrintFileDefault,
			text: fmt.Sprintf("Print on %s...", target),
		})
	} else {
		for i, p := range peers {
			if i >= 50 {
				break
			}
			items = append(items, menuItemSpec{
				id:   CmdPrintFilePeerBase + i,
				text: FormatPeerMenuLabel(p),
			})
		}
	}
	items = append(items, menuItemSpec{
		id:       CmdPrintTip,
		text:     `Tip: Use Ctrl+P -> "Print on <Device>" for Word/Web/Any App`,
		disabled: true,
	})
	items = append(items, menuItemSpec{separator: true})

	// 5. Quick Actions
	items = append(items, menuItemSpec{id: CmdCopyIP, text: "Copy My Tailscale IP"})
	items = append(items, menuItemSpec{id: CmdStatus, text: "Device Network Status"})
	items = append(items, menuItemSpec{id: CmdResync, text: "Sync Now"})
	items = append(items, menuItemSpec{id: CmdOpenLogs, text: "Open Service Logs"})
	items = append(items, menuItemSpec{separator: true})
	items = append(items, menuItemSpec{id: CmdExit, text: "Exit Bridge"})

	return items
}
