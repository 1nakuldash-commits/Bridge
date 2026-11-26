package tray

import (
	"strings"
	"sync"
	"testing"
	"time"

	"bridge/config"
)

func TestTrayManagerBusyAndFlashProtection(t *testing.T) {
	tm, err := NewTrayManager(t.TempDir(), "U1", "U2", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.64.0.2", Online: true},
	})

	tm.mu.Lock()
	if tm.currentState != StateConnected {
		t.Fatalf("expected StateConnected, got %v", tm.currentState)
	}
	tm.mu.Unlock()

	// Printing state sets isBusy=true and resists 3s UpdateNetworkStatus loop overwrites.
	tm.SetPrintingState("Bridge: Printing report.pdf...")
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.64.0.2", Online: true},
		{Name: "U3", IP: "100.64.0.3", Online: false},
	})

	tm.mu.Lock()
	if tm.currentState != StatePrinting {
		t.Fatalf("expected StatePrinting to be protected from UpdateNetworkStatus, got %v", tm.currentState)
	}
	if len(tm.peers) != 2 || tm.peerName != "U2" {
		t.Fatalf("expected UpdateNetworkStatus to still refresh peers/peerName while busy, got peers=%+v peerName=%q", tm.peers, tm.peerName)
	}
	tm.mu.Unlock()

	// FlashPrintComplete clears isBusy, holds StatePrintCompleted for 4s, resists UpdateNetworkStatus.
	tm.FlashPrintComplete("Print Complete", "Printed report.pdf")
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.64.0.2", Online: false},
		{Name: "U3", IP: "100.64.0.3", Online: false},
	})

	tm.mu.Lock()
	if tm.currentState != StatePrintCompleted {
		t.Fatalf("expected StatePrintCompleted to be protected during flash window, got %v", tm.currentState)
	}
	if tm.isBusy {
		t.Fatalf("expected isBusy=false after FlashPrintComplete")
	}
	if !time.Now().Before(tm.flashUntil) {
		t.Fatalf("expected flashUntil in the future")
	}
	tm.mu.Unlock()

	// RestoreNetworkStatus clears flash and immediately applies the latest refreshed peers (0/2 online -> StateDisconnected).
	tm.RestoreNetworkStatus()

	tm.mu.Lock()
	if tm.currentState != StateDisconnected {
		t.Fatalf("expected StateDisconnected after RestoreNetworkStatus, got %v", tm.currentState)
	}
	tm.mu.Unlock()
}

func TestTrayManagerOverlappingFlashSeqDoesNotRestoreEarly(t *testing.T) {
	tm, err := NewTrayManager(t.TempDir(), "U1", "U2", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.64.0.2", Online: true},
	})

	tm.FlashPrintComplete("Print 1", "Printed doc1")
	tm.mu.Lock()
	seq1 := tm.flashSeq
	tm.mu.Unlock()

	// New busy operation starts before flash 1 expires.
	tm.SetSyncingState("Bridge: Syncing doc2...")
	tm.restoreNetworkStatusIfSeq(seq1)

	tm.mu.Lock()
	if tm.currentState != StateSyncing {
		t.Fatalf("stale seq1 timer should not clear active StateSyncing, got %v", tm.currentState)
	}
	tm.mu.Unlock()

	// Flash 2 completes, superseding previous seq.
	tm.FlashSyncComplete("Sync Complete", "Uploaded doc2")
	tm.mu.Lock()
	seq2 := tm.flashSeq
	tm.mu.Unlock()

	tm.restoreNetworkStatusIfSeq(seq1)
	tm.mu.Lock()
	if tm.currentState != StateSyncCompleted {
		t.Fatalf("stale seq1 timer should not clear StateSyncCompleted, got %v", tm.currentState)
	}
	tm.mu.Unlock()

	// Matching seq2 timer restores baseline StateConnected.
	tm.restoreNetworkStatusIfSeq(seq2)
	tm.mu.Lock()
	if tm.currentState != StateConnected {
		t.Fatalf("matching seq2 restore should revert to StateConnected, got %v", tm.currentState)
	}
	tm.mu.Unlock()
}

