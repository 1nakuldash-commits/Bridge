package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileRecord tracks the known state of a synchronized file.
type FileRecord struct {
	RelPath string `json:"rel_path"`
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"` // Unix timestamp in nanoseconds
}

// StateManager persists and provides thread-safe access to sync file states.
type StateManager struct {
	mu        sync.RWMutex
	filePath  string
	records   map[string]FileRecord
}

type stateFileFormat struct {
	Files map[string]FileRecord `json:"files"`
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
		sm.records = sf.Files
	}
	return nil
}

// Save writes current state atomically to disk. Must be called with lock held or via public methods.
func (sm *StateManager) saveLocked() error {
	dir := filepath.Dir(sm.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	sf := stateFileFormat{
		Files: sm.records,
	}

	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := sm.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}

	// Atomic replace
	return os.Rename(tmpPath, sm.filePath)
}

// Get returns the recorded state for a relative file path.
func (sm *StateManager) Get(relPath string) (FileRecord, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	rec, ok := sm.records[filepath.Clean(relPath)]
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
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cleanPath := filepath.Clean(relPath)
	rec.RelPath = cleanPath
	sm.records[cleanPath] = rec
	return sm.saveLocked()
}

// Delete removes a record and saves state.
func (sm *StateManager) Delete(relPath string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cleanPath := filepath.Clean(relPath)
	delete(sm.records, cleanPath)
	return sm.saveLocked()
}

// IsSynced checks whether the given hash matches the last recorded hash for relPath.
func (sm *StateManager) IsSynced(relPath string, hash string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	rec, ok := sm.records[filepath.Clean(relPath)]
	if !ok {
		return false
	}
	return rec.Hash == hash
}

// MarkAsReceived records a file received from a peer so that local events do not echo it back.
func (sm *StateManager) MarkAsReceived(relPath string, hash string, size int64, modTime int64) error {
	return sm.Set(relPath, FileRecord{
		RelPath: filepath.Clean(relPath),
		Hash:    hash,
		Size:    size,
		ModTime: modTime,
	})
}

// ComputeHash computes the SHA-256 hash and size of a file on disk.
func ComputeHash(fullPath string) (string, int64, error) {
	f, err := os.Open(fullPath)
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
