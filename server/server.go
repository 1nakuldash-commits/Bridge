package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bridge/config"
	"bridge/printer"
	bridgesync "bridge/sync"
)

// Server handles HTTP API requests for sync, printing, peer discovery, and health checks.
type Server struct {
	cfg               *config.Config
	stateMgr          *bridgesync.StateManager
	syncEngine        *bridgesync.SyncEngine
	printService      *printer.PrintService
	server            *http.Server
	startTime         time.Time
	peerUpdater       func(peer config.PeerDevice, isNew bool)
	onPrintDispatched func(fileName, peerName string)
	onSyncReceived    func(relPath string)
}

// HealthResponse represents the payload returned by GET /api/health.
type HealthResponse struct {
	Status          string              `json:"status"`
	NodeName        string              `json:"node_name"`
	LocalIP         string              `json:"local_ip,omitempty"`
	PeerName        string              `json:"peer_name,omitempty"`
	PeerURL         string              `json:"peer_url,omitempty"`
	Peers           []config.PeerDevice `json:"peers,omitempty"`
	OnlineCount     int                 `json:"online_count"`
	TotalPeers      int                 `json:"total_peers"`
	PeerReachable   bool                `json:"peer_reachable"`
	PrinterReady    bool                `json:"printer_ready"`
	DefaultPrinter  string              `json:"default_printer"`
	PrintQueueDepth int                 `json:"print_queue_depth"`
	UptimeSeconds   int64               `json:"uptime_seconds"`
}

// NewServer initializes the HTTP server with configured endpoints.
func NewServer(cfg *config.Config, stateMgr *bridgesync.StateManager, printService *printer.PrintService) *Server {
	s := &Server{
		cfg:          cfg,
		stateMgr:     stateMgr,
		printService: printService,
		startTime:    time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/peers", s.authMiddleware(s.handlePeers))
	mux.HandleFunc("/api/sync", s.authMiddleware(s.handleSync))
	mux.HandleFunc("/api/sync/inventory", s.authMiddleware(s.handleSyncInventory))
	mux.HandleFunc("/api/print", s.authMiddleware(s.handlePrint))
	mux.HandleFunc("/api/print-local", s.authMiddleware(s.handlePrintLocal))

	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.ListenPort),
		Handler:      mux,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	return s
}

// SetPeerUpdater sets a callback invoked when a peer device is added or updated.
func (s *Server) SetPeerUpdater(fn func(peer config.PeerDevice, isNew bool)) {
	s.peerUpdater = fn
}

// SetSyncEngine attaches a SyncEngine reference to handle mesh file routing and deletion relay.
func (s *Server) SetSyncEngine(se *bridgesync.SyncEngine) {
	s.syncEngine = se
}

// SetOnPrintDispatched sets a callback invoked when a local file is dispatched for printing.
func (s *Server) SetOnPrintDispatched(fn func(fileName, peerName string)) {
	s.onPrintDispatched = fn
}

// SetOnSyncReceived sets a callback invoked when a file is received via sync upload.
func (s *Server) SetOnSyncReceived(fn func(relPath string)) {
	s.onSyncReceived = fn
}

// Start runs the HTTP listener.
func (s *Server) Start() error {
	log.Printf("[Server] Listening on :%d (Node: %s)", s.cfg.ListenPort, s.cfg.GetNodeName())
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() error {
	return s.server.Close()
}

// authMiddleware enforces the mandatory X-Bridge-Token header and checks for peer auto-discovery.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Bridge-Token")
		if token == "" || token != s.cfg.AuthToken {
			http.Error(w, "Unauthorized: missing or invalid X-Bridge-Token", http.StatusUnauthorized)
			return
		}

		s.checkAutoPair(r)
		next(w, r)
	}
}

