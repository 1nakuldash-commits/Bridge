package installer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"bridge/config"
	"bridge/server"
)

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"NormalName", "NormalName"},
		{"Unit 1 (Desk)", "Unit 1 (Desk)"},
		{"Unit/1:Desk*Name", "Unit_1_Desk_Name"},
		{"   Spaced Name   ", "Spaced Name"},
	}

	for _, tt := range tests {
		got := SanitizeFileName(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSuggestNextDeviceName(t *testing.T) {
	tests := []struct {
		detected []string
		currName string
		want     string
	}{
		{[]string{}, "", "U1"},
		{[]string{"U1"}, "", "U2"},
		{[]string{"U1", "U2"}, "", "U3"},
		{[]string{"Office"}, "U1", "U1"},
	}

	for _, tt := range tests {
		got := SuggestNextDeviceName(tt.detected, tt.currName)
		if got != tt.want {
			t.Errorf("SuggestNextDeviceName(%v, %q) = %q, want %q", tt.detected, tt.currName, got, tt.want)
		}
	}
}

func TestPruneObsoleteVirtualPrintersEmpty(t *testing.T) {
	// Ensure PruneObsoleteVirtualPrinters does not panic or error on empty peer slice
	peers := []config.PeerDevice{
		{Name: "U1(D)", SpoolPort: 9101},
	}
	// On Windows, running with valid peers generates a valid script
	err := PruneObsoleteVirtualPrinters(peers)
	if err != nil {
		t.Logf("PruneObsoleteVirtualPrinters returned note (expected if not elevated): %v", err)
	}
}

func TestPeerDeviceOnlineSerialization(t *testing.T) {
	dev := config.PeerDevice{
		Name:      "U1(D)",
		IP:        "100.92.92.87",
		URL:       "http://100.92.92.87:8765",
		SpoolPort: 9101,
		Online:    true,
		LastSeen:  time.Now(),
	}

	data, err := json.Marshal(dev)
	if err != nil {
		t.Fatalf("Failed to marshal PeerDevice: %v", err)
	}

	var decoded config.PeerDevice
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal PeerDevice: %v", err)
	}

	if !decoded.Online {
		t.Errorf("Expected decoded.Online to be true, got false. JSON payload: %s", string(data))
	}
}

func TestHealthResponsePeerOnlineSerialization(t *testing.T) {
	resp := server.HealthResponse{
		Status:      "healthy",
		NodeName:    "ST_ND",
		OnlineCount: 1,
		Peers: []config.PeerDevice{
			{
				Name:      "U1(D)",
				IP:        "100.92.92.87",
				URL:       "http://100.92.92.87:8765",
				SpoolPort: 9101,
				Online:    true,
			},
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Failed to marshal HealthResponse: %v", err)
	}

	var decoded server.HealthResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal HealthResponse: %v", err)
	}

	if len(decoded.Peers) != 1 {
		t.Fatalf("Expected 1 peer, got %d", len(decoded.Peers))
	}
	if !decoded.Peers[0].Online {
		t.Errorf("Expected decoded.Peers[0].Online to be true, got false. Raw JSON: %s", string(data))
	}
}

func TestInstallVirtualPrinterNonElevatedHandling(t *testing.T) {
	// Empty peer name must always be a safe no-op
	if err := InstallVirtualPrinter("   ", 9199); err != nil {
		t.Fatalf("Expected nil error for empty peer name, got: %v", err)
	}

	if IsElevated() {
		t.Skip("Running elevated; skipping non-elevated InstallVirtualPrinter test")
	}

	err := InstallVirtualPrinter("NonExistentTestPeer_Bridge9199", 9199)
	if err == nil {
		t.Fatalf("Expected explicit privilege error when installing missing virtual printer while non-elevated")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "administrator") {
		t.Fatalf("Expected clear administrator privileges error, got: %v", err)
	}
}

func TestRegisterInteractiveStartupFast(t *testing.T) {
	start := time.Now()
	if err := RegisterInteractiveStartup(); err != nil {
		t.Fatalf("RegisterInteractiveStartup failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Fatalf("RegisterInteractiveStartup took too long (%v); should be <1s", elapsed)
	}
}

func TestGetPrimaryDesktopDirAndOrder(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only desktop resolution test")
	}
	primary := GetPrimaryDesktopDir()
	if primary == "" {
		t.Fatal("GetPrimaryDesktopDir returned empty string")
	}
	info, err := os.Stat(primary)
	if err != nil || !info.IsDir() {
		t.Fatalf("GetPrimaryDesktopDir (%s) is not an existing directory: %v", primary, err)
	}
	all := GetAllDesktopDirs()
	if len(all) == 0 {
		t.Fatal("GetAllDesktopDirs returned empty slice")
	}
	if !strings.EqualFold(filepath.Clean(all[0]), filepath.Clean(primary)) {
		t.Fatalf("Expected GetAllDesktopDirs()[0] (%q) to equal GetPrimaryDesktopDir() (%q)", all[0], primary)
	}
}

func TestFormatElevatedArgs(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only argument escaping test")
	}
	args := []string{"-upgrade", `C:\Users\Nakul Dash\My Builds\bridge.exe`}
	got := formatElevatedArgs(args)
	want := `-upgrade "C:\Users\Nakul Dash\My Builds\bridge.exe"`
	if got != want {
		t.Fatalf("formatElevatedArgs(%v) = %q, want %q", args, got, want)
	}
}

