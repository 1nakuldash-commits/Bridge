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

const (
	defaultDeleteDebounce = 350 * time.Millisecond
	conflictTimeWindow    = 60 * time.Second
	tombstoneMaxAge       = 30 * 24 * time.Hour
)

// SyncEngine coordinates folder watching, stabilization, and multi-peer mesh synchronization.
type SyncEngine struct {
	sharedDir string
	localURL  string
	nodeName  string
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
	inFlightDirty     map[string]bool
	receivingCount    map[string]int
	inFlightDeletesMu sync.Mutex
	inFlightDeletes   map[string]bool
	pendingDeletesMu  sync.Mutex
	pendingDeletes    map[string]*time.Timer
	deleteDebounce    time.Duration

	reconcileMu     sync.Mutex
	reconcilingPeer map[string]bool
	reconcileDone   map[string]chan struct{}

	dirtyPeersMu sync.Mutex
	dirtyPeers   map[string]bool

	stopMu    sync.RWMutex
	stopped   bool
	watcherMu sync.Mutex
	watcher   *fsnotify.Watcher

	callbackMu      sync.RWMutex
	OnSyncStarted   func(relPath string)
	OnSyncCompleted func(relPath string)
	OnSyncFailed    func(relPath string, err error)
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
		sharedDir:       filepath.Clean(sharedDir),
		peerURLs:        make(map[string]bool),
		authToken:       authToken,
		stateMgr:        stateMgr,
		client:          &http.Client{Transport: transport},
		ctx:             ctx,
		cancel:          cancel,
		inFlight:        make(map[string]bool),
		inFlightDirty:   make(map[string]bool),
		receivingCount:  make(map[string]int),
		inFlightDeletes: make(map[string]bool),
		pendingDeletes:  make(map[string]*time.Timer),
		deleteDebounce:  defaultDeleteDebounce,
		reconcilingPeer: make(map[string]bool),
		reconcileDone:   make(map[string]chan struct{}),
		dirtyPeers:      make(map[string]bool),
	}

	if clean := strings.TrimRight(strings.TrimSpace(initialPeerURL), "/"); clean != "" {
		se.peerURLs[clean] = true
	}

	return se
}

// SetNodeName sets this node's friendly name used in conflict copy filenames.
func (se *SyncEngine) SetNodeName(name string) {
	se.peersMu.Lock()
	se.nodeName = strings.TrimSpace(name)
	se.peersMu.Unlock()
}

// GetNodeName returns this node's friendly name.
func (se *SyncEngine) GetNodeName() string {
	se.peersMu.RLock()
	defer se.peersMu.RUnlock()
	if se.nodeName != "" {
		return se.nodeName
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}

// SetOnSyncStarted registers the sync-started callback in a thread-safe manner.
func (se *SyncEngine) SetOnSyncStarted(fn func(relPath string)) {
	se.callbackMu.Lock()
	se.OnSyncStarted = fn
	se.callbackMu.Unlock()
}

// SetOnSyncCompleted registers the sync-completed callback in a thread-safe manner.
func (se *SyncEngine) SetOnSyncCompleted(fn func(relPath string)) {
	se.callbackMu.Lock()
	se.OnSyncCompleted = fn
	se.callbackMu.Unlock()
}

// SetOnSyncFailed registers the sync-failed callback in a thread-safe manner.
func (se *SyncEngine) SetOnSyncFailed(fn func(relPath string, err error)) {
	se.callbackMu.Lock()
	se.OnSyncFailed = fn
	se.callbackMu.Unlock()
}

func (se *SyncEngine) notifySyncStarted(relPath string) {
	se.callbackMu.RLock()
	fn := se.OnSyncStarted
	se.callbackMu.RUnlock()
	if fn != nil {
		fn(relPath)
	}
}

func (se *SyncEngine) notifySyncCompleted(relPath string) {
	se.callbackMu.RLock()
	fn := se.OnSyncCompleted
	se.callbackMu.RUnlock()
	if fn != nil {
		fn(relPath)
	}
}

func (se *SyncEngine) notifySyncFailed(relPath string, err error) {
	se.callbackMu.RLock()
	fn := se.OnSyncFailed
	se.callbackMu.RUnlock()
	if fn != nil {
		fn(relPath, err)
	}
}

// validateSyncRelPath validates a relative sync path against empty, ".", "..", VolumeName,
// absolute paths, and symlink escapes outside sharedDir.
func validateSyncRelPath(sharedDir, relPath string) (string, string, error) {
	if strings.TrimSpace(relPath) == "" {
		return "", "", errors.New("invalid or malicious relative path: empty path")
	}
	if strings.HasPrefix(relPath, "/") || strings.HasPrefix(relPath, "\\") {
		return "", "", errors.New("invalid or malicious relative path: leading separator")
	}
	slashRaw := filepath.ToSlash(relPath)
	for _, seg := range strings.Split(slashRaw, "/") {
		if seg == ".." {
			return "", "", errors.New("invalid or malicious relative path: traversal segment")
		}
	}

	native := filepath.FromSlash(relPath)
	if filepath.VolumeName(native) != "" || filepath.IsAbs(native) {
		return "", "", errors.New("invalid or malicious relative path: absolute or volume path")
	}

	cleanRel := filepath.Clean(native)
	if cleanRel == "" || cleanRel == "." || cleanRel == ".." ||
		strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) ||
		filepath.VolumeName(cleanRel) != "" {
		return "", "", errors.New("invalid or malicious relative path")
	}

	cleanRoot := filepath.Clean(sharedDir)
	targetPath := filepath.Join(cleanRoot, cleanRel)
	relCheck, err := filepath.Rel(cleanRoot, targetPath)
	if err != nil || relCheck == "" || relCheck == "." || strings.HasPrefix(relCheck, "..") {
		return "", "", errors.New("invalid or malicious relative path: escapes shared directory")
	}

	if err := verifyNoSymlinkEscape(cleanRoot, targetPath); err != nil {
		return "", "", err
	}

	return cleanRel, targetPath, nil
}

