package printer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bridge/config"
)

const (
	// defaultMaxSpoolSizeBytes caps a single incoming RAW TCP spool stream at 512 MiB
	// to prevent unbounded disk or memory exhaustion on malformed connections.
	defaultMaxSpoolSizeBytes int64 = 512 * 1024 * 1024
)

// SpoolerListener listens on RAW TCP (default 127.0.0.1:9100) for print jobs streamed
// by Windows Spooler via Standard TCP/IP Port Monitor (tcpmon.dll).
type SpoolerListener struct {
	addr             string
	dispatchFunc     func(filePath string) error
	maxSpoolBytes    int64
	listener         net.Listener
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	OnJobStarted     func(fileName string)
	OnJobCompleted   func(fileName string)
	OnJobFailed      func(fileName string, err error)
}

// NewSpoolerListener creates a listener for Windows RAW TCP printing.
func NewSpoolerListener(addr string, dispatchFunc func(filePath string) error) *SpoolerListener {
	if addr == "" {
		addr = "127.0.0.1:9100"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SpoolerListener{
		addr:          addr,
		dispatchFunc:  dispatchFunc,
		maxSpoolBytes: defaultMaxSpoolSizeBytes,
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Start begins listening on the configured TCP address.
func (sl *SpoolerListener) Start() error {
	l, err := net.Listen("tcp", sl.addr)
	if err != nil {
		return fmt.Errorf("failed to bind spooler listener on %s: %w", sl.addr, err)
	}
	sl.listener = l
	sl.wg.Add(1)
	go sl.acceptLoop()
	log.Printf("[SpoolerListener] RAW TCP print spooler listening on %s", sl.addr)
	return nil
}

// Stop gracefully shuts down the spooler listener.
func (sl *SpoolerListener) Stop() {
	sl.cancel()
	if sl.listener != nil {
		_ = sl.listener.Close()
	}
	sl.wg.Wait()
	log.Printf("[SpoolerListener] Spooler listener stopped.")
}

func (sl *SpoolerListener) acceptLoop() {
	defer sl.wg.Done()

	for {
		conn, err := sl.listener.Accept()
		if err != nil {
			select {
			case <-sl.ctx.Done():
				return
			default:
				log.Printf("[SpoolerListener] Accept error: %v", err)
				return
			}
		}

		sl.wg.Add(1)
		go func(c net.Conn) {
			defer sl.wg.Done()
			sl.handleConnection(c)
		}(conn)
	}
}

func (sl *SpoolerListener) handleConnection(conn net.Conn) {
	defer conn.Close()

	// 120-second deadline for reading spool data over loopback
	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))

	maxBytes := sl.maxSpoolBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxSpoolSizeBytes
	}

	rawTmp, err := os.CreateTemp("", "bridge_spool_raw_*.tmp")
	if err != nil {
		log.Printf("[SpoolerListener] Failed to create temp spool file: %v", err)
		if sl.OnJobFailed != nil {
			sl.OnJobFailed("spool_stream", err)
		}
		return
	}
	rawPath := rawTmp.Name()
	removeRaw := true
	defer func() {
		if removeRaw {
			_ = os.Remove(rawPath)
		}
	}()

	buf := make([]byte, 64*1024)
	written, readErr := io.CopyBuffer(rawTmp, io.LimitReader(conn, maxBytes+1), buf)
	closeErr := rawTmp.Close()

	// Never dispatch truncated data if the socket read failed before clean EOF.
	if readErr != nil {
		log.Printf("[SpoolerListener] Connection read error after %d bytes (aborting truncated job): %v", written, readErr)
		if sl.OnJobFailed != nil {
			sl.OnJobFailed("spool_stream", fmt.Errorf("incomplete spool stream: %w", readErr))
		}
		return
	}
	if closeErr != nil {
		log.Printf("[SpoolerListener] Failed to close temp spool file: %v", closeErr)
		if sl.OnJobFailed != nil {
			sl.OnJobFailed("spool_stream", closeErr)
		}
		return
	}
	if written == 0 {
		return
	}
	if written > maxBytes {
		sizeErr := fmt.Errorf("spool stream exceeded max size limit (%d bytes)", maxBytes)
		log.Printf("[SpoolerListener] %v", sizeErr)
		if sl.OnJobFailed != nil {
			sl.OnJobFailed("spool_stream", sizeErr)
		}
		return
	}

	log.Printf("[SpoolerListener] Received %d bytes print stream from %s", written, conn.RemoteAddr())

	tmpFile, ext, err := extractCleanSpoolFile(rawPath)
	if err != nil {
		log.Printf("[SpoolerListener] Failed to extract clean spool payload: %v", err)
		if sl.OnJobFailed != nil {
			sl.OnJobFailed("spool_stream", err)
		}
		return
	}
	if tmpFile == rawPath {
		// Reused/renamed path already handled
		removeRaw = false
	}
	jobName := filepath.Base(tmpFile)
	_ = ext
	defer os.Remove(tmpFile)

	// Acknowledge and close local spooler TCP connection so Windows Spooler
	// marks the spool job completed without stalling or declaring the printer offline.
	_ = conn.Close()

	if sl.OnJobStarted != nil {
		sl.OnJobStarted(jobName)
	}

	if sl.dispatchFunc != nil {
		if err := sl.dispatchFunc(tmpFile); err != nil {
			log.Printf("[SpoolerListener] Failed to dispatch print job %s: %v", jobName, err)
			if sl.OnJobFailed != nil {
				sl.OnJobFailed(jobName, err)
			}
			return
		}
	}

	if sl.OnJobCompleted != nil {
		sl.OnJobCompleted(jobName)
	}
	log.Printf("[SpoolerListener] Print job %s delivered successfully.", jobName)
}

