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
	"net"
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
	localURL  string
	peersMu   sync.RWMutex
	peerURLs  map[string]bool
	authToken string
	stateMgr  *StateManager
	client    *http.Client
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// In-flight processing deduplication
	inFlightMu        sync.Mutex
	inFlight          map[string]bool
	inFlightDeletesMu sync.Mutex
	inFlightDeletes   map[string]bool
	reconcileMu       sync.Mutex
	reconcilingPeer   map[string]bool

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
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	se := &SyncEngine{
		sharedDir: filepath.Clean(sharedDir),
		peerURLs:  make(map[string]bool),
		authToken: authToken,
		stateMgr:  stateMgr,
		client: &http.Client{
			Transport: transport,
		},
		ctx:             ctx,
		cancel:          cancel,
		inFlight:        make(map[string]bool),
		inFlightDeletes: make(map[string]bool),
		reconcilingPeer: make(map[string]bool),
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

func (se *SyncEngine) addWatchersAndScanDir(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if se.ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if addErr := watcher.Add(path); addErr != nil && !errors.Is(addErr, fsnotify.ErrClosed) {
				log.Printf("[SyncEngine] Failed to watch directory %s: %v", path, addErr)
			}
			return nil
		}
		if strings.HasSuffix(path, ".part") || strings.HasSuffix(path, ".tmp") {
			return nil
		}
		relPath, err := filepath.Rel(se.sharedDir, path)
		if err != nil || strings.HasPrefix(relPath, "..") {
			return nil
		}
		if rec, exists := se.stateMgr.Get(relPath); exists && !rec.Deleted {
			if rec.Size == info.Size() && rec.ModTime == info.ModTime().UnixNano() {
				return nil
			}
		}
		go se.handleLocalFileChange(relPath)
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
					// Run watcher.Add outside the event-reader loop so fsnotify's
					// Windows readDirChangesW cannot deadlock on an unbuffered sendEvent.
					se.wg.Add(1)
					go func(dirPath string) {
						defer se.wg.Done()
						_ = se.addWatchersAndScanDir(watcher, dirPath)
					}(event.Name)
					continue
				}
			}

			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				relPath, err := filepath.Rel(se.sharedDir, event.Name)
				if err == nil && !strings.HasPrefix(relPath, "..") {
					go se.handleLocalFileDeletion(relPath)
				}
				continue
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