func extractRemoteIP(remoteAddr string) string {
	addr := strings.TrimSpace(remoteAddr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	return config.ExtractIP(addr)
}

func isValidNonLoopbackIP(ipStr string) bool {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified()
}

func isValidPeerURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func isLoopbackURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	host := strings.ToLower(config.ExtractIP(rawURL))
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// resolvePeerFromRequest resolves a peer's IP and URL from the provided header URL,
// falling back to r.RemoteAddr's IP when headerURL is empty or loopback and the remote
// address is a valid non-loopback IP.
func resolvePeerFromRequest(headerURL string, r *http.Request) (string, string) {
	cleanURL := strings.TrimRight(strings.TrimSpace(headerURL), "/")
	remoteIP := extractRemoteIP(r.RemoteAddr)

	if cleanURL == "" || !isValidPeerURL(cleanURL) || isLoopbackURL(cleanURL) {
		if isValidNonLoopbackIP(remoteIP) {
			port := "8765"
			if u, err := url.Parse(cleanURL); err == nil && u.Port() != "" {
				port = u.Port()
			}
			return remoteIP, fmt.Sprintf("http://%s:%s", remoteIP, port)
		}
		if isValidPeerURL(cleanURL) && isLoopbackURL(cleanURL) {
			return config.ExtractIP(cleanURL), cleanURL
		}
		return "", ""
	}

	return config.ExtractIP(cleanURL), cleanURL
}

func findMatchingPeerSnapshot(peers []config.PeerDevice, name, ip, peerURL string) (config.PeerDevice, bool) {
	cleanName := strings.TrimSpace(name)
	cleanIP := strings.TrimSpace(ip)
	cleanURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")

	if cleanIP == "" && cleanURL != "" {
		cleanIP = config.ExtractIP(cleanURL)
	}
	if cleanURL == "" && cleanIP != "" {
		cleanURL = fmt.Sprintf("http://%s:8765", cleanIP)
	}

	if cleanURL != "" {
		for _, p := range peers {
			if strings.EqualFold(p.URL, cleanURL) {
				return p, true
			}
		}
	}
	if cleanName != "" {
		for _, p := range peers {
			if strings.EqualFold(p.Name, cleanName) {
				return p, true
			}
		}
	}
	if cleanIP != "" {
		for _, p := range peers {
			if p.IP == cleanIP {
				if cleanURL != "" && p.URL != "" && !strings.EqualFold(cleanURL, p.URL) {
					continue
				}
				if cleanName != "" && p.Name != "" && !strings.EqualFold(cleanName, p.Name) {
					continue
				}
				return p, true
			}
		}
	}
	return config.PeerDevice{}, false
}

func (s *Server) addOrUpdatePeerWithChange(name, ip, peerURL string) (config.PeerDevice, bool, bool) {
	before := s.cfg.GetPeers()
	dev, isNew := s.cfg.AddOrUpdatePeer(name, ip, peerURL)
	if dev.Name == "" && dev.IP == "" && dev.URL == "" {
		return dev, false, false
	}
	if isNew {
		return dev, true, true
	}
	prev, found := findMatchingPeerSnapshot(before, name, ip, peerURL)
	if !found {
		return dev, false, false
	}
	changed := prev.Name != dev.Name || prev.IP != dev.IP || prev.URL != dev.URL || prev.SpoolPort != dev.SpoolPort
	return dev, false, changed
}

func (s *Server) checkAutoPair(r *http.Request) {
	peerHeader := r.Header.Get("X-Bridge-Peer-URL")
	nodeHeader := r.Header.Get("X-Bridge-Node-Name")

	cleanNodeName := strings.TrimSpace(nodeHeader)
	cleanIP, cleanPeerURL := resolvePeerFromRequest(peerHeader, r)

	if cleanIP == "" && cleanPeerURL == "" {
		return
	}

	if config.IsSelfTarget(cleanNodeName, cleanIP, cleanPeerURL, s.cfg.GetNodeName(), s.cfg.ListenPort) {
		return // Strictly ignore self
	}

	dev, isNew, changed := s.addOrUpdatePeerWithChange(cleanNodeName, cleanIP, cleanPeerURL)
	if isNew && dev.Name != "" {
		log.Printf("[Server] Discovered new peer: '%s' (%s) -> Assigned SpoolPort %d", dev.Name, dev.URL, dev.SpoolPort)
		if s.peerUpdater != nil {
			s.peerUpdater(dev, true)
		}
	} else if changed && dev.Name != "" {
		log.Printf("[Server] Updated peer metadata: '%s' (%s) -> SpoolPort %d", dev.Name, dev.URL, dev.SpoolPort)
		if s.peerUpdater != nil {
			s.peerUpdater(dev, false)
		}
	}
}

// handleHealth returns service status, device list, printer readiness, and peer connectivity.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := r.Header.Get("X-Bridge-Token")
	if token == s.cfg.AuthToken && r.Header.Get("X-Bridge-Probe") == "" {
		s.checkAutoPair(r)
	}

	peers := s.cfg.GetPeers()
	onlineCount := 0
	anyReachable := false

	// Fast parallel reachability probe ONLY if explicitly requested via ?probe=sync
	// Routine health checks return instantly using dynamic cached status, eliminating latency
	if r.URL.Query().Get("probe") == "sync" && r.Header.Get("X-Bridge-Probe") == "" && len(peers) > 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		onlineCount = 0

		for i := range peers {
			p := &peers[i]
			if p.URL == "" {
				continue
			}
			wg.Add(1)
			go func(dev *config.PeerDevice) {
				defer wg.Done()
				client := http.Client{Timeout: 500 * time.Millisecond}
				req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", dev.URL), nil)
				if err != nil {
					return
				}
				req.Header.Set("X-Bridge-Token", s.cfg.AuthToken)
				req.Header.Set("X-Bridge-Node-Name", s.cfg.GetNodeName())
				req.Header.Set("X-Bridge-Probe", "1")

				resp, err := client.Do(req)
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						mu.Lock()
						dev.Online = true
						dev.LastSeen = time.Now()
						onlineCount++
						anyReachable = true
						mu.Unlock()
						s.cfg.SetPeerOnlineStatus(dev.URL, true)
					}
				}
			}(p)
		}
		wg.Wait()
	} else {
		for _, p := range peers {
			if p.Online {
				onlineCount++
				anyReachable = true
			}
		}
	}

	queueDepth := 0
	if s.printService != nil {
		queueDepth = s.printService.QueueDepth()
	}

	targetPrinter, _ := printer.GetTargetPhysicalPrinter()
	if targetPrinter == "" {
		targetPrinter, _ = printer.GetDefaultWindowsPrinter()
	}
	printerReady := targetPrinter != "" && !printer.IsVirtualPrinterName(targetPrinter, "")

	resp := HealthResponse{
		Status:          "healthy",
		NodeName:        s.cfg.GetNodeName(),
		LocalIP:         config.ExtractIP(r.Host),
		PeerName:        s.cfg.GetPeerName(),
		PeerURL:         s.cfg.GetPeerURL(),
		Peers:           peers,
		OnlineCount:     onlineCount,
		TotalPeers:      len(peers),
		PeerReachable:   anyReachable,
		PrinterReady:    printerReady,
		DefaultPrinter:  targetPrinter,
		PrintQueueDepth: queueDepth,
		UptimeSeconds:   int64(time.Since(s.startTime).Seconds()),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handlePeers returns or registers known mesh peer devices.
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		peers := s.cfg.GetPeers()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"node_name": s.cfg.GetNodeName(),
			"peers":     peers,
		})
	case http.MethodPost:
		var dev config.PeerDevice
		if err := json.NewDecoder(r.Body).Decode(&dev); err != nil {
			http.Error(w, "Invalid peer JSON", http.StatusBadRequest)
			return
		}
		if config.IsSelfTarget(dev.Name, dev.IP, dev.URL, s.cfg.GetNodeName(), s.cfg.ListenPort) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(dev)
			return
		}
		addedDev, isNew, changed := s.addOrUpdatePeerWithChange(dev.Name, dev.IP, dev.URL)
		if dev.CustomName && strings.TrimSpace(dev.Name) != "" {
			target := addedDev.URL
			if target == "" {
				target = addedDev.IP
			}
			if s.cfg.SetPeerCustomName(target, dev.Name, true) {
				changed = true
				if updated := s.cfg.FindPeer(target); updated != nil {
					addedDev = *updated
				} else {
					addedDev.Name = strings.TrimSpace(dev.Name)
					addedDev.CustomName = true
				}
			}
		}
		if (isNew || changed) && s.cfg.ConfigPath != "" {
			_ = config.SaveConfig(s.cfg.ConfigPath, s.cfg)
		}
		if (isNew || changed) && s.peerUpdater != nil && (addedDev.Name != "" || addedDev.URL != "" || addedDev.IP != "") {
			s.peerUpdater(addedDev, isNew)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(addedDev)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSyncInventory returns a list of local files and SHA256 hashes to resolve mesh discrepancies.
func (s *Server) handleSyncInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.stateMgr == nil {
		http.Error(w, "State manager unavailable", http.StatusServiceUnavailable)
		return
	}

	records := s.stateMgr.GetAll()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(records)
}

