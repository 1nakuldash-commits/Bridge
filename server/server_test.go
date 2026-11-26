package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"bridge/config"
	"bridge/printer"
	bridgesync "bridge/sync"
)

func TestSetSyncEnginePreservesNonLoopbackLocalURL(t *testing.T) {
	tmp := t.TempDir()
	sm, err := bridgesync.NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.SharedDir = filepath.Join(tmp, "shared")

	srv := NewServer(cfg, sm, nil)
	se := bridgesync.NewSyncEngine(cfg.SharedDir, "", cfg.AuthToken, sm)
	wantURL := "http://100.64.1.10:8765"
	se.SetLocalURL(wantURL)

	srv.SetSyncEngine(se)

	if got := se.GetLocalURL(); got != wantURL {
		t.Fatalf("SetSyncEngine overwrote localURL: got %q, want %q", got, wantURL)
	}
}

func TestCheckAutoPairSkipsEmptyIPAndURL(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"

	srv := NewServer(cfg, nil, nil)

	// Simulate SendDirectPrint arriving locally without X-Bridge-Peer-URL.
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Node-Name", "Unit2")

	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	if peers := cfg.GetPeers(); len(peers) != 0 {
		t.Fatalf("expected no peer added when both IP and URL are unavailable, got %+v", peers)
	}
}

func TestCheckAutoPairFallsBackToNonLoopbackRemoteIP(t *testing.T) {
	cases := []struct {
		name       string
		peerHeader string
		remoteAddr string
		nodeName   string
		wantIP     string
		wantURL    string
	}{
		{
			name:       "empty peer header uses remote non-loopback IP",
			peerHeader: "",
			remoteAddr: "100.64.1.25:51234",
			nodeName:   "Unit2",
			wantIP:     "100.64.1.25",
			wantURL:    "http://100.64.1.25:8765",
		},
		{
			name:       "loopback peer header falls back to remote non-loopback IP",
			peerHeader: "http://127.0.0.1:8765",
			remoteAddr: "100.64.1.26:51235",
			nodeName:   "Unit3",
			wantIP:     "100.64.1.26",
			wantURL:    "http://100.64.1.26:8765",
		},
		{
			name:       "localhost peer header falls back to remote non-loopback IP",
			peerHeader: "http://localhost:9000",
			remoteAddr: "100.64.1.27:51236",
			nodeName:   "Unit4",
			wantIP:     "100.64.1.27",
			wantURL:    "http://100.64.1.27:9000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.NodeName = "Unit1"
			cfg.ListenPort = 8765
			cfg.AuthToken = "test-token"
			srv := NewServer(cfg, nil, nil)

			req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			req.RemoteAddr = tc.remoteAddr
			req.Header.Set("X-Bridge-Token", cfg.AuthToken)
			req.Header.Set("X-Bridge-Node-Name", tc.nodeName)
			if tc.peerHeader != "" {
				req.Header.Set("X-Bridge-Peer-URL", tc.peerHeader)
			}

			rec := httptest.NewRecorder()
			srv.handleHealth(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d", rec.Code)
			}

			p := cfg.FindPeer(tc.nodeName)
			if p == nil {
				t.Fatalf("expected peer %q to be registered", tc.nodeName)
			}
			if p.IP != tc.wantIP || p.URL != tc.wantURL {
				t.Fatalf("peer mismatch: got IP=%q URL=%q, want IP=%q URL=%q", p.IP, p.URL, tc.wantIP, tc.wantURL)
			}
		})
	}
}

func TestHandleSyncDeleteLoopbackSenderFallback(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	sm, err := bridgesync.NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	var mu sync.Mutex
	var relayedToOther int
	otherPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			relayedToOther++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer otherPeer.Close()

	var senderGotRelay int
	senderPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			senderGotRelay++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer senderPeer.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.SharedDir = sharedDir

	se := bridgesync.NewSyncEngine(sharedDir, "", cfg.AuthToken, sm)
	se.UpdatePeerURLs([]string{"http://100.64.1.50:8765", otherPeer.URL})
	srv := NewServer(cfg, sm, nil)
	srv.SetSyncEngine(se)

	// Incoming DELETE claims loopback X-Bridge-Peer-URL, but r.RemoteAddr is 100.64.1.50.
	req := httptest.NewRequest(http.MethodDelete, "/api/sync?path=doc.txt", nil)
	req.RemoteAddr = "100.64.1.50:49152"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Peer-URL", "http://127.0.0.1:8765")
	req.Header.Set("X-Bridge-ModTime", "1000")

	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.handleSync)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := relayedToOther
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	se.Stop()

	mu.Lock()
	defer mu.Unlock()
	if relayedToOther == 0 {
		t.Fatalf("expected deletion to be relayed to non-sender peer")
	}
	if senderGotRelay != 0 {
		t.Fatalf("expected sender peer to be excluded, got %d relays", senderGotRelay)
	}
}

func TestHandlePrintAcceptsGeometryHeaders(t *testing.T) {
	tmp := t.TempDir()
	queueDir := filepath.Join(tmp, "queue")
	failedDir := filepath.Join(tmp, "failed")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("MkdirAll queueDir: %v", err)
	}
	ps := printer.NewPrintService("", 5, queueDir, failedDir)
	ps.SetTargetPrinter("TestPhysicalPrinter")

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"

	srv := NewServer(cfg, nil, ps)

	req := httptest.NewRequest(http.MethodPost, "/api/print", bytes.NewReader([]byte("%PDF-1.4\ntest")))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Node-Name", "Unit2")
	req.Header.Set("X-Bridge-Origin", "Unit2")
	req.Header.Set("X-Bridge-FileName", "invoice.pdf")
	req.Header.Set("X-Bridge-Paper-Size", "A4")
	req.Header.Set("X-Bridge-Orientation", "portrait")
	req.Header.Set("X-Bridge-Page-Count", "3")

	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrint)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if ps.QueueDepth() != 1 {
		t.Fatalf("expected queue depth 1, got %d", ps.QueueDepth())
	}
}

