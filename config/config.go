package config

import (
	"encoding/json"
	"errors"
	"fmt"
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
	Name      string    `json:"name"`                 // Friendly name (e.g. "Unit 1", "Unit 2")
	IP        string    `json:"ip"`                   // Tailscale IPv4 address (e.g. "100.64.1.2")
	URL       string    `json:"url"`                  // HTTP endpoint (e.g. "http://100.64.1.2:8765")
	SpoolPort int       `json:"spool_port,omitempty"` // Local TCP port for virtual printer (e.g. 9101, 9102)
	Online    bool      `json:"online,omitempty"`     // Dynamic connectivity state
	LastSeen  time.Time `json:"last_seen,omitempty"`  // Timestamp of last successful contact
}

// Config represents the multi-device application configuration.
type Config struct {
	mu              sync.RWMutex
	Configured      bool         `json:"configured,omitempty"`       // Explicitly confirmed by user in setup wizard
	ConfigPath      string       `json:"-"`
	NodeName        string       `json:"node_name"`                  // Friendly name of this device (e.g. "Unit 1")
	PeerName        string       `json:"peer_name,omitempty"`        // Legacy peer name (backward compatibility)
	PeerURL         string       `json:"peer_url,omitempty"`         // Legacy peer URL (backward compatibility)
	Peers           []PeerDevice `json:"peers,omitempty"`            // Multi-device peers list
	ListenPort      int          `json:"listen_port"`                // HTTP API port (default: 8765)
	AuthToken       string       `json:"auth_token"`                 // Shared secret for X-Bridge-Token
	SharedDir       string       `json:"shared_dir"`                 // Global shared folder (e.g. C:\Bridge\Shared)
	PrintDir        string       `json:"print_dir"`                  // e.g. "C:\\Bridge\\Print"
	SumatraPath     string       `json:"sumatra_path,omitempty"`     // Path to SumatraPDF.exe
	PrintTimeoutSec int          `json:"print_timeout_sec"`          // Timeout for print execution in seconds (default 45)
	StateFile       string       `json:"state_file,omitempty"`       // Path to state.json
	PrintQueueDir   string       `json:"print_queue_dir,omitempty"`  // Staging for incoming print jobs
	PrintFailedDir  string       `json:"print_failed_dir,omitempty"` // Directory for hung/failed print jobs
	PrintSentDir    string       `json:"print_sent_dir,omitempty"`   // Directory for successfully uploaded print jobs
	LogDir          string       `json:"log_dir,omitempty"`          // Directory for log files

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
		ListenPort:      8765,
		AuthToken:       "bridge-secret-token",
		SharedDir:       `C:\Bridge\Shared`,
		PrintDir:        `C:\Bridge\Print`,
		SumatraPath:     `C:\BridgeApp\SumatraPDF.exe`,
		PrintTimeoutSec: 45,
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
	if raw.NodeName == "" {
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
	// Only explicit configured: true (with a non-prototype name) is considered configured.
	// Any old prototype (desktop1/desktop2, Desktop 1/2, Bridge-PC) or configs from prior builds
	// that lack explicit "configured": true must prompt the user for the device name.
	if raw.Configured && !strings.EqualFold(raw.NodeName, "Desktop 1") && !strings.EqualFold(raw.NodeName, "Desktop 2") && !strings.EqualFold(raw.NodeName, "Bridge-PC") && raw.NodeName != "" {
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

	// Ensure each peer has a valid SpoolPort assigned starting at 9101
	usedPorts := make(map[int]bool)
	for i := range cfg.Peers {
		cfg.Peers[i].URL = strings.TrimRight(strings.TrimSpace(cfg.Peers[i].URL), "/")
		if cfg.Peers[i].IP == "" && cfg.Peers[i].URL != "" {
			cfg.Peers[i].IP = ExtractIP(cfg.Peers[i].URL)
		}
		if cfg.Peers[i].SpoolPort > 0 {
			usedPorts[cfg.Peers[i].SpoolPort] = true
		}
	}
	nextPort := 9101
	for i := range cfg.Peers {
		if cfg.Peers[i].SpoolPort <= 0 {
			for usedPorts[nextPort] {
				nextPort++
			}
			cfg.Peers[i].SpoolPort = nextPort
			usedPorts[nextPort] = true
			migrated = true
		}
	}

	// Strictly purge self device from Peers slice if erroneously present from prior versions
	var cleanPeers []PeerDevice
	for _, p := range cfg.Peers {
		if !isSelfTarget(p.Name, p.IP, p.URL, cfg.NodeName, cfg.ListenPort) {
			cleanPeers = append(cleanPeers, p)
		} else {
			migrated = true
		}
	}
	cfg.Peers = cleanPeers

	// Keep legacy fields synchronized with primary peer
	if len(cfg.Peers) > 0 {
		cfg.PeerName = cfg.Peers[0].Name
		cfg.PeerURL = cfg.Peers[0].URL
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
		cfg.PrintTimeoutSec = 45
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
		_ = SaveConfig(path, cfg)
	}

	return cfg, nil
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
	copy(res, c.Peers)
	return res
}

// SetPeers thread-safely updates the peers list and saves configuration.
func (c *Config) SetPeers(peers []PeerDevice) error {
	c.mu.Lock()
	c.Peers = make([]PeerDevice, 0, len(peers))
	for _, p := range peers {
		if isSelfTarget(p.Name, p.IP, p.URL, c.NodeName, c.ListenPort) {
			continue // Strictly exclude self device from peers
		}
		c.Peers = append(c.Peers, p)
	}
	if len(c.Peers) > 0 {
		c.PeerName = c.Peers[0].Name
		c.PeerURL = c.Peers[0].URL
	} else {
		c.PeerName = ""
		c.PeerURL = ""
	}
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

	cleanTarget := strings.TrimRight(strings.TrimSpace(target), "/")
	if cleanTarget == "" {
		return false
	}

	for i := range c.Peers {
		if strings.EqualFold(c.Peers[i].Name, cleanTarget) || strings.EqualFold(c.Peers[i].URL, cleanTarget) || c.Peers[i].IP == cleanTarget {
			c.Peers[i].Online = online
			if online {
				c.Peers[i].LastSeen = time.Now()
			}
			return true
		}
	}
	return false
}

// SetPeerName updates a peer's friendly name and saves config.
func (c *Config) SetPeerName(target, newName string) bool {
	cleanName := strings.TrimSpace(newName)
	cleanTarget := strings.TrimRight(strings.TrimSpace(target), "/")
	if cleanName == "" || cleanTarget == "" {
		return false
	}

	c.mu.Lock()
	changed := false
	for i := range c.Peers {
		if strings.EqualFold(c.Peers[i].Name, cleanTarget) || strings.EqualFold(c.Peers[i].URL, cleanTarget) || c.Peers[i].IP == cleanTarget {
			if c.Peers[i].Name != cleanName {
				c.Peers[i].Name = cleanName
				changed = true
			}
			break
		}
	}
	if changed && len(c.Peers) > 0 {
		c.PeerName = c.Peers[0].Name
	}
	configPath := c.ConfigPath
	c.mu.Unlock()

	if changed && configPath != "" {
		_ = SaveConfig(configPath, c)
	}
	return changed
}

// AllocateSpoolPort assigns the next unused local port (starting from 9101) for a virtual printer.
func (c *Config) AllocateSpoolPort(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, p := range c.Peers {
		if strings.EqualFold(p.Name, name) && p.SpoolPort > 0 {
			return p.SpoolPort
		}
	}

	used := make(map[int]bool)
	for _, p := range c.Peers {
		if p.SpoolPort > 0 {
			used[p.SpoolPort] = true
		}
	}

	port := 9101
	for used[port] {
		port++
	}
	return port
}

// AddOrUpdatePeer adds a peer device or updates an existing matching peer by IP, URL, or Name.
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
	// Priority 2: Match by exact Name (if non-empty)
	// Priority 3: Match by IP only if URLs and Names do not conflict
	idx := -1
	for i, p := range c.Peers {
		if cleanURL != "" && strings.EqualFold(p.URL, cleanURL) {
			idx = i
			break
		}
	}
	if idx == -1 && cleanName != "" {
		for i, p := range c.Peers {
			if strings.EqualFold(p.Name, cleanName) {
				idx = i
				break
			}
		}
	}
	if idx == -1 && cleanIP != "" {
		for i, p := range c.Peers {
			if p.IP == cleanIP {
				// Don't match if URLs or Names explicitly contradict each other
				if cleanURL != "" && p.URL != "" && !strings.EqualFold(cleanURL, p.URL) {
					continue
				}
				if cleanName != "" && p.Name != "" && !strings.EqualFold(cleanName, p.Name) {
					continue
				}
				idx = i
				break
			}
		}
	}

	isNew := idx == -1
	var dev PeerDevice

	if isNew {
		// Allocate next available spool port
		used := make(map[int]bool)
		for _, p := range c.Peers {
			if p.SpoolPort > 0 {
				used[p.SpoolPort] = true
			}
		}
		port := 9101
		for used[port] {
			port++
		}

		if cleanName == "" {
			if cleanIP != "" {
				cleanName = cleanIP
			} else {
				cleanName = fmt.Sprintf("Peer %d", len(c.Peers)+1)
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
		if cleanName != "" && dev.Name != cleanName {
			dev.Name = cleanName
		}
		if cleanIP != "" {
			dev.IP = cleanIP
		}
		if cleanURL != "" {
			dev.URL = cleanURL
		}
		if dev.SpoolPort <= 0 {
			dev.SpoolPort = 9101 + idx
		}
		dev.Online = true
		dev.LastSeen = time.Now()
		c.Peers[idx] = dev
	}

	// Update legacy fields to match primary peer
	if len(c.Peers) > 0 {
		c.PeerName = c.Peers[0].Name
		c.PeerURL = c.Peers[0].URL
	}
	configPath := c.ConfigPath
	c.mu.Unlock()

	if configPath != "" {
		_ = SaveConfig(configPath, c)
	}

	return dev, isNew
}

// RemovePeer removes a peer by name, IP, or URL.
func (c *Config) RemovePeer(target string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	cleanTarget := strings.TrimRight(strings.TrimSpace(target), "/")
	if cleanTarget == "" {
		return false
	}
	idx := -1
	for i, p := range c.Peers {
		if strings.EqualFold(p.Name, cleanTarget) || strings.EqualFold(p.URL, cleanTarget) || p.IP == cleanTarget {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false
	}

	c.Peers = append(c.Peers[:idx], c.Peers[idx+1:]...)
	if len(c.Peers) > 0 {
		c.PeerName = c.Peers[0].Name
		c.PeerURL = c.Peers[0].URL
	} else {
		c.PeerName = ""
		c.PeerURL = ""
	}

	if c.ConfigPath != "" {
		_ = SaveConfig(c.ConfigPath, c)
	}
	return true
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
			cp := p
			return &cp
		}
	}
	// Try URL
	for _, p := range c.Peers {
		if strings.EqualFold(p.URL, cleanTarget) {
			cp := p
			return &cp
		}
	}
	// Try IP
	for _, p := range c.Peers {
		if p.IP == cleanTarget {
			cp := p
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
			cp := p
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

// SaveConfig writes the configuration to a JSON file.
func SaveConfig(path string, cfg *Config) error {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

