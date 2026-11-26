package printer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSampleA4PDF(t *testing.T, path string) {
	t.Helper()
	pdfContent := `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595.28 841.89] >>
endobj
trailer
<< /Size 4 /Root 1 0 R >>
%%EOF`
	if err := os.WriteFile(path, []byte(pdfContent), 0644); err != nil {
		t.Fatalf("writeSampleA4PDF failed: %v", err)
	}
}

func TestOutboundQueue_EnqueueListFlushAndPrune(t *testing.T) {
	tempDir := t.TempDir()
	qDir := filepath.Join(tempDir, "OutboundQueue")
	srcDir := filepath.Join(tempDir, "Sources")
	_ = os.MkdirAll(srcDir, 0755)

	srcPDF1 := filepath.Join(srcDir, "invoice_1.pdf")
	srcPDF2 := filepath.Join(srcDir, "invoice_2.pdf")
	writeSampleA4PDF(t, srcPDF1)
	writeSampleA4PDF(t, srcPDF2)

	q := NewOutboundQueue(qDir)

	// 1. Enqueue jobs for two different peers
	meta1, err := q.Enqueue("PeerOne", "http://100.64.1.10:8765/", srcPDF1, OutboundJobMeta{
		JobName: "invoice_1.pdf",
	})
	if err != nil {
		t.Fatalf("Enqueue 1 failed: %v", err)
	}
	if meta1.PaperSize != "A4" || meta1.Orientation != "portrait" || meta1.PageCount != 1 {
		t.Errorf("Expected auto-detected A4/portrait/1 page, got %+v", meta1)
	}

	meta2, err := q.Enqueue("PeerTwo", "http://100.64.1.20:8765", srcPDF2, OutboundJobMeta{
		JobName: "invoice_2.pdf",
	})
	if err != nil {
		t.Fatalf("Enqueue 2 failed: %v", err)
	}

	if q.Count() != 2 {
		t.Fatalf("Expected Count() == 2, got %d", q.Count())
	}

	// Verify .meta.json and payload persist even if source temp file is deleted
	_ = os.Remove(srcPDF1)
	if _, err := os.Stat(meta1.FilePath); err != nil {
		t.Fatalf("Expected queued payload to survive source removal: %v", err)
	}

	// 2. ListQueued filtered by peerURL
	peer1Jobs, err := q.ListQueued("http://100.64.1.10:8765")
	if err != nil {
		t.Fatalf("ListQueued failed: %v", err)
	}
	if len(peer1Jobs) != 1 || peer1Jobs[0].ID != meta1.ID {
		t.Fatalf("Expected only meta1 for PeerOne, got %+v", peer1Jobs)
	}

	// 3. Flush failure retains job and persists Attempts + LastError on disk
	sent, err := q.FlushPeer(context.Background(), "PeerOne", "http://100.64.1.10:8765", func(ctx context.Context, job OutboundJobMeta) error {
		return errors.New("peer offline")
	})
	if err == nil || sent != 0 {
		t.Fatalf("Expected flush failure with sent=0, got sent=%d err=%v", sent, err)
	}

	reloaded := NewOutboundQueue(qDir)
	afterFail, err := reloaded.ListQueued("http://100.64.1.10:8765")
	if err != nil || len(afterFail) != 1 {
		t.Fatalf("Expected retained job after failure, got %+v (err=%v)", afterFail, err)
	}
	if afterFail[0].Attempts != 1 || afterFail[0].LastError != "peer offline" {
		t.Errorf("Expected Attempts=1 and LastError='peer offline', got %+v", afterFail[0])
	}

	// 4. Flush success sends and removes PeerOne job while leaving PeerTwo intact
	var deliveredIDs []string
	sent, err = reloaded.FlushPeer(context.Background(), "PeerOne", "http://100.64.1.10:8765", func(ctx context.Context, job OutboundJobMeta) error {
		deliveredIDs = append(deliveredIDs, job.ID)
		return nil
	})
	if err != nil || sent != 1 {
		t.Fatalf("Expected FlushPeer success sent=1, got sent=%d err=%v", sent, err)
	}
	if len(deliveredIDs) != 1 || deliveredIDs[0] != meta1.ID {
		t.Errorf("Expected deliveredIDs [%s], got %v", meta1.ID, deliveredIDs)
	}
	if reloaded.Count() != 1 {
		t.Errorf("Expected Count() == 1 remaining (PeerTwo), got %d", reloaded.Count())
	}

	// 5. PruneOld removes aged job metadata + payload while keeping fresh jobs
	meta2Path := filepath.Join(qDir, meta2.ID+".meta.json")
	rawMeta2, err := os.ReadFile(meta2Path)
	if err != nil {
		t.Fatalf("ReadFile meta2 failed: %v", err)
	}
	var m2 OutboundJobMeta
	_ = json.Unmarshal(rawMeta2, &m2)
	m2.CreatedAt = time.Now().Add(-48 * time.Hour)
	updatedRaw, _ := json.Marshal(m2)
	_ = os.WriteFile(meta2Path, updatedRaw, 0644)

	pruned, err := reloaded.PruneOld(24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneOld failed: %v", err)
	}
	if pruned != 1 || reloaded.Count() != 0 {
		t.Errorf("Expected pruned=1 and Count()=0, got pruned=%d count=%d", pruned, reloaded.Count())
	}
	if _, err := os.Stat(meta2.FilePath); !os.IsNotExist(err) {
		t.Errorf("Expected pruned payload file %s to be deleted", meta2.FilePath)
	}

	// 6. Remove non-existent returns os.ErrNotExist; valid Remove deletes job
	if err := reloaded.Remove("missing_id"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Expected os.ErrNotExist for missing ID, got %v", err)
	}
	meta3, err := reloaded.Enqueue("PeerThree", "http://100.64.1.30:8765", srcPDF2, OutboundJobMeta{})
	if err != nil {
		t.Fatalf("Enqueue 3 failed: %v", err)
	}
	if err := reloaded.Remove(meta3.ID); err != nil {
		t.Fatalf("Remove(%s) failed: %v", meta3.ID, err)
	}
	if reloaded.Count() != 0 {
		t.Errorf("Expected Count()=0 after Remove, got %d", reloaded.Count())
	}
}