func TestHandlePeersTriggersPeerUpdaterOnNameOrMetadataChange(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"

	type updateEvent struct {
		peer  config.PeerDevice
		isNew bool
	}
	var mu sync.Mutex
	var events []updateEvent

	srv := NewServer(cfg, nil, nil)
	srv.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		mu.Lock()
		events = append(events, updateEvent{peer: peer, isNew: isNew})
		mu.Unlock()
	})

	postPeer := func(payload config.PeerDevice) {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/peers", bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:50001"
		req.Header.Set("X-Bridge-Token", cfg.AuthToken)
		rec := httptest.NewRecorder()
		srv.authMiddleware(srv.handlePeers)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /api/peers failed: %d %s", rec.Code, rec.Body.String())
		}
	}

	// 1. Brand new peer with placeholder IP name.
	postPeer(config.PeerDevice{Name: "Device (100.64.1.2)", IP: "100.64.1.2", URL: "http://100.64.1.2:8765"})
	// 2. Identical repeat POST must NOT trigger peerUpdater again.
	postPeer(config.PeerDevice{Name: "Device (100.64.1.2)", IP: "100.64.1.2", URL: "http://100.64.1.2:8765"})
	// 3. Name changes to friendly name "U2" -> MUST trigger peerUpdater(dev, false).
	postPeer(config.PeerDevice{Name: "U2", IP: "100.64.1.2", URL: "http://100.64.1.2:8765"})
	// 4. Peer IP/URL changes while Name stays "U2" -> MUST trigger peerUpdater(dev, false).
	postPeer(config.PeerDevice{Name: "U2", IP: "100.64.1.99", URL: "http://100.64.1.99:8765"})

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("expected 3 peerUpdater calls (new, rename, URL change), got %d: %+v", len(events), events)
	}
	if !events[0].isNew || events[0].peer.Name != "Device (100.64.1.2)" {
		t.Fatalf("unexpected event 0: %+v", events[0])
	}
	if events[1].isNew || events[1].peer.Name != "U2" {
		t.Fatalf("expected event 1 to be rename to U2 (isNew=false), got %+v", events[1])
	}
	if events[2].isNew || events[2].peer.IP != "100.64.1.99" || events[2].peer.URL != "http://100.64.1.99:8765" {
		t.Fatalf("expected event 2 to be IP/URL update (isNew=false), got %+v", events[2])
	}
}

func TestCheckAutoPairTriggersPeerUpdaterOnNameChange(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("Device (100.64.1.5)", "100.64.1.5", "http://100.64.1.5:8765")

	var mu sync.Mutex
	var calls []config.PeerDevice
	var news []bool

	srv := NewServer(cfg, nil, nil)
	srv.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		mu.Lock()
		calls = append(calls, peer)
		news = append(news, isNew)
		mu.Unlock()
	})

	// Request reveals real X-Bridge-Node-Name "U5".
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "100.64.1.5:51000"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Node-Name", "U5")
	req.Header.Set("X-Bridge-Peer-URL", "http://100.64.1.5:8765")
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	// Repeat identical request must NOT trigger peerUpdater again.
	rec2 := httptest.NewRecorder()
	srv.handleHealth(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec2.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 peerUpdater call on auto-pair name change, got %d (%+v)", len(calls), calls)
	}
	if news[0] || calls[0].Name != "U5" {
		t.Fatalf("expected isNew=false and Name=U5, got isNew=%v peer=%+v", news[0], calls[0])
	}
}

func TestHandlePrintLocalRejectsUnknownTargetWithoutFallback(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "sample.pdf")
	if err := os.WriteFile(docPath, []byte("%PDF-1.4\n%%EOF"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var mu sync.Mutex
	var primaryHits int
	primarySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/print" {
			mu.Lock()
			primaryHits++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"queued"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer primarySrv.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("U2", "127.0.0.1", primarySrv.URL)

	srv := NewServer(cfg, nil, nil)

	// 1. Explicit unknown target MUST fail with 404 and NOT fall back to Peers[0] (U2).
	req := httptest.NewRequest(http.MethodPost, "/api/print-local", nil)
	req.RemoteAddr = "127.0.0.1:50001"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-FilePath", docPath)
	req.Header.Set("X-Bridge-Target", "NonExistentPeer")
	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrintLocal)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for unknown X-Bridge-Target, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "target peer not found") {
		t.Fatalf("expected error body to contain 'target peer not found', got %q", rec.Body.String())
	}
	mu.Lock()
	if primaryHits != 0 {
		mu.Unlock()
		t.Fatalf("unknown target misrouted print job to primary peer (%d hits)", primaryHits)
	}
	mu.Unlock()

	// 2. Empty target uses primary peer U2 cleanly.
	req2 := httptest.NewRequest(http.MethodPost, "/api/print-local", nil)
	req2.RemoteAddr = "127.0.0.1:50002"
	req2.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req2.Header.Set("X-Bridge-FilePath", docPath)
	rec2 := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrintLocal)(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK when X-Bridge-Target is empty, got %d: %s", rec2.Code, rec2.Body.String())
	}
	mu.Lock()
	if primaryHits != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 primary peer print hit, got %d", primaryHits)
	}
	mu.Unlock()
}