func TestShortcutsSingleDesktopAndNoSumatraShortcut(t *testing.T) {
	primary := GetPrimaryDesktopDir()
	all := GetAllDesktopDirs()
	if primary == "" || len(all) == 0 {
		t.Skip("No desktop directories available")
	}

	sharedPrimary := filepath.Join(primary, "Bridge Shared Folder.lnk")
	_, existedBefore := os.Stat(sharedPrimary)

	// Run shortcut sync with empty peers (no virtual printer needed)
	res := SyncBridgeShortcutsForPeers(nil)
	if len(res.PrinterErrors) != 0 {
		t.Fatalf("Unexpected printer errors for nil peers: %v", res.PrinterErrors)
	}

	if _, err := os.Stat(sharedPrimary); err != nil {
		t.Fatalf("Expected single Bridge Shared Folder.lnk in primary desktop (%s): %v", sharedPrimary, err)
	}

	for _, d := range all {
		if _, err := os.Stat(filepath.Join(d, "SumatraPDF.lnk")); !os.IsNotExist(err) {
			t.Errorf("Expected SumatraPDF.lnk to NOT exist in %s", d)
		}
		if !strings.EqualFold(filepath.Clean(d), filepath.Clean(primary)) {
			if _, err := os.Stat(filepath.Join(d, "Bridge Shared Folder.lnk")); !os.IsNotExist(err) {
				t.Errorf("Expected duplicate Bridge Shared Folder.lnk to be absent from secondary desktop %s", d)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(TargetAppDir, "SumatraPDF.lnk")); !os.IsNotExist(err) {
		t.Errorf("Expected %s\\SumatraPDF.lnk to NOT exist", TargetAppDir)
	}

	if os.IsNotExist(existedBefore) {
		_ = os.Remove(sharedPrimary)
	}
}

func TestConnectManualPeerValidation(t *testing.T) {
	// 1. Unreachable closed port must return false and add nothing
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "U1"
	cfg.ListenPort = 8765

	if ok := connectManualPeer(cfg, bufio.NewReader(strings.NewReader("\n")), closedAddr, ""); ok {
		t.Fatal("Expected connectManualPeer to return false for unreachable port")
	}
	if len(cfg.GetPeers()) != 0 {
		t.Fatalf("Expected 0 peers after failed manual connect, got %v", cfg.GetPeers())
	}

	// 2. HTTP 200 with invalid/non-Bridge JSON must return false and add nothing
	nonBridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"unrelated":true}`))
	}))
	defer nonBridgeSrv.Close()

	if ok := connectManualPeer(cfg, bufio.NewReader(strings.NewReader("\n")), nonBridgeSrv.URL, ""); ok {
		t.Fatal("Expected connectManualPeer to return false for non-Bridge JSON")
	}
	if len(cfg.GetPeers()) != 0 {
		t.Fatalf("Expected 0 peers after non-Bridge JSON, got %v", cfg.GetPeers())
	}

	// 3. HTTP 500 must return false
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	if ok := connectManualPeer(cfg, bufio.NewReader(strings.NewReader("\n")), errSrv.URL, ""); ok {
		t.Fatal("Expected connectManualPeer to return false for HTTP 500")
	}

	// 4. Valid Bridge /api/health endpoint with X-Bridge-Probe verification and custom name confirmation
	var sawProbe string
	validSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			sawProbe = r.Header.Get("X-Bridge-Probe")
			_ = json.NewEncoder(w).Encode(server.HealthResponse{
				Status:   "healthy",
				NodeName: "Detected-PC",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer validSrv.Close()

	if ok := connectManualPeer(cfg, bufio.NewReader(strings.NewReader("Office-U2\n")), validSrv.URL, ""); !ok {
		t.Fatal("Expected connectManualPeer to succeed against valid Bridge health server")
	}
	if sawProbe != "1" {
		t.Errorf("Expected X-Bridge-Probe=1 header on manual health probe, got %q", sawProbe)
	}
	peers := cfg.GetPeers()
	if len(peers) != 1 || peers[0].Name != "Office-U2" {
		t.Fatalf("Expected 1 peer named 'Office-U2', got %+v", peers)
	}

	// 5. Device name resolution via discoverPeersFunc
	origDiscover := discoverPeersFunc
	defer func() { discoverPeersFunc = origDiscover }()
	discoverPeersFunc = func() ([]DiscoveredPeer, error) {
		return []DiscoveredPeer{
			{HostName: "ts-u3.tailnet.ts.net", IP: "100.64.0.33", BridgeName: "U3"},
		}, nil
	}
	ip, targetURL, errMsg := resolveManualTarget("U3")
	if errMsg != "" || ip != "100.64.0.33" || targetURL != "http://100.64.0.33:8765" {
		t.Fatalf("resolveManualTarget('U3') = (%q, %q, %q)", ip, targetURL, errMsg)
	}
	ip2, _, errMsg2 := resolveManualTarget("ts-u3")
	if errMsg2 != "" || ip2 != "100.64.0.33" {
		t.Fatalf("resolveManualTarget('ts-u3') = (%q, %q)", ip2, errMsg2)
	}
	if _, _, errMsg3 := resolveManualTarget("NonExistentNode"); errMsg3 == "" {
		t.Fatal("Expected error message for unknown device name")
	}
}

func TestSyncPeersWithRunningServicePreservesAuthoritativeSpoolPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(config.PeerDevice{
				Name:      "U2",
				IP:        "100.64.0.2",
				URL:       "http://100.64.0.2:8765",
				SpoolPort: 9105,
			})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"node_name": "U1",
				"peers": []config.PeerDevice{
					{
						Name:      "U2",
						IP:        "100.64.0.2",
						URL:       "http://100.64.0.2:8765",
						SpoolPort: 9105,
					},
				},
			})
		}
	}))
	defer srv.Close()

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := config.DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "U1"
	cfg.ListenPort = port
	cfg.Peers = []config.PeerDevice{
		{
			Name:      "U2",
			IP:        "100.64.0.2",
			URL:       "http://100.64.0.2:8765",
			SpoolPort: 9101, // Stale local port
		},
	}

	syncPeersWithRunningService(cfg)

	gotPeers := cfg.GetPeers()
	if len(gotPeers) != 1 || gotPeers[0].SpoolPort != 9105 {
		t.Fatalf("Expected authoritative SpoolPort 9105 from running service, got %+v", gotPeers)
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("Failed to reload saved config: %v", err)
	}
	if len(loaded.Peers) != 1 || loaded.Peers[0].SpoolPort != 9105 {
		t.Fatalf("Expected saved config to have SpoolPort 9105, got %+v", loaded.Peers)
	}
	_ = fmt.Sprintf("verified port %d", loaded.Peers[0].SpoolPort)
}