func TestMenuItemsConsistencyAndActions(t *testing.T) {
	tm, err := NewTrayManager(t.TempDir(), "U1", "", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	hasText := func(items []menuItemSpec, want string) bool {
		for _, it := range items {
			if it.text == want {
				return true
			}
		}
		return false
	}

	// 1. Zero peers: Standby header, default Print on Remote PC, Dashboard, Pair, Tip.
	items := tm.snapshotAndBuildMenuItems()
	if !hasText(items, "Bridge: U1 (Standby)") {
		t.Fatalf("expected Standby header in menu: %+v", items)
	}
	if !hasText(items, "Print on Remote PC...") {
		t.Fatalf("expected fallback Print on Remote PC... item")
	}
	if !hasText(items, "Open Control Dashboard") || !hasText(items, "Pair New Device...") {
		t.Fatalf("expected Dashboard and Pair New Device items")
	}
	if !hasText(items, `Tip: Use Ctrl+P -> "Print on <Device>" for Word/Web/Any App`) {
		t.Fatalf("expected Ctrl+P tip item")
	}

	// 2. One peer vs two peers: uniform "(Online)" / "(Offline)" labels without emoji inconsistencies.
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "Test 4", IP: "100.64.0.4", Online: true},
	})
	onePeerItems := tm.snapshotAndBuildMenuItems()
	if !hasText(onePeerItems, "Print on Test 4 (Online)...") {
		t.Fatalf("expected uniform 1-peer online label, got %+v", onePeerItems)
	}

	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "Test 4", IP: "100.64.0.4", Online: true},
		{Name: "Device (4)", IP: "100.64.0.5", Online: false},
	})
	multiPeerItems := tm.snapshotAndBuildMenuItems()
	if !hasText(multiPeerItems, "Print on Test 4 (Online)...") {
		t.Fatalf("expected uniform multi-peer online label, got %+v", multiPeerItems)
	}
	if !hasText(multiPeerItems, "Print on Device (4) (Offline)...") {
		t.Fatalf("expected uniform multi-peer offline label, got %+v", multiPeerItems)
	}

	// 3. Discovered peers & queued prints appear near top.
	tm.SetQueuedPrintCount(3)
	tm.SetDiscoveredPeers([]DiscoveredDevice{
		{Name: "Office-PC", IP: "100.64.0.9", URL: "http://100.64.0.9:8765", Port: 8765},
	})
	richItems := tm.snapshotAndBuildMenuItems()
	if !hasText(richItems, "Queued Prints (3)") {
		t.Fatalf("expected Queued Prints (3) item, got %+v", richItems)
	}
	discoveredIdx := -1
	sharedIdx := -1
	for i, it := range richItems {
		if it.text == "Connect New Device: Office-PC..." {
			discoveredIdx = i
		}
		if it.id == CmdOpenShared {
			sharedIdx = i
		}
	}
	if discoveredIdx == -1 || sharedIdx == -1 || discoveredIdx >= sharedIdx {
		t.Fatalf("expected Connect New Device near top before Open Shared Folder, got discoveredIdx=%d sharedIdx=%d", discoveredIdx, sharedIdx)
	}

	tm.SetQueuedPrintCount(0)
	if hasText(tm.snapshotAndBuildMenuItems(), "Queued Prints (0)") {
		t.Fatalf("queued prints count 0 should not be shown")
	}
}