func TestCustomPeerNamePreservedAcrossCheckAutoPairAndRemotePeersPost(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	cfg := config.DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("Detected-U2", "100.64.1.10", "http://100.64.1.10:8765")

	var mu sync.Mutex
	var events []config.PeerDevice

	srv := NewServer(cfg, nil, nil)
	srv.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		mu.Lock()
		events = append(events, peer)
		mu.Unlock()
	})

	// 1. Local Dashboard sync posts CustomName=true -> updates name, persists, and triggers peerUpdater
	localBody, _ := json.Marshal(config.PeerDevice{
		Name:       "Office-U2",
		CustomName: true,
		IP:         "100.64.1.10",
		URL:        "http://100.64.1.10:8765",
	})
	reqLocal := httptest.NewRequest(http.MethodPost, "/api/peers", bytes.NewReader(localBody))
	reqLocal.RemoteAddr = "127.0.0.1:50001"
	reqLocal.Header.Set("X-Bridge-Token", cfg.AuthToken)
	recLocal := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(recLocal, reqLocal)
	if recLocal.Code != http.StatusOK {
		t.Fatalf("local POST /api/peers failed: %d %s", recLocal.Code, recLocal.Body.String())
	}

	var localResp config.PeerDevice
	if err := json.NewDecoder(recLocal.Body).Decode(&localResp); err != nil {
		t.Fatalf("decode local POST response: %v", err)
	}
	if localResp.Name != "Office-U2" || !localResp.CustomName {
		t.Fatalf("expected custom peer 'Office-U2' with CustomName=true, got %+v", localResp)
	}

	// 2. Remote checkAutoPair via /api/health with remote NodeName must NOT revert custom name
	reqHealth := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	reqHealth.RemoteAddr = "100.64.1.10:51234"
	reqHealth.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqHealth.Header.Set("X-Bridge-Node-Name", "Detected-U2")
	reqHealth.Header.Set("X-Bridge-Peer-URL", "http://100.64.1.10:8765")
	recHealth := httptest.NewRecorder()
	srv.handleHealth(recHealth, reqHealth)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("GET /api/health failed: %d", recHealth.Code)
	}

	// 3. Remote IntroduceToRemote POST /api/peers (CustomName=false) must NOT revert custom name
	remoteBody, _ := json.Marshal(config.PeerDevice{
		Name:       "Detected-U2",
		CustomName: false,
		IP:         "100.64.1.10",
		URL:        "http://100.64.1.10:8765",
	})
	reqRemote := httptest.NewRequest(http.MethodPost, "/api/peers", bytes.NewReader(remoteBody))
	reqRemote.RemoteAddr = "100.64.1.10:51235"
	reqRemote.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqRemote.Header.Set("X-Bridge-Node-Name", "Detected-U2")
	reqRemote.Header.Set("X-Bridge-Peer-URL", "http://100.64.1.10:8765")
	recRemote := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(recRemote, reqRemote)
	if recRemote.Code != http.StatusOK {
		t.Fatalf("remote POST /api/peers failed: %d %s", recRemote.Code, recRemote.Body.String())
	}

	var remoteResp config.PeerDevice
	if err := json.NewDecoder(recRemote.Body).Decode(&remoteResp); err != nil {
		t.Fatalf("decode remote POST response: %v", err)
	}
	if remoteResp.Name != "Office-U2" || !remoteResp.CustomName {
		t.Fatalf("expected remote POST response to preserve 'Office-U2', got %+v", remoteResp)
	}

	p := cfg.FindPeer("http://100.64.1.10:8765")
	if p == nil || p.Name != "Office-U2" || !p.CustomName {
		t.Fatalf("expected config peer to preserve 'Office-U2' (CustomName=true), got %+v", p)
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(loaded.Peers) != 1 || loaded.Peers[0].Name != "Office-U2" || !loaded.Peers[0].CustomName {
		t.Fatalf("expected persisted peer 'Office-U2' (CustomName=true), got %+v", loaded.Peers)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Name != "Office-U2" || !events[0].CustomName {
		t.Fatalf("expected exactly 1 peerUpdater call for custom rename, got %+v", events)
	}
}


func TestHandleHealthProbeClearsOnlineFalse(t *testing.T) {
	healthyPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Status:   "healthy",
			NodeName: "Healthy-Peer",
		})
	}))
	defer healthyPeer.Close()

	errorPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer errorPeer.Close()

	nonBridge200Peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hello":"not-bridge"}`))
	}))
	defer nonBridge200Peer.Close()

	unreachablePeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	unreachableURL := unreachablePeer.URL
	unreachablePeer.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("Healthy-Peer", "127.0.0.1", healthyPeer.URL)
	cfg.AddOrUpdatePeer("Error-Peer", "127.0.0.1", errorPeer.URL)
	cfg.AddOrUpdatePeer("NonBridge-Peer", "127.0.0.1", nonBridge200Peer.URL)
	cfg.AddOrUpdatePeer("Dead-Peer", "127.0.0.1", unreachableURL)

	// Confirm all peers start marked Online=true.
	for _, p := range cfg.GetPeers() {
		if !p.Online {
			t.Fatalf("expected peer %s to start Online=true", p.Name)
		}
	}

	srv := NewServer(cfg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/health?probe=sync", nil)
	req.RemoteAddr = "127.0.0.1:50010"
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var hr HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&hr); err != nil {
		t.Fatalf("decode HealthResponse: %v", err)
	}
	if hr.OnlineCount != 1 || !hr.PeerReachable {
		t.Fatalf("expected OnlineCount=1 PeerReachable=true, got OnlineCount=%d PeerReachable=%v", hr.OnlineCount, hr.PeerReachable)
	}

	assertPeerOnline := func(peers []config.PeerDevice, source string) {
		t.Helper()
		byName := make(map[string]bool, len(peers))
		for _, p := range peers {
			byName[p.Name] = p.Online
		}
		if !byName["Healthy-Peer"] {
			t.Fatalf("%s: expected Healthy-Peer Online=true", source)
		}
		for _, offlineName := range []string{"Error-Peer", "NonBridge-Peer", "Dead-Peer"} {
			if byName[offlineName] {
				t.Fatalf("%s: expected %s Online=false, got true", source, offlineName)
			}
		}
	}

	assertPeerOnline(hr.Peers, "HealthResponse")
	assertPeerOnline(cfg.GetPeers(), "cfg.GetPeers")
}

