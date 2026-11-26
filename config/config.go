package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PeerDevice represents a remote Bridge node in the multi-device network.
type PeerDevice struct {
	Name         string    `json:"name"`                    // Friendly name (e.g. "Unit 1", "Unit 2")
	CustomName   bool      `json:"custom_name,omitempty"`   // True when user explicitly set a custom local name
	IP           string    `json:"ip"`                      // Tailscale IPv4 address (e.g. "100.64.1.2")
	URL          string    `json:"url"`                     // HTTP endpoint (e.g. "http://100.64.1.2:8765")
	SpoolPort    int       `json:"spool_port,omitempty"`    // Local TCP port for virtual printer (e.g. 9101, 9102)
	Online       bool      `json:"online,omitempty"`        // Dynamic connectivity state
	LastSeen     time.Time `json:"last_seen,omitempty"`     // Timestamp of last successful contact
	PrinterReady *bool     `json:"printer_ready,omitempty"` // nil = unknown/not yet probed, true = connected & ready, false = no physical printer connected
	PrinterName  string    `json:"printer_name,omitempty"`  // Active physical printer name reported by peer
}

// IsPrinterKnownMissing reports whether the peer was probed and confirmed to have no ready physical printer.
func (p PeerDevice) IsPrinterKnownMissing() bool {
	return p.PrinterReady != nil && !*p.PrinterReady
}

// IsPrinterReadyOrUnknown reports whether the peer's printer is ready or has not been probed yet.
func (p PeerDevice) IsPrinterReadyOrUnknown() bool {
	return p.PrinterReady == nil || *p.PrinterReady
}

func clonePeerDevice(p PeerDevice) PeerDevice {
	if p.PrinterReady != nil {
		ready := *p.PrinterReady
		p.PrinterReady = &ready
	}
	return p
}

// IgnoredPeer represents a discovered peer that the user explicitly declined or ignored.
type IgnoredPeer struct {
	Name      string    `json:"name,omitempty"`
	IP        string    `json:"ip,omitempty"`
	URL       string    `json:"url,omitempty"`
	IgnoredAt time.Time `json:"ignored_at"`
}

// Config represents the multi-device application configuration.
type Config struct {
	mu              sync.RWMutex
	Configured      bool          `json:"configured,omitempty"`       // Explicitly confirmed by user in setup wizard
	ConfigPath      string        `json:"-"`
	NodeName        string        `json:"node_name"`                  // Friendly name of this device (e.g. "Unit 1")
	PeerName        string        `json:"peer_name,omitempty"`        // Legacy peer name (backward compatibility)
	PeerURL         string        `json:"peer_url,omitempty"`         // Legacy peer URL (backward compatibility)
	Peers           []PeerDevice  `json:"peers,omitempty"`            // Multi-device peers list
	IgnoredPeers    []IgnoredPeer `json:"ignored_peers,omitempty"`    // Explicitly ignored discovery peers
	ListenPort      int           `json:"listen_port"`                // HTTP API port (default: 8765)
	AuthToken       string        `json:"auth_token"`                 // Shared secret for X-Bridge-Token
	SharedDir       string        `json:"shared_dir"`                 // Global shared folder (e.g. C:\Bridge\Shared)
	PrintDir        string        `json:"print_dir"`                  // e.g. "C:\\Bridge\\Print"
	SumatraPath     string        `json:"sumatra_path,omitempty"`     // Path to SumatraPDF.exe
	PrintTimeoutSec int           `json:"print_timeout_sec"`          // Timeout for print execution in seconds (default 120)
	StateFile       string        `json:"state_file,omitempty"`       // Path to state.json
	PrintQueueDir   string        `json:"print_queue_dir,omitempty"`  // Staging for incoming print jobs
	PrintFailedDir  string        `json:"print_failed_dir,omitempty"` // Directory for hung/failed print jobs
	PrintSentDir    string        `json:"print_sent_dir,omitempty"`   // Directory for successfully uploaded print jobs
	LogDir          string        `json:"log_dir,omitempty"`          // Directory for log files

	// Backward compatibility field
	Role string `json:"role,omitempty"`
}

// DefaultConfig returns a configuration struct with standard default values.
func DefaultConfig() *Config {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "Unit 1"
	}

	return &Config{
		NodeName:        hostname,
		PeerName:        "",
		PeerURL:         "",
		Peers:           make([]PeerDevice, 0),
		IgnoredPeers:    make([]IgnoredPeer, 0),
		ListenPort:      8765,
		AuthToken:       "bridge-secret-token",
		SharedDir:       `C:\Bridge\Shared`,
		PrintDir:        `C:\Bridge\Print`,
		SumatraPath:     `C:\BridgeApp\SumatraPDF.exe`,
		PrintTimeoutSec: 120,
		StateFile:       `C:\BridgeApp\state.json`,
		PrintQueueDir:   `C:\Bridge\PrintQueue`,
		PrintFailedDir:  `C:\Bridge\PrintFailed`,
		PrintSentDir:    `C:\Bridge\Print\Sent`,
		LogDir:          `C:\Bridge\Logs`,
	}
}

