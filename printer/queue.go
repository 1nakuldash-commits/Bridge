package printer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultOutboundQueueDir is the default persistent directory for holding outbound print jobs
// when a target peer is temporarily offline.
const DefaultOutboundQueueDir = `C:\Bridge\OutboundQueue`

// OutboundJobMeta stores persistent metadata alongside a queued outbound spool payload.
type OutboundJobMeta struct {
	ID          string    `json:"id"`
	PeerName    string    `json:"peer_name"`
	PeerURL     string    `json:"peer_url"`
	JobName     string    `json:"job_name"`
	FilePath    string    `json:"file_path"`
	PaperSize   string    `json:"paper_size,omitempty"`
	Orientation string    `json:"orientation,omitempty"`
	PageCount   int       `json:"page_count,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error,omitempty"`
}

// OutboundQueue persists outbound print payloads and metadata on disk so jobs printed while
// a peer is offline are automatically retained and flushed once the peer returns online.
type OutboundQueue struct {
	dir      string
	mu       sync.Mutex
	flushing map[string]bool
	seq      uint64
}

// NewOutboundQueue creates an OutboundQueue backed by dir (defaults to DefaultOutboundQueueDir if empty).
func NewOutboundQueue(dir string) *OutboundQueue {
	clean := strings.TrimSpace(dir)
	if clean == "" {
		clean = DefaultOutboundQueueDir
	}
	return &OutboundQueue{
		dir:      filepath.Clean(clean),
		flushing: make(map[string]bool),
	}
}

// Dir returns the directory path used by the queue.
func (q *OutboundQueue) Dir() string {
	return q.dir
}

