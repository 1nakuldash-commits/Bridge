//go:build windows

package tray

import (
	"strings"
	"syscall"
	"testing"

	"bridge/config"
)

func TestCopyUTF16ZNULTermination(t *testing.T) {
	var title [64]uint16
	longTitle := strings.Repeat("A", 120)
	copyUTF16Z(title[:], longTitle)
	if title[len(title)-1] != 0 {
		t.Fatalf("expected SzInfoTitle last uint16 to be 0, got %d", title[len(title)-1])
	}
	if got := syscall.UTF16ToString(title[:]); len(got) != 63 {
		t.Fatalf("expected truncated 63-char title, got %d chars", len(got))
	}

	var tip [128]uint16
	// Place a 4-byte rune (surrogate pair in UTF-16) right across the 127th code unit boundary.
	surrogateBoundary := strings.Repeat("B", 126) + "🖨️"
	copyUTF16Z(tip[:], surrogateBoundary)
	if tip[len(tip)-1] != 0 {
		t.Fatalf("expected SzTip last uint16 to be 0, got %d", tip[len(tip)-1])
	}
	if tip[126] != 0 {
		t.Fatalf("expected unpaired leading surrogate at boundary to be cleared, got 0x%x", tip[126])
	}

	var info [256]uint16
	copyUTF16Z(info[:], "Hello\x00World"+strings.Repeat("!", 300))
	if info[len(info)-1] != 0 {
		t.Fatalf("expected SzInfo last uint16 to be 0, got %d", info[len(info)-1])
	}
}

func TestMenuPeersSnapshotIsolation(t *testing.T) {
	tm, err := NewTrayManager(t.TempDir(), "U1", "U2", TrayCallbacks{})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U2", IP: "100.64.0.2", Online: true},
		{Name: "U3", IP: "100.64.0.3", Online: true},
	})

	// Simulate showContextMenu snapshotting tm.menuPeers while menu is displayed.
	tm.mu.Lock()
	tm.menuPeers = make([]config.PeerDevice, len(tm.peers))
	copy(tm.menuPeers, tm.peers)
	tm.mu.Unlock()

	// Refresh tm.peers in a different order while the menu is open.
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U3", IP: "100.64.0.3", Online: true},
		{Name: "U2", IP: "100.64.0.2", Online: false},
	})

	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.menuPeers[0].Name != "U2" || tm.menuPeers[1].Name != "U3" {
		t.Fatalf("expected menuPeers snapshot to stay [U2 U3], got %+v", tm.menuPeers)
	}
}