// ExtractIP extracts IPv4 address from host or URL string.
func ExtractIP(raw string) string {
	clean := strings.TrimSpace(raw)
	if strings.Contains(clean, "://") {
		u, err := url.Parse(clean)
		if err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	if host, _, err := strings.Cut(clean, ":"); err {
		clean = host
	}
	return strings.Trim(clean, "/")
}

// isSelfIP checks if targetIP is assigned to any active network interface on this computer, or is loopback.
// isSelfNonLoopbackIP checks if targetIP is assigned to any active non-loopback network interface (e.g. Tailscale).
func isSelfNonLoopbackIP(targetIP string) bool {
	clean := strings.TrimSpace(targetIP)
	if clean == "" || clean == "127.0.0.1" || clean == "::1" || strings.EqualFold(clean, "localhost") {
		return false
	}
	parsed := net.ParseIP(clean)
	if parsed == nil || parsed.IsLoopback() {
		return false
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.Equal(parsed) {
				return true
			}
		}
	}
	return false
}

// isSelfTarget checks whether the name, IP, or URL identifies THIS computer.
func isSelfTarget(name, ip, rawURL, nodeName string, listenPort int) bool {
	cleanName := strings.TrimSpace(name)
	cleanIP := strings.TrimSpace(ip)
	cleanURL := strings.TrimRight(strings.TrimSpace(rawURL), "/")

	if cleanIP == "" && cleanURL != "" {
		cleanIP = ExtractIP(cleanURL)
	}

	if nodeName != "" {
		normNode := strings.ToLower(strings.TrimSpace(nodeName))
		if strings.EqualFold(cleanName, normNode) {
			return true
		}
		if cleanIP != "" && strings.EqualFold(cleanName, fmt.Sprintf("device (%s)", cleanIP)) && strings.EqualFold(cleanIP, normNode) {
			return true
		}
	}

	if h, err := os.Hostname(); err == nil && h != "" {
		if strings.EqualFold(cleanName, strings.TrimSpace(h)) {
			return true
		}
	}

	// For loopback URLs, it is only self if the port matches our own ListenPort
	if cleanURL != "" && listenPort > 0 {
		if strings.EqualFold(cleanURL, fmt.Sprintf("http://127.0.0.1:%d", listenPort)) ||
			strings.EqualFold(cleanURL, fmt.Sprintf("http://localhost:%d", listenPort)) {
			return true
		}
		if u, err := url.Parse(cleanURL); err == nil {
			host := u.Hostname()
			uPort := u.Port()
			if (uPort == "" || uPort == fmt.Sprintf("%d", listenPort)) && isSelfNonLoopbackIP(host) {
				return true
			}
		}
	}

	// For non-loopback IP (e.g. Tailscale IP), check if assigned to this machine
	if cleanIP != "" && isSelfNonLoopbackIP(cleanIP) {
		return true
	}

	return false
}

// IsSelfIP checks if targetIP is assigned to any active non-loopback network interface on this computer.
func IsSelfIP(targetIP string) bool {
	return isSelfNonLoopbackIP(targetIP)
}

// IsSelfTarget checks whether the name, IP, or URL identifies THIS computer.
func IsSelfTarget(name, ip, rawURL, nodeName string, listenPort int) bool {
	return isSelfTarget(name, ip, rawURL, nodeName, listenPort)
}

