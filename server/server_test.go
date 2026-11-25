package server

import (
	"bytes"
	"encoding/json"
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