// handleSync handles upload (POST) and download (GET) for global file synchronization.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		relPath := r.Header.Get("X-Bridge-RelPath")
		checksum := r.Header.Get("X-Bridge-Checksum")
		sizeStr := r.Header.Get("X-Bridge-Size")
		modTimeStr := r.Header.Get("X-Bridge-ModTime")

		if relPath == "" || checksum == "" {
			http.Error(w, "Missing X-Bridge-RelPath or X-Bridge-Checksum header", http.StatusBadRequest)
			return
		}

		var size int64
		if sizeStr != "" {
			size, _ = strconv.ParseInt(sizeStr, 10, 64)
		}

		var modTime int64
		if modTimeStr != "" {
			modTime, _ = strconv.ParseInt(modTimeStr, 10, 64)
		}

		err := bridgesync.ReceiveSyncFile(s.cfg.SharedDir, s.stateMgr, relPath, checksum, size, modTime, r.Body)
		if err != nil {
			log.Printf("[Server] Error receiving sync file %s: %v", relPath, err)
			http.Error(w, fmt.Sprintf("Sync failed: %v", err), http.StatusInternalServerError)
			return
		}

		if s.onSyncReceived != nil {
			s.onSyncReceived(relPath)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"synced","path":"`+relPath+`"}`)

	case http.MethodGet:
		relPath := r.URL.Query().Get("path")
		if relPath == "" {
			relPath = r.Header.Get("X-Bridge-RelPath")
		}
		if relPath == "" {
			http.Error(w, "Missing path parameter", http.StatusBadRequest)
			return
		}

		cleanRel := filepath.Clean(filepath.FromSlash(relPath))
		if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}

		fullPath := filepath.Join(s.cfg.SharedDir, cleanRel)
		f, err := bridgesync.OpenSharedFileForRead(fullPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("File not found: %v", err), http.StatusNotFound)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.Error(w, "Cannot stream directory", http.StatusBadRequest)
			return
		}

		hash, size, err := bridgesync.ComputeHash(fullPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Hash calculation failed: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Bridge-RelPath", filepath.ToSlash(cleanRel))
		w.Header().Set("X-Bridge-Checksum", hash)
		w.Header().Set("X-Bridge-Size", strconv.FormatInt(size, 10))
		w.Header().Set("X-Bridge-ModTime", strconv.FormatInt(info.ModTime().UnixNano(), 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)

	case http.MethodDelete:
		relPath := r.URL.Query().Get("path")
		if relPath == "" {
			relPath = r.Header.Get("X-Bridge-RelPath")
		}
		if relPath == "" {
			http.Error(w, "Missing path parameter", http.StatusBadRequest)
			return
		}

		cleanRel := filepath.Clean(filepath.FromSlash(relPath))
		if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			http.Error(w, "Invalid path", http.StatusBadRequest)
			return
		}

		modTimeStr := r.Header.Get("X-Bridge-ModTime")
		var modTime int64
		if modTimeStr != "" {
			modTime, _ = strconv.ParseInt(modTimeStr, 10, 64)
		}

		rawSender := r.Header.Get("X-Bridge-Peer-URL")
		if rawSender == "" {
			rawSender = r.Header.Get("X-Bridge-Sender")
		}
		_, senderURL := resolvePeerFromRequest(rawSender, r)
		if senderURL == "" {
			if remoteIP := extractRemoteIP(r.RemoteAddr); remoteIP != "" {
				senderURL = fmt.Sprintf("http://%s:8765", remoteIP)
			}
		}

		var err error
		if s.syncEngine != nil {
			err = s.syncEngine.ReceiveSyncDelete(senderURL, cleanRel, modTime)
		} else {
			err = bridgesync.ReceiveSyncDelete(s.cfg.SharedDir, s.stateMgr, cleanRel, modTime)
		}
		if err != nil {
			log.Printf("[Server] Error processing delete sync for %s: %v", cleanRel, err)
			http.Error(w, fmt.Sprintf("Delete failed: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"deleted","path":"`+cleanRel+`"}`)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePrint receives print jobs from any peer device.