// LoadConfig loads, migrates, and validates configuration from a JSON file.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	cfg.ConfigPath = path

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	var raw struct {
		NodeName   string       `json:"node_name"`
		PeerName   string       `json:"peer_name"`
		PeerURL    string       `json:"peer_url"`
		Peers      []PeerDevice `json:"peers"`
		Role       string       `json:"role"`
		Configured bool         `json:"configured"`
	}
	_ = json.Unmarshal(data, &raw)

	migrated := false

	// Automatic Migration from legacy one-way roles
	if strings.TrimSpace(raw.NodeName) == "" {
		if strings.EqualFold(raw.Role, "desktop1") {
			cfg.NodeName = "Desktop 1"
			if cfg.PeerName == "" {
				cfg.PeerName = "Desktop 2"
			}
			migrated = true
		} else if strings.EqualFold(raw.Role, "desktop2") {
			cfg.NodeName = "Desktop 2"
			if cfg.PeerName == "" {
				cfg.PeerName = "Desktop 1"
			}
			migrated = true
		} else {
			cfg.NodeName = DefaultConfig().NodeName
		}
	}
	if raw.Role != "" {
		cfg.Role = ""
		migrated = true
	}

	// Flag if this device needs setup naming prompt:
	// Explicit "configured": true with a non-empty NodeName is honored regardless of device name.
	// Only missing/false configured flag or empty NodeName triggers setup.
	if raw.Configured && strings.TrimSpace(raw.NodeName) != "" {
		cfg.Configured = true
	} else {
		cfg.Configured = false
	}

	// Multi-device peer migration: populate Peers slice from legacy peer_url/peer_name if empty
	if len(cfg.Peers) == 0 && (raw.PeerURL != "" || raw.PeerName != "") {
		peerURL := strings.TrimRight(strings.TrimSpace(raw.PeerURL), "/")
		peerName := strings.TrimSpace(raw.PeerName)
		if peerName == "" && cfg.PeerName != "" {
			peerName = cfg.PeerName
		}
		if peerName == "" {
			peerName = "Unit 2"
		}
		peerIP := ExtractIP(peerURL)
		if peerIP == "" && strings.HasPrefix(peerName, "100.") {
			peerIP = peerName
		}
		if peerURL == "" && peerIP != "" {
			peerURL = fmt.Sprintf("http://%s:8765", peerIP)
		}

		if peerURL != "" || peerName != "" {
			cfg.Peers = append(cfg.Peers, PeerDevice{
				Name:      peerName,
				IP:        peerIP,
				URL:       peerURL,
				SpoolPort: 9101,
			})
			migrated = true
		}
	}

	// Never trust persisted online/last_seen/printer runtime state from disk.
	for i := range cfg.Peers {
		cfg.Peers[i].Online = false
		cfg.Peers[i].LastSeen = time.Time{}
		cfg.Peers[i].PrinterReady = nil
		cfg.Peers[i].PrinterName = ""
	}

	// Prune self, normalize endpoints, and deduplicate SpoolPorts (avoiding 9100 and ListenPort).
	cleanPeers, peersChanged := normalizeAndDeduplicatePeers(cfg.Peers, cfg.NodeName, cfg.ListenPort)
	cfg.Peers = cleanPeers
	if peersChanged {
		migrated = true
	}

	// Keep legacy fields synchronized with primary peer (or cleared when empty).
	if len(cfg.Peers) > 0 {
		cfg.PeerName = cfg.Peers[0].Name
		cfg.PeerURL = cfg.Peers[0].URL
	} else if cfg.PeerName != "" || cfg.PeerURL != "" {
		cfg.PeerName = ""
		cfg.PeerURL = ""
		migrated = true
	}

	// Normalize legacy peer URL if present
	cfg.PeerURL = strings.TrimRight(strings.TrimSpace(cfg.PeerURL), "/")

	// Validation
	if cfg.ListenPort <= 0 || cfg.ListenPort > 65535 {
		return nil, fmt.Errorf("invalid listen_port %d", cfg.ListenPort)
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("auth_token cannot be empty")
	}
	if cfg.PrintTimeoutSec <= 0 {
		cfg.PrintTimeoutSec = 120
	}
	if cfg.StateFile == "" {
		cfg.StateFile = filepath.Join(filepath.Dir(path), "state.json")
	}
	if cfg.PrintQueueDir == "" {
		cfg.PrintQueueDir = filepath.Join(filepath.Dir(cfg.PrintDir), "PrintQueue")
	}
	if cfg.PrintFailedDir == "" {
		cfg.PrintFailedDir = filepath.Join(filepath.Dir(cfg.PrintDir), "PrintFailed")
	}
	if cfg.PrintSentDir == "" {
		cfg.PrintSentDir = filepath.Join(cfg.PrintDir, "Sent")
	}
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(filepath.Dir(cfg.PrintDir), "Logs")
	}

	if migrated {
		if err := SaveConfig(path, cfg); err != nil {
			return nil, fmt.Errorf("failed to persist migrated config %s: %w", path, err)
		}
	}

	return cfg, nil
}

// normalizeAndDeduplicatePeers strips self entries, normalizes URL/IP fields,
// and assigns unique spool ports starting at 9101 (avoiding 9100 and listenPort).
func normalizeAndDeduplicatePeers(peers []PeerDevice, nodeName string, listenPort int) ([]PeerDevice, bool) {
	changed := false
	usedPorts := map[int]bool{9100: true}
	if listenPort > 0 {
		usedPorts[listenPort] = true
	}

	cleanPeers := make([]PeerDevice, 0, len(peers))
	for _, p := range peers {
		if isSelfTarget(p.Name, p.IP, p.URL, nodeName, listenPort) {
			changed = true
			continue
		}
		origURL := p.URL
		origIP := p.IP
		p.Name = strings.TrimSpace(p.Name)
		p.IP = strings.TrimSpace(p.IP)
		p.URL = strings.TrimRight(strings.TrimSpace(p.URL), "/")
		if p.IP == "" && p.URL != "" {
			p.IP = ExtractIP(p.URL)
		}
		if p.URL == "" && p.IP != "" {
			p.URL = fmt.Sprintf("http://%s:8765", p.IP)
		}
		if p.URL != origURL || p.IP != origIP {
			changed = true
		}

		if p.SpoolPort > 0 && !usedPorts[p.SpoolPort] {
			usedPorts[p.SpoolPort] = true
		} else {
			if p.SpoolPort != 0 {
				changed = true
			}
			p.SpoolPort = 0
		}
		cleanPeers = append(cleanPeers, p)
	}

	nextPort := 9101
	for i := range cleanPeers {
		if cleanPeers[i].SpoolPort <= 0 {
			for usedPorts[nextPort] {
				nextPort++
			}
			cleanPeers[i].SpoolPort = nextPort
			usedPorts[nextPort] = true
			changed = true
		}
	}

	return cleanPeers, changed
}

