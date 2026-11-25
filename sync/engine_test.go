package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestSyncEngineClientHasNoTotalTimeoutCap(t *testing.T) {
	tmp := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	se := NewSyncEngine(filepath.Join(tmp, "shared"), "", "tok", sm)
	if se.client.Timeout != 0 {
		t.Fatalf("expected http.Client.Timeout == 0 (no 60s large-transfer cap), got %v", se.client.Timeout)
	}
	if se.client.Transport == nil {
		t.Fatalf("expected configured http.Transport")
	}
}

func TestReconcileWithPeerPullsNewerRemoteEditsAndHandlesUntrackedDiskFiles(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sm, err := NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	now := time.Now()
	olderTime := now.Add(-2 * time.Minute).UnixNano()
	newerTime := now.Add(-10 * time.Second).UnixNano()

	// 1. Tracked file where remote has newer edit (remRec.ModTime > locRec.ModTime).
	oldTrackedContent := []byte("older local tracked content")
	newTrackedContent := []byte("newer remote tracked content")
	trackedPath := filepath.Join(sharedDir, "tracked.txt")
	if err := os.WriteFile(trackedPath, oldTrackedContent, 0644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}
	_ = os.Chtimes(trackedPath, time.Unix(0, olderTime), time.Unix(0, olderTime))
	if err := sm.Set("tracked.txt", FileRecord{
		RelPath: "tracked.txt",
		Hash:    sha256Hex(oldTrackedContent),
		Size:    int64(len(oldTrackedContent)),
		ModTime: olderTime,
	}); err != nil {
		t.Fatalf("sm.Set tracked: %v", err)
	}

	// 2. Untracked file on disk (no state.json entry) where remote is newer.
	oldUntrackedContent := []byte("stale untracked local")
	newUntrackedContent := []byte("fresh remote untracked")
	untrackedPullPath := filepath.Join(sharedDir, "untracked_pull.txt")
	if err := os.WriteFile(untrackedPullPath, oldUntrackedContent, 0644); err != nil {
		t.Fatalf("WriteFile untracked_pull: %v", err)
	}
	_ = os.Chtimes(untrackedPullPath, time.Unix(0, olderTime), time.Unix(0, olderTime))

	// 3. Untracked file on disk (no state.json entry) where local is newer -> should push.
	freshLocalContent := []byte("fresh local untracked")
	staleRemoteContent := []byte("stale remote untracked")
	untrackedPushPath := filepath.Join(sharedDir, "untracked_push.txt")
	if err := os.WriteFile(untrackedPushPath, freshLocalContent, 0644); err != nil {
		t.Fatalf("WriteFile untracked_push: %v", err)
	}
	_ = os.Chtimes(untrackedPushPath, time.Unix(0, newerTime), time.Unix(0, newerTime))

	remoteFiles := map[string][]byte{
		"tracked.txt":        newTrackedContent,
		"untracked_pull.txt": newUntrackedContent,
		"untracked_push.txt": staleRemoteContent,
	}
	inventory := map[string]FileRecord{
		"tracked.txt": {
			RelPath: "tracked.txt",
			Hash:    sha256Hex(newTrackedContent),
			Size:    int64(len(newTrackedContent)),
			ModTime: newerTime,
		},
		"untracked_pull.txt": {
			RelPath: "untracked_pull.txt",
			Hash:    sha256Hex(newUntrackedContent),
			Size:    int64(len(newUntrackedContent)),
			ModTime: newerTime,
		},
		"untracked_push.txt": {
			RelPath: "untracked_push.txt",
			Hash:    sha256Hex(staleRemoteContent),
			Size:    int64(len(staleRemoteContent)),
			ModTime: olderTime,
		},
	}

	var mu sync.Mutex
	uploaded := make(map[string][]byte)
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sync/inventory":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(inventory)
		case "/api/sync":
			switch r.Method {
			case http.MethodGet:
				rel := r.URL.Query().Get("path")
				data, ok := remoteFiles[rel]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
			case http.MethodPost:
				rel := r.Header.Get("X-Bridge-RelPath")
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				uploaded[rel] = body
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer peerSrv.Close()

	se := NewSyncEngine(sharedDir, peerSrv.URL, "tok", sm)
	se.ReconcileWithPeer(peerSrv.URL)

	// Wait for ReconcileWithPeer to finish (before Stop cancels se.ctx).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		se.reconcileMu.Lock()
		busy := se.reconcilingPeer[peerSrv.URL]
		se.reconcileMu.Unlock()
		if !busy {
			// Ensure reconciliation has actually started and finished (check state or uploads)
			mu.Lock()
			_, pushDone := uploaded["untracked_push.txt"]
			mu.Unlock()
			if pushDone && sm.IsSynced("tracked.txt", sha256Hex(newTrackedContent)) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	se.Stop()

	gotTracked, err := os.ReadFile(trackedPath)
	if err != nil || !bytes.Equal(gotTracked, newTrackedContent) {
		t.Fatalf("tracked.txt not updated from newer remote: got %q (err=%v), want %q", gotTracked, err, newTrackedContent)
	}
	if !sm.IsSynced("tracked.txt", sha256Hex(newTrackedContent)) {
		t.Fatalf("stateMgr did not record newer remote hash for tracked.txt")
	}

	gotUntrackedPull, err := os.ReadFile(untrackedPullPath)
	if err != nil || !bytes.Equal(gotUntrackedPull, newUntrackedContent) {
		t.Fatalf("untracked_pull.txt not pulled over older disk file: got %q (err=%v), want %q", gotUntrackedPull, err, newUntrackedContent)
	}

	mu.Lock()
	pushed := uploaded["untracked_push.txt"]
	mu.Unlock()
	if !bytes.Equal(pushed, freshLocalContent) {
		t.Fatalf("expected newer untracked_push.txt to be pushed, got %q", pushed)
	}
}

func TestReconcileWithPeerDeduplicatesConcurrentRuns(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	_ = os.MkdirAll(sharedDir, 0755)
	sm, err := NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sync/inventory" {
			if atomic.AddInt32(&calls, 1) == 1 {
				close(started)
			}
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer peerSrv.Close()

	se := NewSyncEngine(sharedDir, peerSrv.URL, "tok", sm)
	se.ReconcileWithPeer(peerSrv.URL)
	<-started

	for i := 0; i < 4; i++ {
		se.ReconcileWithPeer(peerSrv.URL)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	se.Stop()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 deduplicated inventory call, got %d", got)
	}
}

func TestWatchLoopRecursiveNewDirectoryIngestion(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	stagingDir := filepath.Join(tmp, "staging_folder")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("MkdirAll shared: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stagingDir, "nested", "deep"), 0755); err != nil {
		t.Fatalf("MkdirAll staging: %v", err)
	}

	file1 := []byte("root file inside pasted dir")
	file2 := []byte("deeply nested file inside pasted dir")
	if err := os.WriteFile(filepath.Join(stagingDir, "root.txt"), file1, 0644); err != nil {
		t.Fatalf("WriteFile root.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "nested", "deep", "inner.txt"), file2, 0644); err != nil {
		t.Fatalf("WriteFile inner.txt: %v", err)
	}

	var mu sync.Mutex
	uploaded := make(map[string][]byte)
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sync" && r.Method == http.MethodPost {
			rel := r.Header.Get("X-Bridge-RelPath")
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			uploaded[rel] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer peerSrv.Close()

	sm, err := NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	se := NewSyncEngine(sharedDir, peerSrv.URL, "tok", sm)
	if err := se.Start(); err != nil {
		t.Fatalf("SyncEngine.Start: %v", err)
	}
	defer se.Stop()

	// Move the pre-populated directory tree into SharedDir in one atomic rename.
	destDir := filepath.Join(sharedDir, "pasted_folder")
	if err := os.Rename(stagingDir, destDir); err != nil {
		t.Fatalf("Rename staging into shared: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got1, ok1 := uploaded["pasted_folder/root.txt"]
		got2, ok2 := uploaded["pasted_folder/nested/deep/inner.txt"]
		mu.Unlock()
		if ok1 && ok2 && bytes.Equal(got1, file1) && bytes.Equal(got2, file2) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	got1, ok1 := uploaded["pasted_folder/root.txt"]
	got2, ok2 := uploaded["pasted_folder/nested/deep/inner.txt"]
	mu.Unlock()
	if !ok1 || !bytes.Equal(got1, file1) {
		t.Fatalf("expected pasted_folder/root.txt to be ingested, got ok=%v data=%q", ok1, got1)
	}
	if !ok2 || !bytes.Equal(got2, file2) {
		t.Fatalf("expected pasted_folder/nested/deep/inner.txt to be ingested, got ok=%v data=%q", ok2, got2)
	}

	// Verify watcher was recursively attached to the nested subdirectory as well.
	later := []byte("added after paste")
	if err := os.WriteFile(filepath.Join(destDir, "nested", "deep", "later.txt"), later, 0644); err != nil {
		t.Fatalf("WriteFile later.txt: %v", err)
	}
	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got3, ok3 := uploaded["pasted_folder/nested/deep/later.txt"]
		mu.Unlock()
		if ok3 && bytes.Equal(got3, later) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("expected later file in newly watched subdirectory to be synced")
}

func TestReceiveSyncFileConcurrentAndStateSave(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	statePath := filepath.Join(tmp, "state.json")
	sm, err := NewStateManager(statePath)
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	payload := []byte("concurrent sync payload")
	hash := sha256Hex(payload)
	now := time.Now().UnixNano()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ReceiveSyncFile(sharedDir, sm, "concurrent.bin", hash, int64(len(payload)), now, bytes.NewReader(payload)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent ReceiveSyncFile failed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(sharedDir, "concurrent.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("unexpected concurrent.bin content: got %q (err=%v)", got, err)
	}

	entries, err := os.ReadDir(sharedDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".part" {
			t.Fatalf("leftover .part file found: %s", e.Name())
		}
	}

	if _, err := os.Stat(statePath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("leftover state.json.tmp found")
	}
}

func TestResyncNowScansLocalChangesAndReconcilesAcrossAllPeers(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sm, err := NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	// Pre-record a file that was deleted locally on disk before ResyncNow.
	deletedContent := []byte("deleted locally before resync")
	if err := sm.Set("deleted_local.txt", FileRecord{
		RelPath: "deleted_local.txt",
		Hash:    sha256Hex(deletedContent),
		Size:    int64(len(deletedContent)),
		ModTime: time.Now().Add(-1 * time.Minute).UnixNano(),
	}); err != nil {
		t.Fatalf("sm.Set deleted_local: %v", err)
	}

	// Untracked brand-new local file written directly to disk (not in state.json).
	missedLocal := []byte("brand new untracked local file")
	if err := os.WriteFile(filepath.Join(sharedDir, "missed_local.txt"), missedLocal, 0644); err != nil {
		t.Fatalf("WriteFile missed_local: %v", err)
	}

	remoteFromA := []byte("remote file from peer A")
	peerAInv := map[string]FileRecord{
		"from_peer_a.txt": {
			RelPath: "from_peer_a.txt",
			Hash:    sha256Hex(remoteFromA),
			Size:    int64(len(remoteFromA)),
			ModTime: time.Now().UnixNano(),
		},
	}
	peerBInv := map[string]FileRecord{
		"deleted_local.txt": {
			RelPath: "deleted_local.txt",
			Hash:    sha256Hex(deletedContent),
			Size:    int64(len(deletedContent)),
			ModTime: time.Now().Add(-1 * time.Minute).UnixNano(),
		},
	}

	var mu sync.Mutex
	uploadedA := make(map[string][]byte)
	uploadedB := make(map[string][]byte)
	deletedB := make(map[string]bool)

	newPeerSrv := func(inv map[string]FileRecord, files map[string][]byte, uploaded map[string][]byte, deleted map[string]bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/sync/inventory":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(inv)
			case "/api/sync":
				switch r.Method {
				case http.MethodGet:
					rel := r.URL.Query().Get("path")
					data, ok := files[rel]
					if !ok {
						http.NotFound(w, r)
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(data)
				case http.MethodPost:
					rel := r.Header.Get("X-Bridge-RelPath")
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					uploaded[rel] = body
					mu.Unlock()
					w.WriteHeader(http.StatusOK)
				case http.MethodDelete:
					rel := r.URL.Query().Get("path")
					if rel == "" {
						rel = r.Header.Get("X-Bridge-RelPath")
					}
					mu.Lock()
					if deleted != nil {
						deleted[rel] = true
					}
					mu.Unlock()
					w.WriteHeader(http.StatusOK)
				}
			default:
				http.NotFound(w, r)
			}
		}))
	}

	peerA := newPeerSrv(peerAInv, map[string][]byte{"from_peer_a.txt": remoteFromA}, uploadedA, nil)
	defer peerA.Close()
	peerB := newPeerSrv(peerBInv, nil, uploadedB, deletedB)
	defer peerB.Close()

	se := NewSyncEngine(sharedDir, "", "tok", sm)
	se.UpdatePeerURLs([]string{peerA.URL, peerB.URL})
	defer se.Stop()

	// Synchronous ResyncNow across all peers without starting watcher loop.
	se.ResyncNow()

	mu.Lock()
	gotA := uploadedA["missed_local.txt"]
	gotB := uploadedB["missed_local.txt"]
	delB := deletedB["deleted_local.txt"]
	mu.Unlock()

	if !bytes.Equal(gotA, missedLocal) {
		t.Fatalf("peer A did not receive untracked local file: got %q", gotA)
	}
	if !bytes.Equal(gotB, missedLocal) {
		t.Fatalf("peer B did not receive untracked local file: got %q", gotB)
	}
	if !delB {
		t.Fatalf("peer B did not receive deletion for deleted_local.txt")
	}
	if !sm.IsDeleted("deleted_local.txt") {
		t.Fatalf("expected deleted_local.txt to be tombstoned in stateMgr")
	}
	pulled, err := os.ReadFile(filepath.Join(sharedDir, "from_peer_a.txt"))
	if err != nil || !bytes.Equal(pulled, remoteFromA) {
		t.Fatalf("expected from_peer_a.txt to be pulled locally, got %q (err=%v)", pulled, err)
	}
}

func TestDirectoryRenameSyncsWithoutStaleTombstones(t *testing.T) {
	tmp := t.TempDir()
	sharedDir := filepath.Join(tmp, "shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sm, err := NewStateManager(filepath.Join(tmp, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	var mu sync.Mutex
	uploaded := make(map[string][]byte)
	deleted := make(map[string]int)
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/sync" {
			switch r.Method {
			case http.MethodPost:
				rel := r.Header.Get("X-Bridge-RelPath")
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				uploaded[rel] = body
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			case http.MethodDelete:
				rel := r.URL.Query().Get("path")
				if rel == "" {
					rel = r.Header.Get("X-Bridge-RelPath")
				}
				mu.Lock()
				deleted[rel]++
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer peerSrv.Close()

	var started int32
	var completed int32
	se := NewSyncEngine(sharedDir, peerSrv.URL, "tok", sm)
	se.OnSyncStarted = func(relPath string) {
		atomic.AddInt32(&started, 1)
	}
	se.OnSyncCompleted = func(relPath string) {
		atomic.AddInt32(&completed, 1)
	}
	if err := se.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Create folder dir_alpha with an older-mtime file and wait for initial sync.
	dirAlpha := filepath.Join(sharedDir, "dir_alpha")
	if err := os.MkdirAll(dirAlpha, 0755); err != nil {
		t.Fatalf("MkdirAll dir_alpha: %v", err)
	}
	payload := []byte("important file inside renamed folder")
	alphaFile := filepath.Join(dirAlpha, "doc.txt")
	if err := os.WriteFile(alphaFile, payload, 0644); err != nil {
		t.Fatalf("WriteFile doc.txt: %v", err)
	}
	oldTime := time.Now().Add(-1 * time.Hour)
	_ = os.Chtimes(alphaFile, oldTime, oldTime)

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, ok := uploaded["dir_alpha/doc.txt"]
		mu.Unlock()
		if ok && atomic.LoadInt32(&completed) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	if _, ok := uploaded["dir_alpha/doc.txt"]; !ok {
		mu.Unlock()
		t.Fatalf("initial dir_alpha/doc.txt did not sync")
	}
	mu.Unlock()

	// Also create and remove an empty directory to verify no stale directory tombstone is saved.
	emptyDir := filepath.Join(sharedDir, "empty_dir")
	_ = os.MkdirAll(emptyDir, 0755)
	time.Sleep(150 * time.Millisecond)
	_ = os.Remove(emptyDir)

	// Rename dir_alpha -> dir_beta.
	dirBeta := filepath.Join(sharedDir, "dir_beta")
	if err := os.Rename(dirAlpha, dirBeta); err != nil {
		t.Fatalf("Rename dir_alpha -> dir_beta: %v", err)
	}

	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, okBeta := uploaded["dir_beta/doc.txt"]
		delAlpha := deleted["dir_alpha/doc.txt"]
		mu.Unlock()
		if okBeta && delAlpha > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	_, okBeta := uploaded["dir_beta/doc.txt"]
	delAlpha := deleted["dir_alpha/doc.txt"]
	delete(uploaded, "dir_alpha/doc.txt")
	mu.Unlock()
	if !okBeta {
		t.Fatalf("dir_beta/doc.txt was not uploaded after folder rename")
	}
	if delAlpha == 0 {
		t.Fatalf("dir_alpha/doc.txt deletion was not propagated after folder rename")
	}

	// Rename dir_beta back -> dir_alpha (even though dir_alpha/doc.txt has a tombstone and old mtime).
	if err := os.Rename(dirBeta, dirAlpha); err != nil {
		t.Fatalf("Rename dir_beta -> dir_alpha: %v", err)
	}

	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, okAlphaBack := uploaded["dir_alpha/doc.txt"]
		mu.Unlock()
		if okAlphaBack && atomic.LoadInt32(&completed) == atomic.LoadInt32(&started) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	se.Stop()

	if _, err := os.Stat(alphaFile); err != nil {
		t.Fatalf("expected renamed-back file dir_alpha/doc.txt to remain on disk, got err=%v", err)
	}
	mu.Lock()
	gotBack := uploaded["dir_alpha/doc.txt"]
	dirTomb := deleted["dir_alpha"] + deleted["empty_dir"]
	mu.Unlock()
	if !bytes.Equal(gotBack, payload) {
		t.Fatalf("expected dir_alpha/doc.txt to re-upload after rename back, got %q", gotBack)
	}
	if dirTomb != 0 {
		t.Fatalf("expected no stale directory delete broadcasts for dir_alpha/empty_dir, got %d", dirTomb)
	}
	if _, exists := sm.Get("empty_dir"); exists {
		t.Fatalf("expected no stale tombstone in stateMgr for empty_dir")
	}
	if _, exists := sm.Get("dir_alpha"); exists {
		t.Fatalf("expected no stale tombstone in stateMgr for dir_alpha")
	}
	if s, c := atomic.LoadInt32(&started), atomic.LoadInt32(&completed); s == 0 || s != c {
		t.Fatalf("expected balanced OnSyncStarted (%d) == OnSyncCompleted (%d)", s, c)
	}
}