func TestHandlePeersDelete(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	cfg := config.DefaultConfig()
	cfg.ConfigPath = cfgPath
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("Unit2", "100.64.1.2", "http://100.64.1.2:8765")
	cfg.AddOrUpdatePeer("Unit3", "100.64.1.3", "http://100.64.1.3:8765")

	var mu sync.Mutex
	var removed []config.PeerDevice
	srv := NewServer(cfg, nil, nil)
	srv.SetPeerRemover(func(peer config.PeerDevice) {
		mu.Lock()
		removed = append(removed, peer)
		mu.Unlock()
	})

	// 1. Unauthenticated DELETE must return 401.
	unauthReq := httptest.NewRequest(http.MethodDelete, "/api/peers?name=Unit2", nil)
	unauthRec := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", unauthRec.Code)
	}

	// 2. Delete Unit2 via JSON body (with X-Bridge-Node-Name set so we also verify authMiddleware doesn't re-pair).
	body, _ := json.Marshal(map[string]string{
		"url":  "http://100.64.1.2:8765",
		"ip":   "100.64.1.2",
		"name": "Unit2",
	})
	req1 := httptest.NewRequest(http.MethodDelete, "/api/peers", bytes.NewReader(body))
	req1.RemoteAddr = "100.64.1.2:55000"
	req1.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req1.Header.Set("X-Bridge-Node-Name", "Unit2")
	req1.Header.Set("X-Bridge-Peer-URL", "http://100.64.1.2:8765")
	rec1 := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("DELETE /api/peers failed: %d %s", rec1.Code, rec1.Body.String())
	}

	var resp1 struct {
		Status  string              `json:"status"`
		Removed bool                `json:"removed"`
		Peers   []config.PeerDevice `json:"peers"`
	}
	if err := json.NewDecoder(rec1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decode DELETE response: %v", err)
	}
	if resp1.Status != "removed" || !resp1.Removed || len(resp1.Peers) != 1 || resp1.Peers[0].Name != "Unit3" {
		t.Fatalf("unexpected DELETE response: %+v", resp1)
	}

	// 3. Delete Unit3 via query parameter.
	req2 := httptest.NewRequest(http.MethodDelete, "/api/peers?ip=100.64.1.3", nil)
	req2.RemoteAddr = "127.0.0.1:55001"
	req2.Header.Set("X-Bridge-Token", cfg.AuthToken)
	rec2 := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("DELETE /api/peers by query failed: %d %s", rec2.Code, rec2.Body.String())
	}
	var resp2 struct {
		Status  string              `json:"status"`
		Removed bool                `json:"removed"`
		Peers   []config.PeerDevice `json:"peers"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decode DELETE query response: %v", err)
	}
	if resp2.Status != "removed" || !resp2.Removed || len(resp2.Peers) != 0 {
		t.Fatalf("unexpected DELETE query response: %+v", resp2)
	}

	// 4. Delete non-existent peer returns removed=false and does not fire callback.
	req3 := httptest.NewRequest(http.MethodDelete, "/api/peers?name=Ghost", nil)
	req3.RemoteAddr = "127.0.0.1:55002"
	req3.Header.Set("X-Bridge-Token", cfg.AuthToken)
	rec3 := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for non-existent delete, got %d", rec3.Code)
	}
	var resp3 struct {
		Status  string              `json:"status"`
		Removed bool                `json:"removed"`
		Peers   []config.PeerDevice `json:"peers"`
	}
	if err := json.NewDecoder(rec3.Body).Decode(&resp3); err != nil {
		t.Fatalf("decode missing DELETE response: %v", err)
	}
	if resp3.Removed {
		t.Fatalf("expected removed=false for missing peer, got %+v", resp3)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(removed) != 2 || removed[0].Name != "Unit2" || removed[1].Name != "Unit3" {
		t.Fatalf("unexpected peerRemover events: %+v", removed)
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(loaded.Peers) != 0 || loaded.PeerName != "" || loaded.PeerURL != "" {
		t.Fatalf("expected persisted config to have no peers and cleared legacy fields, got peers=%+v peerName=%q peerURL=%q", loaded.Peers, loaded.PeerName, loaded.PeerURL)
	}
}

func TestHandlePeersPostValidationAndMigration(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("Unit2", "100.64.1.2", "http://100.64.1.2:8765")

	srv := NewServer(cfg, nil, nil)

	// 1. Invalid URL rejected with 400.
	badBody, _ := json.Marshal(config.PeerDevice{Name: "Bad", IP: "100.64.1.9", URL: "ftp://100.64.1.9:8765"})
	reqBad := httptest.NewRequest(http.MethodPost, "/api/peers", bytes.NewReader(badBody))
	reqBad.RemoteAddr = "127.0.0.1:50001"
	reqBad.Header.Set("X-Bridge-Token", cfg.AuthToken)
	recBad := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid URL, got %d: %s", recBad.Code, recBad.Body.String())
	}

	// 2. Oversized body (> 64KB) rejected with 413.
	bigPayload := `{"name":"` + strings.Repeat("a", 70*1024) + `"}`
	reqBig := httptest.NewRequest(http.MethodPost, "/api/peers", strings.NewReader(bigPayload))
	reqBig.RemoteAddr = "127.0.0.1:50001"
	reqBig.Header.Set("X-Bridge-Token", cfg.AuthToken)
	recBig := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(recBig, reqBig)
	if recBig.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 Request Entity Too Large, got %d: %s", recBig.Code, recBig.Body.String())
	}

	// 3. Explicit migrate: true from non-loopback updates existing Unit2 address via UpdatePeerAddressByName.
	migratePayload := `{"name":"Unit2","ip":"100.64.1.55","url":"http://100.64.1.55:8765","migrate":true}`
	reqMig := httptest.NewRequest(http.MethodPost, "/api/peers", strings.NewReader(migratePayload))
	reqMig.RemoteAddr = "100.64.1.55:50001"
	reqMig.Header.Set("X-Bridge-Token", cfg.AuthToken)
	recMig := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePeers)(recMig, reqMig)
	if recMig.Code != http.StatusOK {
		t.Fatalf("migrate POST /api/peers failed: %d %s", recMig.Code, recMig.Body.String())
	}
	peers := cfg.GetPeers()
	if len(peers) != 1 || peers[0].Name != "Unit2" || peers[0].IP != "100.64.1.55" || peers[0].URL != "http://100.64.1.55:8765" {
		t.Fatalf("expected Unit2 to migrate cleanly without duplicate peer, got %+v", peers)
	}
}

func TestHandlePrintForwardsPrintOptions(t *testing.T) {
	tmp := t.TempDir()
	queueDir := filepath.Join(tmp, "queue")
	failedDir := filepath.Join(tmp, "failed")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("MkdirAll queueDir: %v", err)
	}
	ps := printer.NewPrintService("", 5, queueDir, failedDir)
	ps.SetTargetPrinter("TestPhysicalPrinter")

	gotJobCh := make(chan printer.PrintJob, 1)
	ps.OnJobStarted = func(job printer.PrintJob) {
		select {
		case gotJobCh <- job:
		default:
		}
	}
	if err := ps.Start(); err != nil {
		t.Fatalf("PrintService.Start: %v", err)
	}
	defer ps.Stop()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"

	srv := NewServer(cfg, nil, ps)

	req := httptest.NewRequest(http.MethodPost, "/api/print", bytes.NewReader([]byte("%PDF-1.4\ntest")))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Node-Name", "Unit2")
	req.Header.Set("X-Bridge-Origin", "Unit2")
	req.Header.Set("X-Bridge-FileName", "brochure.pdf")
	req.Header.Set("X-Bridge-Job-Name", "Q4 Brochure.pdf")
	req.Header.Set("X-Bridge-Paper-Size", "Letter")
	req.Header.Set("X-Bridge-Orientation", "landscape")
	req.Header.Set("X-Bridge-Page-Count", "7")
	req.Header.Set("X-Bridge-Copies", "3")
	req.Header.Set("X-Bridge-Color", "monochrome")

	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrint)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case job := <-gotJobCh:
		if job.Options.PaperSize != "Letter" ||
			job.Options.Orientation != "landscape" ||
			job.Options.PageCount != 7 ||
			job.Options.Copies != 3 ||
			job.Options.Color == nil || *job.Options.Color ||
			job.Options.SourceNode != "Unit2" ||
			job.Options.JobName != "Q4 Brochure.pdf" ||
			job.SourceNode != "Unit2" ||
			job.FileName != "Q4 Brochure.pdf" {
			t.Fatalf("PrintOptions not forwarded accurately: job=%+v", job)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for PrintJob OnJobStarted")
	}
}

func TestHandlePrintPropagatesSourceNodeFallbackAndJobName(t *testing.T) {
	tmp := t.TempDir()
	queueDir := filepath.Join(tmp, "queue")
	failedDir := filepath.Join(tmp, "failed")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("MkdirAll queueDir: %v", err)
	}
	ps := printer.NewPrintService("", 5, queueDir, failedDir)
	ps.SetTargetPrinter("TestPhysicalPrinter")

	gotJobCh := make(chan printer.PrintJob, 2)
	ps.OnJobStarted = func(job printer.PrintJob) {
		gotJobCh <- job
	}
	if err := ps.Start(); err != nil {
		t.Fatalf("PrintService.Start: %v", err)
	}
	defer ps.Stop()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	srv := NewServer(cfg, nil, ps)

	// 1. No X-Bridge-Origin -> falls back to X-Bridge-Node-Name; X-Bridge-Job-Name trimmed.
	req1 := httptest.NewRequest(http.MethodPost, "/api/print", bytes.NewReader([]byte("%PDF-1.4\ntest")))
	req1.RemoteAddr = "127.0.0.1:50000"
	req1.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req1.Header.Set("X-Bridge-Node-Name", "  LaptopNode  ")
	req1.Header.Set("X-Bridge-FileName", "print_1700000000123.pdf")
	req1.Header.Set("X-Bridge-Job-Name", "  Invoice 2026.pdf  ")

	rec1 := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrint)(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec1.Code, rec1.Body.String())
	}

	select {
	case job := <-gotJobCh:
		if job.SourceNode != "LaptopNode" || job.Options.SourceNode != "LaptopNode" {
			t.Fatalf("expected SourceNode=LaptopNode, got job.SourceNode=%q opts.SourceNode=%q", job.SourceNode, job.Options.SourceNode)
		}
		if job.Options.JobName != "Invoice 2026.pdf" || job.FileName != "Invoice 2026.pdf" {
			t.Fatalf("expected JobName/FileName=Invoice 2026.pdf, got FileName=%q opts.JobName=%q", job.FileName, job.Options.JobName)
		}
		if got := job.FriendlyLabel(); got != "Invoice 2026.pdf from LaptopNode" {
			t.Fatalf("unexpected FriendlyLabel: got %q, want %q", got, "Invoice 2026.pdf from LaptopNode")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for job 1")
	}

	// 2. X-Bridge-Origin takes precedence over X-Bridge-Node-Name.
	req2 := httptest.NewRequest(http.MethodPost, "/api/print", bytes.NewReader([]byte("%PDF-1.4\ntest")))
	req2.RemoteAddr = "127.0.0.1:50000"
	req2.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req2.Header.Set("X-Bridge-Node-Name", "RelayNode")
	req2.Header.Set("X-Bridge-Origin", "OriginPC")
	req2.Header.Set("X-Bridge-FileName", "print_1700000000456.pdf")

	rec2 := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrint)(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec2.Code, rec2.Body.String())
	}

	select {
	case job := <-gotJobCh:
		if job.SourceNode != "OriginPC" || job.Options.SourceNode != "OriginPC" {
			t.Fatalf("expected SourceNode=OriginPC, got job.SourceNode=%q opts.SourceNode=%q", job.SourceNode, job.Options.SourceNode)
		}
		if got := job.FriendlyLabel(); got != "Document from OriginPC" {
			t.Fatalf("unexpected FriendlyLabel: got %q, want %q", got, "Document from OriginPC")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for job 2")
	}
}

func TestHandlePrintLocalRejectsNonLoopbackAndNonRegularFile(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "sample.pdf")
	if err := os.WriteFile(docPath, []byte("%PDF-1.4\n%%EOF"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var mu sync.Mutex
	var printHits int
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/print" {
			mu.Lock()
			printHits++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"queued"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer peerSrv.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("U2", "127.0.0.1", peerSrv.URL)

	var dispatched int
	srv := NewServer(cfg, nil, nil)
	srv.SetOnPrintDispatched(func(fileName, peerName string) {
		mu.Lock()
		dispatched++
		mu.Unlock()
	})

	// 1. Non-loopback client MUST be rejected with 403 Forbidden.
	reqRemote := httptest.NewRequest(http.MethodPost, "/api/print-local", nil)
	reqRemote.RemoteAddr = "100.64.1.25:50000"
	reqRemote.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqRemote.Header.Set("X-Bridge-FilePath", docPath)
	recRemote := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrintLocal)(recRemote, reqRemote)
	if recRemote.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-loopback client, got %d: %s", recRemote.Code, recRemote.Body.String())
	}

	// 2. Directory path (non-regular file) from loopback MUST be rejected with 400 Bad Request.
	reqDir := httptest.NewRequest(http.MethodPost, "/api/print-local", nil)
	reqDir.RemoteAddr = "127.0.0.1:50000"
	reqDir.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqDir.Header.Set("X-Bridge-FilePath", tmp)
	recDir := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrintLocal)(recDir, reqDir)
	if recDir.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for directory path, got %d: %s", recDir.Code, recDir.Body.String())
	}

	// 3. IPv6 loopback [::1] with valid regular file succeeds.
	reqIPv6 := httptest.NewRequest(http.MethodPost, "/api/print-local", nil)
	reqIPv6.RemoteAddr = "[::1]:50000"
	reqIPv6.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqIPv6.Header.Set("X-Bridge-FilePath", docPath)
	recIPv6 := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrintLocal)(recIPv6, reqIPv6)
	if recIPv6.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for [::1] loopback client, got %d: %s", recIPv6.Code, recIPv6.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if printHits != 1 || dispatched != 1 {
		t.Fatalf("expected 1 print hit and 1 dispatch callback, got printHits=%d dispatched=%d", printHits, dispatched)
	}
}

func TestHandleSyncJSONEscapingAndRelay(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sm, err := bridgesync.NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	var mu sync.Mutex
	var peerAHits int
	peerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sync" && r.Method == http.MethodPost {
			mu.Lock()
			peerAHits++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer peerA.Close()

	var peerCRelayedPath string
	var peerCRelayedBody []byte
	peerC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sync" && r.Method == http.MethodPost {
			data, _ := io.ReadAll(r.Body)
			mu.Lock()
			peerCRelayedPath = r.Header.Get("X-Bridge-RelPath")
			peerCRelayedBody = data
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"synced"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer peerC.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "UnitB"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.SharedDir = sharedDir

	// 1. Nil stateMgr returns 503 Service Unavailable.
	srvNoState := NewServer(cfg, nil, nil)
	reqNoState := httptest.NewRequest(http.MethodPost, "/api/sync", strings.NewReader("abc"))
	reqNoState.Header.Set("X-Bridge-Token", cfg.AuthToken)
	reqNoState.Header.Set("X-Bridge-RelPath", "test.txt")
	reqNoState.Header.Set("X-Bridge-Checksum", "deadbeef")
	recNoState := httptest.NewRecorder()
	srvNoState.authMiddleware(srvNoState.handleSync)(recNoState, reqNoState)
	if recNoState.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when stateMgr is nil, got %d", recNoState.Code)
	}

	// 2. Upload with special characters (backslashes/quotes) returns valid escaped JSON and relays A->B->C.
	se := bridgesync.NewSyncEngine(sharedDir, "", cfg.AuthToken, sm)
	se.UpdatePeerURLs([]string{peerA.URL, peerC.URL})

	srv := NewServer(cfg, sm, nil)
	srv.SetSyncEngine(se)

	var onSyncPath string
	srv.SetOnSyncReceived(func(relPath string) {
		mu.Lock()
		onSyncPath = relPath
		mu.Unlock()
	})

	payload := []byte("relay-payload-content")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	// Use backslash + quote (quote is valid on Linux/JSON test; backslash tests Windows path escaping in JSON).
	relPath := `sub\file_name.txt`
	if filepath.Separator == '/' {
		relPath = `sub/quote"file.txt`
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sync", bytes.NewReader(payload))
	req.RemoteAddr = "127.0.0.1:50020"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Peer-URL", peerA.URL)
	req.Header.Set("X-Bridge-RelPath", relPath)
	req.Header.Set("X-Bridge-Checksum", hash)
	req.Header.Set("X-Bridge-Size", fmt.Sprintf("%d", len(payload)))
	req.Header.Set("X-Bridge-ModTime", fmt.Sprintf("%d", time.Now().UnixNano()))

	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.handleSync)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/sync failed: %d %s", rec.Code, rec.Body.String())
	}

	var syncResp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &syncResp); err != nil {
		t.Fatalf("POST /api/sync response is invalid JSON (%q): %v", rec.Body.String(), err)
	}
	if syncResp["status"] != "synced" || syncResp["path"] != relPath {
		t.Fatalf("unexpected POST /api/sync JSON: %+v", syncResp)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		relayed := len(peerCRelayedBody) > 0
		mu.Unlock()
		if relayed {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	se.Stop()

	mu.Lock()
	if onSyncPath != relPath {
		t.Fatalf("expected onSyncReceived(%q), got %q", relPath, onSyncPath)
	}
	if peerAHits != 0 {
		t.Fatalf("expected sender peerA to be excluded from relay, got %d hits", peerAHits)
	}
	if string(peerCRelayedBody) != string(payload) || peerCRelayedPath != "sub/file_name.txt" && filepath.Separator == '\\' {
		if string(peerCRelayedBody) != string(payload) {
			t.Fatalf("expected peerC to receive relayed payload %q, got %q (path=%q)", string(payload), string(peerCRelayedBody), peerCRelayedPath)
		}
	}
	mu.Unlock()

	// 3. DELETE /api/sync also returns valid escaped JSON for backslash paths.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/sync?path=sub%5Cfile_name.txt", nil)
	delReq.RemoteAddr = "127.0.0.1:50021"
	delReq.Header.Set("X-Bridge-Token", cfg.AuthToken)
	delReq.Header.Set("X-Bridge-ModTime", fmt.Sprintf("%d", time.Now().UnixNano()))
	delRec := httptest.NewRecorder()
	srv.authMiddleware(srv.handleSync)(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/sync failed: %d %s", delRec.Code, delRec.Body.String())
	}
	var delResp map[string]string
	if err := json.Unmarshal(delRec.Body.Bytes(), &delResp); err != nil {
		t.Fatalf("DELETE /api/sync response is invalid JSON (%q): %v", delRec.Body.String(), err)
	}
	if delResp["status"] != "deleted" {
		t.Fatalf("unexpected DELETE /api/sync JSON: %+v", delResp)
	}
}

func TestCheckAutoPairTighteningAndTimeouts(t *testing.T) {
	tmp := t.TempDir()
	sm, err := bridgesync.NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.SharedDir = tmp
	if err := cfg.AddIgnoredPeer("IgnoredNode", "100.64.1.77", "http://100.64.1.77:8765"); err != nil {
		t.Fatalf("AddIgnoredPeer: %v", err)
	}

	srv := NewServer(cfg, sm, nil)

	// Verify http.Server timeout configuration allows long file/print streams.
	if srv.server.ReadTimeout != 0 || srv.server.WriteTimeout != 0 {
		t.Fatalf("expected ReadTimeout=0 and WriteTimeout=0, got ReadTimeout=%v WriteTimeout=%v", srv.server.ReadTimeout, srv.server.WriteTimeout)
	}
	if srv.server.ReadHeaderTimeout != 30*time.Second || srv.server.IdleTimeout != 120*time.Second {
		t.Fatalf("expected ReadHeaderTimeout=30s and IdleTimeout=120s, got ReadHeaderTimeout=%v IdleTimeout=%v", srv.server.ReadHeaderTimeout, srv.server.IdleTimeout)
	}

	// 1. Arbitrary authenticated API call (e.g. GET /api/sync/inventory) must NOT auto-pair unknown peers.
	invReq := httptest.NewRequest(http.MethodGet, "/api/sync/inventory", nil)
	invReq.RemoteAddr = "100.64.1.88:50000"
	invReq.Header.Set("X-Bridge-Token", cfg.AuthToken)
	invReq.Header.Set("X-Bridge-Node-Name", "ArbitraryCaller")
	invReq.Header.Set("X-Bridge-Peer-URL", "http://100.64.1.88:8765")
	invRec := httptest.NewRecorder()
	srv.authMiddleware(srv.handleSyncInventory)(invRec, invReq)
	if invRec.Code != http.StatusOK {
		t.Fatalf("inventory failed: %d", invRec.Code)
	}
	if len(cfg.GetPeers()) != 0 {
		t.Fatalf("expected arbitrary API call not to auto-pair, got %+v", cfg.GetPeers())
	}

	// 2. Ignored peer on /api/health must NOT auto-pair.
	igReq := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	igReq.RemoteAddr = "100.64.1.77:50000"
	igReq.Header.Set("X-Bridge-Token", cfg.AuthToken)
	igReq.Header.Set("X-Bridge-Node-Name", "IgnoredNode")
	igReq.Header.Set("X-Bridge-Peer-URL", "http://100.64.1.77:8765")
	igRec := httptest.NewRecorder()
	srv.handleHealth(igRec, igReq)
	if igRec.Code != http.StatusOK {
		t.Fatalf("health failed: %d", igRec.Code)
	}
	if len(cfg.GetPeers()) != 0 {
		t.Fatalf("expected ignored peer not to auto-pair, got %+v", cfg.GetPeers())
	}

	// 3. Health check without any pairing header (X-Bridge-Node-Name/Peer-Name/Peer-URL) must NOT auto-pair.
	noHdrReq := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	noHdrReq.RemoteAddr = "100.64.1.89:50000"
	noHdrReq.Header.Set("X-Bridge-Token", cfg.AuthToken)
	noHdrRec := httptest.NewRecorder()
	srv.handleHealth(noHdrRec, noHdrReq)
	if len(cfg.GetPeers()) != 0 {
		t.Fatalf("expected health check without pairing headers not to auto-pair, got %+v", cfg.GetPeers())
	}
}

func TestHandlePrintRejectsWhenNoPhysicalPrinter(t *testing.T) {
	tmp := t.TempDir()
	queueDir := filepath.Join(tmp, "queue")
	failedDir := filepath.Join(tmp, "failed")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("MkdirAll queueDir: %v", err)
	}
	ps := printer.NewPrintService("", 5, queueDir, failedDir)

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"

	srv := NewServer(cfg, nil, ps)
	srv.checkPrinterReady = func() (string, error) {
		return "", printer.ErrNoPhysicalPrinter
	}

	var mu sync.Mutex
	var rejectedSource, rejectedReason string
	var rejectedCalls int
	srv.SetOnPrintRejected(func(sourceNode, reason string) {
		mu.Lock()
		rejectedSource = sourceNode
		rejectedReason = reason
		rejectedCalls++
		mu.Unlock()
	})

	req := httptest.NewRequest(http.MethodPost, "/api/print", bytes.NewReader([]byte("%PDF-1.4\ntest")))
	req.RemoteAddr = "100.64.1.25:50000"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Node-Name", "LaptopNode")
	req.Header.Set("X-Bridge-Origin", "LaptopNode")
	req.Header.Set("X-Bridge-FileName", "invoice.pdf")

	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrint)(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Printer not connected on Unit1") || !strings.Contains(body, printer.ErrNoPhysicalPrinter.Error()) {
		t.Fatalf("unexpected 412 body: %q", body)
	}
	if ps.QueueDepth() != 0 {
		t.Fatalf("expected rejected job not to be queued, got QueueDepth=%d", ps.QueueDepth())
	}

	mu.Lock()
	defer mu.Unlock()
	if rejectedCalls != 1 || rejectedSource != "LaptopNode" || !strings.Contains(rejectedReason, printer.ErrNoPhysicalPrinter.Error()) {
		t.Fatalf("unexpected onPrintRejected invocation: calls=%d source=%q reason=%q", rejectedCalls, rejectedSource, rejectedReason)
	}
}