// isPlaceholderPeerName reports whether name is an auto-generated placeholder
// such as "Device (<ip>)", the raw IP itself, "Remote PC", or "Peer N".
func isPlaceholderPeerName(name, ip string) bool {
	clean := strings.TrimSpace(name)
	if clean == "" {
		return true
	}
	if ip != "" && (clean == ip || strings.EqualFold(clean, fmt.Sprintf("Device (%s)", ip))) {
		return true
	}
	low := strings.ToLower(clean)
	if strings.HasPrefix(low, "device (") && strings.HasSuffix(low, ")") {
		return true
	}
	if strings.EqualFold(clean, "Remote PC") {
		return true
	}
	return net.ParseIP(clean) != nil
}

// findPeerIndexLocked locates a peer by URL -> IP -> Name priority.
// Caller must hold c.mu (read or write lock).
func (c *Config) findPeerIndexLocked(target string) int {
	cleanTarget := strings.TrimRight(strings.TrimSpace(target), "/")
	if cleanTarget == "" {
		return -1
	}
	for i := range c.Peers {
		if c.Peers[i].URL != "" && strings.EqualFold(c.Peers[i].URL, cleanTarget) {
			return i
		}
	}
	for i := range c.Peers {
		if c.Peers[i].IP != "" && c.Peers[i].IP == cleanTarget {
			return i
		}
	}
	for i := range c.Peers {
		if c.Peers[i].Name != "" && strings.EqualFold(c.Peers[i].Name, cleanTarget) {
			return i
		}
	}
	return -1
}

// syncLegacyPeerFieldsLocked keeps legacy PeerName/PeerURL synchronized with Peers[0].
// Caller must hold c.mu.Lock().
func (c *Config) syncLegacyPeerFieldsLocked() bool {
	if len(c.Peers) > 0 {
		changed := c.PeerName != c.Peers[0].Name || c.PeerURL != c.Peers[0].URL
		c.PeerName = c.Peers[0].Name
		c.PeerURL = c.Peers[0].URL
		return changed
	}
	changed := c.PeerName != "" || c.PeerURL != ""
	c.PeerName = ""
	c.PeerURL = ""
	return changed
}

// GetNodeName returns the current node's friendly name.
func (c *Config) GetNodeName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.NodeName
}

// SetNodeName updates the current node's friendly name and saves config.
func (c *Config) SetNodeName(name string) error {
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		return nil
	}
	c.mu.Lock()
	if c.NodeName == cleanName {
		c.mu.Unlock()
		return nil
	}
	c.NodeName = cleanName
	// Prune any peer that matches the new self node name
	var cleanPeers []PeerDevice
	for _, p := range c.Peers {
		if !isSelfTarget(p.Name, p.IP, p.URL, cleanName, c.ListenPort) {
			cleanPeers = append(cleanPeers, p)
		}
	}
	c.Peers = cleanPeers
	c.syncLegacyPeerFieldsLocked()
	configPath := c.ConfigPath
	c.mu.Unlock()

	if configPath != "" {
		return SaveConfig(configPath, c)
	}
	return nil
}

// GetPeers returns a copy of all configured peer devices.
func (c *Config) GetPeers() []PeerDevice {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make([]PeerDevice, len(c.Peers))
	for i, p := range c.Peers {
		res[i] = clonePeerDevice(p)
	}
	return res
}

// SetPeers thread-safely updates the peers list and saves configuration.
func (c *Config) SetPeers(peers []PeerDevice) error {
	c.mu.Lock()
	cleanPeers, _ := normalizeAndDeduplicatePeers(peers, c.NodeName, c.ListenPort)
	c.Peers = cleanPeers
	c.syncLegacyPeerFieldsLocked()
	configPath := c.ConfigPath
	c.mu.Unlock()

	if configPath != "" {
		return SaveConfig(configPath, c)
	}
	return nil
}

// GetPeerURLs returns a slice of all active peer URLs.
func (c *Config) GetPeerURLs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var urls []string
	for _, p := range c.Peers {
		if p.URL != "" {
			urls = append(urls, p.URL)
		}
	}
	return urls
}

// SetPeerOnlineStatus thread-safely updates the online connectivity state of a peer.
func (c *Config) SetPeerOnlineStatus(target string, online bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := c.findPeerIndexLocked(target)
	if idx == -1 {
		return false
	}
	c.Peers[idx].Online = online
	if online {
		c.Peers[idx].LastSeen = time.Now()
	}
	return true
}

// SetPeerPrinterStatus thread-safely updates the transient printer readiness state of a peer.
func (c *Config) SetPeerPrinterStatus(target string, ready bool, printerName string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	idx := c.findPeerIndexLocked(target)
	if idx == -1 {
		return false
	}
	readyCopy := ready
	c.Peers[idx].PrinterReady = &readyCopy
	c.Peers[idx].PrinterName = strings.TrimSpace(printerName)
	return true
}