// RobustRemove removes a file from disk with Windows read-only flag stripping and retry backoff.
// If the file is stubbornly locked by another process, it renames it outside the active namespace
// so it immediately vanishes from user sight and directory scans, and finishes removal in background.
func RobustRemove(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	// 1. Remove Windows read-only attribute if set
	_ = os.Chmod(path, 0666)

	// 2. Immediate removal attempt
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}

	// 3. Retry loop for transient locks (Windows Defender, Indexer, Explorer thumbnails, file handles closing)
	for i := 0; i < 8; i++ {
		time.Sleep(time.Duration(20*(1<<i)) * time.Millisecond) // 20ms, 40ms, 80ms, 160ms...
		_ = os.Chmod(path, 0666)
		err = os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
	}

	// 4. Last resort: If still locked by a stubborn process, rename it to a temporary hidden file
	// outside the active namespace so it disappears immediately from Explorer and won't be seen by scanner!
	tempTrash := fmt.Sprintf("%s.deleted_%d.tmp", path, time.Now().UnixNano())
	if renameErr := os.Rename(path, tempTrash); renameErr == nil {
		go func(p string) {
			for attempt := 0; attempt < 20; attempt++ {
				time.Sleep(500 * time.Millisecond)
				_ = os.Chmod(p, 0666)
				remErr := os.Remove(p)
				if remErr == nil || os.IsNotExist(remErr) {
					return
				}
			}
		}(tempTrash)
		return nil
	}

	return err
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

		// Quick check: if recorded in state with same size and modtime and not deleted, skip computing hash
		if rec, exists := se.stateMgr.Get(relPath); exists {
			if rec.Deleted {
				// Zombie file defense: if marked as deleted and modtime on disk <= tombstone modtime,
				// it's an old file that should have been deleted! Purge it from disk now!
				if info.ModTime().UnixNano() <= rec.ModTime {
					_ = RobustRemove(path)
					return nil
				}
			} else {
				if rec.Size == info.Size() && rec.ModTime == info.ModTime().UnixNano() {
					return nil
				}
			}
		}

		go se.handleLocalFileChange(relPath)
		return nil
	})

	// Detect any files recorded in state that were deleted on disk and propagate deletion
	for relPath, rec := range se.stateMgr.GetAll() {
		fullPath := filepath.Join(se.sharedDir, filepath.FromSlash(relPath))
		info, err := os.Stat(fullPath)
		if os.IsNotExist(err) {
			if !rec.Deleted {
				go se.handleLocalFileDeletion(relPath)
			}
		} else if err == nil && !info.IsDir() {
			if rec.Deleted && info.ModTime().UnixNano() <= rec.ModTime {
				// Zombie file on disk! Purge it!
				_ = RobustRemove(fullPath)
			}
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
		f, err := OpenSharedFileForRead(fullPath)
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
		if os.IsNotExist(err) {
			se.handleLocalFileDeletion(cleanRel)
		}
		return
	}

	// Zombie file defense: if tombstone exists and file on disk is not newer than tombstone,
	// do NOT upload; purge zombie file from disk!
	if rec, exists := se.stateMgr.Get(cleanRel); exists && rec.Deleted {
		if info.ModTime().UnixNano() <= rec.ModTime {
			log.Printf("[SyncEngine] Suppressing upload of zombie deleted file %s (purging from disk)", cleanRel)
			_ = RobustRemove(fullPath)
			return
		}
		log.Printf("[SyncEngine] Detected genuine re-creation of file %s after deletion", cleanRel)
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

func (se *SyncEngine) handleLocalFileDeletion(relPath string) {
	cleanRel := filepath.Clean(relPath)
	fullPath := filepath.Join(se.sharedDir, cleanRel)

	// Verify file is genuinely deleted on disk
	if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
		return
	}

	// Check if this path was a directory containing tracked files
	normSlash := filepath.ToSlash(cleanRel)
	hasPrefixFiles := false
	for k := range se.stateMgr.GetAll() {
		kSlash := filepath.ToSlash(k)
		if strings.HasPrefix(kSlash, normSlash+"/") {
			hasPrefixFiles = true
			go se.handleLocalFileDeletion(k)
		}
	}
	if hasPrefixFiles {
		return
	}

	se.inFlightDeletesMu.Lock()
	if se.inFlightDeletes[cleanRel] {
		se.inFlightDeletesMu.Unlock()
		return
	}
	se.inFlightDeletes[cleanRel] = true
	se.inFlightDeletesMu.Unlock()

	defer func() {
		se.inFlightDeletesMu.Lock()
		delete(se.inFlightDeletes, cleanRel)
		se.inFlightDeletesMu.Unlock()
	}()

	// If already recorded as deleted, suppress re-broadcast
	if rec, exists := se.stateMgr.Get(cleanRel); exists && rec.Deleted {
		return
	}

	deleteTime := time.Now().UnixNano()
	_ = se.stateMgr.MarkAsDeleted(cleanRel, deleteTime)

	log.Printf("[SyncEngine] Detected locally deleted file: %s (recording tombstone)", cleanRel)
	se.broadcastDeleteToPeers(cleanRel, deleteTime, "")
}

func normalizeHostPort(rawURL string) string {
	raw := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		h := u.Hostname()
		p := u.Port()
		if p == "" {
			p = "8765"
		}
		return strings.ToLower(h + ":" + p)
	}
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "https://")
	if idx := strings.Index(raw, "/"); idx != -1 {
		raw = raw[:idx]
	}
	if strings.Contains(raw, ":") {
		return strings.ToLower(raw)
	}
	return strings.ToLower(raw + ":8765")
}