func TestHandleHealthProbePopulatesPeerPrinterStatus(t *testing.T) {
	readyPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Status:         "healthy",
			NodeName:       "Printer-PC",
			PrinterReady:   true,
			DefaultPrinter: "HP LaserJet Pro",
		})
	}))
	defer readyPeer.Close()

	noPrinterPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Status:         "healthy",
			NodeName:       "Laptop-PC",
			PrinterReady:   false,
			DefaultPrinter: "",
		})
	}))
	defer noPrinterPeer.Close()

	offlinePeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer offlinePeer.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("Printer-PC", "127.0.0.1", readyPeer.URL)
	cfg.AddOrUpdatePeer("Laptop-PC", "127.0.0.1", noPrinterPeer.URL)
	cfg.AddOrUpdatePeer("Offline-PC", "127.0.0.1", offlinePeer.URL)

	srv := NewServer(cfg, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/health?probe=sync", nil)
	req.RemoteAddr = "127.0.0.1:50011"
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var hr HealthResponse
	if err := json.NewDecoder(rec.Body).Decode(&hr); err != nil {
		t.Fatalf("decode HealthResponse: %v", err)
	}
	if !hr.PrinterReady && hr.DefaultPrinter != "" {
		t.Fatalf("expected DefaultPrinter to be empty when PrinterReady=false, got %q", hr.DefaultPrinter)
	}

	assertPeers := func(peers []config.PeerDevice, source string) {
		t.Helper()
		byName := make(map[string]config.PeerDevice, len(peers))
		for _, p := range peers {
			byName[p.Name] = p
		}

		rp, ok := byName["Printer-PC"]
		if !ok || !rp.Online || rp.PrinterReady == nil || !*rp.PrinterReady || rp.PrinterName != "HP LaserJet Pro" {
			t.Fatalf("%s: unexpected Printer-PC state: %+v", source, rp)
		}

		np, ok := byName["Laptop-PC"]
		if !ok || !np.Online || np.PrinterReady == nil || *np.PrinterReady || !np.IsPrinterKnownMissing() || np.PrinterName != "" {
			t.Fatalf("%s: unexpected Laptop-PC state: %+v", source, np)
		}

		op, ok := byName["Offline-PC"]
		if !ok || op.Online || op.PrinterReady != nil || op.PrinterName != "" {
			t.Fatalf("%s: unexpected Offline-PC state: %+v", source, op)
		}
	}

	assertPeers(hr.Peers, "HealthResponse.Peers")
	assertPeers(cfg.GetPeers(), "cfg.GetPeers")
}