func TestNotificationSeverityInferenceAndExplicitLevel(t *testing.T) {
	tm, err := NewTrayManager(t.TempDir(), "U1", "U2", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	type event struct {
		title string
		msg   string
		level NotifyLevel
	}
	var mu sync.Mutex
	var events []event
	tm.onBalloon = func(title, message string, level NotifyLevel) {
		mu.Lock()
		events = append(events, event{title, message, level})
		mu.Unlock()
	}

	tm.ShowNotification("Print Error", "Failed to reach printer")
	tm.ShowNotification("Print Failed", "Corrupt PDF")
	tm.ShowNotification("Peer Offline", "Device 2 is offline")
	tm.ShowNotification("Warning: Spooler", "Spooler slow")
	tm.ShowNotification("Print Complete", "Printed doc.pdf")
	tm.ShowNotificationLevel("Print Complete", "Forced warning", NotifyWarning)
	tm.ShowNotificationLevel("Status", "Forced error", NotifyError)

	want := []NotifyLevel{
		NotifyError,
		NotifyError,
		NotifyWarning,
		NotifyWarning,
		NotifyInfo,
		NotifyWarning,
		NotifyError,
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != len(want) {
		t.Fatalf("expected %d events, got %d", len(want), len(events))
	}
	for i, w := range want {
		if events[i].level != w {
			t.Fatalf("event %d (%q): expected level %v, got %v", i, events[i].title, w, events[i].level)
		}
	}
}

func TestFlashSyncCompleteDebounced(t *testing.T) {
	tm, err := NewTrayManager(t.TempDir(), "U1", "U2", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	tm.mu.Lock()
	tm.syncDebounceWindow = 40 * time.Millisecond
	tm.syncMaxDebounceWait = 150 * time.Millisecond
	tm.mu.Unlock()

	type balloon struct {
		title string
		msg   string
		level NotifyLevel
	}
	ch := make(chan balloon, 10)
	tm.onBalloon = func(title, message string, level NotifyLevel) {
		ch <- balloon{title, message, level}
	}

	// Burst of 20 files should aggregate into a single "Synced 20 files" notification.
	for i := 1; i <= 20; i++ {
		tm.FlashSyncCompleteDebounced("docs/file.txt")
	}

	select {
	case b := <-ch:
		if b.msg != "Synced 20 files" {
			t.Fatalf("expected 'Synced 20 files', got %q", b.msg)
		}
		if b.level != NotifyInfo {
			t.Fatalf("expected NotifyInfo, got %v", b.level)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for debounced sync notification")
	}

	// Ensure no extra balloons were emitted.
	time.Sleep(80 * time.Millisecond)
	if len(ch) != 0 {
		t.Fatalf("expected exactly 1 aggregated balloon, got %d extra", len(ch))
	}

	// Single file shows the basename.
	tm.FlashSyncCompleteDebounced(`C:\Bridge\Shared\budget.pdf`)
	select {
	case b := <-ch:
		if b.msg != "Synced budget.pdf" {
			t.Fatalf("expected 'Synced budget.pdf', got %q", b.msg)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for single debounced sync notification")
	}

	// Numeric count input.
	tm.FlashSyncCompleteDebounced("5")
	select {
	case b := <-ch:
		if b.msg != "Synced 5 files" {
			t.Fatalf("expected 'Synced 5 files', got %q", b.msg)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for numeric debounced sync notification")
	}
}

func TestFormatStatusTextAndDefaultStatus(t *testing.T) {
	raw := "Bridge Status\nThis Device:\tU1\n  ● Test4       IP: 100.64.0.2      Port: 9101   Online\n"
	formatted := FormatStatusText(raw)
	if strings.Contains(formatted, "\t") {
		t.Fatalf("expected tabs to be removed, got %q", formatted)
	}
	if strings.Contains(formatted, "   ") {
		t.Fatalf("expected wide spacing runs to be normalized, got %q", formatted)
	}

	tm, err := NewTrayManager(t.TempDir(), "U1", "", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.64.0.2", SpoolPort: 9101, Online: true},
	})
	tm.SetQueuedPrintCount(2)
	def := tm.buildDefaultStatusText()
	if !strings.Contains(def, "Queued Prints: 2") || !strings.Contains(def, "U2 (Online) | IP: 100.64.0.2 | Port: 9101") {
		t.Fatalf("unexpected default status text:\n%s", def)
	}
}

func TestFormatPeerMenuLabelPrinterStates(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	cases := []struct {
		name string
		peer config.PeerDevice
		want string
	}{
		{
			name: "offline with unknown printer",
			peer: config.PeerDevice{Name: "Desk-A", Online: false, PrinterReady: nil},
			want: "Print on Desk-A (Offline)...",
		},
		{
			name: "offline with missing printer still shows Offline",
			peer: config.PeerDevice{Name: "Desk-B", Online: false, PrinterReady: boolPtr(false)},
			want: "Print on Desk-B (Offline)...",
		},
		{
			name: "offline with ready printer still shows Offline",
			peer: config.PeerDevice{Name: "Desk-C", Online: false, PrinterReady: boolPtr(true)},
			want: "Print on Desk-C (Offline)...",
		},
		{
			name: "online with unknown printer",
			peer: config.PeerDevice{Name: "Laptop-1", Online: true, PrinterReady: nil},
			want: "Print on Laptop-1 (Online)...",
		},
		{
			name: "online with ready printer",
			peer: config.PeerDevice{Name: "Laptop-2", Online: true, PrinterReady: boolPtr(true)},
			want: "Print on Laptop-2 (Online)...",
		},
		{
			name: "online with missing printer",
			peer: config.PeerDevice{Name: "Laptop-3", Online: true, PrinterReady: boolPtr(false)},
			want: "Print on Laptop-3 (Online - No Printer)...",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatPeerMenuLabel(tc.peer); got != tc.want {
				t.Fatalf("FormatPeerMenuLabel(%+v) = %q, want %q", tc.peer, got, tc.want)
			}
		})
	}

	tm, err := NewTrayManager(t.TempDir(), "U1", "", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "NoPrinterPC", IP: "100.64.0.8", Online: true, PrinterReady: boolPtr(false)},
	})
	items := tm.snapshotAndBuildMenuItems()
	found := false
	for _, it := range items {
		if it.text == "Print on NoPrinterPC (Online - No Printer)..." {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected menu items to include No Printer label, got %+v", items)
	}
}
