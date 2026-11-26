//go:build windows

package tray

import (
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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
	tm.SetDiscoveredPeers([]DiscoveredDevice{
		{Name: "NewA", IP: "100.64.0.10", URL: "http://100.64.0.10:8765"},
		{Name: "NewB", IP: "100.64.0.11", URL: "http://100.64.0.11:8765"},
	})

	// Snapshot both menuPeers and menuDiscovered as showContextMenu does.
	_ = tm.snapshotAndBuildMenuItems()

	// Refresh peers and discovered peers in a different order while the menu is open.
	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "U3", IP: "100.64.0.3", Online: true},
		{Name: "U2", IP: "100.64.0.2", Online: false},
	})
	tm.SetDiscoveredPeers([]DiscoveredDevice{
		{Name: "NewB", IP: "100.64.0.11", URL: "http://100.64.0.11:8765"},
	})

	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.menuPeers[0].Name != "U2" || tm.menuPeers[1].Name != "U3" {
		t.Fatalf("expected menuPeers snapshot to stay [U2 U3], got %+v", tm.menuPeers)
	}
	if len(tm.menuDiscovered) != 2 || tm.menuDiscovered[0].Name != "NewA" {
		t.Fatalf("expected menuDiscovered snapshot to stay [NewA NewB], got %+v", tm.menuDiscovered)
	}
}

func TestNotifyInfoFlagMapping(t *testing.T) {
	if got := notifyInfoFlag(NotifyInfo); got != NIIF_INFO {
		t.Fatalf("expected NIIF_INFO, got %d", got)
	}
	if got := notifyInfoFlag(NotifyWarning); got != NIIF_WARNING {
		t.Fatalf("expected NIIF_WARNING, got %d", got)
	}
	if got := notifyInfoFlag(NotifyError); got != NIIF_ERROR {
		t.Fatalf("expected NIIF_ERROR, got %d", got)
	}
}

func TestPrintFileFilterStructure(t *testing.T) {
	if !strings.HasSuffix(printFileFilter, "\x00\x00") {
		t.Fatalf("expected printFileFilter to end with double NUL")
	}
	parts := strings.Split(strings.TrimSuffix(printFileFilter, "\x00\x00"), "\x00")
	if len(parts) < 4 || len(parts)%2 != 0 {
		t.Fatalf("expected even number of label/pattern pairs, got %d", len(parts))
	}
	if !strings.Contains(parts[0], "PDF, XPS & Images") {
		t.Fatalf("expected primary filter label to describe PDF, XPS & Images, got %q", parts[0])
	}
	if !strings.Contains(parts[1], "*.pdf") || !strings.Contains(parts[1], "*.oxps") || !strings.Contains(parts[1], "*.tif") {
		t.Fatalf("expected primary filter pattern to include pdf, oxps, tif, got %q", parts[1])
	}
	if parts[len(parts)-2] != "All Files (*.*)" || parts[len(parts)-1] != "*.*" {
		t.Fatalf("expected All Files (*.*) as secondary/final filter, got %q / %q", parts[len(parts)-2], parts[len(parts)-1])
	}
}