func TestHandlePrintLocalPropagates412WhenPeerHasNoPrinter(t *testing.T) {
	tmp := t.TempDir()
	docPath := filepath.Join(tmp, "report.pdf")
	if err := os.WriteFile(docPath, []byte("%PDF-1.4\n%%EOF"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var mu sync.Mutex
	var gotJobName string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/print" {
			mu.Lock()
			gotJobName = r.Header.Get("X-Bridge-Job-Name")
			mu.Unlock()
			http.Error(w, "Printer not connected on Unit2: no physical printer connected", http.StatusPreconditionFailed)
			return
		}
		http.NotFound(w, r)
	}))
	defer peerSrv.Close()

	cfg := config.DefaultConfig()
	cfg.NodeName = "Unit1"
	cfg.ListenPort = 8765
	cfg.AuthToken = "test-token"
	cfg.AddOrUpdatePeer("Unit2", "127.0.0.1", peerSrv.URL)

	var dispatched int
	srv := NewServer(cfg, nil, nil)
	srv.SetOnPrintDispatched(func(fileName, peerName string) {
		mu.Lock()
		dispatched++
		mu.Unlock()
	})

	req := httptest.NewRequest(http.MethodPost, "/api/print-local", nil)
	req.RemoteAddr = "127.0.0.1:50001"
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-FilePath", docPath)
	req.Header.Set("X-Bridge-Target", "Unit2")

	rec := httptest.NewRecorder()
	srv.authMiddleware(srv.handlePrintLocal)(rec, req)

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 Precondition Failed, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Printer not connected on Unit2") {
		t.Fatalf("expected 412 body to preserve peer reason, got %q", rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if gotJobName != "report.pdf" {
		t.Fatalf("expected X-Bridge-Job-Name=%q, got %q", "report.pdf", gotJobName)
	}
	if dispatched != 0 {
		t.Fatalf("expected onPrintDispatched not to be called on 412 rejection, got %d", dispatched)
	}
}