// extractCleanSpoolFile inspects a streamed spool file on disk, strips any PJL wrappers
// without loading large streams into RAM, determines the accurate extension (.pdf/.xps/.oxps/etc.),
// and places the clean payload at a unique temp file path.
func extractCleanSpoolFile(rawPath string) (string, string, error) {
	f, err := os.Open(rawPath)
	if err != nil {
		return "", "", err
	}
	fi, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return "", "", statErr
	}
	totalSize := fi.Size()

	headSize := int64(4096)
	if totalSize < headSize {
		headSize = totalSize
	}
	header := make([]byte, headSize)
	n, _ := io.ReadFull(f, header)
	header = header[:n]
	_ = f.Close()

	var payloadStart int64
	payloadEnd := totalSize
	ext := ""

	switch {
	case bytes.HasPrefix(header, []byte("PK\x03\x04")):
		payloadStart = 0
		payloadEnd = findPayloadEnd(rawPath, 0, ".xps")
		ext = ".xps"
	case bytes.HasPrefix(header, []byte("%PDF-")):
		payloadStart = 0
		payloadEnd = findPayloadEnd(rawPath, 0, ".pdf")
		ext = ".pdf"
	case bytes.HasPrefix(header, []byte("\x1b%-12345X")) || bytes.HasPrefix(header, []byte("@PJL")):
		if start, foundExt, ok := findSpoolPayloadStart(rawPath, 1024*1024); ok {
			payloadStart = start
			payloadEnd = findPayloadEnd(rawPath, start, foundExt)
			ext = foundExt
		} else {
			ext = DetectSpoolFormat(header)
		}
	default:
		if start, foundExt, ok := findSpoolPayloadStart(rawPath, 64*1024); ok && (foundExt == ".pdf" || foundExt == ".xps") {
			payloadStart = start
			payloadEnd = findPayloadEnd(rawPath, start, foundExt)
			ext = foundExt
		} else {
			ext = DetectSpoolFormat(header)
		}
	}

	if payloadEnd < payloadStart || payloadEnd > totalSize {
		payloadEnd = totalSize
	}

	cleanPath := filepath.Join(os.TempDir(), fmt.Sprintf("print_%d%s", time.Now().UnixNano(), ext))
	if payloadStart == 0 && payloadEnd == totalSize {
		if err := os.Rename(rawPath, cleanPath); err == nil {
			if ext == ".xps" {
				if detected := detectZIPSpoolFormatFromFile(cleanPath); detected != "" && detected != ext {
					betterPath := strings.TrimSuffix(cleanPath, ext) + detected
					if renErr := os.Rename(cleanPath, betterPath); renErr == nil {
						cleanPath = betterPath
						ext = detected
					}
				}
			}
			return cleanPath, ext, nil
		}
	}

	src, err := os.Open(rawPath)
	if err != nil {
		return "", "", err
	}
	if _, err := src.Seek(payloadStart, io.SeekStart); err != nil {
		_ = src.Close()
		return "", "", err
	}
	dst, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		_ = src.Close()
		return "", "", err
	}
	buf := make([]byte, 64*1024)
	_, copyErr := io.CopyBuffer(dst, io.LimitReader(src, payloadEnd-payloadStart), buf)
	closeDstErr := dst.Close()
	_ = src.Close()
	if copyErr != nil {
		_ = os.Remove(cleanPath)
		return "", "", copyErr
	}
	if closeDstErr != nil {
		_ = os.Remove(cleanPath)
		return "", "", closeDstErr
	}

	if ext == ".xps" {
		if detected := detectZIPSpoolFormatFromFile(cleanPath); detected != "" && detected != ext {
			betterPath := strings.TrimSuffix(cleanPath, ext) + detected
			if renErr := os.Rename(cleanPath, betterPath); renErr == nil {
				cleanPath = betterPath
				ext = detected
			}
		}
	}

	return cleanPath, ext, nil
}

