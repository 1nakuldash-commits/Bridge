package printer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"bridge/config"
)

// SpoolerListener listens on RAW TCP (default 127.0.0.1:9100) for print jobs streamed
// by Windows Spooler via Standard TCP/IP Port Monitor (tcpmon.dll).
type SpoolerListener struct {
	addr           string
	dispatchFunc   func(filePath string) error
	listener       net.Listener
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	OnJobStarted   func(fileName string)
	OnJobCompleted func(fileName string)
	OnJobFailed    func(fileName string, err error)
}

// NewSpoolerListener creates a listener for Windows RAW TCP printing.
func NewSpoolerListener(addr string, dispatchFunc func(filePath string) error) *SpoolerListener {
	if addr == "" {
		addr = "127.0.0.1:9100"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &SpoolerListener{
		addr:         addr,
		dispatchFunc: dispatchFunc,
		ctx:          ctx,
		cancel:       cancel,
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

	// 120-second timeout for reading spool data over loopback
	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))

	data, err := io.ReadAll(conn)
	if err != nil && len(data) == 0 {
		log.Printf("[SpoolerListener] Connection read error: %v", err)
		return
	}

	if len(data) == 0 {
		return
	}

	log.Printf("[SpoolerListener] Received %d bytes print stream from %s", len(data), conn.RemoteAddr())

	ext := DetectSpoolFormat(data)
	jobName := fmt.Sprintf("print_%d%s", time.Now().UnixNano(), ext)

	tmpFile := filepath.Join(os.TempDir(), jobName)
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		log.Printf("[SpoolerListener] Failed to write temp spool file: %v", err)
		if sl.OnJobFailed != nil {
			sl.OnJobFailed(jobName, err)
		}
		return
	}
	defer os.Remove(tmpFile)

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

// DetectSpoolFormat inspects byte headers to determine whether stream is XPS or PDF.
func DetectSpoolFormat(data []byte) string {
	headLen := len(data)
	if headLen > 4096 {
		headLen = 4096
	}
	header := data[:headLen]

	// XPS is an Open Packaging Convention zip container starting with PK\x03\x04
	if bytes.HasPrefix(header, []byte("PK\x03\x04")) {
		return ".xps"
	}

	// Native PDF stream starting with %PDF-
	if bytes.HasPrefix(header, []byte("%PDF-")) || bytes.Contains(header, []byte("%PDF-")) {
		return ".pdf"
	}

	// PJL encapsulated stream
	if bytes.HasPrefix(header, []byte("\x1b%-12345X")) {
		if bytes.Contains(header, []byte("%PDF-")) {
			return ".pdf"
		}
		if bytes.Contains(header, []byte("PK\x03\x04")) {
			return ".xps"
		}
	}

	// If zip signature anywhere in initial buffer
	if bytes.Contains(header, []byte("PK\x03\x04")) {
		return ".xps"
	}

	return ".pdf"
}

// MultiSpoolerListener manages multiple RAW TCP listeners for routing Windows print jobs
// from distinct virtual printers (e.g. "Print on Unit 2", "Print on Unit 3") to their respective peers.
type MultiSpoolerListener struct {
	mu           sync.Mutex
	listeners    map[int]*SpoolerListener
	dispatchFunc func(peer config.PeerDevice, filePath string) error

	OnJobStarted   func(peer config.PeerDevice, fileName string)
	OnJobCompleted func(peer config.PeerDevice, fileName string)
	OnJobFailed    func(peer config.PeerDevice, fileName string, err error)
}

// NewMultiSpoolerListener creates a manager for multi-peer virtual printer listeners.
func NewMultiSpoolerListener(dispatchFunc func(peer config.PeerDevice, filePath string) error) *MultiSpoolerListener {
	return &MultiSpoolerListener{
		listeners:    make(map[int]*SpoolerListener),
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

	if _, exists := msl.listeners[port]; exists {
		return nil // already active
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	capturedPeer := peer

	listener := NewSpoolerListener(addr, func(filePath string) error {
		if msl.dispatchFunc != nil {
			return msl.dispatchFunc(capturedPeer, filePath)
		}
		return nil
	})

	listener.OnJobStarted = func(fileName string) {
		if msl.OnJobStarted != nil {
			msl.OnJobStarted(capturedPeer, fileName)
		}
	}
	listener.OnJobCompleted = func(fileName string) {
		if msl.OnJobCompleted != nil {
			msl.OnJobCompleted(capturedPeer, fileName)
		}
	}
	listener.OnJobFailed = func(fileName string, err error) {
		if msl.OnJobFailed != nil {
			msl.OnJobFailed(capturedPeer, fileName, err)
		}
	}

	if err := listener.Start(); err != nil {
		return fmt.Errorf("failed to bind spooler listener for %s on %s: %w", peer.Name, addr, err)
	}

	msl.listeners[port] = listener
	log.Printf("[MultiSpoolerListener] Started listener on %s for virtual printer 'Print on %s'", addr, peer.Name)
	return nil
}

// StopAll stops all active peer spooler listeners.
func (msl *MultiSpoolerListener) StopAll() {
	msl.mu.Lock()
	defer msl.mu.Unlock()

	for port, l := range msl.listeners {
		l.Stop()
		delete(msl.listeners, port)
	}
	log.Printf("[MultiSpoolerListener] All spooler listeners stopped.")
}

