package printer

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"bridge/config"
)

func buildTestXPSArchive(t *testing.T, openXPS bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	ns := "http://schemas.microsoft.com/xps/2005/06"
	relsNS := "http://schemas.microsoft.com/xps/2005/06/fixedrepresentation"
	if openXPS {
		ns = "http://schemas.openxps.org/oxps/v1.0"
		relsNS = "http://schemas.openxps.org/oxps/v1.0/fixedrepresentation"
	}

	writeEntry := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}

	writeEntry("[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="fpage" ContentType="application/vnd.ms-package.xps-fixedpage+xml"/></Types>`)
	writeEntry("_rels/.rels", fmt.Sprintf(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Type="%s" Target="/FixedDocSeq.fdseq" Id="r1"/></Relationships>`, relsNS))
	writeEntry("FixedDocSeq.fdseq", fmt.Sprintf(`<FixedDocumentSequence xmlns="%s"><DocumentReference Source="/Documents/1/FixedDoc.fdoc"/></FixedDocumentSequence>`, ns))
	writeEntry("Documents/1/Pages/1.fpage", fmt.Sprintf(`<FixedPage xmlns="%s" Width="793.7" Height="1122.5" xml:lang="en-US"/>`, ns))

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestDetectSpoolFormat_AllFormats(t *testing.T) {
	msXPS := buildTestXPSArchive(t, false)
	openXPS := buildTestXPSArchive(t, true)

	emfHeader := make([]byte, 64)
	binary.LittleEndian.PutUint32(emfHeader[0:4], 1)
	copy(emfHeader[40:44], []byte(" EMF"))

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"PDF", []byte("%PDF-1.7\n1 0 obj\n%%EOF"), ".pdf"},
		{"MS-XPS", msXPS, ".xps"},
		{"OpenXPS", openXPS, ".oxps"},
		{"PNG", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), ".png"},
		{"JPEG", []byte("\xff\xd8\xff\xe0\x00\x10JFIF"), ".jpg"},
		{"GIF", []byte("GIF89a\x01\x00\x01\x00"), ".gif"},
		{"BMP", []byte("BM\x36\x00\x00\x00"), ".bmp"},
		{"TIFF_LE", []byte("II*\x00\x08\x00\x00\x00"), ".tif"},
		{"TIFF_BE", []byte("MM\x00*\x00\x00\x00\x08"), ".tif"},
		{"PostScript", []byte("%!PS-Adobe-3.0\n%%Pages: 1\n"), ".ps"},
		{"PCL_UEL", []byte("\x1b%-12345X@PJL JOB\r\n@PJL ENTER LANGUAGE = PCL\r\n"), ".pcl"},
		{"PCL_Reset", []byte("\x1bE\x1b&l26AHello PCL"), ".pcl"},
		{"EMF", emfHeader, ".emf"},
		{"UnknownBinary", []byte{0x00, 0xDE, 0xAD, 0xBE, 0xEF, 0x42, 0x11}, ".bin"},
		{"PlainUnknownText", []byte("RANDOM UNRECOGNIZED SPOOL STREAM"), ".bin"},
		{"Empty", nil, ".bin"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectSpoolFormat(tc.data)
			if got != tc.want {
				t.Errorf("DetectSpoolFormat(%s) = %q; want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestExtractCleanSpoolPayload_OpenXPSAndPostScript(t *testing.T) {
	openXPS := buildTestXPSArchive(t, true)
	pjlOXPS := append([]byte("\x1b%-12345X@PJL JOB\r\n@PJL ENTER LANGUAGE = XPS\r\n"), openXPS...)
	pjlOXPS = append(pjlOXPS, []byte("\x1b%-12345X@PJL EOJ\r\n")...)

	clean, ext := ExtractCleanSpoolPayload(pjlOXPS)
	if ext != ".oxps" {
		t.Fatalf("Expected .oxps for PJL-wrapped OpenXPS, got %q", ext)
	}
	if !bytes.Equal(clean, openXPS) {
		t.Errorf("Extracted OpenXPS payload did not match original ZIP bytes")
	}

	pjlPS := []byte("\x1b%-12345X@PJL JOB\r\n@PJL ENTER LANGUAGE = POSTSCRIPT\r\n%!PS-Adobe-3.0\nshowpage\n\x1b%-12345X@PJL EOJ\r\n")
	cleanPS, psExt := ExtractCleanSpoolPayload(pjlPS)
	if psExt != ".ps" {
		t.Fatalf("Expected .ps for PJL-wrapped PostScript, got %q", psExt)
	}
	if !bytes.HasPrefix(cleanPS, []byte("%!PS")) || bytes.Contains(cleanPS, []byte("@PJL")) {
		t.Errorf("Expected stripped PostScript payload, got %q", string(cleanPS))
	}
}

type failingReadConn struct {
	chunks    [][]byte
	err       error
	idx       int
	closed    bool
	localAddr net.Addr
}

func (c *failingReadConn) Read(p []byte) (int, error) {
	if c.idx < len(c.chunks) {
		n := copy(p, c.chunks[c.idx])
		c.chunks[c.idx] = c.chunks[c.idx][n:]
		if len(c.chunks[c.idx]) == 0 {
			c.idx++
		}
		return n, nil
	}
	return 0, c.err
}
func (c *failingReadConn) Write(p []byte) (int, error)  { return len(p), nil }
func (c *failingReadConn) Close() error                 { c.closed = true; return nil }
func (c *failingReadConn) LocalAddr() net.Addr          { return c.localAddr }
func (c *failingReadConn) RemoteAddr() net.Addr         { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55555} }
func (c *failingReadConn) SetDeadline(time.Time) error      { return nil }
func (c *failingReadConn) SetReadDeadline(time.Time) error  { return nil }
func (c *failingReadConn) SetWriteDeadline(time.Time) error { return nil }

func TestSpoolerListener_AbortsTruncatedStreamOnReadError(t *testing.T) {
	dispatched := false
	var failedErr error

	sl := NewSpoolerListener("127.0.0.1:0", func(filePath string) error {
		dispatched = true
		return nil
	})
	sl.OnJobFailed = func(fileName string, err error) {
		failedErr = err
	}

	conn := &failingReadConn{
		chunks: [][]byte{[]byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>")},
		err:    errors.New("simulated TCP reset"),
	}
	sl.handleConnection(conn)

	if dispatched {
		t.Fatal("Expected dispatchFunc NOT to be called when connection read failed with non-EOF error")
	}
	if failedErr == nil || !strings.Contains(failedErr.Error(), "simulated TCP reset") {
		t.Errorf("Expected OnJobFailed with read error, got %v", failedErr)
	}
	if !conn.closed {
		t.Errorf("Expected connection to be closed")
	}
}

func reserveFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestMultiSpoolerListener_SyncListenersAndRemove(t *testing.T) {
	msl := NewMultiSpoolerListener(func(peer config.PeerDevice, filePath string) error {
		return nil
	})
	defer msl.StopAll()

	p1 := reserveFreePort(t)
	p2 := reserveFreePort(t)
	p3 := reserveFreePort(t)

	peers := []config.PeerDevice{
		{Name: "Unit A", IP: "100.64.0.1", URL: "http://100.64.0.1:8765", SpoolPort: p1},
		{Name: "Unit B", IP: "100.64.0.2", URL: "http://100.64.0.2:8765", SpoolPort: p2},
	}

	if err := msl.SyncListeners(peers); err != nil {
		t.Fatalf("Initial SyncListeners failed: %v", err)
	}
	if got := msl.ActivePorts(); len(got) != 2 {
		t.Fatalf("Expected 2 active ports, got %v", got)
	}

	// Reassign Unit A from p1 -> p3 and remove Unit B (p2)
	updatedPeers := []config.PeerDevice{
		{Name: "Unit A", IP: "100.64.0.1", URL: "http://100.64.0.1:8765", SpoolPort: p3},
	}
	if err := msl.SyncListeners(updatedPeers); err != nil {
		t.Fatalf("Updated SyncListeners failed: %v", err)
	}

	ports := msl.ActivePorts()
	if len(ports) != 1 || ports[0] != p3 {
		t.Fatalf("Expected only port %d active after SyncListeners, got %v", p3, ports)
	}

	// Verify p1 and p2 are truly closed (we can re-bind them immediately)
	for _, closedPort := range []int{p1, p2} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", closedPort))
		if err != nil {
			t.Errorf("Expected port %d to be closed and reboundable, but Listen failed: %v", closedPort, err)
		} else {
			_ = ln.Close()
		}
	}

	// Verify RemovePeerListener works by peer name
	if !msl.RemovePeerListener("Unit A") {
		t.Errorf("Expected RemovePeerListener('Unit A') to return true")
	}
	if len(msl.ActivePorts()) != 0 {
		t.Errorf("Expected 0 active ports after RemovePeerListener, got %v", msl.ActivePorts())
	}
	if msl.RemovePeerListener("NonExistent") {
		t.Errorf("Expected RemovePeerListener('NonExistent') to return false")
	}

	// Ensure reading body of existing clean file works via extractCleanSpoolFile
	tmp, err := os.CreateTemp("", "spool_extract_*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	openXPS := buildTestXPSArchive(t, true)
	_, _ = tmp.Write(openXPS)
	_ = tmp.Close()
	defer os.Remove(tmp.Name())

	cleanPath, ext, err := extractCleanSpoolFile(tmp.Name())
	if err != nil {
		t.Fatalf("extractCleanSpoolFile failed: %v", err)
	}
	defer os.Remove(cleanPath)
	if ext != ".oxps" {
		t.Errorf("Expected .oxps from extractCleanSpoolFile, got %q", ext)
	}
	cleanBytes, err := os.ReadFile(cleanPath)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadFile(cleanPath) failed: %v", err)
	}
	if !bytes.Equal(cleanBytes, openXPS) {
		t.Errorf("Clean file content mismatch")
	}
}