// findSpoolPayloadStart scans up to maxScanBytes in fixed chunks for %PDF-, PK\x03\x04, or %!PS.
func findSpoolPayloadStart(path string, maxScanBytes int64) (int64, string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", false
	}
	defer f.Close()

	const chunkSize = 64 * 1024
	const overlap = 16
	buf := make([]byte, chunkSize+overlap)
	carry := 0
	var offset int64

	for offset < maxScanBytes {
		n, readErr := f.Read(buf[carry : carry+chunkSize])
		if n == 0 {
			break
		}
		window := buf[:carry+n]

		type candidate struct {
			idx int
			ext string
		}
		var best *candidate
		consider := func(idx int, ext string) {
			if idx >= 0 && (best == nil || idx < best.idx) {
				best = &candidate{idx: idx, ext: ext}
			}
		}
		consider(bytes.Index(window, []byte("%PDF-")), ".pdf")
		consider(bytes.Index(window, []byte("PK\x03\x04")), ".xps")
		consider(bytes.Index(window, []byte("%!PS")), ".ps")

		if best != nil {
			return offset - int64(carry) + int64(best.idx), best.ext, true
		}

		offset += int64(n)
		if readErr != nil {
			break
		}
		if len(window) >= overlap {
			copy(buf[:overlap], window[len(window)-overlap:])
			carry = overlap
		} else {
			copy(buf[:len(window)], window)
			carry = len(window)
		}
	}
	return 0, "", false
}

// ExtractCleanSpoolPayload strips PJL / spool wrappers and returns clean document payload and extension.
func ExtractCleanSpoolPayload(data []byte) ([]byte, string) {
	if len(data) == 0 {
		return data, ".bin"
	}

	// 1. Clean XPS / OpenXPS container (PK\x03\x04 at offset 0)
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		clean := stripZIPSpoolTrailer(data)
		return clean, detectZIPSpoolFormat(clean)
	}

	// 2. Clean PDF container (%PDF- at offset 0)
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		clean := stripPDFSpoolTrailer(data)
		return clean, ".pdf"
	}

	// 3. PJL / Spool wrapper: check for encapsulated PDF
	if pdfIdx := bytes.Index(data, []byte("%PDF-")); pdfIdx != -1 {
		clean := stripPDFSpoolTrailer(data[pdfIdx:])
		return clean, ".pdf"
	}

	// 4. PJL / Spool wrapper: check for encapsulated XPS / OpenXPS
	if xpsIdx := bytes.Index(data, []byte("PK\x03\x04")); xpsIdx != -1 {
		clean := stripZIPSpoolTrailer(data[xpsIdx:])
		return clean, detectZIPSpoolFormat(clean)
	}

	// 5. PJL / Spool wrapper: check for encapsulated PostScript
	if psIdx := bytes.Index(data, []byte("%!PS")); psIdx != -1 {
		clean := data[psIdx:]
		if uelIdx := bytes.Index(clean[4:], []byte("\x1b%-12345X")); uelIdx != -1 {
			clean = clean[:4+uelIdx]
		}
		return clean, ".ps"
	}

	return data, DetectSpoolFormat(data)
}