func verifyNoSymlinkEscape(cleanRoot, targetPath string) error {
	resolvedRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to resolve shared directory: %w", err)
	}

	// Check if target itself is a symlink
	if linfo, lerr := os.Lstat(targetPath); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
		resolvedTarget, rerr := filepath.EvalSymlinks(targetPath)
		if rerr != nil {
			return errors.New("invalid or malicious relative path: unresolved symlink")
		}
		rel, rerr := filepath.Rel(resolvedRoot, resolvedTarget)
		if rerr != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return errors.New("invalid or malicious relative path: symlink escapes shared directory")
		}
	}

	// Walk ancestors from targetPath up to cleanRoot to catch directory symlinks
	curr := targetPath
	for {
		if curr == cleanRoot || len(curr) <= len(cleanRoot) {
			break
		}
		if _, serr := os.Lstat(curr); serr == nil {
			resolvedCurr, rerr := filepath.EvalSymlinks(curr)
			if rerr != nil {
				return errors.New("invalid or malicious relative path: unresolved symlink ancestor")
			}
			rel, rerr := filepath.Rel(resolvedRoot, resolvedCurr)
			if rerr != nil || strings.HasPrefix(rel, "..") {
				return errors.New("invalid or malicious relative path: symlink ancestor escapes shared directory")
			}
			break
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}
	return nil
}

func (se *SyncEngine) spawnTracked(fn func()) bool {
	se.stopMu.RLock()
	if se.stopped || se.ctx.Err() != nil {
		se.stopMu.RUnlock()
		return false
	}
	se.wg.Add(1)
	se.stopMu.RUnlock()

	go func() {
		defer se.wg.Done()
		fn()
	}()
	return true
}

func (se *SyncEngine) isInFlightOrReceiving(cleanRel string) bool {
	key := normalizeStateKey(cleanRel)
	se.inFlightMu.Lock()
	defer se.inFlightMu.Unlock()
	return se.inFlight[key] || se.receivingCount[key] > 0
}

func (se *SyncEngine) markPeerDirty(peerURL string) {
	clean := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if clean == "" {
		return
	}
	se.dirtyPeersMu.Lock()
	se.dirtyPeers[clean] = true
	se.dirtyPeersMu.Unlock()
}

func (se *SyncEngine) consumePeerDirty(peerURL string) bool {
	clean := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if clean == "" {
		return false
	}
	se.dirtyPeersMu.Lock()
	dirty := se.dirtyPeers[clean]
	delete(se.dirtyPeers, clean)
	se.dirtyPeersMu.Unlock()
	return dirty
}

// IsPeerDirty reports whether a peer has pending missed sync operations requiring reconciliation.
func (se *SyncEngine) IsPeerDirty(peerURL string) bool {
	clean := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	se.dirtyPeersMu.Lock()
	defer se.dirtyPeersMu.Unlock()
	return se.dirtyPeers[clean]
}

// MarkPeerDirty marks a peer as needing catch-up reconciliation.
func (se *SyncEngine) MarkPeerDirty(peerURL string) {
	se.markPeerDirty(peerURL)
}

func (se *SyncEngine) getDirtyPeers() []string {
	se.dirtyPeersMu.Lock()
	defer se.dirtyPeersMu.Unlock()
	res := make([]string, 0, len(se.dirtyPeers))
	for p := range se.dirtyPeers {
		res = append(res, p)
	}
	return res
}

func (se *SyncEngine) removeWatcher(dirPath string) {
	se.watcherMu.Lock()
	w := se.watcher
	se.watcherMu.Unlock()
	if w != nil {
		_ = w.Remove(dirPath)
	}
}

func (se *SyncEngine) cleanupOrphanedTempFiles() {
	_ = filepath.Walk(se.sharedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".part") || strings.HasSuffix(path, ".tmp") {
			if remErr := RobustRemove(path); remErr == nil {
				log.Printf("[SyncEngine] Cleaned up orphaned temp file on startup: %s", path)
			}
		}
		return nil
	})
}

// Start begins watching the shared directory and running the periodic scan.
func (se *SyncEngine) Start() error {
	if err := os.MkdirAll(se.sharedDir, 0755); err != nil {
		return fmt.Errorf("failed to create shared directory: %w", err)
	}

	se.cleanupOrphanedTempFiles()
	if se.stateMgr != nil {
		if pruned, err := se.stateMgr.PruneTombstones(tombstoneMaxAge); err == nil && pruned > 0 {
			log.Printf("[SyncEngine] Pruned %d expired tombstones on startup", pruned)
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}

	if err := se.addWatchersRecursive(watcher, se.sharedDir); err != nil {
		watcher.Close()
		return fmt.Errorf("failed to watch shared directory: %w", err)
	}

	se.watcherMu.Lock()
	se.watcher = watcher
	se.watcherMu.Unlock()

	se.wg.Add(1)
	go se.watchLoop(watcher)

	se.wg.Add(1)
	go se.scanLoop()

	log.Printf("[SyncEngine] Started watching %s (peers: %v)", se.sharedDir, se.getPeerURLs())
	return nil
}

// Stop halts the sync engine gracefully.
func (se *SyncEngine) Stop() {
	se.stopMu.Lock()
	se.stopped = true
	se.cancel()
	se.stopMu.Unlock()

	se.pendingDeletesMu.Lock()
	for k, timer := range se.pendingDeletes {
		timer.Stop()
		delete(se.pendingDeletes, k)
	}
	se.pendingDeletesMu.Unlock()

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
		if err != nil || strings.HasPrefix(relPath, "..") || relPath == "." {
			return nil
		}
		se.cancelPendingDeletion(relPath)
		if rec, exists := se.stateMgr.Get(relPath); exists && !rec.Deleted {
			diskMod := info.ModTime().UnixNano()
			if rec.Size == info.Size() && (rec.ModTime == diskMod || (rec.DiskModTime != 0 && rec.DiskModTime == diskMod)) {
				return nil
			}
		}
		se.spawnTracked(func() {
			se.handleLocalFileEvent(relPath, true)
		})
		return nil
	})
}

func (se *SyncEngine) cancelPendingDeletion(relPath string) {
	key := normalizeStateKey(relPath)
	if key == "" {
		return
	}
	se.pendingDeletesMu.Lock()
	if timer, ok := se.pendingDeletes[key]; ok {
		timer.Stop()
		delete(se.pendingDeletes, key)
	}
	se.pendingDeletesMu.Unlock()
}