// SetPeerName updates a peer's friendly name and saves config unless the peer uses a local custom name.
func (c *Config) SetPeerName(target, newName string) bool {
	cleanName := strings.TrimSpace(newName)
	if cleanName == "" || strings.TrimSpace(target) == "" {
		return false
	}

	c.mu.Lock()
	changed := false
	idx := c.findPeerIndexLocked(target)
	if idx != -1 && !c.Peers[idx].CustomName {
		if c.Peers[idx].Name != cleanName {
			c.Peers[idx].Name = cleanName
			changed = true
		}
	}
	if changed {
		c.syncLegacyPeerFieldsLocked()
	}
	configPath := c.ConfigPath
	c.mu.Unlock()

	if changed && configPath != "" {
		if err := SaveConfig(configPath, c); err != nil {
			log.Printf("[Config] Warning: failed to save config after SetPeerName: %v", err)
		}
	}
	return changed
}

// SetPeerCustomName sets a peer's friendly name and CustomName flag, saving config if changed.
func (c *Config) SetPeerCustomName(target, customName string, custom bool) bool {
	cleanName := strings.TrimSpace(customName)
	if cleanName == "" || strings.TrimSpace(target) == "" {
		return false
	}

	c.mu.Lock()
	idx := c.findPeerIndexLocked(target)

	changed := false
	if idx != -1 {
		if c.Peers[idx].Name != cleanName || c.Peers[idx].CustomName != custom {
			c.Peers[idx].Name = cleanName
			c.Peers[idx].CustomName = custom
			changed = true
		}
	}
	if changed {
		c.syncLegacyPeerFieldsLocked()
	}
	configPath := c.ConfigPath
	c.mu.Unlock()

	if changed && configPath != "" {
		if err := SaveConfig(configPath, c); err != nil {
			log.Printf("[Config] Warning: failed to save config after SetPeerCustomName: %v", err)
		}
	}
	return changed
}

// AllocateSpoolPort assigns the next unused local port (starting from 9101) for a virtual printer.
func (c *Config) AllocateSpoolPort(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, p := range c.Peers {
		if strings.EqualFold(p.Name, name) && p.SpoolPort > 0 && p.SpoolPort != 9100 && p.SpoolPort != c.ListenPort {
			return p.SpoolPort
		}
	}

	return c.nextAvailableSpoolPortLocked(-1)
}

func (c *Config) nextAvailableSpoolPortLocked(excludeIdx int) int {
	used := map[int]bool{9100: true}
	if c.ListenPort > 0 {
		used[c.ListenPort] = true
	}
	for i, p := range c.Peers {
		if i != excludeIdx && p.SpoolPort > 0 {
			used[p.SpoolPort] = true
		}
	}
	port := 9101
	for used[port] {
		port++
	}
	return port
}

