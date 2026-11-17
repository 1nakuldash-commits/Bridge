package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// SyncEngine coordinates folder watching, stabilization, and multi-peer mesh synchronization.
type SyncEngine struct {
	sharedDir string
	peersMu   sync.RWMutex
	peerURLs  map[string]bool
	authToken string
	stateMgr  *StateManager
	client    *http.Client
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// In-flight processing deduplication
	inFlightMu sync.Mutex
	inFlight   map[string]bool

	OnSyncStarted   func(relPath string)
	OnSyncCompleted func(relPath string)
}

var backoffSteps = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

// NewSyncEngine initializes a new multi-peer synchronization engine.
func NewSyncEngine(sharedDir, initialPeerURL, authToken string, stateMgr *StateManager) *SyncEngine {
	ctx, cancel := context.WithCancel(context.Background())
	se := &SyncEngine{
		sharedDir: filepath.Clean(sharedDir),
		peerURLs:  make(map[string]bool),
		authToken: authToken,
		stateMgr:  stateMgr,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		ctx:      ctx,
		cancel:   cancel,
		inFlight: make(map[string]bool),
	}

	if clean := strings.TrimRight(strings.TrimSpace(initialPeerURL), "/"); clean != "" {
		se.peerURLs[clean] = true
	}

	return se
}

// Start begins watching the shared directory and running the periodic scan.
func (se *SyncEngine) Start() error {
	if err := os.MkdirAll(se.sharedDir, 0755); err != nil {
		return fmt.Errorf("failed to create shared directory: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}

	if err := se.addWatchersRecursive(watcher, se.sharedDir); err != nil {
		watcher.Close()
		return fmt.Errorf("failed to watch shared directory: %w", err)
	}

	se.wg.Add(1)
	go se.watchLoop(watcher)

	se.wg.Add(1)
	go se.scanLoop()

	log.Printf("[SyncEngine] Started watching %s (peers: %v)", se.sharedDir, se.getPeerURLs())
	return nil
}

// Stop halts the sync engine gracefully.
func (se *SyncEngine) Stop() {
	se.cancel()
	se.wg.Wait()
	log.Printf("[SyncEngine] Stopped.")
}

func (se *SyncEngine) addWatchersRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return watcher.Add(path)
		}
		return nil
	})
}

// watchLoop handles fsnotify events.
func (se *SyncEngine) watchLoop(watcher *fsnotify.Watcher) {
	defer se.wg.Done()
	defer watcher.Close()

	for {
		select {
		case <-se.ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			// Ignore temporary or part files
			if strings.HasSuffix(event.Name, ".part") || strings.HasSuffix(event.Name, ".tmp") {
				continue
			}

			if event.Has(fsnotify.Create) {
				fi, err := os.Stat(event.Name)
				if err == nil && fi.IsDir() {
					_ = watcher.Add(event.Name)
					continue
				}
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
				relPath, err := filepath.Rel(se.sharedDir, event.Name)
				if err == nil && !strings.HasPrefix(relPath, "..") {
					go se.handleLocalFileChange(relPath)
				}
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[SyncEngine] Watcher error: %v", err)
		}
	}
}

// scanLoop runs a fallback scan every 30 seconds to catch any missed events.
func (se *SyncEngine) scanLoop() {
	defer se.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Initial scan shortly after startup
	select {
	case <-time.After(1 * time.Second):
		se.scanDirectory()
	case <-se.ctx.Done():
		return
	}

	for {
		select {
		case <-se.ctx.Done():
			return
		case <-ticker.C:
			se.scanDirectory()
		}
	}
}

func (se *SyncEngine) scanDirectory() {
	_ = filepath.Walk(se.sharedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".part") || strings.HasSuffix(path, ".tmp") {
			return nil
		}

		relPath, err := filepath.Rel(se.sharedDir, path)
		if err != nil || strings.HasPrefix(relPath, "..") {
			return nil
		}

		// Quick check: if recorded in state with same size and modtime, skip computing hash
		if rec, exists := se.stateMgr.Get(relPath); exists {
			if rec.Size == info.Size() && rec.ModTime == info.ModTime().UnixNano() {
				return nil
			}
		}

		go se.handleLocalFileChange(relPath)
		return nil
	})

	// Clean up any state records for files that were deleted on disk
	for relPath := range se.stateMgr.GetAll() {
		fullPath := filepath.Join(se.sharedDir, filepath.FromSlash(relPath))
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			_ = se.stateMgr.Delete(relPath)
		}
	}
}

// WaitForStableFile ensures the file size does not change for 500ms and is readable.
func WaitForStableFile(ctx context.Context, fullPath string) (os.FileInfo, error) {
	const checkInterval = 500 * time.Millisecond
	const maxWait = 60 * time.Second
	deadline := time.Now().Add(maxWait)

	var lastSize int64 = -1

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for file %s to stabilize", fullPath)
		}

		info, err := os.Stat(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, err
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Try opening for read to ensure no exclusive locks
		f, err := os.Open(fullPath)
		if err != nil {
			// File is still being written or locked
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = f.Close()

		currentSize := info.Size()
		if lastSize != -1 && currentSize == lastSize {
			// Size remained identical across the check interval
			return info, nil
		}

		lastSize = currentSize
		time.Sleep(checkInterval)
	}
}