func (se *SyncEngine) broadcastDeleteToPeers(relPath string, modTime int64, excludePeer string) {
	peers := se.getPeerURLs()
	if len(peers) == 0 {
		return
	}

	cleanExclude := strings.TrimRight(strings.TrimSpace(excludePeer), "/")
	normExclude := normalizeHostPort(cleanExclude)

	for _, peer := range peers {
		if cleanExclude != "" && strings.EqualFold(peer, cleanExclude) {
			continue
		}
		if normExclude != "" && normalizeHostPort(peer) == normExclude {
			continue
		}
		go se.uploadDeleteToPeerWithRetry(peer, relPath, modTime)
	}
}

func (se *SyncEngine) uploadDeleteToPeerWithRetry(peerURL, relPath string, modTime int64) {
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-se.ctx.Done():
			return
		default:
		}

		err := se.uploadDelete(peerURL, relPath, modTime)
		if err == nil {
			log.Printf("[SyncEngine] Successfully propagated deletion of %s to peer %s", relPath, peerURL)
			return
		}

		if attempt < maxRetries-1 {
			delay := backoffSteps[attempt]
			log.Printf("[SyncEngine] Delete %s to %s failed (attempt %d/%d): %v. Retrying in %v...", relPath, peerURL, attempt+1, maxRetries, err, delay)
			select {
			case <-se.ctx.Done():
				return
			case <-time.After(delay):
			}

			// If file was recreated locally in the meantime, abort deletion retry!
			if !se.stateMgr.IsDeleted(relPath) {
				return
			}
		} else {
			log.Printf("[SyncEngine] Peer %s unreachable for deletion of %s. Will reconcile upon reconnection.", peerURL, relPath)
		}
	}
}