func (c *Config) hasPeerNameCollisionLocked(name string, excludeIdx int) bool {
	if name == "" {
		return false
	}
	for i, p := range c.Peers {
		if i != excludeIdx && strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// HasPeerNameCollision reports whether a different peer already uses the given friendly name.
func (c *Config) HasPeerNameCollision(name, ip, peerURL string) bool {
	cleanName := strings.TrimSpace(name)
	cleanIP := strings.TrimSpace(ip)
	cleanURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanIP == "" && cleanURL != "" {
		cleanIP = ExtractIP(cleanURL)
	}
	if cleanName == "" {
		return false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, p := range c.Peers {
		if !strings.EqualFold(p.Name, cleanName) {
			continue
		}
		if cleanURL != "" && p.URL != "" && strings.EqualFold(cleanURL, p.URL) {
			continue
		}
		if cleanIP != "" && p.IP != "" && cleanIP == p.IP {
			continue
		}
		return true
	}
	return false
}

// AddOrUpdatePeer adds a peer device or updates an existing matching peer by URL or IP.
// It avoids silently overwriting a different peer that merely shares a friendly Name.
// It returns the updated PeerDevice and a boolean indicating if this was a new peer.
func (c *Config) AddOrUpdatePeer(name, ip, peerURL string) (PeerDevice, bool) {
	c.mu.Lock()

	cleanName := strings.TrimSpace(name)
	cleanIP := strings.TrimSpace(ip)
	cleanURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")

	// Never add self device as a peer!
	if isSelfTarget(cleanName, cleanIP, cleanURL, c.NodeName, c.ListenPort) {
		c.mu.Unlock()
		return PeerDevice{}, false
	}

	if cleanIP == "" && cleanURL != "" {
		cleanIP = ExtractIP(cleanURL)
	}
	if cleanURL == "" && cleanIP != "" {
		cleanURL = fmt.Sprintf("http://%s:8765", cleanIP)
	}

	// Search for existing peer:
	// Priority 1: Match by exact URL
	// Priority 2: Match by IP when URLs do not contradict (supports placeholder/hostname updates)
	// Priority 3: Match by Name only if neither side provides a conflicting IP/URL
	idx := -1
	if cleanURL != "" {
		for i, p := range c.Peers {
			if p.URL != "" && strings.EqualFold(p.URL, cleanURL) {
				idx = i
				break
			}
		}
	}
	if idx == -1 && cleanIP != "" {
		for i, p := range c.Peers {
			if p.IP == cleanIP {
				if cleanURL != "" && p.URL != "" && !strings.EqualFold(cleanURL, p.URL) {
					continue
				}
				idx = i
				break
			}
		}
	}
	if idx == -1 && cleanName != "" && !isPlaceholderPeerName(cleanName, cleanIP) {
		for i, p := range c.Peers {
			if strings.EqualFold(p.Name, cleanName) {
				if cleanURL != "" && p.URL != "" && !strings.EqualFold(cleanURL, p.URL) {
					continue
				}
				if cleanIP != "" && p.IP != "" && cleanIP != p.IP {
					continue
				}
				idx = i
				break
			}
		}
	}

	isNew := idx == -1
	changed := isNew
	var dev PeerDevice

	if isNew {
		port := c.nextAvailableSpoolPortLocked(-1)

		if cleanName == "" {
			if cleanIP != "" {
				cleanName = cleanIP
			} else {
				cleanName = fmt.Sprintf("Peer %d", len(c.Peers)+1)
			}
		} else if c.hasPeerNameCollisionLocked(cleanName, -1) {
			if cleanIP != "" {
				cleanName = fmt.Sprintf("%s (%s)", cleanName, cleanIP)
			} else {
				cleanName = fmt.Sprintf("%s (%d)", cleanName, len(c.Peers)+1)
			}
		}

		dev = PeerDevice{
			Name:      cleanName,
			IP:        cleanIP,
			URL:       cleanURL,
			SpoolPort: port,
			Online:    true,
			LastSeen:  time.Now(),
		}
		c.Peers = append(c.Peers, dev)
	} else {
		dev = c.Peers[idx]
		prevName := dev.Name
		prevIP := dev.IP
		prevURL := dev.URL
		prevPort := dev.SpoolPort

		if !dev.CustomName && cleanName != "" && dev.Name != cleanName {
			// Do not replace a real friendly name with a placeholder such as "Device (<ip>)".
			if !isPlaceholderPeerName(cleanName, cleanIP) || isPlaceholderPeerName(dev.Name, dev.IP) {
				candidateName := cleanName
				if c.hasPeerNameCollisionLocked(candidateName, idx) && cleanIP != "" {
					candidateName = fmt.Sprintf("%s (%s)", cleanName, cleanIP)
				}
				dev.Name = candidateName
			}
		}
		if cleanIP != "" {
			dev.IP = cleanIP
		}
		if cleanURL != "" {
			dev.URL = cleanURL
		}
		if dev.SpoolPort <= 0 || dev.SpoolPort == 9100 || dev.SpoolPort == c.ListenPort {
			dev.SpoolPort = c.nextAvailableSpoolPortLocked(idx)
		}
		if dev.Name != prevName || dev.IP != prevIP || dev.URL != prevURL || dev.SpoolPort != prevPort {
			changed = true
		}
		dev.Online = true
		dev.LastSeen = time.Now()
		c.Peers[idx] = dev
	}

	if c.syncLegacyPeerFieldsLocked() {
		changed = true
	}
	configPath := c.ConfigPath
	c.mu.Unlock()

	if changed && configPath != "" {
		if err := SaveConfig(configPath, c); err != nil {
			log.Printf("[Config] Warning: failed to save config after AddOrUpdatePeer: %v", err)
		}
	}

	return clonePeerDevice(dev), isNew
}

// UpdatePeerAddressByName explicitly updates an existing peer's IP and URL by matching its Name
// (used when a known peer migrates to a new IP/URL). Falls back to AddOrUpdatePeer if no match exists.
func (c *Config) UpdatePeerAddressByName(name, ip, peerURL string) (PeerDevice, bool) {
	cleanName := strings.TrimSpace(name)
	cleanIP := strings.TrimSpace(ip)
	cleanURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanIP == "" && cleanURL != "" {
		cleanIP = ExtractIP(cleanURL)
	}
	if cleanURL == "" && cleanIP != "" {
		cleanURL = fmt.Sprintf("http://%s:8765", cleanIP)
	}
	if isSelfTarget(cleanName, cleanIP, cleanURL, c.NodeName, c.ListenPort) {
		return PeerDevice{}, false
	}

	c.mu.Lock()
	idx := -1
	if cleanURL != "" {
		for i, p := range c.Peers {
			if p.URL != "" && strings.EqualFold(p.URL, cleanURL) {
				idx = i
				break
			}
		}
	}
	if idx == -1 && cleanName != "" && !isPlaceholderPeerName(cleanName, cleanIP) {
		for i, p := range c.Peers {
			if strings.EqualFold(p.Name, cleanName) {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		c.mu.Unlock()
		return c.AddOrUpdatePeer(name, ip, peerURL)
	}

	dev := c.Peers[idx]
	changed := false
	if cleanIP != "" && dev.IP != cleanIP {
		dev.IP = cleanIP
		changed = true
	}
	if cleanURL != "" && dev.URL != cleanURL {
		dev.URL = cleanURL
		changed = true
	}
	dev.Online = true
	dev.LastSeen = time.Now()
	c.Peers[idx] = dev
	if c.syncLegacyPeerFieldsLocked() {
		changed = true
	}
	configPath := c.ConfigPath
	c.mu.Unlock()

	if changed && configPath != "" {
		if err := SaveConfig(configPath, c); err != nil {
			log.Printf("[Config] Warning: failed to save config after UpdatePeerAddressByName: %v", err)
		}
	}
	return clonePeerDevice(dev), false
}

// RemovePeer removes a peer by URL, IP, or Name (in that priority order).
func (c *Config) RemovePeer(target string) bool {
	c.mu.Lock()
	idx := c.findPeerIndexLocked(target)
	if idx == -1 {
		c.mu.Unlock()
		return false
	}

	c.Peers = append(c.Peers[:idx], c.Peers[idx+1:]...)
	c.syncLegacyPeerFieldsLocked()
	configPath := c.ConfigPath
	c.mu.Unlock()

	if configPath != "" {
		if err := SaveConfig(configPath, c); err != nil {
			log.Printf("[Config] Warning: failed to save config after RemovePeer: %v", err)
		}
	}
	return true
}

// AddIgnoredPeer records a discovered peer as ignored/declined and persists config if ConfigPath is set.
func (c *Config) AddIgnoredPeer(name, ip, peerURL string) error {
	cleanName := strings.TrimSpace(name)
	cleanIP := strings.TrimSpace(ip)
	cleanURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanIP == "" && cleanURL != "" {
		cleanIP = ExtractIP(cleanURL)
	}
	if cleanName == "" && cleanIP == "" && cleanURL == "" {
		return nil
	}

	c.mu.Lock()
	now := time.Now()
	updated := false
	for i := range c.IgnoredPeers {
		ig := &c.IgnoredPeers[i]
		matchURL := cleanURL != "" && ig.URL != "" && strings.EqualFold(ig.URL, cleanURL)
		matchIP := cleanIP != "" && ig.IP != "" && ig.IP == cleanIP
		matchName := cleanName != "" && ig.Name != "" && strings.EqualFold(ig.Name, cleanName) &&
			(cleanIP == "" || ig.IP == "" || cleanIP == ig.IP)
		if matchURL || matchIP || matchName {
			if cleanName != "" {
				ig.Name = cleanName
			}
			if cleanIP != "" {
				ig.IP = cleanIP
			}
			if cleanURL != "" {
				ig.URL = cleanURL
			}
			ig.IgnoredAt = now
			updated = true
			break
		}
	}
	if !updated {
		c.IgnoredPeers = append(c.IgnoredPeers, IgnoredPeer{
			Name:      cleanName,
			IP:        cleanIP,
			URL:       cleanURL,
			IgnoredAt: now,
		})
	}
	configPath := c.ConfigPath
	c.mu.Unlock()

	if configPath != "" {
		return SaveConfig(configPath, c)
	}
	return nil
}

// RemoveIgnoredPeer removes matching entries from IgnoredPeers by URL, IP, or Name and persists config.
func (c *Config) RemoveIgnoredPeer(target string) error {
	cleanTarget := strings.TrimRight(strings.TrimSpace(target), "/")
	if cleanTarget == "" {
		return nil
	}

	c.mu.Lock()
	filtered := make([]IgnoredPeer, 0, len(c.IgnoredPeers))
	removed := false
	for _, ig := range c.IgnoredPeers {
		if (ig.URL != "" && strings.EqualFold(ig.URL, cleanTarget)) ||
			(ig.IP != "" && ig.IP == cleanTarget) ||
			(ig.Name != "" && strings.EqualFold(ig.Name, cleanTarget)) {
			removed = true
			continue
		}
		filtered = append(filtered, ig)
	}
	c.IgnoredPeers = filtered
	configPath := c.ConfigPath
	c.mu.Unlock()

	if removed && configPath != "" {
		return SaveConfig(configPath, c)
	}
	return nil
}

// IsPeerIgnored reports whether a discovered peer matches an entry in IgnoredPeers.
func (c *Config) IsPeerIgnored(name, ip, peerURL string) bool {
	cleanName := strings.TrimSpace(name)
	cleanIP := strings.TrimSpace(ip)
	cleanURL := strings.TrimRight(strings.TrimSpace(peerURL), "/")
	if cleanIP == "" && cleanURL != "" {
		cleanIP = ExtractIP(cleanURL)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ig := range c.IgnoredPeers {
		if cleanURL != "" && ig.URL != "" && strings.EqualFold(ig.URL, cleanURL) {
			return true
		}
		if cleanIP != "" && ig.IP != "" && ig.IP == cleanIP {
			return true
		}
		if cleanName != "" && ig.Name != "" && strings.EqualFold(ig.Name, cleanName) {
			if cleanIP != "" && ig.IP != "" && cleanIP != ig.IP {
				continue
			}
			return true
		}
	}
	return false
}

// GetIgnoredPeers returns a copy of all ignored discovery peers.
func (c *Config) GetIgnoredPeers() []IgnoredPeer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make([]IgnoredPeer, len(c.IgnoredPeers))
	copy(res, c.IgnoredPeers)
	return res
}

// FindPeer finds a peer by name, IP, or URL.
func (c *Config) FindPeer(target string) *PeerDevice {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cleanTarget := strings.TrimRight(strings.TrimSpace(target), "/")
	if cleanTarget == "" {
		return nil
	}
	// Try Name first
	for _, p := range c.Peers {
		if strings.EqualFold(p.Name, cleanTarget) {
			cp := clonePeerDevice(p)
			return &cp
		}
	}
	// Try URL
	for _, p := range c.Peers {
		if strings.EqualFold(p.URL, cleanTarget) {
			cp := clonePeerDevice(p)
			return &cp
		}
	}
	// Try IP
	for _, p := range c.Peers {
		if p.IP == cleanTarget {
			cp := clonePeerDevice(p)
			return &cp
		}
	}
	return nil
}

// FindPeerBySpoolPort finds a peer by its assigned virtual printer spool port.
func (c *Config) FindPeerBySpoolPort(port int) *PeerDevice {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, p := range c.Peers {
		if p.SpoolPort == port {
			cp := clonePeerDevice(p)
			return &cp
		}
	}
	return nil
}

// GetPeerName returns the primary peer PC's friendly name (backward compatibility).
func (c *Config) GetPeerName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.Peers) > 0 && c.Peers[0].Name != "" {
		return c.Peers[0].Name
	}
	if c.PeerName != "" {
		return c.PeerName
	}
	return "Remote PC"
}

// UpdatePeerName updates the primary peer's friendly name dynamically.
func (c *Config) UpdatePeerName(name string) error {
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		return nil
	}

	c.mu.Lock()
	if len(c.Peers) > 0 {
		c.Peers[0].Name = cleanName
	}
	c.PeerName = cleanName
	configPath := c.ConfigPath
	c.mu.Unlock()

	if configPath != "" {
		return SaveConfig(configPath, c)
	}
	return nil
}

// GetPeerURL returns the currently configured primary peer URL.
func (c *Config) GetPeerURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.Peers) > 0 && c.Peers[0].URL != "" {
		return c.Peers[0].URL
	}
	return c.PeerURL
}