func (se *SyncEngine) handleLocalFileChange(relPath string) {
	cleanRel := filepath.Clean(relPath)
	se.inFlightMu.Lock()
	if se.inFlight[cleanRel] {
		se.inFlightMu.Unlock()
		return
	}
	se.inFlight[cleanRel] = true
	se.inFlightMu.Unlock()

	defer func() {
		se.inFlightMu.Lock()
		delete(se.inFlight, cleanRel)
		se.inFlightMu.Unlock()
	}()

	fullPath := filepath.Join(se.sharedDir, cleanRel)

	info, err := WaitForStableFile(se.ctx, fullPath)
	if err != nil {
		return
	}

	hash, size, err := ComputeHash(fullPath)
	if err != nil {
		log.Printf("[SyncEngine] Failed to compute hash for %s: %v", relPath, err)
		return
	}

	// Loop Prevention: if already synced with this hash, suppress re-upload!
	if se.stateMgr.IsSynced(relPath, hash) {
		return
	}

	log.Printf("[SyncEngine] Detected modified/new file %s (size: %d, sha256: %s)", relPath, size, hash[:8])
	se.broadcastToPeers(relPath, fullPath, hash, size, info.ModTime().UnixNano())
}

func (se *SyncEngine) broadcastToPeers(relPath, fullPath, hash string, size, modTime int64) {
	// Immediately mark in local state so subsequent scans or watcher events see this version as processed
	_ = se.stateMgr.Set(relPath, FileRecord{
		RelPath: relPath,
		Hash:    hash,
		Size:    size,
		ModTime: modTime,
	})

	peers := se.getPeerURLs()
	if len(peers) == 0 {
		return
	}

	if se.OnSyncStarted != nil {
		se.OnSyncStarted(relPath)
	}

	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			se.uploadToPeerWithRetry(p, relPath, fullPath, hash, size, modTime)
		}(peer)
	}

	go func() {
		wg.Wait()
		if se.OnSyncCompleted != nil {
			se.OnSyncCompleted(relPath)
		}
	}()
}

func (se *SyncEngine) uploadToPeerWithRetry(peerURL, relPath, fullPath, hash string, size, modTime int64) {
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-se.ctx.Done():
			return
		default:
		}

		err := se.uploadFile(peerURL, relPath, fullPath, hash, size, modTime)
		if err == nil {
			log.Printf("[SyncEngine] Successfully synced %s to peer %s", relPath, peerURL)
			return
		}

		if attempt < maxRetries-1 {
			delay := backoffSteps[attempt]
			log.Printf("[SyncEngine] Sync %s to %s failed (attempt %d/%d): %v. Retrying in %v...", relPath, peerURL, attempt+1, maxRetries, err, delay)
			select {
			case <-se.ctx.Done():
				return
			case <-time.After(delay):
			}

			// Re-verify file still exists and has same hash before retrying
			currHash, _, err := ComputeHash(fullPath)
			if err != nil || currHash != hash {
				return
			}
		} else {
			log.Printf("[SyncEngine] Peer %s unreachable after %d attempts for %s. Will reconcile upon reconnection.", peerURL, maxRetries, relPath)
		}
	}
}

// ReconcileWithPeer reconciles inventory with a peer to push missed files and pull remote files.
func (se *SyncEngine) ReconcileWithPeer(peerURL string) {
	cleanPeer := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanPeer == "" {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(se.ctx, 30*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/sync/inventory", cleanPeer), nil)
		if err != nil {
			return
		}
		req.Header.Set("X-Bridge-Token", se.authToken)

		resp, err := se.client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}

		var remoteRecords map[string]FileRecord
		if err := json.NewDecoder(resp.Body).Decode(&remoteRecords); err != nil {
			return
		}

		localRecords := se.stateMgr.GetAll()

		// 1. Push: files local has that remote is missing or has older/different hash
		for relPath, locRec := range localRecords {
			fullPath := filepath.Join(se.sharedDir, filepath.FromSlash(relPath))
			info, err := os.Stat(fullPath)
			if err != nil || info.IsDir() {
				continue
			}

			remRec, exists := remoteRecords[relPath]
			if !exists || remRec.Hash != locRec.Hash {
				log.Printf("[SyncEngine] Catch-up reconciliation: pushing %s to online peer %s", relPath, cleanPeer)
				_ = se.uploadFile(cleanPeer, relPath, fullPath, locRec.Hash, locRec.Size, locRec.ModTime)
			}
		}

		// 2. Pull: files remote has that local is missing or has different hash
		for relPath, remRec := range remoteRecords {
			locRec, exists := localRecords[relPath]
			fullPath := filepath.Join(se.sharedDir, filepath.FromSlash(relPath))
			_, statErr := os.Stat(fullPath)
			localMissing := os.IsNotExist(statErr) || !exists

			if localMissing || locRec.Hash != remRec.Hash {
				log.Printf("[SyncEngine] Catch-up reconciliation: pulling %s from online peer %s", relPath, cleanPeer)
				_ = se.downloadFile(cleanPeer, relPath, remRec.Hash, remRec.Size, remRec.ModTime)
			}
		}
	}()
}