func (se *SyncEngine) scheduleLocalDeletion(relPath string) {
	cleanRel, _, err := validateSyncRelPath(se.sharedDir, relPath)
	if err != nil {
		return
	}
	key := normalizeStateKey(cleanRel)
	delay := se.deleteDebounce
	if delay <= 0 {
		delay = defaultDeleteDebounce
	}

	se.stopMu.RLock()
	stopped := se.stopped || se.ctx.Err() != nil
	se.stopMu.RUnlock()
	if stopped {
		return
	}

	se.pendingDeletesMu.Lock()
	if prev, ok := se.pendingDeletes[key]; ok {
		prev.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		se.pendingDeletesMu.Lock()
		if cur, ok := se.pendingDeletes[key]; ok && cur == timer {
			delete(se.pendingDeletes, key)
		} else {
			se.pendingDeletesMu.Unlock()
			return
		}
		se.pendingDeletesMu.Unlock()

		se.spawnTracked(func() {
			se.handleLocalFileDeletion(cleanRel)
		})
	})
	se.pendingDeletes[key] = timer
	se.pendingDeletesMu.Unlock()
}

// watchLoop handles fsnotify events.
func (se *SyncEngine) watchLoop(watcher *fsnotify.Watcher) {
	defer se.wg.Done()
	defer func() {
		se.watcherMu.Lock()
		if se.watcher == watcher {
			se.watcher = nil
		}
		se.watcherMu.Unlock()
		watcher.Close()
	}()

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
					se.spawnTracked(func() {
						_ = se.addWatchersAndScanDir(watcher, event.Name)
					})
					continue
				}
			}

			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				relPath, err := filepath.Rel(se.sharedDir, event.Name)
				if err == nil && !strings.HasPrefix(relPath, "..") && relPath != "." {
					se.scheduleLocalDeletion(relPath)
				}
				continue
			}

			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
				if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
					continue
				}
				relPath, err := filepath.Rel(se.sharedDir, event.Name)
				if err == nil && !strings.HasPrefix(relPath, "..") && relPath != "." {
					se.cancelPendingDeletion(relPath)
					isCreate := event.Has(fsnotify.Create)
					se.spawnTracked(func() {
						se.handleLocalFileEvent(relPath, isCreate)
					})
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

// scanLoop runs a fallback scan every 30 seconds to catch missed events,
// reconcile dirty/recovered peers, and periodically prune old tombstones.
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

	tickCount := 0
	for {
		select {
		case <-se.ctx.Done():
			return
		case <-ticker.C:
			tickCount++
			se.scanDirectory()

			// Reconcile any dirty peers on every 30s tick, and all peers every 60s
			if tickCount%2 == 0 {
				for _, peer := range se.getPeerURLs() {
					se.ReconcileWithPeer(peer)
				}
			} else {
				for _, peer := range se.getDirtyPeers() {
					se.ReconcileWithPeer(peer)
				}
			}

			if tickCount%20 == 0 && se.stateMgr != nil {
				_, _ = se.stateMgr.PruneTombstones(tombstoneMaxAge)
			}
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
		if err != nil || strings.HasPrefix(relPath, "..") || relPath == "." {
			return nil
		}
		if _, _, vErr := validateSyncRelPath(se.sharedDir, relPath); vErr != nil {
			return nil
		}

		// Quick check: if recorded in state with same size and modtime and not deleted, skip computing hash.
		// If rec.Deleted is true, treat the file on disk as a newly pasted/recreated local file
		// rather than destructively auto-deleting user files with older mtimes.
		if rec, exists := se.stateMgr.Get(relPath); exists && !rec.Deleted {
			diskMod := info.ModTime().UnixNano()
			if rec.Size == info.Size() && (rec.ModTime == diskMod || (rec.DiskModTime != 0 && rec.DiskModTime == diskMod)) {
				return nil
			}
		}

		se.spawnTracked(func() {
			se.handleLocalFileEvent(relPath, true)
		})
		return nil
	})

	// Detect any files recorded in state that were deleted on disk and propagate deletion
	for relPath, rec := range se.stateMgr.GetAll() {
		if rec.Deleted {
			continue
		}
		cleanRel, fullPath, err := validateSyncRelPath(se.sharedDir, relPath)
		if err != nil {
			continue
		}
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			rp := cleanRel
			se.spawnTracked(func() {
				se.handleLocalFileDeletion(rp)
			})
		}
	}
}

type localScanDelta struct {
	updated []FileRecord
	deleted []FileRecord
}

func (se *SyncEngine) scanDirectorySync() localScanDelta {
	var delta localScanDelta

	_ = filepath.Walk(se.sharedDir, func(path string, info os.FileInfo, err error) error {
		if se.ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil || info.IsDir() {
			return nil
		}

		if strings.HasSuffix(path, ".part") || strings.HasSuffix(path, ".tmp") {
			return nil
		}

		relPath, err := filepath.Rel(se.sharedDir, path)
		if err != nil || strings.HasPrefix(relPath, "..") || relPath == "." {
			return nil
		}
		cleanRel, _, vErr := validateSyncRelPath(se.sharedDir, relPath)
		if vErr != nil {
			return nil
		}
		slashRel := normalizeStateKey(cleanRel)

		if se.isInFlightOrReceiving(cleanRel) {
			return nil
		}

		diskModTime := info.ModTime().UnixNano()
		rec, exists := se.stateMgr.Get(slashRel)
		if exists && !rec.Deleted {
			if rec.Size == info.Size() && (rec.ModTime == diskModTime || (rec.DiskModTime != 0 && rec.DiskModTime == diskModTime)) {
				return nil
			}
		}

		hash, size, hashErr := ComputeHash(path)
		if hashErr != nil {
			return nil
		}

		effectiveModTime := diskModTime
		if exists && rec.Deleted && effectiveModTime <= rec.ModTime {
			effectiveModTime = rec.ModTime + int64(time.Millisecond)
		}

		newRec := FileRecord{
			RelPath:     slashRel,
			Hash:        hash,
			Size:        size,
			ModTime:     effectiveModTime,
			DiskModTime: diskModTime,
		}
		if err := se.stateMgr.Set(slashRel, newRec); err == nil {
			delta.updated = append(delta.updated, newRec)
		}
		return nil
	})

	for relPath, rec := range se.stateMgr.GetAll() {
		if se.ctx.Err() != nil {
			break
		}
		if rec.Deleted {
			continue
		}
		cleanRel, fullPath, err := validateSyncRelPath(se.sharedDir, relPath)
		if err != nil {
			continue
		}
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			if !se.isInFlightOrReceiving(cleanRel) {
				deleteTime := time.Now().UnixNano()
				_ = se.stateMgr.MarkAsDeleted(relPath, deleteTime)
				delta.deleted = append(delta.deleted, FileRecord{
					RelPath: normalizeStateKey(cleanRel),
					ModTime: deleteTime,
					Deleted: true,
				})
			}
		}
	}

	return delta
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// WaitForStableFile ensures the file size does not change for 500ms and is readable.
func WaitForStableFile(ctx context.Context, fullPath string) (os.FileInfo, error) {
	const checkInterval = 500 * time.Millisecond
	const maxWait = 60 * time.Second
	deadline := time.Now().Add(maxWait)

	var lastSize int64 = -1

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for file %s to stabilize", fullPath)
		}

		info, err := os.Stat(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, err
			}
			if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}

		if info.IsDir() {
			return info, nil
		}

		// Try opening for read to ensure no exclusive locks
		f, err := OpenSharedFileForRead(fullPath)
		if err != nil {
			// File is still being written or locked
			if err := sleepWithContext(ctx, 200*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}
		_ = f.Close()

		currentSize := info.Size()
		if lastSize != -1 && currentSize == lastSize {
			// Size remained identical across the check interval
			return info, nil
		}

		lastSize = currentSize
		if err := sleepWithContext(ctx, checkInterval); err != nil {
			return nil, err
		}
	}
}