func (s *Server) handlePrint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.printService == nil {
		http.Error(w, "Print service not configured on this host", http.StatusBadRequest)
		return
	}

	// Echo loopback prevention check: reject if origin matches this node
	origin := r.Header.Get("X-Bridge-Origin")
	if origin == "" {
		origin = r.Header.Get("X-Bridge-Node-Name")
	}
	if origin != "" && strings.EqualFold(origin, s.cfg.GetNodeName()) {
		log.Printf("[Server] REJECTED loopback print job: origin '%s' matches local node name '%s'", origin, s.cfg.GetNodeName())
		http.Error(w, fmt.Sprintf("Loopback print rejected: origin '%s' matches local node name", origin), http.StatusBadRequest)
		return
	}

	fileName := r.Header.Get("X-Bridge-FileName")
	if fileName == "" {
		fileName = "print_job.pdf"
	}

	paperSize := strings.TrimSpace(r.Header.Get("X-Bridge-Paper-Size"))
	orientation := strings.TrimSpace(r.Header.Get("X-Bridge-Orientation"))
	pageCountStr := strings.TrimSpace(r.Header.Get("X-Bridge-Page-Count"))
	var pageCount int
	if pageCountStr != "" {
		if n, err := strconv.Atoi(pageCountStr); err == nil && n >= 0 {
			pageCount = n
		}
	}
	if paperSize != "" || orientation != "" || pageCount > 0 {
		log.Printf("[Server] Incoming print job %q metadata: paper=%q orientation=%q pages=%d", fileName, paperSize, orientation, pageCount)
	}

	job, err := s.printService.SaveIncomingJob(fileName, r.Body)
	if err != nil {
		if strings.Contains(err.Error(), "full") {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to stage print job: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "queued",
		"job_id": job.ID,
	})
}

