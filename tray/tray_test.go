package tray

import (
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