func (se *SyncEngine) downloadFile(peerURL, relPath, expectedHash string, size, modTime int64) error {
	destURL := fmt.Sprintf("%s/api/sync?path=%s", peerURL, url.QueryEscape(filepath.ToSlash(relPath)))
	req, err := http.NewRequestWithContext(se.ctx, "GET", destURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Bridge-Token", se.authToken)

	resp, err := se.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned HTTP %d on download", resp.StatusCode)
	}

	return ReceiveSyncFile(se.sharedDir, se.stateMgr, relPath, expectedHash, size, modTime, resp.Body)
}

func (se *SyncEngine) getPeerURLs() []string {
	se.peersMu.RLock()
	defer se.peersMu.RUnlock()
	res := make([]string, 0, len(se.peerURLs))
	for p := range se.peerURLs {
		res = append(res, p)
	}
	return res
}

// UpdatePeerURL updates or adds a peer URL (backward compatibility).
func (se *SyncEngine) UpdatePeerURL(peerURL string) {
	clean := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if clean == "" {
		return
	}
	se.peersMu.Lock()
	se.peerURLs[clean] = true
	se.peersMu.Unlock()
	log.Printf("[SyncEngine] Peer URL registered: %s (Total peers: %d)", clean, len(se.getPeerURLs()))
}

// UpdatePeerURLs replaces the active peer URLs list.
func (se *SyncEngine) UpdatePeerURLs(urls []string) {
	se.peersMu.Lock()
	se.peerURLs = make(map[string]bool)
	for _, u := range urls {
		if clean := strings.TrimRight(strings.TrimSpace(u), "/"); clean != "" {
			se.peerURLs[clean] = true
		}
	}
	se.peersMu.Unlock()
	log.Printf("[SyncEngine] Updated peer URLs: %v", se.getPeerURLs())
}

// RemovePeerURL removes a peer URL from the sync engine.
func (se *SyncEngine) RemovePeerURL(peerURL string) {
	clean := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	se.peersMu.Lock()
	delete(se.peerURLs, clean)
	se.peersMu.Unlock()
}

func (se *SyncEngine) uploadFile(peerURL, relPath, fullPath, hash string, size, modTime int64) error {
	if peerURL == "" {
		return errors.New("peer URL is empty")
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()

	destURL := fmt.Sprintf("%s/api/sync", peerURL)
	req, err := http.NewRequestWithContext(se.ctx, "POST", destURL, f)
	if err != nil {
		return err
	}

	req.Header.Set("X-Bridge-Token", se.authToken)
	req.Header.Set("X-Bridge-RelPath", filepath.ToSlash(relPath))
	req.Header.Set("X-Bridge-Checksum", hash)
	req.Header.Set("X-Bridge-Size", strconv.FormatInt(size, 10))
	req.Header.Set("X-Bridge-ModTime", strconv.FormatInt(modTime, 10))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := se.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("peer returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// ReceiveSyncFile handles an incoming sync request on the receiver side.
func ReceiveSyncFile(sharedDir string, stateMgr *StateManager, relPath, expectedHash string, size, modTime int64, r io.Reader) error {
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		return errors.New("invalid or malicious relative path")
	}

	targetPath := filepath.Join(sharedDir, cleanRel)
	targetDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", targetDir, err)
	}

	partPath := targetPath + ".part"
	partFile, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create part file: %w", err)
	}

	// Stream incoming bytes to .part file and calculate SHA256 simultaneously
	h := sha256.New()
	writer := io.MultiWriter(partFile, h)

	written, err := io.Copy(writer, r)
	_ = partFile.Close()
	if err != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("failed to write received data: %w", err)
	}

	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		_ = os.Remove(partPath)
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	// Set original modification time if provided
	if modTime > 0 {
		mtime := time.Unix(0, modTime)
		_ = os.Chtimes(partPath, mtime, mtime)
	}

	// Atomic replace
	if err := os.Rename(partPath, targetPath); err != nil {
		_ = os.Remove(partPath)
		return fmt.Errorf("failed to atomically place file: %w", err)
	}

	// Record in state manager to prevent loop echo!
	if err := stateMgr.MarkAsReceived(cleanRel, actualHash, written, modTime); err != nil {
		log.Printf("[SyncEngine] Warning: failed to save state for %s: %v", cleanRel, err)
	}

	log.Printf("[SyncEngine] Successfully received and placed %s (%d bytes)", cleanRel, written)
	return nil
}
