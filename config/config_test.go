package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAddOrUpdatePeerSkipsSaveWhenPersistentFieldsUnchanged(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "U1"
	cfg.Configured = true
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// First add must persist
	dev, isNew := cfg.AddOrUpdatePeer("U2", "100.64.0.2", "http://100.64.0.2:8765")
	if !isNew || dev.Name != "U2" {
		t.Fatalf("expected new peer U2, got isNew=%v dev=%+v", isNew, dev)
	}

	// Backdate file modtime + record exact file bytes
	oldTime := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	if err := os.Chtimes(cfgPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}
	beforeBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	// Repeated authenticated call with unchanged persistent fields (and empty IP) must NOT rewrite config.json
	dev2, isNew2 := cfg.AddOrUpdatePeer("U2", "", "http://100.64.0.2:8765")
	if isNew2 {
		t.Fatalf("expected existing peer U2, got isNew=true")
	}
	if !dev2.Online || dev2.LastSeen.IsZero() {
		t.Fatalf("expected runtime state Online=true and non-zero LastSeen, got %+v", dev2)
	}

	infoAfter, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !infoAfter.ModTime().Equal(oldTime) {
		t.Fatalf("expected config.json not to be rewritten on unchanged peer fields; modtime changed from %v to %v", oldTime, infoAfter.ModTime())
	}
	afterBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile after failed: %v", err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatalf("expected config.json bytes to remain identical")
	}

	// Changing a persistent field (Name) MUST persist
	dev3, isNew3 := cfg.AddOrUpdatePeer("U2-Renamed", "", "http://100.64.0.2:8765")
	if isNew3 || dev3.Name != "U2-Renamed" {
		t.Fatalf("expected updated peer name U2-Renamed, got isNew=%v dev=%+v", isNew3, dev3)
	}
	updatedBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile updated failed: %v", err)
	}
	if !strings.Contains(string(updatedBytes), "U2-Renamed") {
		t.Fatalf("expected renamed peer to be saved to config.json, got:\n%s", string(updatedBytes))
	}
}

func TestSaveConfigAtomicAndNoTempLeftovers(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-Atomic"
	cfg.Configured = true

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c := DefaultConfig()
			c.ConfigPath = cfgPath
			c.NodeName = "Unit-Atomic"
			c.Configured = true
			c.ListenPort = 8765 + idx
			if err := SaveConfig(cfgPath, c); err != nil {
				t.Errorf("concurrent SaveConfig failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// Must always load valid JSON with zero leftover temp files
	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig after concurrent atomic saves failed: %v", err)
	}
	if loaded.NodeName != "Unit-Atomic" || !loaded.Configured {
		t.Fatalf("unexpected loaded config: %+v", loaded)
	}

	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Fatalf("unexpected leftover file in config dir: %s", e.Name())
		}
	}
}