func stripZIPSpoolTrailer(clean []byte) []byte {
	if eocdIdx := bytes.LastIndex(clean, []byte("PK\x05\x06")); eocdIdx != -1 && len(clean) >= eocdIdx+22 {
		commentLen := int(binary.LittleEndian.Uint16(clean[eocdIdx+20 : eocdIdx+22]))
		zipEnd := eocdIdx + 22 + commentLen
		if zipEnd <= len(clean) {
			return clean[:zipEnd]
		}
	}
	if len(clean) > 4 {
		if uelIdx := bytes.Index(clean[4:], []byte("\x1b%-12345X")); uelIdx != -1 {
			return clean[:4+uelIdx]
		}
	}
	return clean
}

func stripPDFSpoolTrailer(clean []byte) []byte {
	if eofIdx := bytes.LastIndex(clean, []byte("%%EOF")); eofIdx != -1 {
		end := eofIdx + 5
		for end < len(clean) && (clean[end] == '\r' || clean[end] == '\n' || clean[end] == ' ') {
			end++
		}
		if end <= len(clean) {
			return clean[:end]
		}
	} else if len(clean) > 5 {
		if uelIdx := bytes.Index(clean[5:], []byte("\x1b%-12345X")); uelIdx != -1 {
			return clean[:5+uelIdx]
		}
	}
	return clean
}

// DetectSpoolFormat inspects magic bytes and headers to accurately classify spool streams
// into .pdf, .xps, .oxps, image extensions, .ps, .pcl, .emf, or .bin.
func DetectSpoolFormat(data []byte) string {
	if len(data) == 0 {
		return ".bin"
	}

	headLen := len(data)
	if headLen > 4096 {
		headLen = 4096
	}
	header := data[:headLen]

	// 1. Standard image formats (PNG, JPEG, GIF, BMP, TIFF)
	if imgExt := detectImageMagic(header); imgExt != "" {
		return imgExt
	}

	// 2. Open Packaging Convention zip container (MS-XPS vs OpenXPS)
	if bytes.HasPrefix(header, []byte("PK\x03\x04")) {
		return detectZIPSpoolFormat(data)
	}

	// 3. Native PDF stream starting with %PDF-
	if bytes.HasPrefix(header, []byte("%PDF-")) {
		return ".pdf"
	}

	// 4. PostScript stream (%!PS)
	if bytes.HasPrefix(header, []byte("%!PS")) {
		return ".ps"
	}

	// 5. Windows Enhanced Metafile (EMF): record type 1 (0x01000000) + " EMF" signature
	if isEMFStream(header) {
		return ".emf"
	}

	// 6. PJL encapsulated stream
	if bytes.HasPrefix(header, []byte("\x1b%-12345X")) || bytes.HasPrefix(header, []byte("@PJL")) {
		if bytes.Contains(header, []byte("%PDF-")) {
			return ".pdf"
		}
		if idx := bytes.Index(header, []byte("PK\x03\x04")); idx >= 0 {
			return detectZIPSpoolFormat(data[idx:])
		}
		if bytes.Contains(header, []byte("%!PS")) || bytes.Contains(bytes.ToUpper(header), []byte("ENTER LANGUAGE = POSTSCRIPT")) {
			return ".ps"
		}
		return ".pcl"
	}

	// 7. Raw PCL escape sequences
	if bytes.HasPrefix(header, []byte("\x1bE")) ||
		bytes.HasPrefix(header, []byte("\x1b&l")) ||
		bytes.HasPrefix(header, []byte("\x1b*")) {
		return ".pcl"
	}

	// 8. Embedded signatures within initial spool header
	if bytes.Contains(header, []byte("%PDF-")) {
		return ".pdf"
	}
	if idx := bytes.Index(header, []byte("PK\x03\x04")); idx >= 0 {
		return detectZIPSpoolFormat(data[idx:])
	}
	if bytes.Contains(header, []byte("%!PS")) {
		return ".ps"
	}

	return ".bin"
}