func (se *SyncEngine) uploadDelete(peerURL, relPath string, modTime int64) error {
	if peerURL == "" {
		return errors.New("peer URL is empty")
	}

	destURL := fmt.Sprintf("%s/api/sync?path=%s", peerURL, url.QueryEscape(filepath.ToSlash(relPath)))
	ctx, cancel := context.WithTimeout(se.ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "DELETE", destURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("X-Bridge-Token", se.authToken)
	req.Header.Set("X-Bridge-RelPath", filepath.ToSlash(relPath))
	req.Header.Set("X-Bridge-ModTime", strconv.FormatInt(modTime, 10))
	if localURL := se.GetLocalURL(); localURL != "" {
		req.Header.Set("X-Bridge-Sender", localURL)
		req.Header.Set("X-Bridge-Peer-URL", localURL)
	}

	resp, err := se.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("peer returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	return nil
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

// ReconcileWithPeer reconciles inventory with a peer to push missed files, pull remote files, and synchronize deletions.
func (se *SyncEngine) ReconcileWithPeer(peerURL string) {
	cleanPeer := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanPeer == "" {
		return
	}

	se.reconcileMu.Lock()
	if se.reconcilingPeer[cleanPeer] || se.ctx.Err() != nil {
		se.reconcileMu.Unlock()
		return
	}
	se.reconcilingPeer[cleanPeer] = true
	se.wg.Add(1)
	se.reconcileMu.Unlock()

	go func() {
		defer se.wg.Done()
		defer func() {
			se.reconcileMu.Lock()
			delete(se.reconcilingPeer, cleanPeer)
			se.reconcileMu.Unlock()
		}()

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

		// 1. Check local records against remote records
		for relPath, locRec := range localRecords {
			if se.ctx.Err() != nil {
				return
			}
			fullPath := filepath.Join(se.sharedDir, filepath.FromSlash(relPath))
			remRec, remExists := remoteRecords[relPath]
			if !remExists {
				if alt, ok := remoteRecords[filepath.ToSlash(relPath)]; ok {
					remRec = alt
					remExists = true
				}
			}

			if locRec.Deleted {
				// Local has a tombstone for this file. Ensure file is purged from disk.
				_ = RobustRemove(fullPath)
				if remExists && !remRec.Deleted {
					if locRec.ModTime >= remRec.ModTime {
						log.Printf("[SyncEngine] Catch-up reconciliation: pushing deletion of %s to peer %s", relPath, cleanPeer)
						_ = se.uploadDelete(cleanPeer, relPath, locRec.ModTime)
					} else {
						log.Printf("[SyncEngine] Catch-up reconciliation: pulling newer file %s from peer %s", relPath, cleanPeer)
						_ = se.downloadFile(cleanPeer, relPath, remRec.Hash, remRec.Size, remRec.ModTime)
					}
				}
				continue
			}

			// Local file is active
			info, err := os.Stat(fullPath)
			if err != nil || info.IsDir() {
				// Local file is missing from disk!
				go se.handleLocalFileDeletion(relPath)
				continue
			}

			diskModTime := info.ModTime().UnixNano()
			if info.Size() != locRec.Size || diskModTime != locRec.ModTime {
				if diskHash, diskSize, hashErr := ComputeHash(fullPath); hashErr == nil {
					locRec.Hash = diskHash
					locRec.Size = diskSize
					locRec.ModTime = diskModTime
					_ = se.stateMgr.Set(relPath, locRec)
				}
			}

			if !remExists {
				log.Printf("[SyncEngine] Catch-up reconciliation: pushing %s to online peer %s", relPath, cleanPeer)
				_ = se.uploadFile(cleanPeer, relPath, fullPath, locRec.Hash, locRec.Size, locRec.ModTime)
			} else if remRec.Deleted {
				if remRec.ModTime >= locRec.ModTime {
					log.Printf("[SyncEngine] Catch-up reconciliation: applying remote deletion of %s from peer %s", relPath, cleanPeer)
					_ = se.ReceiveSyncDelete(cleanPeer, relPath, remRec.ModTime)
				} else {
					log.Printf("[SyncEngine] Catch-up reconciliation: re-pushing newer file %s to peer %s", relPath, cleanPeer)
					_ = se.uploadFile(cleanPeer, relPath, fullPath, locRec.Hash, locRec.Size, locRec.ModTime)
				}
			} else if remRec.Hash != locRec.Hash {
				if locRec.ModTime >= remRec.ModTime {
					log.Printf("[SyncEngine] Catch-up reconciliation: pushing newer version of %s to peer %s", relPath, cleanPeer)
					_ = se.uploadFile(cleanPeer, relPath, fullPath, locRec.Hash, locRec.Size, locRec.ModTime)
				} else {
					log.Printf("[SyncEngine] Catch-up reconciliation: pulling newer version of %s from peer %s", relPath, cleanPeer)
					_ = se.downloadFile(cleanPeer, relPath, remRec.Hash, remRec.Size, remRec.ModTime)
				}
			}
		}

		// 2. Check remote records that local does NOT have
		for relPath, remRec := range remoteRecords {
			if se.ctx.Err() != nil {
				return
			}
			cleanRel := filepath.Clean(filepath.FromSlash(relPath))
			if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
				continue
			}
			if _, locExists := localRecords[cleanRel]; locExists {
				continue // Already processed
			}
			if _, locExists := localRecords[relPath]; locExists {
				continue // Already processed
			}

			fullPath := filepath.Join(se.sharedDir, cleanRel)
			if remRec.Deleted {
				// Remote has deleted tombstone; purge any stale file on disk and record tombstone locally
				_ = RobustRemove(fullPath)
				_ = se.stateMgr.MarkAsDeleted(cleanRel, remRec.ModTime)
				continue
			}

			info, err := os.Stat(fullPath)
			if os.IsNotExist(err) {
				log.Printf("[SyncEngine] Catch-up reconciliation: pulling %s from online peer %s", cleanRel, cleanPeer)
				_ = se.downloadFile(cleanPeer, cleanRel, remRec.Hash, remRec.Size, remRec.ModTime)
			} else if err == nil && !info.IsDir() {
				diskHash, diskSize, hashErr := ComputeHash(fullPath)
				if hashErr != nil {
					continue
				}
				diskModTime := info.ModTime().UnixNano()
				if diskHash == remRec.Hash {
					_ = se.stateMgr.MarkAsReceived(cleanRel, diskHash, diskSize, diskModTime)
				} else if remRec.ModTime > diskModTime {
					log.Printf("[SyncEngine] Catch-up reconciliation: pulling newer remote %s over untracked local file from peer %s", cleanRel, cleanPeer)
					_ = se.downloadFile(cleanPeer, cleanRel, remRec.Hash, remRec.Size, remRec.ModTime)
				} else {
					log.Printf("[SyncEngine] Catch-up reconciliation: pushing newer untracked local %s to peer %s", cleanRel, cleanPeer)
					_ = se.stateMgr.Set(cleanRel, FileRecord{
						RelPath: cleanRel,
						Hash:    diskHash,
						Size:    diskSize,
						ModTime: diskModTime,
					})
					_ = se.uploadFile(cleanPeer, cleanRel, fullPath, diskHash, diskSize, diskModTime)
				}
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

// SetLocalURL sets this node's URL for identifying sender in mesh propagation.
func (se *SyncEngine) SetLocalURL(u string) {
	se.peersMu.Lock()
	se.localURL = strings.TrimRight(strings.TrimSpace(u), "/")
	se.peersMu.Unlock()
}

// GetLocalURL returns this node's configured URL.
func (se *SyncEngine) GetLocalURL() string {
	se.peersMu.RLock()
	defer se.peersMu.RUnlock()
	return se.localURL
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

	f, err := OpenSharedFileForRead(fullPath)
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
	if localURL := se.GetLocalURL(); localURL != "" {
		req.Header.Set("X-Bridge-Sender", localURL)
		req.Header.Set("X-Bridge-Peer-URL", localURL)
	}

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

	partFile, err := os.CreateTemp(targetDir, filepath.Base(targetPath)+".*.part")
	if err != nil {
		return fmt.Errorf("failed to create part file: %w", err)
	}
	partPath := partFile.Name()

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

	_ = os.Chmod(partPath, 0644)

	// Set original modification time if provided
	if modTime > 0 {
		mtime := time.Unix(0, modTime)
		_ = os.Chtimes(partPath, mtime, mtime)
	}

	// Atomic replace with brief Windows file-lock retry
	if err := renameWithRetry(partPath, targetPath); err != nil {
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

// ReceiveSyncDelete on SyncEngine handles an incoming deletion request from a peer,
// purges the file from disk using RobustRemove, records the tombstone, and performs
// mesh routing relay to propagate the deletion across other connected peers.
func (se *SyncEngine) ReceiveSyncDelete(senderURL, relPath string, modTime int64) error {
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		return errors.New("invalid or malicious relative path")
	}

	targetPath := filepath.Join(se.sharedDir, cleanRel)

	if modTime <= 0 {
		modTime = time.Now().UnixNano()
	}

	// Suppress duplicate processing if already tombstoned with newer/same timestamp
	if rec, exists := se.stateMgr.Get(cleanRel); exists && rec.Deleted && rec.ModTime >= modTime {
		_ = RobustRemove(targetPath)
		return nil
	}

	// Record tombstone first to prevent watcher or local loops
	if err := se.stateMgr.MarkAsDeleted(cleanRel, modTime); err != nil {
		log.Printf("[SyncEngine] Warning: failed to save deletion state for %s: %v", cleanRel, err)
	}

	// Purge from disk using RobustRemove (clears read-only flag, retries on file locks)
	if err := RobustRemove(targetPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[SyncEngine] Warning: robust remove failed for %s: %v", targetPath, err)
	}

	log.Printf("[SyncEngine] Successfully received deletion and placed tombstone for %s", cleanRel)

	// Mesh Routing Relay: Propagate deletion to all other peers in the mesh
	se.broadcastDeleteToPeers(cleanRel, modTime, senderURL)
	return nil
}

// ReceiveSyncDelete handles an incoming deletion request from a peer (standalone function for backward compatibility).
func ReceiveSyncDelete(sharedDir string, stateMgr *StateManager, relPath string, modTime int64) error {
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		return errors.New("invalid or malicious relative path")
	}

	targetPath := filepath.Join(sharedDir, cleanRel)

	if modTime <= 0 {
		modTime = time.Now().UnixNano()
	}

	// Record tombstone first to prevent watcher or local loops
	if err := stateMgr.MarkAsDeleted(cleanRel, modTime); err != nil {
		log.Printf("[SyncEngine] Warning: failed to save deletion state for %s: %v", cleanRel, err)
	}

	if err := RobustRemove(targetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file %s: %w", targetPath, err)
	}

	log.Printf("[SyncEngine] Successfully received deletion and placed tombstone for %s", cleanRel)
	return nil
}