func (se *SyncEngine) handleLocalFileChange(relPath string) {
	se.handleLocalFileEvent(relPath, false)
}

func (se *SyncEngine) handleLocalFileEvent(relPath string, fromCreate bool) {
	cleanRel, fullPath, err := validateSyncRelPath(se.sharedDir, relPath)
	if err != nil {
		return
	}
	key := normalizeStateKey(cleanRel)

	se.inFlightMu.Lock()
	if se.receivingCount[key] > 0 {
		se.inFlightMu.Unlock()
		return
	}
	if se.inFlight[key] {
		se.inFlightDirty[key] = true
		se.inFlightMu.Unlock()
		return
	}
	se.inFlight[key] = true
	delete(se.inFlightDirty, key)
	se.inFlightMu.Unlock()

	defer func() {
		se.inFlightMu.Lock()
		delete(se.inFlight, key)
		delete(se.inFlightDirty, key)
		se.inFlightMu.Unlock()
	}()

	for {
		if se.ctx.Err() != nil {
			return
		}

		info, err := WaitForStableFile(se.ctx, fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				if rec, exists := se.stateMgr.Get(key); exists && !rec.Deleted {
					se.handleLocalFileDeletion(cleanRel)
				}
			}
			return
		}
		if info.IsDir() {
			return
		}

		diskModTime := info.ModTime().UnixNano()
		uploadModTime := diskModTime

		// If a tombstone exists for this path, do NOT delete the local file or mutate its
		// disk mtime via os.Chtimes; advance only the logical sync ModTime so peers accept it.
		if rec, exists := se.stateMgr.Get(key); exists && rec.Deleted {
			if uploadModTime <= rec.ModTime {
				uploadModTime = rec.ModTime + int64(time.Millisecond)
			}
			log.Printf("[SyncEngine] Detected genuine local file %s over prior tombstone (logical modtime=%d)", key, uploadModTime)
		}

		hash, size, err := ComputeHash(fullPath)
		if err != nil {
			log.Printf("[SyncEngine] Failed to compute hash for %s: %v", key, err)
		} else if !se.stateMgr.IsSynced(key, hash) {
			log.Printf("[SyncEngine] Detected modified/new file %s (size: %d, sha256: %s)", key, size, hash[:8])
			se.broadcastToPeers(key, fullPath, hash, size, uploadModTime, diskModTime)
		}

		se.inFlightMu.Lock()
		dirty := se.inFlightDirty[key]
		delete(se.inFlightDirty, key)
		if !dirty {
			se.inFlightMu.Unlock()
			return
		}
		se.inFlightMu.Unlock()
		_ = fromCreate
	}
}

func (se *SyncEngine) handleLocalFileDeletion(relPath string) {
	cleanRel, fullPath, err := validateSyncRelPath(se.sharedDir, relPath)
	if err != nil {
		return
	}
	key := normalizeStateKey(cleanRel)

	// Verify file is genuinely deleted on disk
	if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
		return
	}

	// Release any directory watch handle for removed/renamed folders
	se.removeWatcher(fullPath)

	// Check if this path was a directory containing tracked files
	hasPrefixFiles := false
	for k, childRec := range se.stateMgr.GetAll() {
		kSlash := normalizeStateKey(k)
		if strings.HasPrefix(kSlash, key+"/") {
			hasPrefixFiles = true
			if !childRec.Deleted {
				childPath := kSlash
				se.spawnTracked(func() {
					se.handleLocalFileDeletion(childPath)
				})
			}
		}
	}
	if hasPrefixFiles {
		return
	}

	se.inFlightDeletesMu.Lock()
	if se.inFlightDeletes[key] {
		se.inFlightDeletesMu.Unlock()
		return
	}
	se.inFlightDeletes[key] = true
	se.inFlightDeletesMu.Unlock()

	defer func() {
		se.inFlightDeletesMu.Lock()
		delete(se.inFlightDeletes, key)
		se.inFlightDeletesMu.Unlock()
	}()

	// Only tombstone and propagate paths that were actually tracked as active files.
	rec, exists := se.stateMgr.Get(key)
	if !exists || rec.Deleted {
		return
	}

	deleteTime := time.Now().UnixNano()
	_ = se.stateMgr.MarkAsDeleted(key, deleteTime)

	log.Printf("[SyncEngine] Detected locally deleted file: %s (recording tombstone)", key)
	se.broadcastDeleteToPeers(key, deleteTime, "")
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

func shouldExcludePeer(peer, excludePeer string) bool {
	cleanExclude := strings.TrimRight(strings.TrimSpace(excludePeer), "/")
	if cleanExclude == "" {
		return false
	}
	if strings.EqualFold(peer, cleanExclude) {
		return true
	}
	normExclude := normalizeHostPort(cleanExclude)
	return normExclude != "" && normalizeHostPort(peer) == normExclude
}