func isEMFStream(header []byte) bool {
	if !bytes.HasPrefix(header, []byte{0x01, 0x00, 0x00, 0x00}) {
		return false
	}
	if len(header) >= 44 && bytes.Equal(header[40:44], []byte(" EMF")) {
		return true
	}
	return bytes.Contains(header, []byte(" EMF"))
}

func hasOpenXPSMarkers(raw []byte) bool {
	lower := bytes.ToLower(raw)
	return bytes.Contains(lower, []byte("schemas.openxps.org")) ||
		bytes.Contains(lower, []byte("oxps/v1.0")) ||
		bytes.Contains(lower, []byte("application/oxps"))
}

func detectZIPSpoolFormat(data []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err == nil {
		return detectZIPSpoolFormatFromReader(zr)
	}
	// Fallback for incomplete/header-only ZIP buffers
	sample := data
	if len(sample) > 64*1024 {
		sample = sample[:64*1024]
	}
	if hasOpenXPSMarkers(sample) {
		return ".oxps"
	}
	return ".xps"
}

func detectZIPSpoolFormatFromFile(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	return detectZIPSpoolFormatFromReader(&zr.Reader)
}

func detectZIPSpoolFormatFromReader(zr *zip.Reader) string {
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		if lower != "[content_types].xml" &&
			lower != "_rels/.rels" &&
			!strings.HasSuffix(lower, ".fdseq") &&
			!strings.HasSuffix(lower, ".fpage") &&
			!strings.HasSuffix(lower, ".fdoc") &&
			!strings.HasSuffix(lower, ".rels") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(rc, 64*1024))
		_ = rc.Close()
		if hasOpenXPSMarkers(buf) {
			return ".oxps"
		}
	}
	return ".xps"
}

// MultiSpoolerListener manages multiple RAW TCP listeners for routing Windows print jobs
// from distinct virtual printers (e.g. "Print on Unit 2", "Print on Unit 3") to their respective peers.
type MultiSpoolerListener struct {
	mu           sync.Mutex
	listeners    map[int]*SpoolerListener
	peersByPort  map[int]config.PeerDevice
	dispatchFunc func(peer config.PeerDevice, filePath string) error

	OnJobStarted   func(peer config.PeerDevice, fileName string)
	OnJobCompleted func(peer config.PeerDevice, fileName string)
	OnJobFailed    func(peer config.PeerDevice, fileName string, err error)
}

// NewMultiSpoolerListener creates a manager for multi-peer virtual printer listeners.
func NewMultiSpoolerListener(dispatchFunc func(peer config.PeerDevice, filePath string) error) *MultiSpoolerListener {
	return &MultiSpoolerListener{
		listeners:    make(map[int]*SpoolerListener),
		peersByPort:  make(map[int]config.PeerDevice),
		dispatchFunc: dispatchFunc,
	}
}

