package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