func (se *SyncEngine) broadcastDeleteToPeers(relPath string, modTime int64, excludePeer string) {
	peers := se.getPeerURLs()
	if len(peers) == 0 {
		return
	}

	for _, peer := range peers {
		if shouldExcludePeer(peer, excludePeer) {
			continue
		}
		p := peer
		se.spawnTracked(func() {
			se.uploadDeleteToPeerWithRetry(p, relPath, modTime)
		})
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
			if se.consumePeerDirty(peerURL) {
				se.ReconcileWithPeer(peerURL)
			}
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
			se.markPeerDirty(peerURL)
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

func (se *SyncEngine) broadcastToPeers(relPath, fullPath, hash string, size, modTime, diskModTime int64) {
	// Immediately mark in local state so subsequent scans or watcher events see this version as processed
	_ = se.stateMgr.Set(relPath, FileRecord{
		RelPath:     normalizeStateKey(relPath),
		Hash:        hash,
		Size:        size,
		ModTime:     modTime,
		DiskModTime: diskModTime,
	})

	peers := se.getPeerURLs()
	if len(peers) == 0 {
		return
	}

	se.notifySyncStarted(relPath)

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for _, peer := range peers {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if err := se.uploadToPeerWithRetry(p, relPath, fullPath, hash, size, modTime); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(peer)
	}

	finish := func() {
		wg.Wait()
		if firstErr != nil {
			se.notifySyncFailed(relPath, firstErr)
		} else {
			se.notifySyncCompleted(relPath)
		}
	}
	if !se.spawnTracked(finish) {
		finish()
	}
}

func (se *SyncEngine) broadcastRelayFileToPeers(relPath, fullPath, hash string, size, modTime int64, excludePeer string) {
	peers := se.getPeerURLs()
	if len(peers) == 0 {
		return
	}

	for _, peer := range peers {
		if shouldExcludePeer(peer, excludePeer) {
			continue
		}
		p := peer
		se.spawnTracked(func() {
			_ = se.uploadToPeerWithRetry(p, relPath, fullPath, hash, size, modTime)
		})
	}
}

func (se *SyncEngine) uploadToPeerWithRetry(peerURL, relPath, fullPath, hash string, size, modTime int64) error {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-se.ctx.Done():
			return se.ctx.Err()
		default:
		}

		err := se.uploadFile(peerURL, relPath, fullPath, hash, size, modTime)
		if err == nil {
			log.Printf("[SyncEngine] Successfully synced %s to peer %s", relPath, peerURL)
			if se.consumePeerDirty(peerURL) {
				se.ReconcileWithPeer(peerURL)
			}
			return nil
		}
		lastErr = err

		if attempt < maxRetries-1 {
			delay := backoffSteps[attempt]
			log.Printf("[SyncEngine] Sync %s to %s failed (attempt %d/%d): %v. Retrying in %v...", relPath, peerURL, attempt+1, maxRetries, err, delay)
			select {
			case <-se.ctx.Done():
				return se.ctx.Err()
			case <-time.After(delay):
			}

			// Re-verify file still exists and has same hash before retrying
			currHash, _, err := ComputeHash(fullPath)
			if err != nil || currHash != hash {
				return nil
			}
		} else {
			log.Printf("[SyncEngine] Peer %s unreachable after %d attempts for %s. Will reconcile upon reconnection.", peerURL, maxRetries, relPath)
			se.markPeerDirty(peerURL)
		}
	}
	return lastErr
}

// ResyncNow synchronously scans the shared directory for local changes and
// reconciles inventory across all configured peers.
func (se *SyncEngine) ResyncNow() {
	se.stopMu.RLock()
	if se.stopped || se.ctx.Err() != nil {
		se.stopMu.RUnlock()
		return
	}
	se.wg.Add(1)
	se.stopMu.RUnlock()
	defer se.wg.Done()

	se.scanDirectorySync()

	peers := se.getPeerURLs()
	if len(peers) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, peer := range peers {
		cleanPeer := strings.TrimRight(strings.TrimSpace(peer), "/")
		if cleanPeer == "" {
			continue
		}
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			se.waitAndReconcilePeer(p, false)
		}(cleanPeer)
	}
	wg.Wait()
}

func (se *SyncEngine) waitAndReconcilePeer(cleanPeer string, runScan bool) {
	for {
		if se.ctx.Err() != nil {
			return
		}
		se.reconcileMu.Lock()
		if se.reconcilingPeer[cleanPeer] {
			doneCh := se.reconcileDone[cleanPeer]
			se.reconcileMu.Unlock()
			if doneCh != nil {
				select {
				case <-doneCh:
				case <-se.ctx.Done():
					return
				}
			} else {
				return
			}
			continue
		}
		doneCh := make(chan struct{})
		se.reconcilingPeer[cleanPeer] = true
		se.reconcileDone[cleanPeer] = doneCh
		se.reconcileMu.Unlock()

		se.executeReconcilePeer(cleanPeer, runScan)

		se.reconcileMu.Lock()
		delete(se.reconcilingPeer, cleanPeer)
		delete(se.reconcileDone, cleanPeer)
		close(doneCh)
		se.reconcileMu.Unlock()
		return
	}
}

// ReconcileWithPeer reconciles inventory with a peer to push missed files, pull remote files, and synchronize deletions.
func (se *SyncEngine) ReconcileWithPeer(peerURL string) {
	cleanPeer := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanPeer == "" {
		return
	}

	se.stopMu.RLock()
	if se.stopped || se.ctx.Err() != nil {
		se.stopMu.RUnlock()
		return
	}
	se.reconcileMu.Lock()
	if se.reconcilingPeer[cleanPeer] {
		se.reconcileMu.Unlock()
		se.stopMu.RUnlock()
		return
	}
	doneCh := make(chan struct{})
	se.reconcilingPeer[cleanPeer] = true
	se.reconcileDone[cleanPeer] = doneCh
	se.wg.Add(1)
	se.reconcileMu.Unlock()
	se.stopMu.RUnlock()

	go func() {
		defer se.wg.Done()
		defer func() {
			se.reconcileMu.Lock()
			delete(se.reconcilingPeer, cleanPeer)
			delete(se.reconcileDone, cleanPeer)
			close(doneCh)
			se.reconcileMu.Unlock()
		}()

		se.executeReconcilePeer(cleanPeer, true)
	}()
}