// EnsurePeerListener starts a TCP listener on 127.0.0.1:port for the given peer if not already running.
func (msl *MultiSpoolerListener) EnsurePeerListener(peer config.PeerDevice) error {
	msl.mu.Lock()
	defer msl.mu.Unlock()

	port := peer.SpoolPort
	if port <= 0 {
		return fmt.Errorf("invalid spool port %d for peer %s", port, peer.Name)
	}

	if msl.peersByPort == nil {
		msl.peersByPort = make(map[int]config.PeerDevice)
	}
	msl.peersByPort[port] = peer

	if _, exists := msl.listeners[port]; exists {
		return nil // already active, peer state updated
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	listenerPort := port

	listener := NewSpoolerListener(addr, func(filePath string) error {
		msl.mu.Lock()
		curPeer := msl.peersByPort[listenerPort]
		msl.mu.Unlock()
		if msl.dispatchFunc != nil {
			return msl.dispatchFunc(curPeer, filePath)
		}
		return nil
	})

	listener.OnJobStarted = func(fileName string) {
		msl.mu.Lock()
		curPeer := msl.peersByPort[listenerPort]
		msl.mu.Unlock()
		if msl.OnJobStarted != nil {
			msl.OnJobStarted(curPeer, fileName)
		}
	}
	listener.OnJobCompleted = func(fileName string) {
		msl.mu.Lock()
		curPeer := msl.peersByPort[listenerPort]
		msl.mu.Unlock()
		if msl.OnJobCompleted != nil {
			msl.OnJobCompleted(curPeer, fileName)
		}
	}
	listener.OnJobFailed = func(fileName string, err error) {
		msl.mu.Lock()
		curPeer := msl.peersByPort[listenerPort]
		msl.mu.Unlock()
		if msl.OnJobFailed != nil {
			msl.OnJobFailed(curPeer, fileName, err)
		}
	}

	if err := listener.Start(); err != nil {
		return fmt.Errorf("failed to bind spooler listener for %s on %s: %w", peer.Name, addr, err)
	}

	msl.listeners[port] = listener
	log.Printf("[MultiSpoolerListener] Started listener on %s for virtual printer 'Print on %s'", addr, peer.Name)
	return nil
}

// RemovePeerListener stops and removes any listener matching a peer name, URL, IP, or port number.
// Returns true if at least one listener was stopped and removed.
func (msl *MultiSpoolerListener) RemovePeerListener(peerNameOrPort string) bool {
	target := strings.TrimSpace(peerNameOrPort)
	if target == "" {
		return false
	}

	targetPort, portErr := strconv.Atoi(target)
	var toStop []*SpoolerListener

	msl.mu.Lock()
	for port, l := range msl.listeners {
		p := msl.peersByPort[port]
		matched := (portErr == nil && port == targetPort) ||
			strings.EqualFold(p.Name, target) ||
			strings.EqualFold(strings.TrimRight(p.URL, "/"), strings.TrimRight(target, "/")) ||
			(p.IP != "" && strings.EqualFold(p.IP, target))
		if matched {
			toStop = append(toStop, l)
			delete(msl.listeners, port)
			delete(msl.peersByPort, port)
		}
	}
	msl.mu.Unlock()

	for _, l := range toStop {
		l.Stop()
	}
	return len(toStop) > 0
}

// SyncListeners reconciles active TCP spooler listeners with the current peer list:
// it stops listeners for removed or port-reassigned peers and ensures listeners for active peers.
func (msl *MultiSpoolerListener) SyncListeners(peers []config.PeerDevice) error {
	desiredPorts := make(map[int]config.PeerDevice, len(peers))
	for _, p := range peers {
		if p.SpoolPort > 0 {
			desiredPorts[p.SpoolPort] = p
		}
	}

	var toStop []*SpoolerListener
	var stoppedPorts []int

	msl.mu.Lock()
	for port, l := range msl.listeners {
		if _, keep := desiredPorts[port]; !keep {
			toStop = append(toStop, l)
			stoppedPorts = append(stoppedPorts, port)
			delete(msl.listeners, port)
			delete(msl.peersByPort, port)
		}
	}
	msl.mu.Unlock()

	for i, l := range toStop {
		l.Stop()
		log.Printf("[MultiSpoolerListener] Closed obsolete listener on 127.0.0.1:%d", stoppedPorts[i])
	}

	var errs []error
	for _, p := range peers {
		if p.SpoolPort <= 0 {
			continue
		}
		if err := msl.EnsurePeerListener(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ActivePorts returns a sorted slice of TCP ports currently bound by this manager.
func (msl *MultiSpoolerListener) ActivePorts() []int {
	msl.mu.Lock()
	defer msl.mu.Unlock()

	ports := make([]int, 0, len(msl.listeners))
	for port := range msl.listeners {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

// StopAll stops all active peer spooler listeners.
func (msl *MultiSpoolerListener) StopAll() {
	msl.mu.Lock()
	toStop := make([]*SpoolerListener, 0, len(msl.listeners))
	for port, l := range msl.listeners {
		toStop = append(toStop, l)
		delete(msl.listeners, port)
		delete(msl.peersByPort, port)
	}
	msl.mu.Unlock()

	for _, l := range toStop {
		l.Stop()
	}
	log.Printf("[MultiSpoolerListener] All spooler listeners stopped.")
}