// Enqueue copies the spool payload at filePath into the persistent queue directory alongside a .meta.json file.
func (q *OutboundQueue) Enqueue(peerName, peerURL, filePath string, jobMeta OutboundJobMeta) (OutboundJobMeta, error) {
	if strings.TrimSpace(filePath) == "" {
		return OutboundJobMeta{}, errors.New("outbound queue: source file path is required")
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		return OutboundJobMeta{}, fmt.Errorf("outbound queue: failed to stat %s: %w", filePath, err)
	}
	if fi.IsDir() {
		return OutboundJobMeta{}, fmt.Errorf("outbound queue: %s is a directory", filePath)
	}
	if fi.Size() == 0 {
		return OutboundJobMeta{}, fmt.Errorf("outbound queue: %s is empty", filePath)
	}

	// Inspect geometry before taking lock so file reads never block queue operations.
	geom := DetectDocumentGeometry(filePath)

	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == "" {
		ext = geom.Format
		if ext == "" {
			ext = ".bin"
		}
	}

	meta := jobMeta
	if meta.ID == "" {
		meta.ID = fmt.Sprintf("%d_%04d", time.Now().UnixNano(), atomic.AddUint64(&q.seq, 1))
	}
	if strings.ContainsAny(meta.ID, `/\`) {
		return OutboundJobMeta{}, fmt.Errorf("outbound queue: invalid job ID %q", meta.ID)
	}
	if strings.TrimSpace(peerName) != "" {
		meta.PeerName = strings.TrimSpace(peerName)
	}
	if strings.TrimSpace(peerURL) != "" {
		meta.PeerURL = strings.TrimRight(strings.TrimSpace(peerURL), "/")
	} else {
		meta.PeerURL = strings.TrimRight(strings.TrimSpace(meta.PeerURL), "/")
	}
	if meta.JobName == "" {
		meta.JobName = filepath.Base(filePath)
	}
	if meta.PaperSize == "" {
		meta.PaperSize = geom.PaperName
	}
	if meta.Orientation == "" {
		meta.Orientation = geom.Orientation
	}
	if meta.PageCount == 0 && geom.PageCount > 0 {
		meta.PageCount = geom.PageCount
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if err := os.MkdirAll(q.dir, 0755); err != nil {
		return OutboundJobMeta{}, fmt.Errorf("outbound queue: failed to create directory %s: %w", q.dir, err)
	}

	payloadPath := filepath.Join(q.dir, meta.ID+ext)
	meta.FilePath = payloadPath

	if err := copyFileAtomic(filePath, payloadPath); err != nil {
		return OutboundJobMeta{}, fmt.Errorf("outbound queue: failed to stage payload: %w", err)
	}

	if err := q.writeMetaLocked(meta); err != nil {
		_ = os.Remove(payloadPath)
		return OutboundJobMeta{}, fmt.Errorf("outbound queue: failed to write metadata: %w", err)
	}

	return meta, nil
}

// ListQueued returns all queued jobs, optionally filtered by peerURL (empty returns all jobs),
// sorted oldest-first by CreatedAt.
func (q *OutboundQueue) ListQueued(peerURL string) ([]OutboundJobMeta, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.listQueuedLocked(peerURL)
}

func (q *OutboundQueue) listQueuedLocked(peerURL string) ([]OutboundJobMeta, error) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	targetURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	var jobs []OutboundJobMeta

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".meta.json") {
			continue
		}
		metaPath := filepath.Join(q.dir, e.Name())
		raw, readErr := os.ReadFile(metaPath)
		if readErr != nil {
			continue
		}
		var meta OutboundJobMeta
		if jsonErr := json.Unmarshal(raw, &meta); jsonErr != nil {
			continue
		}
		if meta.ID == "" {
			meta.ID = strings.TrimSuffix(e.Name(), ".meta.json")
		}
		if meta.FilePath == "" || !fileExists(meta.FilePath) {
			if resolved := q.resolvePayloadPathLocked(meta.ID); resolved != "" {
				meta.FilePath = resolved
			} else {
				continue
			}
		}
		if targetURL != "" && !strings.EqualFold(strings.TrimRight(meta.PeerURL, "/"), targetURL) {
			continue
		}
		jobs = append(jobs, meta)
	}

	sort.Slice(jobs, func(i, j int) bool {
		if !jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
		}
		return jobs[i].ID < jobs[j].ID
	})

	return jobs, nil
}

// Count returns the total number of queued outbound jobs across all peers.
func (q *OutboundQueue) Count() int {
	jobs, err := q.ListQueued("")
	if err != nil {
		return 0
	}
	return len(jobs)
}

// Remove deletes a queued job's metadata and payload from disk by job ID.
func (q *OutboundQueue) Remove(id string) error {
	cleanID := strings.TrimSpace(id)
	if cleanID == "" || strings.ContainsAny(cleanID, `/\`) {
		return fmt.Errorf("outbound queue: invalid job ID %q", id)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	return q.removeLocked(cleanID)
}

func (q *OutboundQueue) removeLocked(id string) error {
	metaPath := filepath.Join(q.dir, id+".meta.json")
	var payloadPath string
	if raw, err := os.ReadFile(metaPath); err == nil {
		var meta OutboundJobMeta
		if json.Unmarshal(raw, &meta) == nil && meta.FilePath != "" {
			payloadPath = meta.FilePath
		}
	}
	if payloadPath == "" {
		payloadPath = q.resolvePayloadPathLocked(id)
	}

	metaErr := os.Remove(metaPath)
	var payloadErr error
	if payloadPath != "" {
		payloadErr = os.Remove(payloadPath)
	}

	if os.IsNotExist(metaErr) && (payloadPath == "" || os.IsNotExist(payloadErr)) {
		return os.ErrNotExist
	}
	if metaErr != nil && !os.IsNotExist(metaErr) {
		return metaErr
	}
	if payloadErr != nil && !os.IsNotExist(payloadErr) {
		return payloadErr
	}
	return nil
}

// FlushPeer attempts to deliver all queued jobs matching peerURL (or peerName if peerURL is empty)
// in FIFO order using sendFn. Delivered jobs are removed; failed jobs record incremented Attempts
// and LastError and remain queued on disk. Returns the number of successfully sent jobs and any error.
func (q *OutboundQueue) FlushPeer(
	ctx context.Context,
	peerName string,
	peerURL string,
	sendFn func(ctx context.Context, job OutboundJobMeta) error,
) (int, error) {
	if sendFn == nil {
		return 0, errors.New("outbound queue: sendFn is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cleanURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	cleanName := strings.TrimSpace(peerName)

	q.mu.Lock()
	allJobs, err := q.listQueuedLocked("")
	if err != nil {
		q.mu.Unlock()
		return 0, err
	}

	var matching []OutboundJobMeta
	for _, job := range allJobs {
		if !matchesOutboundPeer(job, cleanName, cleanURL) {
			continue
		}
		if q.flushing[job.ID] {
			continue
		}
		q.flushing[job.ID] = true
		matching = append(matching, job)
	}
	q.mu.Unlock()

	if len(matching) == 0 {
		return 0, nil
	}

	defer func() {
		q.mu.Lock()
		for _, job := range matching {
			delete(q.flushing, job.ID)
		}
		q.mu.Unlock()
	}()

	sent := 0
	for _, job := range matching {
		if err := ctx.Err(); err != nil {
			return sent, err
		}

		// Re-target to current peerURL if caller provided an updated URL for this peerName
		dispatchJob := job
		if cleanURL != "" {
			dispatchJob.PeerURL = cleanURL
		}

		if err := sendFn(ctx, dispatchJob); err != nil {
			q.mu.Lock()
			job.Attempts++
			job.LastError = err.Error()
			_ = q.writeMetaLocked(job)
			q.mu.Unlock()
			return sent, err
		}

		q.mu.Lock()
		_ = q.removeLocked(job.ID)
		q.mu.Unlock()
		sent++
	}

	return sent, nil
}

// PruneOld removes queued jobs (and orphaned temp/meta files) older than maxAge.
// Returns the number of jobs pruned.
func (q *OutboundQueue) PruneOld(maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}

	cutoff := time.Now().Add(-maxAge)

	q.mu.Lock()
	defer q.mu.Unlock()

	entries, err := os.ReadDir(q.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	pruned := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)

		if strings.HasSuffix(lower, ".tmp") {
			if info, infoErr := e.Info(); infoErr == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(q.dir, name))
			}
			continue
		}

		if !strings.HasSuffix(lower, ".meta.json") {
			continue
		}

		metaPath := filepath.Join(q.dir, name)
		raw, readErr := os.ReadFile(metaPath)
		if readErr != nil {
			continue
		}

		var meta OutboundJobMeta
		var createdAt time.Time
		if json.Unmarshal(raw, &meta) == nil {
			createdAt = meta.CreatedAt
		}
		if meta.ID == "" {
			meta.ID = strings.TrimSuffix(name, ".meta.json")
		}
		if createdAt.IsZero() {
			if info, infoErr := e.Info(); infoErr == nil {
				createdAt = info.ModTime()
			}
		}

		if !createdAt.IsZero() && createdAt.Before(cutoff) {
			if !q.flushing[meta.ID] && q.removeLocked(meta.ID) == nil {
				pruned++
			}
		}
	}

	return pruned, nil
}

func matchesOutboundPeer(job OutboundJobMeta, peerName, peerURL string) bool {
	jobURL := strings.TrimRight(strings.TrimSpace(job.PeerURL), "/")
	if peerURL != "" && jobURL != "" && strings.EqualFold(jobURL, peerURL) {
		return true
	}
	if peerName != "" && strings.EqualFold(strings.TrimSpace(job.PeerName), peerName) {
		return true
	}
	return peerURL == "" && peerName == ""
}

func (q *OutboundQueue) resolvePayloadPathLocked(id string) string {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return ""
	}
	prefix := id + "."
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.HasPrefix(name, prefix) && !strings.HasSuffix(lower, ".meta.json") && !strings.HasSuffix(lower, ".tmp") {
			return filepath.Join(q.dir, name)
		}
	}
	return ""
}

func (q *OutboundQueue) writeMetaLocked(meta OutboundJobMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(q.dir, meta.ID+".meta.json")
	tmpPath := metaPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, metaPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func copyFileAtomic(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	tmpPath := dstPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	buf := make([]byte, 64*1024)
	_, copyErr := io.CopyBuffer(dst, src, buf)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
