package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// FileRecord tracks the known state of a synchronized file.
type FileRecord struct {
	RelPath     string `json:"rel_path"`
	Hash        string `json:"hash"`
	Size        int64  `json:"size"`
	ModTime     int64  `json:"mod_time"`                // Logical sync timestamp in nanoseconds
	DiskModTime int64  `json:"disk_mod_time,omitempty"` // Actual local disk modtime (ns) when distinct from ModTime
	Deleted     bool   `json:"deleted,omitempty"`       // Tombstone marker for deleted files
}

// StateManager persists and provides thread-safe access to sync file states.
type StateManager struct {
	mu       sync.RWMutex
	saveMu   sync.Mutex
	filePath string
	records  map[string]FileRecord
	seq      uint64
	savedSeq uint64
}

type stateFileFormat struct {
	Files map[string]FileRecord `json:"files"`
}

func normalizeStateKey(relPath string) string {
	trimmed := strings.TrimSpace(relPath)
	if trimmed == "" {
		return ""
	}
	clean := filepath.Clean(filepath.FromSlash(trimmed))
	return filepath.ToSlash(clean)
}

// NewStateManager loads or initializes a StateManager for the given state file path.
func NewStateManager(filePath string) (*StateManager, error) {
	sm := &StateManager{
		filePath: filePath,
		records:  make(map[string]FileRecord),
	}

	if err := sm.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load state file: %w", err)
	}

	return sm, nil
}

func (sm *StateManager) load() error {
	data, err := os.ReadFile(sm.filePath)
	if err != nil {
		return err
	}

	var sf stateFileFormat
	if err := json.Unmarshal(data, &sf); err != nil {
		return err
	}

	if sf.Files != nil {
		normalized := make(map[string]FileRecord, len(sf.Files))
		for k, rec := range sf.Files {
			normKey := normalizeStateKey(k)
			if normKey == "" || normKey == "." {
				continue
			}
			rec.RelPath = normKey
			if runtime.GOOS == "windows" {
				for existingKey, existingRec := range normalized {
					if strings.EqualFold(existingKey, normKey) && existingKey != normKey {
						if existingRec.ModTime > rec.ModTime {
							rec = existingRec
						}
						delete(normalized, existingKey)
						break
					}
				}
			}
			normalized[normKey] = rec
		}
		sm.records = normalized
	}
	return nil
}

// renameWithRetry renames src to dst with brief exponential backoff to tolerate
// transient Windows file locks (Defender, Search Indexer, Explorer).
func renameWithRetry(src, dst string) error {
	var err error
	for i := 0; i < 6; i++ {
		err = os.Rename(src, dst)
		if err == nil {
			return nil
		}
		_ = os.Chmod(dst, 0666)
		time.Sleep(time.Duration(20*(1<<i)) * time.Millisecond)
	}
	return err
}

func (sm *StateManager) findKeyLocked(normKey string) (string, FileRecord, bool) {
	if normKey == "" {
		return "", FileRecord{}, false
	}
	if rec, ok := sm.records[normKey]; ok {
		return normKey, rec, true
	}
	if runtime.GOOS == "windows" {
		for k, rec := range sm.records {
			if strings.EqualFold(k, normKey) {
				return k, rec, true
			}
		}
	}
	return "", FileRecord{}, false
}