func TestLoadConfigPreservesExplicitConfiguredTrue(t *testing.T) {
	tempDir := t.TempDir()

	cases := []struct {
		name           string
		jsonText       string
		wantConfigured bool
	}{
		{
			name:           "Explicit configured true with Desktop 1",
			jsonText:       `{"configured":true,"node_name":"Desktop 1","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with Desktop 2",
			jsonText:       `{"configured":true,"node_name":"Desktop 2","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with Bridge-PC",
			jsonText:       `{"configured":true,"node_name":"Bridge-PC","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with custom name",
			jsonText:       `{"configured":true,"node_name":"Office-Unit","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: true,
		},
		{
			name:           "Explicit configured true with empty node_name still triggers setup",
			jsonText:       `{"configured":true,"node_name":"","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: false,
		},
		{
			name:           "Missing configured flag triggers setup",
			jsonText:       `{"node_name":"Desktop 1","listen_port":8765,"auth_token":"tok"}`,
			wantConfigured: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(tempDir, strings.ReplaceAll(tc.name, " ", "_")+".json")
			if err := os.WriteFile(p, []byte(tc.jsonText), 0644); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}
			cfg, err := LoadConfig(p)
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}
			if cfg.Configured != tc.wantConfigured {
				t.Fatalf("LoadConfig(%s): Configured = %v, want %v", tc.name, cfg.Configured, tc.wantConfigured)
			}
		})
	}
}

func TestCustomPeerNamePreservedAcrossAutoUpdates(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "U1"
	cfg.Configured = true

	dev, isNew := cfg.AddOrUpdatePeer("Detected-U2", "100.64.0.2", "http://100.64.0.2:8765")
	if !isNew || dev.Name != "Detected-U2" || dev.CustomName {
		t.Fatalf("unexpected initial peer: isNew=%v dev=%+v", isNew, dev)
	}

	if changed := cfg.SetPeerCustomName("http://100.64.0.2:8765", "Office-U2", true); !changed {
		t.Fatal("expected SetPeerCustomName to report change")
	}

	// Repeating identical custom name must be a no-op
	if changed := cfg.SetPeerCustomName("http://100.64.0.2:8765", "Office-U2", true); changed {
		t.Fatal("expected duplicate SetPeerCustomName to return false")
	}

	// Auto-learning via SetPeerName must NOT overwrite a custom peer name
	if changed := cfg.SetPeerName("http://100.64.0.2:8765", "Detected-U2"); changed {
		t.Fatal("expected SetPeerName to skip peer with CustomName=true")
	}

	// AddOrUpdatePeer from remote auto-pair/gossip must NOT overwrite a custom peer name
	devAfter, isNewAfter := cfg.AddOrUpdatePeer("Detected-U2", "100.64.0.2", "http://100.64.0.2:8765")
	if isNewAfter {
		t.Fatal("expected existing peer on AddOrUpdatePeer")
	}
	if devAfter.Name != "Office-U2" || !devAfter.CustomName {
		t.Fatalf("expected custom peer name 'Office-U2' to be preserved, got %+v", devAfter)
	}

	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.PeerName != "Office-U2" || len(loaded.Peers) != 1 || loaded.Peers[0].Name != "Office-U2" || !loaded.Peers[0].CustomName {
		t.Fatalf("expected persisted custom peer name 'Office-U2', got PeerName=%q Peers=%+v", loaded.PeerName, loaded.Peers)
	}

	// Clearing CustomName allows subsequent auto-learning again
	if changed := cfg.SetPeerCustomName("http://100.64.0.2:8765", "Detected-U2", false); !changed {
		t.Fatal("expected clearing CustomName to report change")
	}
	if changed := cfg.SetPeerName("http://100.64.0.2:8765", "Detected-U2-Renamed"); !changed {
		t.Fatal("expected SetPeerName to update non-custom peer")
	}
	if got := cfg.FindPeer("http://100.64.0.2:8765"); got == nil || got.Name != "Detected-U2-Renamed" || got.CustomName {
		t.Fatalf("expected updated non-custom peer, got %+v", got)
	}
}

func TestOnlineStatusNotPersistedAcrossSaveAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-1"
	cfg.Configured = true
	if cfg.PrintTimeoutSec != 120 {
		t.Fatalf("expected default PrintTimeoutSec=120, got %d", cfg.PrintTimeoutSec)
	}

	dev, _ := cfg.AddOrUpdatePeer("Unit-2", "100.64.0.2", "http://100.64.0.2:8765")
	if !dev.Online || dev.LastSeen.IsZero() {
		t.Fatalf("expected in-memory Online=true and LastSeen set after AddOrUpdatePeer, got %+v", dev)
	}

	// PeerDevice must still serialize Online/LastSeen for HTTP responses (/api/health, /api/peers)
	apiBytes, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("marshal PeerDevice failed: %v", err)
	}
	if !strings.Contains(string(apiBytes), `"online":true`) || !strings.Contains(string(apiBytes), `"last_seen"`) {
		t.Fatalf("expected PeerDevice JSON to include online & last_seen for HTTP responses, got %s", string(apiBytes))
	}

	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Verify in-memory state was NOT cleared by SaveConfig
	inMem := cfg.GetPeers()
	if len(inMem) != 1 || !inMem[0].Online || inMem[0].LastSeen.IsZero() {
		t.Fatalf("SaveConfig should not clear in-memory peer Online/LastSeen, got %+v", inMem)
	}

	// Verify disk JSON does NOT contain online:true (and LastSeen is zero)
	diskBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if strings.Contains(string(diskBytes), `"online"`) {
		t.Fatalf("expected SaveConfig not to persist online to disk, got:\n%s", string(diskBytes))
	}
	var diskCfg struct {
		Peers []PeerDevice `json:"peers"`
	}
	if err := json.Unmarshal(diskBytes, &diskCfg); err != nil {
		t.Fatalf("Unmarshal diskBytes failed: %v", err)
	}
	if len(diskCfg.Peers) != 1 || diskCfg.Peers[0].Online || !diskCfg.Peers[0].LastSeen.IsZero() {
		t.Fatalf("expected disk peers to have Online=false and zero LastSeen, got %+v", diskCfg.Peers)
	}

	// Even if a legacy config.json on disk contains "online": true, LoadConfig must clear it
	staleJSON := `{
		"configured": true,
		"node_name": "Unit-1",
		"listen_port": 8765,
		"auth_token": "tok",
		"peers": [
			{"name": "Unit-2", "ip": "100.64.0.2", "url": "http://100.64.0.2:8765", "spool_port": 9101, "online": true, "last_seen": "2026-01-01T00:00:00Z"}
		]
	}`
	if err := os.WriteFile(cfgPath, []byte(staleJSON), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.PrintTimeoutSec != 120 {
		t.Fatalf("expected missing PrintTimeoutSec to default to 120, got %d", loaded.PrintTimeoutSec)
	}
	if len(loaded.Peers) != 1 || loaded.Peers[0].Online || !loaded.Peers[0].LastSeen.IsZero() {
		t.Fatalf("expected LoadConfig to clear Online and LastSeen, got %+v", loaded.Peers)
	}
}

func TestIgnoredPeersLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-1"
	cfg.Configured = true

	if err := cfg.AddIgnoredPeer("Printer-PC", "100.64.0.99", "http://100.64.0.99:8765"); err != nil {
		t.Fatalf("AddIgnoredPeer failed: %v", err)
	}
	if !cfg.IsPeerIgnored("", "100.64.0.99", "") {
		t.Fatal("expected IsPeerIgnored true by IP")
	}
	if !cfg.IsPeerIgnored("", "", "http://100.64.0.99:8765/") {
		t.Fatal("expected IsPeerIgnored true by URL")
	}
	if !cfg.IsPeerIgnored("printer-pc", "", "") {
		t.Fatal("expected IsPeerIgnored true by Name")
	}
	if cfg.IsPeerIgnored("Printer-PC", "100.64.0.55", "http://100.64.0.55:8765") {
		t.Fatal("expected IsPeerIgnored false when name matches but IP differs")
	}

	// Reload from disk and verify ignored peers persisted
	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	ignored := loaded.GetIgnoredPeers()
	if len(ignored) != 1 || ignored[0].IP != "100.64.0.99" || ignored[0].IgnoredAt.IsZero() {
		t.Fatalf("unexpected reloaded ignored peers: %+v", ignored)
	}

	if err := loaded.RemoveIgnoredPeer("100.64.0.99"); err != nil {
		t.Fatalf("RemoveIgnoredPeer failed: %v", err)
	}
	if loaded.IsPeerIgnored("Printer-PC", "100.64.0.99", "http://100.64.0.99:8765") {
		t.Fatal("expected peer to no longer be ignored after RemoveIgnoredPeer")
	}
	reloaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig after remove failed: %v", err)
	}
	if len(reloaded.GetIgnoredPeers()) != 0 {
		t.Fatalf("expected 0 ignored peers after removal, got %+v", reloaded.GetIgnoredPeers())
	}
}

func TestSpoolPortDeduplicationInLoadConfigAndSetPeers(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	rawJSON := `{
		"configured": true,
		"node_name": "Unit-1",
		"listen_port": 9102,
		"auth_token": "tok",
		"peers": [
			{"name": "P1", "ip": "100.64.0.2", "url": "http://100.64.0.2:8765", "spool_port": 9100},
			{"name": "P2", "ip": "100.64.0.3", "url": "http://100.64.0.3:8765", "spool_port": 9101},
			{"name": "P3", "ip": "100.64.0.4", "url": "http://100.64.0.4:8765", "spool_port": 9101},
			{"name": "P4", "ip": "100.64.0.5", "url": "http://100.64.0.5:8765", "spool_port": 9102}
		]
	}`
	if err := os.WriteFile(cfgPath, []byte(rawJSON), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	assertValidUniquePorts := func(peers []PeerDevice, listenPort int) {
		t.Helper()
		seen := make(map[int]string)
		for _, p := range peers {
			if p.SpoolPort <= 9100 || p.SpoolPort == listenPort {
				t.Fatalf("peer %s got forbidden SpoolPort %d (listenPort=%d)", p.Name, p.SpoolPort, listenPort)
			}
			if prev, dup := seen[p.SpoolPort]; dup {
				t.Fatalf("duplicate SpoolPort %d on %s and %s", p.SpoolPort, prev, p.Name)
			}
			seen[p.SpoolPort] = p.Name
		}
	}
	assertValidUniquePorts(cfg.GetPeers(), 9102)

	if err := cfg.SetPeers([]PeerDevice{
		{Name: "A", IP: "100.64.0.10", URL: "http://100.64.0.10:8765", SpoolPort: 9101},
		{Name: "B", IP: "100.64.0.11", URL: "http://100.64.0.11:8765", SpoolPort: 9101},
		{Name: "C", IP: "100.64.0.12", URL: "http://100.64.0.12:8765", SpoolPort: 9100},
	}); err != nil {
		t.Fatalf("SetPeers failed: %v", err)
	}
	assertValidUniquePorts(cfg.GetPeers(), 9102)
}

func TestRemovePeerAndSelfPruneClearsLegacyFieldsAndUsesUnifiedPriority(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-1"
	cfg.Configured = true

	// Add two peers, one with placeholder name updating by IP and one sharing a friendly name
	p1, isNew1 := cfg.AddOrUpdatePeer("Device (100.64.0.2)", "100.64.0.2", "http://100.64.0.2:8765")
	if !isNew1 || p1.Name != "Device (100.64.0.2)" {
		t.Fatalf("unexpected initial peer: %+v", p1)
	}
	p1Updated, isNew1Up := cfg.AddOrUpdatePeer("Office-PC", "100.64.0.2", "http://100.64.0.2:8765")
	if isNew1Up || p1Updated.Name != "Office-PC" {
		t.Fatalf("expected IP match to update placeholder name to Office-PC, got isNew=%v dev=%+v", isNew1Up, p1Updated)
	}
	// Placeholder probe must NOT overwrite real name
	p1AfterProbe, _ := cfg.AddOrUpdatePeer("Device (100.64.0.2)", "100.64.0.2", "")
	if p1AfterProbe.Name != "Office-PC" {
		t.Fatalf("placeholder probe overwrote real name: %+v", p1AfterProbe)
	}

	// Adding a different peer that shares the friendly name "Office-PC" must NOT overwrite 100.64.0.2
	p2, isNew2 := cfg.AddOrUpdatePeer("Office-PC", "100.64.0.3", "http://100.64.0.3:8765")
	if !isNew2 || p2.IP != "100.64.0.3" || p2.Name == "Office-PC" {
		t.Fatalf("expected distinct disambiguated peer for name collision, got isNew=%v dev=%+v", isNew2, p2)
	}
	if len(cfg.GetPeers()) != 2 {
		t.Fatalf("expected 2 peers after name collision, got %d", len(cfg.GetPeers()))
	}

	// Remove by URL (priority URL -> IP -> Name)
	if !cfg.RemovePeer("http://100.64.0.3:8765") {
		t.Fatal("expected RemovePeer by URL to succeed")
	}
	if !cfg.RemovePeer("100.64.0.2") {
		t.Fatal("expected RemovePeer by IP to succeed")
	}
	if len(cfg.GetPeers()) != 0 || cfg.PeerName != "" || cfg.PeerURL != "" {
		t.Fatalf("expected legacy PeerName/PeerURL cleared when Peers becomes empty, got PeerName=%q PeerURL=%q Peers=%+v", cfg.PeerName, cfg.PeerURL, cfg.GetPeers())
	}

	// SetNodeName pruning last peer must also clear legacy fields
	cfg.AddOrUpdatePeer("TempSelf", "100.64.0.9", "http://100.64.0.9:8765")
	if err := cfg.SetNodeName("TempSelf"); err != nil {
		t.Fatalf("SetNodeName failed: %v", err)
	}
	if len(cfg.GetPeers()) != 0 || cfg.PeerName != "" || cfg.PeerURL != "" {
		t.Fatalf("expected SetNodeName pruning all peers to clear legacy fields, got PeerName=%q PeerURL=%q", cfg.PeerName, cfg.PeerURL)
	}
}

func TestPeerPrinterStatusHelpersAndPersistence(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "config.json")

	cfg := DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-1"
	cfg.Configured = true

	// Initial unprobed peer has PrinterReady == nil
	dev, _ := cfg.AddOrUpdatePeer("Unit-2", "100.64.0.2", "http://100.64.0.2:8765")
	if dev.PrinterReady != nil || dev.PrinterName != "" {
		t.Fatalf("expected initial peer PrinterReady=nil and PrinterName='', got %+v", dev)
	}
	if dev.IsPrinterKnownMissing() {
		t.Fatal("expected IsPrinterKnownMissing() == false when PrinterReady is nil")
	}
	if !dev.IsPrinterReadyOrUnknown() {
		t.Fatal("expected IsPrinterReadyOrUnknown() == true when PrinterReady is nil")
	}

	// Unknown target returns false
	if cfg.SetPeerPrinterStatus("http://100.64.0.99:8765", true, "Ghost") {
		t.Fatal("expected SetPeerPrinterStatus to return false for unknown target")
	}

	// Set ready = true by URL (with trailing slash) and verify live JSON serialization
	if !cfg.SetPeerPrinterStatus("http://100.64.0.2:8765/", true, "  HP LaserJet  ") {
		t.Fatal("expected SetPeerPrinterStatus by URL to succeed")
	}
	got := cfg.FindPeer("Unit-2")
	if got == nil || got.PrinterReady == nil || !*got.PrinterReady || got.PrinterName != "HP LaserJet" {
		t.Fatalf("expected PrinterReady=true and PrinterName='HP LaserJet', got %+v", got)
	}
	if got.IsPrinterKnownMissing() {
		t.Fatal("expected IsPrinterKnownMissing() == false when PrinterReady is true")
	}
	if !got.IsPrinterReadyOrUnknown() {
		t.Fatal("expected IsPrinterReadyOrUnknown() == true when PrinterReady is true")
	}

	liveTrueBytes, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(got) failed: %v", err)
	}
	if !strings.Contains(string(liveTrueBytes), `"printer_ready":true`) || !strings.Contains(string(liveTrueBytes), `"printer_name":"HP LaserJet"`) {
		t.Fatalf("expected live PeerDevice JSON to include printer_ready:true and printer_name, got %s", string(liveTrueBytes))
	}

	// Update to ready = false by IP and verify *bool serializes "printer_ready":false
	if !cfg.SetPeerPrinterStatus("100.64.0.2", false, "") {
		t.Fatal("expected SetPeerPrinterStatus by IP to succeed")
	}
	gotFalse := cfg.FindPeer("100.64.0.2")
	if gotFalse == nil || gotFalse.PrinterReady == nil || *gotFalse.PrinterReady || gotFalse.PrinterName != "" {
		t.Fatalf("expected PrinterReady=false and empty PrinterName, got %+v", gotFalse)
	}
	if !gotFalse.IsPrinterKnownMissing() {
		t.Fatal("expected IsPrinterKnownMissing() == true when PrinterReady is false")
	}
	if gotFalse.IsPrinterReadyOrUnknown() {
		t.Fatal("expected IsPrinterReadyOrUnknown() == false when PrinterReady is false")
	}
	liveFalseBytes, err := json.Marshal(gotFalse)
	if err != nil {
		t.Fatalf("json.Marshal(gotFalse) failed: %v", err)
	}
	if !strings.Contains(string(liveFalseBytes), `"printer_ready":false`) {
		t.Fatalf("expected live PeerDevice JSON to include printer_ready:false, got %s", string(liveFalseBytes))
	}

	// Update back to ready = true by Name before SaveConfig
	if !cfg.SetPeerPrinterStatus("unit-2", true, "Canon PIXMA") {
		t.Fatal("expected SetPeerPrinterStatus by Name to succeed")
	}

	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// In-memory state must NOT be cleared by SaveConfig
	inMem := cfg.GetPeers()
	if len(inMem) != 1 || inMem[0].PrinterReady == nil || !*inMem[0].PrinterReady || inMem[0].PrinterName != "Canon PIXMA" {
		t.Fatalf("SaveConfig should not clear in-memory PrinterReady/PrinterName, got %+v", inMem)
	}

	// Disk config.json must NOT contain printer_ready or printer_name
	diskBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if strings.Contains(string(diskBytes), `"printer_ready"`) || strings.Contains(string(diskBytes), `"printer_name"`) {
		t.Fatalf("expected SaveConfig not to persist printer_ready or printer_name to disk, got:\n%s", string(diskBytes))
	}

	// Even if config.json on disk has stale printer_ready / printer_name, LoadConfig must clear them
	staleJSON := `{
		"configured": true,
		"node_name": "Unit-1",
		"listen_port": 8765,
		"auth_token": "tok",
		"peers": [
			{"name": "Unit-2", "ip": "100.64.0.2", "url": "http://100.64.0.2:8765", "spool_port": 9101, "printer_ready": false, "printer_name": "Stale Printer"}
		]
	}`
	if err := os.WriteFile(cfgPath, []byte(staleJSON), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(loaded.Peers) != 1 || loaded.Peers[0].PrinterReady != nil || loaded.Peers[0].PrinterName != "" {
		t.Fatalf("expected LoadConfig to clear PrinterReady and PrinterName, got %+v", loaded.Peers)
	}
}