func sanitizeConflictNode(node string) string {
	clean := strings.TrimSpace(node)
	if clean == "" {
		clean = "local"
	}
	replacer := strings.NewReplacer(
		":", "_",
		"/", "_",
		"\\", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	return replacer.Replace(clean)
}

func (se *SyncEngine) preserveConflictCopy(cleanRel, fullPath string) (string, error) {
	dir := filepath.Dir(cleanRel)
	base := filepath.Base(cleanRel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	nodeLabel := sanitizeConflictNode(se.GetNodeName())
	ts := time.Now().Format("20060102-150405")

	var conflictRel, conflictFull string
	for attempt := 0; attempt < 100; attempt++ {
		suffix := ""
		if attempt > 0 {
			suffix = fmt.Sprintf("-%d", attempt)
		}
		name := fmt.Sprintf("%s (conflict %s %s%s)%s", stem, nodeLabel, ts, suffix, ext)
		if dir != "" && dir != "." {
			conflictRel = filepath.Join(dir, name)
		} else {
			conflictRel = name
		}
		conflictFull = filepath.Join(se.sharedDir, conflictRel)
		if _, err := os.Stat(conflictFull); os.IsNotExist(err) {
			break
		}
	}

	src, err := OpenSharedFileForRead(fullPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.OpenFile(conflictFull, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(conflictFull)
		return "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(conflictFull)
		return "", err
	}

	if info, err := os.Stat(fullPath); err == nil {
		_ = os.Chtimes(conflictFull, info.ModTime(), info.ModTime())
	}

	if hash, size, err := ComputeHash(conflictFull); err == nil {
		var mod int64
		if info, serr := os.Stat(conflictFull); serr == nil {
			mod = info.ModTime().UnixNano()
		}
		_ = se.stateMgr.Set(conflictRel, FileRecord{
			RelPath:     normalizeStateKey(conflictRel),
			Hash:        hash,
			Size:        size,
			ModTime:     mod,
			DiskModTime: mod,
		})
	}
	log.Printf("[SyncEngine] Preserved conflict copy %s before overwriting %s", normalizeStateKey(conflictRel), normalizeStateKey(cleanRel))
	return conflictRel, nil
}

func isCloseConflict(remModTime, locModTime int64, diskModifiedLocally bool) bool {
	if diskModifiedLocally {
		return true
	}
	diff := remModTime - locModTime
	if diff < 0 {
		diff = -diff
	}
	return time.Duration(diff) <= conflictTimeWindow
}

func (se *SyncEngine) executeReconcilePeer(cleanPeer string, runScan bool) {
	if runScan {
		delta := se.scanDirectorySync()
		for _, peer := range se.getPeerURLs() {
			if strings.EqualFold(peer, cleanPeer) {
				continue
			}
			p := peer
			for _, rec := range delta.updated {
				r := rec
				fp := filepath.Join(se.sharedDir, filepath.FromSlash(r.RelPath))
				se.spawnTracked(func() {
					_ = se.uploadToPeerWithRetry(p, r.RelPath, fp, r.Hash, r.Size, r.ModTime)
				})
			}
			for _, rec := range delta.deleted {
				r := rec
				se.spawnTracked(func() {
					se.uploadDeleteToPeerWithRetry(p, r.RelPath, r.ModTime)
				})
			}
		}
	}

	ctx, cancel := context.WithTimeout(se.ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/sync/inventory", cleanPeer), nil)
	if err != nil {
		se.markPeerDirty(cleanPeer)
		return
	}
	req.Header.Set("X-Bridge-Token", se.authToken)

	resp, err := se.client.Do(req)
	if err != nil {
		se.markPeerDirty(cleanPeer)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		se.markPeerDirty(cleanPeer)
		return
	}

	var rawRemote map[string]FileRecord
	if err := json.NewDecoder(resp.Body).Decode(&rawRemote); err != nil {
		se.markPeerDirty(cleanPeer)
		return
	}

	remoteRecords := make(map[string]FileRecord, len(rawRemote))
	for k, v := range rawRemote {
		norm := normalizeStateKey(k)
		if norm == "" || norm == "." {
			continue
		}
		v.RelPath = norm
		remoteRecords[norm] = v
	}

	localRecords := se.stateMgr.GetAll()
	hadTransferError := false

	// 1. Check local records against remote records
	for relPath, locRec := range localRecords {
		if se.ctx.Err() != nil {
			return
		}
		cleanRel, fullPath, vErr := validateSyncRelPath(se.sharedDir, relPath)
		if vErr != nil {
			continue
		}
		normKey := normalizeStateKey(cleanRel)
		remRec, remExists := remoteRecords[normKey]

		if locRec.Deleted {
			if remExists && !remRec.Deleted {
				if locRec.ModTime >= remRec.ModTime {
					log.Printf("[SyncEngine] Catch-up reconciliation: pushing deletion of %s to peer %s", normKey, cleanPeer)
					if err := se.uploadDelete(cleanPeer, normKey, locRec.ModTime); err != nil {
						hadTransferError = true
					}
				} else {
					log.Printf("[SyncEngine] Catch-up reconciliation: pulling newer file %s from peer %s", normKey, cleanPeer)
					if err := se.downloadFile(cleanPeer, normKey, remRec.Hash, remRec.Size, remRec.ModTime); err != nil {
						hadTransferError = true
					}
				}
			}
			continue
		}

		// Local file is active
		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			// Local file is missing from disk!
			deleteTime := time.Now().UnixNano()
			_ = se.stateMgr.MarkAsDeleted(normKey, deleteTime)
			if remExists && !remRec.Deleted {
				if err := se.uploadDelete(cleanPeer, normKey, deleteTime); err != nil {
					hadTransferError = true
				}
			}
			se.broadcastDeleteToPeers(normKey, deleteTime, cleanPeer)
			continue
		}

		diskModifiedLocally := false
		diskModTime := info.ModTime().UnixNano()
		if info.Size() != locRec.Size || (diskModTime != locRec.ModTime && (locRec.DiskModTime == 0 || diskModTime != locRec.DiskModTime)) {
			if diskHash, diskSize, hashErr := ComputeHash(fullPath); hashErr == nil {
				if diskHash != locRec.Hash {
					diskModifiedLocally = true
				}
				locRec.Hash = diskHash
				locRec.Size = diskSize
				locRec.ModTime = diskModTime
				locRec.DiskModTime = diskModTime
				_ = se.stateMgr.Set(normKey, locRec)
			}
		}

		if !remExists {
			log.Printf("[SyncEngine] Catch-up reconciliation: pushing %s to online peer %s", normKey, cleanPeer)
			if err := se.uploadFile(cleanPeer, normKey, fullPath, locRec.Hash, locRec.Size, locRec.ModTime); err != nil {
				hadTransferError = true
			}
		} else if remRec.Deleted {
			if remRec.ModTime >= locRec.ModTime {
				log.Printf("[SyncEngine] Catch-up reconciliation: applying remote deletion of %s from peer %s", normKey, cleanPeer)
				if err := se.ReceiveSyncDelete(cleanPeer, normKey, remRec.ModTime); err != nil {
					hadTransferError = true
				}
			} else {
				log.Printf("[SyncEngine] Catch-up reconciliation: re-pushing newer file %s to peer %s", normKey, cleanPeer)
				if err := se.uploadFile(cleanPeer, normKey, fullPath, locRec.Hash, locRec.Size, locRec.ModTime); err != nil {
					hadTransferError = true
				}
			}
		} else if remRec.Hash != locRec.Hash {
			if locRec.ModTime >= remRec.ModTime {
				log.Printf("[SyncEngine] Catch-up reconciliation: pushing newer version of %s to peer %s", normKey, cleanPeer)
				if err := se.uploadFile(cleanPeer, normKey, fullPath, locRec.Hash, locRec.Size, locRec.ModTime); err != nil {
					hadTransferError = true
				}
			} else {
				if isCloseConflict(remRec.ModTime, locRec.ModTime, diskModifiedLocally) {
					_, _ = se.preserveConflictCopy(cleanRel, fullPath)
				}
				log.Printf("[SyncEngine] Catch-up reconciliation: pulling newer version of %s from peer %s", normKey, cleanPeer)
				if err := se.downloadFile(cleanPeer, normKey, remRec.Hash, remRec.Size, remRec.ModTime); err != nil {
					hadTransferError = true
				}
			}
		}
	}

	// 2. Check remote records that local does NOT have
	for normKey, remRec := range remoteRecords {
		if se.ctx.Err() != nil {
			return
		}
		cleanRel, fullPath, vErr := validateSyncRelPath(se.sharedDir, normKey)
		if vErr != nil {
			continue
		}
		if _, locExists := localRecords[normKey]; locExists {
			continue // Already processed
		}
		if _, locExists := se.stateMgr.Get(normKey); locExists {
			continue
		}

		if remRec.Deleted {
			// Remote has deleted tombstone; record tombstone locally if file is not present on disk
			if _, serr := os.Stat(fullPath); os.IsNotExist(serr) {
				_ = se.stateMgr.MarkAsDeleted(normKey, remRec.ModTime)
			}
			continue
		}

		info, err := os.Stat(fullPath)
		if os.IsNotExist(err) {
			log.Printf("[SyncEngine] Catch-up reconciliation: pulling %s from online peer %s", normKey, cleanPeer)
			if err := se.downloadFile(cleanPeer, normKey, remRec.Hash, remRec.Size, remRec.ModTime); err != nil {
				hadTransferError = true
			}
		} else if err == nil && !info.IsDir() {
			diskHash, diskSize, hashErr := ComputeHash(fullPath)
			if hashErr != nil {
				continue
			}
			diskModTime := info.ModTime().UnixNano()
			if diskHash == remRec.Hash {
				_ = se.stateMgr.MarkAsReceived(normKey, diskHash, diskSize, diskModTime)
			} else if remRec.ModTime > diskModTime {
				if isCloseConflict(remRec.ModTime, diskModTime, false) {
					_, _ = se.preserveConflictCopy(cleanRel, fullPath)
				}
				log.Printf("[SyncEngine] Catch-up reconciliation: pulling newer remote %s over untracked local file from peer %s", normKey, cleanPeer)
				if err := se.downloadFile(cleanPeer, normKey, remRec.Hash, remRec.Size, remRec.ModTime); err != nil {
					hadTransferError = true
				}
			} else {
				log.Printf("[SyncEngine] Catch-up reconciliation: pushing newer untracked local %s to peer %s", normKey, cleanPeer)
				_ = se.stateMgr.Set(normKey, FileRecord{
					RelPath:     normKey,
					Hash:        diskHash,
					Size:        diskSize,
					ModTime:     diskModTime,
					DiskModTime: diskModTime,
				})
				if err := se.uploadFile(cleanPeer, normKey, fullPath, diskHash, diskSize, diskModTime); err != nil {
					hadTransferError = true
				}
			}
		}
	}

	if hadTransferError {
		se.markPeerDirty(cleanPeer)
	} else {
		se.consumePeerDirty(cleanPeer)
	}
}

func (se *SyncEngine) downloadFile(peerURL, relPath, expectedHash string, size, modTime int64) error {
	if _, _, err := validateSyncRelPath(se.sharedDir, relPath); err != nil {
		return err
	}

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

	// Prefer authoritative response headers when provided by the peer
	if hdrHash := strings.TrimSpace(resp.Header.Get("X-Bridge-Checksum")); hdrHash != "" {
		expectedHash = hdrHash
	}
	if hdrSize := strings.TrimSpace(resp.Header.Get("X-Bridge-Size")); hdrSize != "" {
		if parsed, parseErr := strconv.ParseInt(hdrSize, 10, 64); parseErr == nil && parsed >= 0 {
			size = parsed
		}
	}
	if hdrMod := strings.TrimSpace(resp.Header.Get("X-Bridge-ModTime")); hdrMod != "" {
		if parsed, parseErr := strconv.ParseInt(hdrMod, 10, 64); parseErr == nil && parsed > 0 {
			modTime = parsed
		}
	}

	return se.receiveSyncFile(peerURL, relPath, expectedHash, size, modTime, resp.Body, false)
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
	se.consumePeerDirty(clean)
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

// ReceiveSyncFile on SyncEngine receives an incoming file upload from a peer,
// writes it atomically while suppressing watchLoop echo, and relays it to all
// other connected peers excluding senderURL.
func (se *SyncEngine) ReceiveSyncFile(senderURL, relPath, expectedHash string, size, modTime int64, r io.Reader) error {
	return se.receiveSyncFile(senderURL, relPath, expectedHash, size, modTime, r, true)
}

func (se *SyncEngine) receiveSyncFile(senderURL, relPath, expectedHash string, size, modTime int64, r io.Reader, relay bool) error {
	cleanRel, targetPath, actualHash, written, alreadySynced, err := receiveSyncFileCore(se.sharedDir, se.stateMgr, relPath, expectedHash, size, modTime, r, se)
	if err != nil {
		return err
	}
	if relay && !alreadySynced {
		se.broadcastRelayFileToPeers(cleanRel, targetPath, actualHash, written, modTime, senderURL)
	}
	return nil
}

// ReceiveSyncFile handles an incoming sync request on the receiver side (standalone function for backward compatibility).
func ReceiveSyncFile(sharedDir string, stateMgr *StateManager, relPath, expectedHash string, size, modTime int64, r io.Reader) error {
	_, _, _, _, _, err := receiveSyncFileCore(sharedDir, stateMgr, relPath, expectedHash, size, modTime, r, nil)
	return err
}

func receiveSyncFileCore(sharedDir string, stateMgr *StateManager, relPath, expectedHash string, size, modTime int64, r io.Reader, se *SyncEngine) (string, string, string, int64, bool, error) {
	cleanRel, targetPath, err := validateSyncRelPath(sharedDir, relPath)
	if err != nil {
		return "", "", "", 0, false, err
	}
	normKey := normalizeStateKey(cleanRel)

	targetDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", "", "", 0, false, fmt.Errorf("failed to create directory %s: %w", targetDir, err)
	}
	if err := verifyNoSymlinkEscape(filepath.Clean(sharedDir), targetPath); err != nil {
		return "", "", "", 0, false, err
	}

	partFile, err := os.CreateTemp(targetDir, filepath.Base(targetPath)+".*.part")
	if err != nil {
		return "", "", "", 0, false, fmt.Errorf("failed to create part file: %w", err)
	}
	partPath := partFile.Name()

	// Stream incoming bytes to .part file and calculate SHA256 simultaneously
	h := sha256.New()
	writer := io.MultiWriter(partFile, h)

	written, err := io.Copy(writer, r)
	_ = partFile.Close()
	if err != nil {
		_ = os.Remove(partPath)
		return "", "", "", 0, false, fmt.Errorf("failed to write received data: %w", err)
	}

	actualHash := hex.EncodeToString(h.Sum(nil))
	if expectedHash == "" || !strings.EqualFold(actualHash, expectedHash) {
		_ = os.Remove(partPath)
		return "", "", "", 0, false, fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	_ = os.Chmod(partPath, 0644)

	// Set original modification time if provided
	if modTime > 0 {
		mtime := time.Unix(0, modTime)
		_ = os.Chtimes(partPath, mtime, mtime)
	}

	alreadySynced := false
	var prevRec FileRecord
	var hadPrev bool
	if stateMgr != nil {
		alreadySynced = stateMgr.IsSynced(normKey, actualHash)
		prevRec, hadPrev = stateMgr.Get(normKey)
	}

	// Suppress watchLoop echo BEFORE renaming .part -> targetPath
	if se != nil {
		se.cancelPendingDeletion(normKey)
		se.inFlightMu.Lock()
		se.receivingCount[normKey]++
		se.inFlightMu.Unlock()
		defer func() {
			se.inFlightMu.Lock()
			if se.receivingCount[normKey] <= 1 {
				delete(se.receivingCount, normKey)
			} else {
				se.receivingCount[normKey]--
			}
			se.inFlightMu.Unlock()
		}()
	}

	if stateMgr != nil {
		var diskMod int64
		if info, serr := os.Stat(partPath); serr == nil {
			diskMod = info.ModTime().UnixNano()
		}
		saveMod := modTime
		if saveMod <= 0 {
			saveMod = diskMod
		}
		if err := stateMgr.Set(normKey, FileRecord{
			RelPath:     normKey,
			Hash:        actualHash,
			Size:        written,
			ModTime:     saveMod,
			DiskModTime: diskMod,
		}); err != nil {
			log.Printf("[SyncEngine] Warning: failed to save state for %s: %v", normKey, err)
		}
	}

	// Atomic replace with brief Windows file-lock retry
	if err := renameWithRetry(partPath, targetPath); err != nil {
		_ = os.Remove(partPath)
		if stateMgr != nil {
			if hadPrev {
				_ = stateMgr.Set(normKey, prevRec)
			} else {
				_ = stateMgr.Delete(normKey)
			}
		}
		return "", "", "", 0, false, fmt.Errorf("failed to atomically place file: %w", err)
	}

	log.Printf("[SyncEngine] Successfully received and placed %s (%d bytes)", normKey, written)
	return normKey, targetPath, actualHash, written, alreadySynced, nil
}

// ReceiveSyncDelete on SyncEngine handles an incoming deletion request from a peer,
// purges the file from disk using RobustRemove, records the tombstone, and performs
// mesh routing relay to propagate the deletion across other connected peers.
func (se *SyncEngine) ReceiveSyncDelete(senderURL, relPath string, modTime int64) error {
	cleanRel, targetPath, err := validateSyncRelPath(se.sharedDir, relPath)
	if err != nil {
		return err
	}
	normKey := normalizeStateKey(cleanRel)

	if modTime <= 0 {
		modTime = time.Now().UnixNano()
	}

	// Suppress duplicate processing if already tombstoned with newer/same timestamp
	if rec, exists := se.stateMgr.Get(normKey); exists && rec.Deleted && rec.ModTime >= modTime {
		_ = RobustRemove(targetPath)
		return nil
	}

	se.cancelPendingDeletion(normKey)

	// Record tombstone first to prevent watcher or local loops
	if err := se.stateMgr.MarkAsDeleted(normKey, modTime); err != nil {
		log.Printf("[SyncEngine] Warning: failed to save deletion state for %s: %v", normKey, err)
	}

	// Purge from disk using RobustRemove (clears read-only flag, retries on file locks)
	if err := RobustRemove(targetPath); err != nil && !os.IsNotExist(err) {
		log.Printf("[SyncEngine] Warning: robust remove failed for %s: %v", targetPath, err)
	}

	log.Printf("[SyncEngine] Successfully received deletion and placed tombstone for %s", normKey)

	// Mesh Routing Relay: Propagate deletion to all other peers in the mesh
	se.broadcastDeleteToPeers(normKey, modTime, senderURL)
	return nil
}

// ReceiveSyncDelete handles an incoming deletion request from a peer (standalone function for backward compatibility).
func ReceiveSyncDelete(sharedDir string, stateMgr *StateManager, relPath string, modTime int64) error {
	cleanRel, targetPath, err := validateSyncRelPath(sharedDir, relPath)
	if err != nil {
		return err
	}
	normKey := normalizeStateKey(cleanRel)

	if modTime <= 0 {
		modTime = time.Now().UnixNano()
	}

	// Record tombstone first to prevent watcher or local loops
	if err := stateMgr.MarkAsDeleted(normKey, modTime); err != nil {
		log.Printf("[SyncEngine] Warning: failed to save deletion state for %s: %v", normKey, err)
	}

	if err := RobustRemove(targetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file %s: %w", targetPath, err)
	}

	log.Printf("[SyncEngine] Successfully received deletion and placed tombstone for %s", normKey)
	return nil
}