// flushToDisk serializes and atomically writes the latest state snapshot without holding sm.mu across disk I/O.
func (sm *StateManager) flushToDisk(targetSeq uint64) error {
	sm.saveMu.Lock()
	defer sm.saveMu.Unlock()

	sm.mu.RLock()
	if sm.savedSeq >= targetSeq {
		sm.mu.RUnlock()
		return nil
	}
	latestSeq := sm.seq
	snapshot := make(map[string]FileRecord, len(sm.records))
	for k, v := range sm.records {
		snapshot[k] = v
	}
	sm.mu.RUnlock()

	dir := filepath.Dir(sm.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	sf := stateFileFormat{
		Files: snapshot,
	}

	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(dir, filepath.Base(sm.filePath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	// Atomic replace with brief retry for transient Windows file locks
	if err := renameWithRetry(tmpPath, sm.filePath); err != nil {
		return err
	}
	cleanupTmp = false

	sm.mu.Lock()
	if latestSeq > sm.savedSeq {
		sm.savedSeq = latestSeq
	}
	sm.mu.Unlock()
	return nil
}

// Get returns the recorded state for a relative file path.
func (sm *StateManager) Get(relPath string) (FileRecord, bool) {
	normKey := normalizeStateKey(relPath)
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, rec, ok := sm.findKeyLocked(normKey)
	return rec, ok
}

// GetAll returns a copy of all tracked file records.
func (sm *StateManager) GetAll() map[string]FileRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	res := make(map[string]FileRecord, len(sm.records))
	for k, v := range sm.records {
		res[k] = v
	}
	return res
}

// Set updates or inserts a record and saves state.
func (sm *StateManager) Set(relPath string, rec FileRecord) error {
	normKey := normalizeStateKey(relPath)
	if normKey == "" || normKey == "." {
		return fmt.Errorf("invalid relative path %q", relPath)
	}

	sm.mu.Lock()
	if runtime.GOOS == "windows" {
		if oldKey, _, ok := sm.findKeyLocked(normKey); ok && oldKey != normKey {
			delete(sm.records, oldKey)
		}
	}
	rec.RelPath = normKey
	sm.records[normKey] = rec
	sm.seq++
	targetSeq := sm.seq
	sm.mu.Unlock()

	return sm.flushToDisk(targetSeq)
}

// Delete removes a record and saves state.
func (sm *StateManager) Delete(relPath string) error {
	normKey := normalizeStateKey(relPath)

	sm.mu.Lock()
	existingKey, _, ok := sm.findKeyLocked(normKey)
	if !ok {
		sm.mu.Unlock()
		return nil
	}
	delete(sm.records, existingKey)
	sm.seq++
	targetSeq := sm.seq
	sm.mu.Unlock()

	return sm.flushToDisk(targetSeq)
}

// PruneTombstones removes deletion tombstones older than maxAge and persists if any were removed.
func (sm *StateManager) PruneTombstones(maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge).UnixNano()

	sm.mu.Lock()
	pruned := 0
	for k, rec := range sm.records {
		if rec.Deleted && rec.ModTime > 0 && rec.ModTime < cutoff {
			delete(sm.records, k)
			pruned++
		}
	}
	if pruned == 0 {
		sm.mu.Unlock()
		return 0, nil
	}
	sm.seq++
	targetSeq := sm.seq
	sm.mu.Unlock()

	return pruned, sm.flushToDisk(targetSeq)
}

// IsSynced checks whether the given hash matches the last recorded hash for relPath.
func (sm *StateManager) IsSynced(relPath string, hash string) bool {
	normKey := normalizeStateKey(relPath)
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, rec, ok := sm.findKeyLocked(normKey)
	if !ok || rec.Deleted {
		return false
	}
	return rec.Hash == hash
}

// IsDeleted checks if the file is recorded as deleted (tombstone).
func (sm *StateManager) IsDeleted(relPath string) bool {
	normKey := normalizeStateKey(relPath)
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, rec, ok := sm.findKeyLocked(normKey)
	return ok && rec.Deleted
}

// MarkAsDeleted records a deletion tombstone so that local events do not echo it back.
func (sm *StateManager) MarkAsDeleted(relPath string, modTime int64) error {
	normKey := normalizeStateKey(relPath)
	return sm.Set(normKey, FileRecord{
		RelPath: normKey,
		Hash:    "",
		Size:    0,
		ModTime: modTime,
		Deleted: true,
	})
}

// MarkAsReceived records a file received from a peer so that local events do not echo it back.
func (sm *StateManager) MarkAsReceived(relPath string, hash string, size int64, modTime int64) error {
	normKey := normalizeStateKey(relPath)
	return sm.Set(normKey, FileRecord{
		RelPath: normKey,
		Hash:    hash,
		Size:    size,
		ModTime: modTime,
	})
}

// ComputeHash computes the SHA-256 hash and size of a file on disk.
func ComputeHash(fullPath string) (string, int64, error) {
	f, err := OpenSharedFileForRead(fullPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}

	return hex.EncodeToString(h.Sum(nil)), size, nil
}