// handlePrintLocal receives requests to print a local file on a specified peer PC (e.g. from SendTo menu).
func (s *Server) handlePrintLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filePath := r.Header.Get("X-Bridge-FilePath")
	if filePath == "" {
		data, err := io.ReadAll(io.LimitReader(r.Body, 1024))
		if err != nil || len(data) == 0 {
			http.Error(w, "Missing file path to print", http.StatusBadRequest)
			return
		}
		filePath = strings.TrimSpace(string(data))
	}

	if _, err := os.Stat(filePath); err != nil {
		http.Error(w, fmt.Sprintf("File not found: %s", filePath), http.StatusBadRequest)
		return
	}

	// Resolve target peer (from X-Bridge-Target header or default to primary)
	target := strings.TrimSpace(r.Header.Get("X-Bridge-Target"))
	peerURL := ""
	peerName := ""

	if target != "" {
		p := s.cfg.FindPeer(target)
		if p == nil {
			http.Error(w, fmt.Sprintf("target peer not found: %s", target), http.StatusNotFound)
			return
		}
		if p.URL == "" {
			http.Error(w, fmt.Sprintf("target peer %s has no reachable URL", p.Name), http.StatusServiceUnavailable)
			return
		}
		peerURL = p.URL
		peerName = p.Name
	} else {
		peerURL = s.cfg.GetPeerURL()
		peerName = s.cfg.GetPeerName()
	}

	if peerURL == "" {
		http.Error(w, "No destination peer PC is connected yet", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	err := printer.SendDirectPrint(ctx, peerURL, s.cfg.AuthToken, filePath,
		"X-Bridge-Node-Name", s.cfg.GetNodeName(),
		"X-Bridge-Origin", s.cfg.GetNodeName(),
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to send print job: %v", err), http.StatusInternalServerError)
		return
	}

	if s.onPrintDispatched != nil {
		s.onPrintDispatched(filepath.Base(filePath), peerName)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "dispatched",
		"file_name": filepath.Base(filePath),
		"peer_name": peerName,
	})
}

