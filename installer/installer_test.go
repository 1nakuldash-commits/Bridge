package installer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	if len(peers) != 1 || peers[0].Name != "Office-U2" || !peers[0].CustomName {
		t.Fatalf("Expected 1 peer named 'Office-U2' with CustomName=true, got %+v", peers)
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

func TestConfirmPeerNameAndSyncPreservesCustomName(t *testing.T) {
	// 1. Verify confirmPeerName returns isCustom only for distinct non-self custom names
	name, isCustom := confirmPeerName(bufio.NewReader(strings.NewReader("\n")), "100.64.0.2", "Detected-PC", "U1")
	if name != "Detected-PC" || isCustom {
		t.Fatalf("Enter should keep detected name with isCustom=false, got (%q, %v)", name, isCustom)
	}

	name, isCustom = confirmPeerName(bufio.NewReader(strings.NewReader("detected-pc\n")), "100.64.0.2", "Detected-PC", "U1")
	if !strings.EqualFold(name, "Detected-PC") || isCustom {
		t.Fatalf("Case-insensitive match to detected name should have isCustom=false, got (%q, %v)", name, isCustom)
	}

	name, isCustom = confirmPeerName(bufio.NewReader(strings.NewReader("u1\n")), "100.64.0.2", "Detected-PC", "U1")
	if name != "Detected-PC" || isCustom {
		t.Fatalf("Self node name should fall back to detected name with isCustom=false, got (%q, %v)", name, isCustom)
	}

	name, isCustom = confirmPeerName(bufio.NewReader(strings.NewReader("Office-U2\n")), "100.64.0.2", "Detected-PC", "U1")
	if name != "Office-U2" || !isCustom {
		t.Fatalf("Custom name should return ('Office-U2', true), got (%q, %v)", name, isCustom)
	}

	// 2. Verify connectDetectedPeer + syncPeersWithRunningService preserve CustomName=true
	var postedPeer config.PeerDevice
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&postedPeer); err != nil {
				t.Fatalf("decode POST /api/peers: %v", err)
			}
			postedPeer.SpoolPort = 9103
			_ = json.NewEncoder(w).Encode(postedPeer)
		case http.MethodGet:
			// Simulate GET failing or returning authoritative peer with CustomName preserved
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"node_name": "U1",
				"peers":     []config.PeerDevice{postedPeer},
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

	connectDetectedPeer(cfg, "Office-U2", true, "100.64.0.2", "http://100.64.0.2:8765", nil, "")

	// Re-running connectDetectedPeer with detected name and isCustom=false must NOT overwrite existing custom name
	connectDetectedPeer(cfg, "Detected-PC", false, "100.64.0.2", "http://100.64.0.2:8765", nil, "")

	syncPeersWithRunningService(cfg)

	if postedPeer.Name != "Office-U2" || !postedPeer.CustomName {
		t.Fatalf("Expected POST /api/peers payload to carry Name='Office-U2' and CustomName=true, got %+v", postedPeer)
	}

	gotPeers := cfg.GetPeers()
	if len(gotPeers) != 1 || gotPeers[0].Name != "Office-U2" || !gotPeers[0].CustomName || gotPeers[0].SpoolPort != 9103 {
		t.Fatalf("Expected synced peer 'Office-U2' with CustomName=true and SpoolPort=9103, got %+v", gotPeers)
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(loaded.Peers) != 1 || loaded.Peers[0].Name != "Office-U2" || !loaded.Peers[0].CustomName {
		t.Fatalf("Expected persisted peer 'Office-U2' with CustomName=true, got %+v", loaded.Peers)
	}
}

func TestFirewallRuleArgs(t *testing.T) {
	wantDel := []string{"advfirewall", "firewall", "delete", "rule", "name=" + FirewallRule}
	if gotDel := firewallRuleDeleteArgs(); !reflect.DeepEqual(gotDel, wantDel) {
		t.Fatalf("firewallRuleDeleteArgs() = %v, want %v", gotDel, wantDel)
	}

	tests := []struct {
		port     int
		wantPort string
	}{
		{0, "localport=8765,9100-9120"},
		{-1, "localport=8765,9100-9120"},
		{8765, "localport=8765,9100-9120"},
		{9876, "localport=9876,9100-9120"},
	}

	for _, tt := range tests {
		got := firewallRuleAddArgs(tt.port)
		want := []string{
			"advfirewall", "firewall", "add", "rule",
			"name=" + FirewallRule,
			"dir=in",
			"action=allow",
			"protocol=TCP",
			tt.wantPort,
			"profile=any",
			"description=Bridge multi-device file sync and virtual print communication",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("firewallRuleAddArgs(%d) = %v, want %v", tt.port, got, want)
		}
	}
}

func TestBridgePortName(t *testing.T) {
	if got := BridgePortName("Unit 2", 9101); got != "Bridge - Unit 2 (9101)" {
		t.Fatalf("BridgePortName('Unit 2', 9101) = %q", got)
	}
	if got := BridgePortName("Lab/PC:1", 0); got != "Bridge - Lab_PC_1 (9101)" {
		t.Fatalf("BridgePortName('Lab/PC:1', 0) = %q", got)
	}
}

func TestDeregisterFromAllPeersSendsDelete(t *testing.T) {
	var receivedMethod string
	var receivedToken string
	var receivedBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/peers" {
			receivedMethod = r.Method
			receivedToken = r.Header.Get("X-Bridge-Token")
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"removed","removed":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Device-2"
	cfg.AuthToken = "test-secret-xyz"
	cfg.ListenPort = 8765
	cfg.Peers = []config.PeerDevice{
		{
			Name:      "Device-1",
			IP:        "127.0.0.1",
			URL:       srv.URL,
			SpoolPort: 9101,
		},
	}

	deregisterFromAllPeers(cfg, "100.64.0.22", srv.Client())

	if receivedMethod != http.MethodDelete {
		t.Fatalf("expected DELETE /api/peers, got %q", receivedMethod)
	}
	if receivedToken != "test-secret-xyz" {
		t.Fatalf("expected X-Bridge-Token 'test-secret-xyz', got %q", receivedToken)
	}
	if receivedBody["name"] != "Device-2" || receivedBody["ip"] != "100.64.0.22" || receivedBody["url"] != "http://100.64.0.22:8765" {
		t.Fatalf("unexpected DELETE /api/peers body: %+v", receivedBody)
	}
}

func TestDiscoverTailscalePeersFiltersNonBridgeAndIgnored(t *testing.T) {
	// 1. Healthy Bridge node
	healthySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(server.HealthResponse{
			Status:         "healthy",
			NodeName:       "Real-Bridge-Node",
			PrinterReady:   true,
			DefaultPrinter: "HP LaserJet",
		})
	}))
	defer healthySrv.Close()

	// 2. Non-Bridge HTTP 200 server (e.g. generic web app returning 200 OK without Bridge HealthResponse)
	nonBridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>ok</html>`))
	}))
	defer nonBridgeSrv.Close()

	// 3. HTTP 200 JSON missing status=healthy
	badStatusSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":    "degraded",
			"node_name": "FakeNode",
		})
	}))
	defer badStatusSrv.Close()

	statusJSON := []byte(`{
		"Peer": {
			"k1": {"HostName": "real-pc", "TailscaleIPs": ["100.64.0.10"], "Online": false, "OS": "windows"},
			"k2": {"HostName": "web-pc", "TailscaleIPs": ["100.64.0.20"], "Online": true, "OS": "linux"},
			"k3": {"HostName": "uninstalled-pc", "TailscaleIPs": ["100.64.0.30"], "Online": true, "OS": "windows"},
			"k4": {"HostName": "bad-status-pc", "TailscaleIPs": ["100.64.0.40"], "Online": true, "OS": "windows"}
		}
	}`)

	peers, err := discoverTailscalePeersFromJSON(statusJSON, func(ip string) probeHealthInfo {
		switch ip {
		case "100.64.0.10":
			return probeBridgeHealthURL(healthySrv.URL + "/api/health")
		case "100.64.0.20":
			return probeBridgeHealthURL(nonBridgeSrv.URL + "/api/health")
		case "100.64.0.40":
			return probeBridgeHealthURL(badStatusSrv.URL + "/api/health")
		default:
			return probeHealthInfo{}
		}
	})
	if err != nil {
		t.Fatalf("discoverTailscalePeersFromJSON failed: %v", err)
	}
	if len(peers) != 4 {
		t.Fatalf("expected 4 tailscale peer entries, got %d", len(peers))
	}

	byIP := make(map[string]DiscoveredPeer)
	for _, p := range peers {
		byIP[p.IP] = p
	}
	if !byIP["100.64.0.10"].BridgeActive || !byIP["100.64.0.10"].Online || byIP["100.64.0.10"].BridgeName != "Real-Bridge-Node" || !byIP["100.64.0.10"].PrinterReady || byIP["100.64.0.10"].PrinterName != "HP LaserJet" {
		t.Fatalf("expected 100.64.0.10 to be BridgeActive & Online with printer info, got %+v", byIP["100.64.0.10"])
	}
	for _, nonBridgeIP := range []string{"100.64.0.20", "100.64.0.30", "100.64.0.40"} {
		dp := byIP[nonBridgeIP]
		if dp.BridgeActive || dp.Online {
			t.Fatalf("expected %s (non-Bridge/uninstalled despite Tailscale Online=true) to have BridgeActive=false and Online=false, got %+v", nonBridgeIP, dp)
		}
	}

	// Verify collectUnlinkedDiscoveryCandidates & PromptNewDiscoveries filter out IgnoredPeers and non-Bridge HTTP 200
	origDiscover := discoverPeersFunc
	defer func() { discoverPeersFunc = origDiscover }()

	ignoredSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(server.HealthResponse{
			Status:   "healthy",
			NodeName: "Ignored-Node",
		})
	}))
	defer ignoredSrv.Close()

	_, igPortStr, _ := net.SplitHostPort(strings.TrimPrefix(ignoredSrv.URL, "http://"))
	_ = igPortStr

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit-1"
	_ = cfg.AddIgnoredPeer("Ignored-Node", "100.64.0.50", "http://100.64.0.50:8765")

	discoverPeersFunc = func() ([]DiscoveredPeer, error) {
		return []DiscoveredPeer{
			{HostName: "ignored", IP: "100.64.0.50", Online: true, BridgeActive: true, BridgeName: "Ignored-Node"},
			{HostName: "web-pc", IP: "100.64.0.20", Online: false, BridgeActive: false},
		}, nil
	}

	cands := collectUnlinkedDiscoveryCandidates(cfg, "100.64.0.1", false)
	if len(cands) != 0 {
		t.Fatalf("expected 0 candidates when peers are ignored or non-Bridge, got %+v", cands)
	}
}

func TestConfirmPeerNameCollisionCheck(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeName = "U1"
	cfg.Peers = []config.PeerDevice{
		{Name: "Office-PC", IP: "100.64.0.2", URL: "http://100.64.0.2:8765", SpoolPort: 9101},
	}

	// Attempting to name a new peer (100.64.0.3) the same as existing peer 'Office-PC' must be rejected
	name, isCustom := confirmPeerNameWithConfig(bufio.NewReader(strings.NewReader("Office-PC\n")), cfg, "100.64.0.3", "http://100.64.0.3:8765", "New-PC", "U1")
	if name != "New-PC" || isCustom {
		t.Fatalf("expected colliding peer name to be rejected and keep 'New-PC', got (%q, %v)", name, isCustom)
	}
}

func TestPromptNewDiscoveriesIgnoreAndSkip(t *testing.T) {
	// Serve a healthy Bridge node on a loopback httptest server; we test collectUnlinkedDiscoveryCandidates
	// and PromptNewDiscoveries with IgnoredPeers persistence.
	origDiscover := discoverPeersFunc
	defer func() { discoverPeersFunc = origDiscover }()

	// Because collectUnlinkedDiscoveryCandidates probes http://<ip>:8765/api/health, we spin up a listener
	// or test PromptNewDiscoveries when no new devices exist / verify IgnoredPeers filtering.
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := config.DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-1"
	if err := cfg.AddIgnoredPeer("Old-Device", "100.64.0.88", "http://100.64.0.88:8765"); err != nil {
		t.Fatalf("AddIgnoredPeer failed: %v", err)
	}

	discoverPeersFunc = func() ([]DiscoveredPeer, error) {
		return []DiscoveredPeer{
			{HostName: "old-dev", IP: "100.64.0.88", Online: true, BridgeActive: true, BridgeName: "Old-Device"},
		}, nil
	}

	if err := PromptNewDiscoveries(cfg, bufio.NewReader(strings.NewReader("s\n"))); err != nil {
		t.Fatalf("PromptNewDiscoveries failed: %v", err)
	}
	if len(cfg.GetPeers()) != 0 {
		t.Fatalf("expected ignored peer to not be connected, got %+v", cfg.GetPeers())
	}
}

func TestUninstallServicePreDeleteDeregisterWithTempConfig(t *testing.T) {
	var deleted bool
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/peers" && r.Method == http.MethodDelete {
			deleted = true
			gotMethod = r.Method
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := config.DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit-2"
	cfg.Peers = []config.PeerDevice{
		{Name: "Unit-1", IP: "127.0.0.1", URL: srv.URL, SpoolPort: 9101},
	}
	if err := config.SaveConfig(cfgPath, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	deregisterFromAllPeers(loaded, "100.64.0.2", srv.Client())
	if !deleted || gotMethod != http.MethodDelete {
		t.Fatalf("expected DELETE /api/peers to be sent to configured peer")
	}
}

func TestProbeBridgeHealthCapturesPrinterInfo(t *testing.T) {
	srvWithPrinter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Bridge-Probe") != "1" {
			t.Errorf("expected X-Bridge-Probe=1, got %q", r.Header.Get("X-Bridge-Probe"))
		}
		_ = json.NewEncoder(w).Encode(server.HealthResponse{
			Status:         "healthy",
			NodeName:       "Print-Server-1",
			PeerName:       "Laptop-1",
			PrinterReady:   true,
			DefaultPrinter: "  HP LaserJet Pro M404  ",
		})
	}))
	defer srvWithPrinter.Close()

	srvNoPrinter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(server.HealthResponse{
			Status:         "healthy",
			NodeName:       "Client-Only-PC",
			PrinterReady:   false,
			DefaultPrinter: "",
		})
	}))
	defer srvNoPrinter.Close()

	statusJSON := []byte(`{
		"Peer": {
			"k1": {"HostName": "print-host", "TailscaleIPs": ["100.64.1.10"], "Online": true, "OS": "windows"},
			"k2": {"HostName": "client-host", "TailscaleIPs": ["100.64.1.20"], "Online": true, "OS": "windows"}
		}
	}`)

	peers, err := discoverTailscalePeersFromJSON(statusJSON, func(ip string) probeHealthInfo {
		switch ip {
		case "100.64.1.10":
			return probeBridgeHealthURL(srvWithPrinter.URL + "/api/health")
		case "100.64.1.20":
			return probeBridgeHealthURL(srvNoPrinter.URL + "/api/health")
		default:
			return probeHealthInfo{}
		}
	})
	if err != nil {
		t.Fatalf("discoverTailscalePeersFromJSON failed: %v", err)
	}

	byIP := make(map[string]DiscoveredPeer)
	for _, p := range peers {
		byIP[p.IP] = p
	}

	p1 := byIP["100.64.1.10"]
	if !p1.BridgeActive || !p1.Online || p1.BridgeName != "Print-Server-1" || p1.MyName != "Laptop-1" || !p1.PrinterReady || p1.PrinterName != "HP LaserJet Pro M404" {
		t.Fatalf("unexpected DiscoveredPeer for printer-ready peer: %+v", p1)
	}

	p2 := byIP["100.64.1.20"]
	if !p2.BridgeActive || !p2.Online || p2.BridgeName != "Client-Only-PC" || p2.PrinterReady || p2.PrinterName != "" {
		t.Fatalf("unexpected DiscoveredPeer for printerless peer: %+v", p2)
	}

	if tag := formatDiscoveredPrinterTag(p1.PrinterReady); tag != "(Printer: Ready)" {
		t.Fatalf("expected (Printer: Ready), got %q", tag)
	}
	if tag := formatDiscoveredPrinterTag(p2.PrinterReady); tag != "(No Printer)" {
		t.Fatalf("expected (No Printer), got %q", tag)
	}
}

func TestDashboardPeerHealthPropagationAndFormatting(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	cfgPeers := []config.PeerDevice{
		{Name: "OfflinePC", IP: "100.64.0.1", URL: "http://100.64.0.1:8765", SpoolPort: 9101},
		{Name: "NoPrinterPC", IP: "100.64.0.2", URL: "http://100.64.0.2:8765", SpoolPort: 9102},
		{Name: "NamedPrinterPC", IP: "100.64.0.3", URL: "http://100.64.0.3:8765", SpoolPort: 9103},
		{Name: "ReadyUnnamedPC", IP: "100.64.0.4", URL: "http://100.64.0.4:8765", SpoolPort: 9104},
		{Name: "LegacyPC", IP: "100.64.0.5", URL: "http://100.64.0.5:8765", SpoolPort: 9105},
		{Name: "LongPrinterPC", IP: "100.64.0.6", URL: "http://100.64.0.6:8765", SpoolPort: 9106},
	}

	healthPeers := []config.PeerDevice{
		{Name: "OfflinePC", IP: "100.64.0.1", URL: "http://100.64.0.1:8765", SpoolPort: 9101, Online: false, PrinterReady: boolPtr(false)},
		{Name: "NoPrinterPC", IP: "100.64.0.2", URL: "http://100.64.0.2:8765", SpoolPort: 9102, Online: true, PrinterReady: boolPtr(false)},
		{Name: "NamedPrinterPC", IP: "100.64.0.3", URL: "http://100.64.0.3:8765", SpoolPort: 9103, Online: true, PrinterReady: boolPtr(true), PrinterName: "Canon LBP2900"},
		{Name: "ReadyUnnamedPC", IP: "100.64.0.4", URL: "http://100.64.0.4:8765", SpoolPort: 9104, Online: true, PrinterReady: boolPtr(true), PrinterName: ""},
		{Name: "LegacyPC", IP: "100.64.0.5", URL: "http://100.64.0.5:8765", SpoolPort: 9105, Online: true, PrinterReady: nil, PrinterName: ""},
		{Name: "LongPrinterPC", IP: "100.64.0.6", URL: "http://100.64.0.6:8765", SpoolPort: 9106, Online: true, PrinterReady: boolPtr(true), PrinterName: "HP OfficeJet Pro 9015e All-in-One Series"},
	}

	merged, onlineCount := applyPeerHealthStatus(append([]config.PeerDevice(nil), cfgPeers...), healthPeers, false)
	if onlineCount != 5 {
		t.Fatalf("expected 5 online peers, got %d", onlineCount)
	}

	wantStatuses := []string{
		"[OFFLINE]",
		"[ONLINE | No Printer]",
		"[ONLINE | Printer: Canon LBP2900]",
		"[ONLINE | Printer Ready]",
		"[ONLINE]",
		"[ONLINE | Printer: HP OfficeJet Pro 9015e Al...]",
	}
	for i, want := range wantStatuses {
		got := formatDashboardPeerStatus(merged[i])
		if got != want {
			t.Errorf("peer %d (%s) status = %q, want %q", i, merged[i].Name, got, want)
		}
	}

	// Verify stopped service clears Online and printer state
	stoppedPeers, stoppedOnline := applyPeerHealthStatus(append([]config.PeerDevice(nil), cfgPeers...), healthPeers, true)
	if stoppedOnline != 0 {
		t.Fatalf("expected 0 online peers when service stopped, got %d", stoppedOnline)
	}
	for _, p := range stoppedPeers {
		if p.Online || p.PrinterReady != nil || p.PrinterName != "" {
			t.Fatalf("expected cleared state when service stopped, got %+v", p)
		}
		if got := formatDashboardPeerStatus(p); got != "[OFFLINE]" {
			t.Fatalf("expected [OFFLINE] when service stopped, got %q", got)
		}
	}
}

func TestPrinterlessSetupAndDiscoveryHandling(t *testing.T) {
	origPrinterFn := getTargetPhysicalPrinterFunc
	origDiscoverFn := discoverPeersFunc
	defer func() {
		getTargetPhysicalPrinterFunc = origPrinterFn
		discoverPeersFunc = origDiscoverFn
	}()

	// 1. Local printer error produces friendly note without failing
	getTargetPhysicalPrinterFunc = func() (string, error) {
		return "", errors.New("no physical printer connected")
	}
	var buf bytes.Buffer
	reportLocalPrinterStatus(&buf)
	out := buf.String()
	if !strings.Contains(out, "None detected") || !strings.Contains(out, "sync files and send prints") {
		t.Fatalf("expected friendly printerless note, got: %q", out)
	}

	// 2. Local physical printer present reports Connected & Ready
	getTargetPhysicalPrinterFunc = func() (string, error) {
		return "Brother HL-L2350DW", nil
	}
	buf.Reset()
	reportLocalPrinterStatus(&buf)
	if !strings.Contains(buf.String(), "Brother HL-L2350DW (Connected & Ready)") {
		t.Fatalf("expected Connected & Ready message, got: %q", buf.String())
	}

	// 3. PromptNewDiscoveries succeeds cleanly on a printerless machine
	getTargetPhysicalPrinterFunc = func() (string, error) {
		return "", errors.New("no physical printer found")
	}
	discoverPeersFunc = func() ([]DiscoveredPeer, error) {
		return nil, nil
	}

	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.ConfigPath = filepath.Join(tmpDir, "config.json")
	cfg.NodeName = "Client-Laptop"

	if err := PromptNewDiscoveries(cfg, bufio.NewReader(strings.NewReader("s\n"))); err != nil {
		t.Fatalf("PromptNewDiscoveries failed on printerless device: %v", err)
	}
}