// UpdatePeerURL updates the primary peer URL and re-saves config if changed.
func (c *Config) UpdatePeerURL(newURL string) error {
	cleanURL := strings.TrimRight(strings.TrimSpace(newURL), "/")
	if cleanURL == "" {
		return nil
	}

	c.mu.Lock()
	if len(c.Peers) > 0 {
		c.Peers[0].URL = cleanURL
		if c.Peers[0].IP == "" {
			c.Peers[0].IP = ExtractIP(cleanURL)
		}
	} else {
		c.Peers = append(c.Peers, PeerDevice{
			Name:      "Remote PC",
			IP:        ExtractIP(cleanURL),
			URL:       cleanURL,
			SpoolPort: 9101,
		})
	}
	c.PeerURL = cleanURL
	configPath := c.ConfigPath
	c.mu.Unlock()

	if configPath != "" {
		return SaveConfig(configPath, c)
	}
	return nil
}

func (c *Config) cloneForPersistenceLocked() *Config {
	peers := make([]PeerDevice, len(c.Peers))
	for i, p := range c.Peers {
		p.Online = false
		p.LastSeen = time.Time{}
		p.PrinterReady = nil
		p.PrinterName = ""
		peers[i] = p
	}
	var ignored []IgnoredPeer
	if len(c.IgnoredPeers) > 0 {
		ignored = make([]IgnoredPeer, len(c.IgnoredPeers))
		copy(ignored, c.IgnoredPeers)
	}
	return &Config{
		Configured:      c.Configured,
		NodeName:        c.NodeName,
		PeerName:        c.PeerName,
		PeerURL:         c.PeerURL,
		Peers:           peers,
		IgnoredPeers:    ignored,
		ListenPort:      c.ListenPort,
		AuthToken:       c.AuthToken,
		SharedDir:       c.SharedDir,
		PrintDir:        c.PrintDir,
		SumatraPath:     c.SumatraPath,
		PrintTimeoutSec: c.PrintTimeoutSec,
		StateFile:       c.StateFile,
		PrintQueueDir:   c.PrintQueueDir,
		PrintFailedDir:  c.PrintFailedDir,
		PrintSentDir:    c.PrintSentDir,
		LogDir:          c.LogDir,
		Role:            c.Role,
	}
}

// SaveConfig writes the configuration to a JSON file atomically without persisting transient peer online status.
func SaveConfig(path string, cfg *Config) error {
	cfg.mu.RLock()
	snapshot := cfg.cloneForPersistenceLocked()
	cfg.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
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
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		renameErr = os.Rename(tmpPath, path)
		if renameErr == nil {
			cleanupTmp = false
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 15 * time.Millisecond)
	}
	return renameErr
}