func TestDispatchMenuCallbacksAndDiscoveredPeers(t *testing.T) {
	dashCh := make(chan struct{}, 1)
	pairCh := make(chan struct{}, 1)
	discCh := make(chan DiscoveredDevice, 1)

	tm, err := NewTrayManager(t.TempDir(), "U1", "", TrayCallbacks{
		OnOpenDashboard: func() { dashCh <- struct{}{} },
		OnPairDevice:    func() { pairCh <- struct{}{} },
		OnConnectDiscovered: func(d DiscoveredDevice) {
			discCh <- d
		},
	})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	tm.SetDiscoveredPeers([]DiscoveredDevice{
		{Name: "Lab-Printer", IP: "100.64.0.20", URL: "http://100.64.0.20:8765", Port: 8765},
	})
	_ = tm.snapshotAndBuildMenuItems()

	tm.dispatchMenuCommand(CmdOpenDashboard)
	tm.dispatchMenuCommand(CmdPairDevice)
	tm.dispatchMenuCommand(CmdConnectDiscoveredBase)

	select {
	case <-dashCh:
	case <-time.After(time.Second):
		t.Fatal("OnOpenDashboard was not invoked")
	}
	select {
	case <-pairCh:
	case <-time.After(time.Second):
		t.Fatal("OnPairDevice was not invoked")
	}
	select {
	case d := <-discCh:
		if d.Name != "Lab-Printer" || d.IP != "100.64.0.20" {
			t.Fatalf("unexpected discovered device payload: %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("OnConnectDiscovered was not invoked")
	}
}

func TestOfflinePeerPrintDialogWarning(t *testing.T) {
	var queuedPeer, queuedFile string
	var printedPeer, printedFile string
	var pickerCalls atomic.Int32

	tm, err := NewTrayManager(t.TempDir(), "U1", "U2", TrayCallbacks{
		OnQueueOfflinePrint: func(peerName, filePath string) {
			queuedPeer = peerName
			queuedFile = filePath
		},
		OnPrintFileToPeer: func(peerName, filePath string) {
			printedPeer = peerName
			printedFile = filePath
		},
	})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "OfflinePC", IP: "100.64.0.2", Online: false},
		{Name: "OnlinePC", IP: "100.64.0.3", Online: true},
	})
	tm.filePickerFn = func(title string) (string, error) {
		pickerCalls.Add(1)
		return `C:\docs\invoice.pdf`, nil
	}

	// 1. User cancels offline warning -> file picker MUST NOT be opened.
	var confirmPrompt string
	tm.confirmFn = func(title, msg string) bool {
		confirmPrompt = msg
		return false
	}
	tm.handlePrintFileDialog("OfflinePC")
	if !strings.Contains(confirmPrompt, "OfflinePC is currently offline") {
		t.Fatalf("expected offline warning prompt, got %q", confirmPrompt)
	}
	if pickerCalls.Load() != 0 {
		t.Fatalf("expected file picker not to open when offline warning canceled, got %d", pickerCalls.Load())
	}

	// 2. User confirms offline warning -> opens picker and routes to OnQueueOfflinePrint.
	tm.confirmFn = func(title, msg string) bool { return true }
	tm.handlePrintFileDialog("OfflinePC")
	if pickerCalls.Load() != 1 || queuedPeer != "OfflinePC" || queuedFile != `C:\docs\invoice.pdf` {
		t.Fatalf("expected queued offline print, pickerCalls=%d peer=%q file=%q", pickerCalls.Load(), queuedPeer, queuedFile)
	}

	// 3. Online peer skips confirmation and dispatches directly to OnPrintFileToPeer.
	tm.confirmFn = func(title, msg string) bool {
		t.Fatal("confirmation dialog should not appear for online peer")
		return false
	}
	tm.handlePrintFileDialog("OnlinePC")
	if pickerCalls.Load() != 2 || printedPeer != "OnlinePC" || printedFile != `C:\docs\invoice.pdf` {
		t.Fatalf("expected direct print to OnlinePC, pickerCalls=%d peer=%q file=%q", pickerCalls.Load(), printedPeer, printedFile)
	}
}

func TestMissingPrinterPrePickerWarning(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	var printedPeer, printedFile string
	var queuedPeer, queuedFile string
	var pickerCalls atomic.Int32

	tm, err := NewTrayManager(t.TempDir(), "U1", "U2", TrayCallbacks{
		OnPrintFileToPeer: func(peerName, filePath string) {
			printedPeer = peerName
			printedFile = filePath
		},
		OnQueueOfflinePrint: func(peerName, filePath string) {
			queuedPeer = peerName
			queuedFile = filePath
		},
	})
	if err != nil {
		t.Fatalf("NewTrayManager failed: %v", err)
	}
	defer tm.Stop()

	tm.UpdateNetworkStatus("U1", []config.PeerDevice{
		{Name: "NoPrinterPC", IP: "100.64.0.2", Online: true, PrinterReady: boolPtr(false)},
		{Name: "ReadyPC", IP: "100.64.0.3", Online: true, PrinterReady: boolPtr(true)},
		{Name: "UnknownPC", IP: "100.64.0.4", Online: true, PrinterReady: nil},
		{Name: "OfflineNoPrinterPC", IP: "100.64.0.5", Online: false, PrinterReady: boolPtr(false)},
	})

	tm.filePickerFn = func(title string) (string, error) {
		pickerCalls.Add(1)
		return `C:\docs\letter.pdf`, nil
	}

	// 1. Online + PrinterReady=false -> shows "Printer Not Connected" warning and returns BEFORE opening file picker.
	var alertTitle, alertBody string
	var alertCalls atomic.Int32
	tm.alertFn = func(title, message string) {
		alertCalls.Add(1)
		alertTitle = title
		alertBody = message
	}
	tm.confirmFn = func(title, message string) bool {
		t.Fatalf("confirmFn should not be called for online peer without printer")
		return false
	}

	tm.handlePrintFileDialog("NoPrinterPC")
	if alertCalls.Load() != 1 {
		t.Fatalf("expected 1 alert dialog for NoPrinterPC, got %d", alertCalls.Load())
	}
	if alertTitle != "Printer Not Connected" {
		t.Fatalf("expected title %q, got %q", "Printer Not Connected", alertTitle)
	}
	wantBody := "No physical printer is connected on NoPrinterPC.\n\nNoPrinterPC is currently online, so file sharing works, but you cannot print to it until a printer is connected on NoPrinterPC."
	if alertBody != wantBody {
		t.Fatalf("unexpected alert body:\ngot:  %q\nwant: %q", alertBody, wantBody)
	}
	if pickerCalls.Load() != 0 {
		t.Fatalf("expected file picker NOT to open when printer is missing, got %d", pickerCalls.Load())
	}
	if printedPeer != "" {
		t.Fatalf("expected OnPrintFileToPeer not to be called, got %q", printedPeer)
	}

	// 2. Online + PrinterReady=true -> skips warning, opens file picker, prints.
	tm.alertFn = func(title, message string) {
		t.Fatalf("unexpected alert dialog %q: %s", title, message)
	}
	tm.handlePrintFileDialog("ReadyPC")
	if pickerCalls.Load() != 1 || printedPeer != "ReadyPC" || printedFile != `C:\docs\letter.pdf` {
		t.Fatalf("expected direct print to ReadyPC, pickerCalls=%d peer=%q file=%q", pickerCalls.Load(), printedPeer, printedFile)
	}

	// 3. Online + PrinterReady=nil (unknown/legacy) -> skips warning, opens file picker, prints.
	tm.handlePrintFileDialog("UnknownPC")
	if pickerCalls.Load() != 2 || printedPeer != "UnknownPC" || printedFile != `C:\docs\letter.pdf` {
		t.Fatalf("expected direct print to UnknownPC, pickerCalls=%d peer=%q file=%q", pickerCalls.Load(), printedPeer, printedFile)
	}

	// 4. Offline peer (even with PrinterReady=false) -> uses existing Offline confirmation & queueing, NOT missing printer alert.
	var confirmCalled atomic.Int32
	tm.confirmFn = func(title, message string) bool {
		confirmCalled.Add(1)
		return true
	}
	tm.handlePrintFileDialog("OfflineNoPrinterPC")
	if confirmCalled.Load() != 1 {
		t.Fatalf("expected offline confirmation dialog, got %d calls", confirmCalled.Load())
	}
	if pickerCalls.Load() != 3 || queuedPeer != "OfflineNoPrinterPC" || queuedFile != `C:\docs\letter.pdf` {
		t.Fatalf("expected queued print for OfflineNoPrinterPC, pickerCalls=%d peer=%q file=%q", pickerCalls.Load(), queuedPeer, queuedFile)
	}
}
