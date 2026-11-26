package installer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"bridge/config"
	"bridge/printer"
	"bridge/server"
)

// RunInteractiveSetup presents an automated, multi-device guided setup.
func RunInteractiveSetup() error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                       BRIDGE AUTOMATED SETUP                         ")
	fmt.Println("   Multi-Device Mesh File Sync & Silent Virtual Printing Over Tailscale")
	fmt.Println("======================================================================")
	fmt.Println("This setup will automatically verify your environment, connect to     ")
	fmt.Println("Tailscale, and link your devices into a synchronized network.         ")
	fmt.Println("======================================================================")
	fmt.Println()

	// 1. Detect or Install Tailscale, with automated browser login
	localIP, err := EnsureTailscaleConnected()
	for localIP == "" {
		fmt.Println()
		if err != nil {
			fmt.Printf("[!] Tailscale connection error: %v\n", err)
		} else {
			fmt.Println("[!] Tailscale is not connected yet.")
		}
		fmt.Print("Press Enter to retry Tailscale connection, or 'c' to continue anyway: ")
		ans, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(ans)) == "c" {
			break
		}
		localIP, err = EnsureTailscaleConnected()
	}

	// 2. Check Silent Print Engine (SumatraPDF)
	fmt.Print("[*] Checking Silent Print Engine (SumatraPDF)... ")
	sumatra := FindSumatraPDF()
	if sumatra == "" {
		fmt.Println("Not found!")
		fmt.Println("[*] SumatraPDF is required for native-quality silent background printing.")
		fmt.Println("[*] Downloading and provisioning standalone SumatraPDF engine...")
		installedPath, err := InstallSumatraPDF()
		if err != nil {
			fmt.Printf("[!] Note on print engine auto-install: %v\n", err)
		} else {
			sumatra = installedPath
			fmt.Printf("[✓] Silent Print Engine ready: %s\n", sumatra)
		}
	} else {
		fmt.Printf("Found: %s\n", sumatra)
		removeSumatraShortcuts()
	}

	cfg := config.DefaultConfig()
	if sumatra != "" {
		cfg.SumatraPath = sumatra
	}

	// Load existing configuration if present to offer previous name as default
	if existingCfg, err := config.LoadConfig(TargetConfig); err == nil && existingCfg.NodeName != "" {
		cfg.NodeName = existingCfg.NodeName
		cfg.Peers = existingCfg.GetPeers()
		if existingCfg.AuthToken != "" {
			cfg.AuthToken = existingCfg.AuthToken
		}
	}

	// 3. Auto-detect active Bridge devices on Tailscale first to establish context
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("               AUTO-DETECTING ACTIVE NETWORK DEVICES                  ")
	fmt.Println("======================================================================")
	if localIP != "" {
		fmt.Printf(">>> THIS DEVICE'S TAILSCALE IP: %s <<<\n", localIP)
	}
	fmt.Print("[*] Scanning your Tailscale network for active Bridge devices... ")

	type activePeerInfo struct {
		name         string
		ip           string
		url          string
		printerReady bool
		printerName  string
		meshPeers    []config.PeerDevice
	}
	var detectedPeers []activePeerInfo
	var detectedNames []string

	tsPeers, err := discoverPeersFunc()
	if err == nil && len(tsPeers) > 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		seenIPs := make(map[string]bool)
		seenNames := make(map[string]bool)

		for _, p := range tsPeers {
			if !p.BridgeActive || !p.Online || p.IP == localIP {
				continue
			}
			wg.Add(1)
			go func(peer DiscoveredPeer) {
				defer wg.Done()
				peerURL := fmt.Sprintf("http://%s:8765", peer.IP)
				// Probe peer's /api/health with probe header (strictly read-only, does not trigger auto-pair)
				client := http.Client{Timeout: 4 * time.Second}
				req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", peerURL), nil)
				if err == nil {
					req.Header.Set("X-Bridge-Token", cfg.AuthToken)
					req.Header.Set("X-Bridge-Probe", "1")
					resp, err := client.Do(req)
					if err == nil && resp.StatusCode == http.StatusOK {
						var hr struct {
							Status         string              `json:"status"`
							NodeName       string              `json:"node_name"`
							PrinterReady   bool                `json:"printer_ready"`
							DefaultPrinter string              `json:"default_printer"`
							Peers          []config.PeerDevice `json:"peers"`
						}
						nodeName := peer.BridgeName
						printerReady := peer.PrinterReady
						printerName := peer.PrinterName
						var meshPeers []config.PeerDevice
						decodedOK := json.NewDecoder(resp.Body).Decode(&hr) == nil &&
							strings.EqualFold(strings.TrimSpace(hr.Status), "healthy") &&
							strings.TrimSpace(hr.NodeName) != ""
						if decodedOK {
							nodeName = hr.NodeName
							printerReady = hr.PrinterReady
							printerName = strings.TrimSpace(hr.DefaultPrinter)
							meshPeers = hr.Peers
						}
						resp.Body.Close()

						if !decodedOK {
							return
						}

						cleanName := strings.TrimSpace(nodeName)
						normName := strings.ToLower(cleanName)
						normIP := strings.TrimSpace(peer.IP)

						mu.Lock()
						if !seenIPs[normIP] && !seenNames[normName] && !config.IsSelfTarget(cleanName, peer.IP, peerURL, cfg.NodeName, cfg.ListenPort) {
							seenIPs[normIP] = true
							seenNames[normName] = true
							detectedPeers = append(detectedPeers, activePeerInfo{
								name:         cleanName,
								ip:           peer.IP,
								url:          peerURL,
								printerReady: printerReady,
								printerName:  printerName,
								meshPeers:    meshPeers,
							})
						}
						mu.Unlock()
						return
					}
					if resp != nil {
						resp.Body.Close()
					}
				}
			}(p)
		}
		wg.Wait()
	}
	fmt.Printf("Done! (Found %d)\n", len(detectedPeers))

	// Build clean, deduplicated list of active peer device names (strictly unique, excluding self)
	seenNameSet := make(map[string]bool)
	if cfg.NodeName != "" {
		seenNameSet[strings.ToLower(strings.TrimSpace(cfg.NodeName))] = true
	}
	for _, p := range detectedPeers {
		clean := strings.TrimSpace(p.name)
		low := strings.ToLower(clean)
		if clean != "" && !seenNameSet[low] {
			seenNameSet[low] = true
			detectedNames = append(detectedNames, clean)
		}
		for _, mp := range p.meshPeers {
			mpClean := strings.TrimSpace(mp.Name)
			mpLow := strings.ToLower(mpClean)
			if mpClean != "" && !seenNameSet[mpLow] && mp.IP != localIP && !config.IsSelfTarget(mpClean, mp.IP, mp.URL, cfg.NodeName, cfg.ListenPort) {
				seenNameSet[mpLow] = true
				detectedNames = append(detectedNames, mpClean)
			}
		}
	}

	// 4. User Experience: Ask for THIS device's name with intelligent default
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                       DEVICE CONFIGURATION                           ")
	fmt.Println("======================================================================")

	defaultName := SuggestNextDeviceName(detectedNames, cfg.NodeName)

	if len(detectedNames) > 0 {
		fmt.Printf("[*] Active Bridge devices detected on network: %s\n", strings.Join(detectedNames, ", "))
	}
	fmt.Printf("What name to give THIS device? (e.g. U1, U2, Unit 1) [Default: '%s']: ", defaultName)
	nameInput, _ := reader.ReadString('\n')
	nameInput = strings.TrimSpace(nameInput)
	if nameInput != "" {
		cfg.NodeName = nameInput
	} else {
		cfg.NodeName = defaultName
	}
	cfg.Configured = true

	// Prune self device from detected peers if matching
	var filteredDetectedPeers []activePeerInfo
	for _, p := range detectedPeers {
		if !config.IsSelfTarget(p.name, p.ip, p.url, cfg.NodeName, cfg.ListenPort) {
			filteredDetectedPeers = append(filteredDetectedPeers, p)
		}
	}
	detectedPeers = filteredDetectedPeers

	fmt.Println()
	fmt.Printf("[✓] Configured this device as: '%s'\n", cfg.NodeName)
	if localIP != "" {
		fmt.Printf("[✓] Device IP: %s\n", localIP)
	}
	reportLocalPrinterStatus(os.Stdout)

	// 5. Connect and pair devices with clear confirmation
	if len(detectedPeers) == 0 {
		// Zero peers: This is Device 1 (first device on the network)
		fmt.Println("[*] No other active Bridge devices detected on your network.")
		fmt.Printf("[✓] '%s' will act as your first network device.\n", cfg.NodeName)
		fmt.Println()
		fmt.Println("When setting up your next device (e.g. U2), start Bridge.")
		fmt.Printf("It will automatically detect this device ('%s' - %s) and prompt to link them!\n", cfg.NodeName, localIP)
		fmt.Println()
		fmt.Println("Enter peer IP address or device name if connecting to a specific peer,")
		fmt.Print("or press Enter if this is Device 1 (or setting up other devices later): ")
		manualInput, _ := reader.ReadString('\n')
		manualInput = strings.TrimSpace(manualInput)
		if manualInput != "" && !isNegativeReply(manualInput) {
			connectManualPeer(cfg, reader, manualInput, localIP)
		} else {
			fmt.Println()
			fmt.Println("======================================================================")
			fmt.Printf("               SETUP COMPLETED FOR '%s'!\n", cfg.NodeName)
			fmt.Println("======================================================================")
			if localIP != "" {
				fmt.Printf(">>> THIS DEVICE'S IP: %s <<<\n", localIP)
			}
			fmt.Println("======================================================================")
		}
	} else if len(detectedPeers) == 1 {
		// Exactly 1 active device found (e.g. setting up Device 2, found U1)
		p := detectedPeers[0]
		fmt.Println()
		fmt.Printf("[*] Current active device found: '%s' (IP: %s) %s\n", p.name, p.ip, formatDiscoveredPrinterTag(p.printerReady))
		fmt.Printf("Connect to '%s' (%s)? [Y = Connect, N = Skip, or enter another IP/name]: ", p.name, p.ip)
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, p.ip, p.url, p.name, cfg.NodeName)
			connectDetectedPeer(cfg, confirmedName, isCustom, p.ip, p.url, p.meshPeers, localIP)
		} else if !isNegativeReply(ans) {
			connectManualPeer(cfg, reader, ans, localIP)
		}
	} else {
		// Multiple active devices found (e.g. setting up Device 3, found U1 and U2)
		fmt.Println()
		fmt.Println("======================================================================")
		fmt.Println("                  REGISTERED DEVICES ON YOUR NETWORK                  ")
		fmt.Println("======================================================================")
		fmt.Println("Active Bridge devices already registered:")
		for i, p := range detectedPeers {
			fmt.Printf("    [%d] %-15s (IP: %s) %s\n", i+1, p.name, p.ip, formatDiscoveredPrinterTag(p.printerReady))
		}
		fmt.Println("======================================================================")
		fmt.Print("Connect to these devices? [Y = Connect all, 1-N = Pick one, N = Skip, or enter IP/name]: ")
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			linkedMap := make(map[string]bool)
			for _, p := range detectedPeers {
				normIP := strings.TrimSpace(p.ip)
				normName := strings.ToLower(strings.TrimSpace(p.name))
				if linkedMap[normIP] || linkedMap[normName] {
					continue
				}
				linkedMap[normIP] = true
				linkedMap[normName] = true
				confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, p.ip, p.url, p.name, cfg.NodeName)
				connectDetectedPeer(cfg, confirmedName, isCustom, p.ip, p.url, p.meshPeers, localIP)
			}
		} else if !isNegativeReply(ans) {
			idx := 0
			if _, scanErr := fmt.Sscanf(ans, "%d", &idx); scanErr == nil && idx >= 1 && idx <= len(detectedPeers) {
				sel := detectedPeers[idx-1]
				confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, sel.ip, sel.url, sel.name, cfg.NodeName)
				connectDetectedPeer(cfg, confirmedName, isCustom, sel.ip, sel.url, sel.meshPeers, localIP)
			} else {
				connectManualPeer(cfg, reader, ans, localIP)
			}
		}
	}

	// 5. Shared Secret Key
	if cfg.AuthToken == "" {
		cfg.AuthToken = "bridge-secret-token"
	}

	// 6. Install Service, Shortcuts, and Virtual Printers
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("CONFIGURING BRIDGE SYSTEM SERVICE & PRINTERS...")
	fmt.Println("======================================================================")

	printerRes, installErr := installServiceWithReport(cfg)
	if installErr != nil {
		fmt.Printf("\n[!] Setup warning: %v\n", installErr)
	}

	fmt.Println("[*] Launching Bridge background service in Windows Taskbar tray...")
	if err := RestartService(); err != nil {
		fmt.Printf("[!] Note starting background service: %v\n", err)
	}

	printSetupSuccess(cfg, localIP, printerRes, installErr, reader)
	return nil
}

var (
	discoverPeersFunc            = DiscoverTailscalePeers
	getTargetPhysicalPrinterFunc = printer.GetTargetPhysicalPrinter
)

func formatDiscoveredPrinterTag(printerReady bool) string {
	if printerReady {
		return "(Printer: Ready)"
	}
	return "(No Printer)"
}

func reportLocalPrinterStatus(w io.Writer) {
	if w == nil {
		w = os.Stdout
	}
	targetPrinter, err := getTargetPhysicalPrinterFunc()
	if err == nil && targetPrinter != "" {
		fmt.Fprintf(w, "[✓] Local physical printer: %s (Connected & Ready)\n", targetPrinter)
		return
	}
	fmt.Fprintln(w, "[*] Local physical printer: None detected")
	fmt.Fprintln(w, "    Note: This device can still sync files and send prints to other devices.")
}

func truncateDisplayName(s string, maxLen int) string {
	clean := strings.TrimSpace(s)
	if maxLen <= 0 {
		return clean
	}
	runes := []rune(clean)
	if len(runes) <= maxLen {
		return clean
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func formatDashboardPeerStatus(p config.PeerDevice) string {
	if !p.Online {
		return "[OFFLINE]"
	}
	if p.IsPrinterKnownMissing() {
		return "[ONLINE | No Printer]"
	}
	if strings.TrimSpace(p.PrinterName) != "" {
		return fmt.Sprintf("[ONLINE | Printer: %s]", truncateDisplayName(p.PrinterName, 28))
	}
	if p.PrinterReady != nil && *p.PrinterReady {
		return "[ONLINE | Printer Ready]"
	}
	return "[ONLINE]"
}

func applyPeerHealthStatus(peers []config.PeerDevice, healthPeers []config.PeerDevice, serviceStopped bool) ([]config.PeerDevice, int) {
	if serviceStopped {
		for i := range peers {
			peers[i].Online = false
			peers[i].PrinterReady = nil
			peers[i].PrinterName = ""
		}
		return peers, 0
	}

	if len(healthPeers) > len(peers) {
		peers = append([]config.PeerDevice(nil), healthPeers...)
	}

	peerHealthMap := make(map[string]config.PeerDevice)
	addHealthKey := func(key string, hp config.PeerDevice) {
		if key == "" {
			return
		}
		if existing, exists := peerHealthMap[key]; !exists || (!existing.Online && hp.Online) {
			peerHealthMap[key] = hp
		}
	}
	for _, hp := range healthPeers {
		if hp.Name != "" {
			addHealthKey("name:"+strings.ToLower(strings.TrimSpace(hp.Name)), hp)
		}
		if hp.IP != "" {
			addHealthKey("ip:"+strings.TrimSpace(hp.IP), hp)
		}
		if hp.URL != "" {
			addHealthKey("url:"+strings.TrimSpace(hp.URL), hp)
		}
		if hp.SpoolPort > 0 {
			addHealthKey(fmt.Sprintf("port:%d", hp.SpoolPort), hp)
		}
	}

	lookupHealth := func(p config.PeerDevice) (config.PeerDevice, bool) {
		if p.URL != "" {
			if hp, ok := peerHealthMap["url:"+strings.TrimSpace(p.URL)]; ok {
				return hp, true
			}
		}
		if p.IP != "" {
			if hp, ok := peerHealthMap["ip:"+strings.TrimSpace(p.IP)]; ok {
				return hp, true
			}
		}
		if p.Name != "" {
			if hp, ok := peerHealthMap["name:"+strings.ToLower(strings.TrimSpace(p.Name))]; ok {
				return hp, true
			}
		}
		if p.SpoolPort > 0 {
			if hp, ok := peerHealthMap[fmt.Sprintf("port:%d", p.SpoolPort)]; ok {
				return hp, true
			}
		}
		return config.PeerDevice{}, false
	}

	onlineCount := 0
	for i := range peers {
		p := &peers[i]
		if hp, ok := lookupHealth(*p); ok {
			p.Online = hp.Online
			if hp.PrinterReady != nil {
				ready := *hp.PrinterReady
				p.PrinterReady = &ready
			} else {
				p.PrinterReady = nil
			}
			p.PrinterName = strings.TrimSpace(hp.PrinterName)
		} else {
			p.Online = false
			p.PrinterReady = nil
			p.PrinterName = ""
		}
		if p.Online {
			onlineCount++
		}
	}
	return peers, onlineCount
}

func isNegativeReply(ans string) bool {
	low := strings.ToLower(strings.TrimSpace(ans))
	return low == "n" || low == "no" || low == "skip" || low == "cancel"
}

func confirmPeerName(reader *bufio.Reader, ip, detectedName, selfNodeName string) (string, bool) {
	return confirmPeerNameWithConfig(reader, nil, ip, "", detectedName, selfNodeName)
}

func confirmPeerNameWithConfig(reader *bufio.Reader, cfg *config.Config, ip, peerURL, detectedName, selfNodeName string) (string, bool) {
	if selfNodeName == "" && cfg != nil {
		selfNodeName = cfg.GetNodeName()
	}
	cleanDetected := strings.TrimSpace(detectedName)
	if cleanDetected == "" {
		cleanDetected = fmt.Sprintf("Device (%s)", ip)
	}
	if cfg != nil && cfg.HasPeerNameCollision(cleanDetected, ip, peerURL) && ip != "" {
		cleanDetected = fmt.Sprintf("%s (%s)", cleanDetected, ip)
	}

	fmt.Printf("Confirm name for %s [Enter to keep '%s', or type custom name]: ", ip, cleanDetected)
	custom, _ := reader.ReadString('\n')
	custom = strings.TrimSpace(custom)
	confirmed := cleanDetected
	isCustom := false
	if custom != "" {
		if selfNodeName != "" && strings.EqualFold(custom, strings.TrimSpace(selfNodeName)) {
			fmt.Printf("[!] '%s' matches this device's name; keeping '%s'.\n", custom, cleanDetected)
		} else if cfg != nil && cfg.HasPeerNameCollision(custom, ip, peerURL) {
			fmt.Printf("[!] '%s' collides with an existing paired peer; keeping '%s'.\n", custom, cleanDetected)
		} else {
			confirmed = custom
			isCustom = !strings.EqualFold(custom, cleanDetected)
		}
	}
	fmt.Printf("[*] Virtual Printer 'Print on %s' will be created.\n", SanitizeFileName(confirmed))
	return confirmed, isCustom
}

func connectDetectedPeer(cfg *config.Config, name string, isCustom bool, ip, targetURL string, meshPeers []config.PeerDevice, localIP string) {
	cleanName := strings.TrimSpace(name)
	if config.IsSelfTarget(cleanName, ip, targetURL, cfg.NodeName, cfg.ListenPort) {
		return // Strictly ignore self device
	}
	if localIP != "" && ip == localIP {
		return // Strictly ignore self IP
	}

	addedDev, isNew := cfg.AddOrUpdatePeer(cleanName, ip, targetURL)
	target := addedDev.URL
	if target == "" {
		target = addedDev.IP
	}
	if isCustom {
		if cfg.SetPeerCustomName(target, cleanName, true) {
			addedDev.Name = cleanName
			addedDev.CustomName = true
		}
	} else if !addedDev.CustomName {
		cfg.SetPeerCustomName(target, addedDev.Name, false)
	}
	if isNew {
		fmt.Printf("[*] Linking with '%s' (%s)...\n", addedDev.Name, ip)
		fmt.Printf("[✓] Linked with '%s' (Port %d). Preparing 'Print on %s'...\n", addedDev.Name, addedDev.SpoolPort, SanitizeFileName(addedDev.Name))
	}

	// Introduce ourselves to the remote peer
	if localIP != "" {
		IntroduceToRemote(targetURL, cfg.NodeName, localIP, cfg.AuthToken)
	}

	// Inherit and link with any other mesh peers discovered from this device (excluding self and already added)
	for _, mp := range meshPeers {
		mpCleanName := strings.TrimSpace(mp.Name)
		if config.IsSelfTarget(mpCleanName, mp.IP, mp.URL, cfg.NodeName, cfg.ListenPort) {
			continue
		}
		if (localIP == "" || mp.IP != localIP) && mp.IP != ip {
			if cfg.FindPeer(mpCleanName) == nil && cfg.FindPeer(mp.IP) == nil {
				added, isNew := cfg.AddOrUpdatePeer(mpCleanName, mp.IP, mp.URL)
				if isNew {
					fmt.Printf("[✓] Discovered mesh device: '%s' (%s)\n", added.Name, added.IP)
					if mp.URL != "" && localIP != "" {
						IntroduceToRemote(mp.URL, cfg.NodeName, localIP, cfg.AuthToken)
					}
				}
			}
		}
	}
}

func resolveManualTarget(input string) (ip, targetURL, errMsg string) {
	clean := strings.TrimSpace(input)
	if clean == "" {
		return "", "", "empty peer input"
	}

	low := strings.ToLower(clean)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		u, err := url.Parse(clean)
		if err == nil && u.Hostname() != "" {
			host := u.Hostname()
			port := u.Port()
			if port == "" {
				port = "8765"
			}
			if net.ParseIP(host) != nil {
				return host, fmt.Sprintf("%s://%s:%s", u.Scheme, host, port), ""
			}
			clean = host
		}
	} else if host, port, err := net.SplitHostPort(clean); err == nil && net.ParseIP(host) != nil && port != "" {
		return host, fmt.Sprintf("http://%s:%s", host, port), ""
	} else if net.ParseIP(clean) != nil {
		return clean, fmt.Sprintf("http://%s:8765", clean), ""
	}

	// Not a literal IP: resolve as Tailscale or Bridge device name via DiscoverTailscalePeers()
	tsPeers, err := discoverPeersFunc()
	if err != nil {
		return "", "", fmt.Sprintf("could not resolve device name %q (%v); enter its Tailscale IP (100.x.y.z)", clean, err)
	}

	var hostMatch *DiscoveredPeer
	for i := range tsPeers {
		p := &tsPeers[i]
		if p.BridgeName != "" && strings.EqualFold(strings.TrimSpace(p.BridgeName), clean) {
			return p.IP, fmt.Sprintf("http://%s:8765", p.IP), ""
		}
		hn := strings.TrimSpace(p.HostName)
		shortHost := strings.Split(hn, ".")[0]
		if hostMatch == nil && (strings.EqualFold(hn, clean) || (shortHost != "" && strings.EqualFold(shortHost, clean))) {
			hostMatch = p
		}
	}
	if hostMatch != nil {
		return hostMatch.IP, fmt.Sprintf("http://%s:8765", hostMatch.IP), ""
	}

	return "", "", fmt.Sprintf("no Tailscale or Bridge device named %q was found; verify the device is online or enter its Tailscale IP (100.x.y.z)", clean)
}

func connectManualPeer(cfg *config.Config, reader *bufio.Reader, input, localIP string) bool {
	cleanIP, targetURL, resErr := resolveManualTarget(input)
	if resErr != "" {
		fmt.Printf("[!] %s. No peer or virtual printer was added.\n", resErr)
		return false
	}

	if (localIP != "" && cleanIP == localIP) || config.IsSelfTarget("", cleanIP, targetURL, cfg.NodeName, cfg.ListenPort) {
		fmt.Printf("[!] Target %s resolves to this device ('%s'). Cannot pair a device with itself.\n", cleanIP, cfg.NodeName)
		return false
	}

	fmt.Printf("[*] Probing Bridge service at %s/api/health...\n", targetURL)

	client := http.Client{Timeout: 4 * time.Second}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", targetURL), nil)
	if err != nil {
		fmt.Printf("[!] Invalid peer target %s (%v). No peer or virtual printer was added.\n", targetURL, err)
		return false
	}
	req.Header.Set("X-Bridge-Token", cfg.AuthToken)
	req.Header.Set("X-Bridge-Node-Name", cfg.NodeName)
	if localIP != "" {
		listenPort := cfg.ListenPort
		if listenPort <= 0 {
			listenPort = 8765
		}
		req.Header.Set("X-Bridge-Peer-URL", fmt.Sprintf("http://%s:%d", localIP, listenPort))
	}
	req.Header.Set("X-Bridge-Probe", "1")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[!] Could not reach Bridge at %s (%v).\n", targetURL, err)
		fmt.Println("    Verify Tailscale is connected and bridge.exe is running on that device. No peer or virtual printer was added.")
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("[!] Peer at %s returned HTTP %d (expected 200 OK). No peer or virtual printer was added.\n", targetURL, resp.StatusCode)
		return false
	}

	var hr server.HealthResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&hr); decErr != nil || strings.TrimSpace(hr.NodeName) == "" || strings.TrimSpace(hr.Status) == "" {
		fmt.Printf("[!] Endpoint at %s did not return valid Bridge health JSON (not a Bridge node). No peer or virtual printer was added.\n", targetURL)
		return false
	}

	if config.IsSelfTarget(hr.NodeName, cleanIP, targetURL, cfg.NodeName, cfg.ListenPort) {
		fmt.Printf("[!] Endpoint at %s identifies as this device ('%s'). Skipping self-pairing.\n", targetURL, hr.NodeName)
		return false
	}

	fmt.Printf("[✓] Verified active Bridge device '%s' at %s %s\n", hr.NodeName, cleanIP, formatDiscoveredPrinterTag(hr.PrinterReady))
	confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, cleanIP, targetURL, hr.NodeName, cfg.NodeName)
	connectDetectedPeer(cfg, confirmedName, isCustom, cleanIP, targetURL, hr.Peers, localIP)
	return true
}

func printSetupSuccess(cfg *config.Config, localIP string, printerRes ShortcutSyncResult, installErr error, reader *bufio.Reader) {
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                   SETUP COMPLETED SUCCESSFULLY!                      ")
	fmt.Println("======================================================================")
	fmt.Println("[✓] Bridge multi-device service is active in your user session.")
	fmt.Println("[✓] Multifunctional Status Dot is active in your Windows Taskbar!")
	fmt.Println("[✓] Global Shared Folder created on Desktop: 'Bridge Shared Folder' (C:\\Bridge\\Shared)")

	succeeded := make(map[string]bool, len(printerRes.SucceededPrinters))
	for _, name := range printerRes.SucceededPrinters {
		succeeded[name] = true
	}

	peers := cfg.GetPeers()
	if len(peers) > 0 {
		for _, p := range peers {
			cleanPeer := SanitizeFileName(p.Name)
			if succeeded[cleanPeer] {
				fmt.Printf("[✓] Windows Virtual Printer created: 'Print on %s' (Select in Ctrl+P!)\n", cleanPeer)
			} else if pErr, hasErr := printerRes.PrinterErrors[cleanPeer]; hasErr {
				fmt.Printf("[!] Windows Virtual Printer 'Print on %s' failed: %v\n", cleanPeer, pErr)
			} else if installErr != nil {
				fmt.Printf("[!] Windows Virtual Printer 'Print on %s' was NOT created: %v\n", cleanPeer, installErr)
			} else {
				fmt.Printf("[!] Windows Virtual Printer 'Print on %s' could not be verified.\n", cleanPeer)
			}
			fmt.Printf("[✓] Right-Click Send-To added: 'Print on %s'\n", cleanPeer)
		}
	} else {
		fmt.Println("[✓] Virtual printers will be created automatically as soon as peer devices connect.")
	}

	fmt.Println()
	if localIP != "" && len(peers) == 0 {
		fmt.Println("NEXT STEP ON YOUR OTHER COMPUTERS:")
		fmt.Println("1. Run 'bridge.exe' on your second computer.")
		fmt.Printf("2. Bridge will automatically detect this computer ('%s' - %s) on Tailscale!\n", cfg.NodeName, localIP)
		fmt.Println("3. Press 'Y' to connect and your multi-device mesh & virtual printers will configure instantly!")
	}
	fmt.Println("======================================================================")
	fmt.Println("Press Enter to finish setup...")
	_, _ = reader.ReadString('\n')
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func countDirRegularFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

func isServiceStopped(status string) bool {
	up := strings.ToUpper(status)
	return strings.Contains(up, "STOPPED") || strings.Contains(up, "OFFLINE")
}

// RunDashboard displays status and actions when bridge.exe is launched interactively.
func RunDashboard() error {
	reader := bufio.NewReader(os.Stdin)

	for {
		cfg, err := config.LoadConfig(TargetConfig)
		if err != nil {
			return RunInteractiveSetup()
		}

		health := queryHealth(cfg.ListenPort)
		serviceStopped := isServiceStopped(health.Status)

		fmt.Println()
		fmt.Println("======================================================================")
		fmt.Println("                 BRIDGE MULTI-DEVICE DASHBOARD                        ")
		fmt.Println("======================================================================")
		fmt.Printf(" This Computer:    %s\n", cfg.GetNodeName())
		localIP := GetTailscaleIP()
		if localIP != "" {
			fmt.Printf(" Tailscale IP:     %s\n", localIP)
		}
		fmt.Printf(" Service Status:   %s\n", health.Status)

		targetPrinter, err := getTargetPhysicalPrinterFunc()
		if err == nil && targetPrinter != "" {
			fmt.Printf(" Local Printer:    %s (Connected & Ready)\n", targetPrinter)
		} else {
			fmt.Println(" Local Printer:    None (No physical printer connected - File Sync & Remote Send active)")
		}

		peers, onlineCount := applyPeerHealthStatus(cfg.GetPeers(), health.Peers, serviceStopped)

		printerStatusMap := CheckVirtualPrinters(peers)

		fmt.Printf(" Connected Peers:  %d total (%d online)\n", len(peers), onlineCount)
		if len(peers) > 0 {
			for i, p := range peers {
				statusStr := formatDashboardPeerStatus(p)
				cleanPeer := SanitizeFileName(p.Name)
				vpBadge := "Missing"
				if printerStatusMap[p.Name] {
					vpBadge = "OK"
				}
				fmt.Printf("   [%d] %-15s IP: %-15s Port: %-5d Status: %-22s | Print on %s: %s\n",
					i+1, p.Name, p.IP, p.SpoolPort, statusStr, cleanPeer, vpBadge)
			}
		} else {
			fmt.Println("   (No peers connected yet - waiting for other devices)")
		}

		failedDir := cfg.PrintFailedDir
		if strings.TrimSpace(failedDir) == "" {
			failedDir = `C:\Bridge\PrintFailed`
		}
		failedCount := countDirRegularFiles(failedDir)
		outboundCount := printer.NewOutboundQueue(printer.DefaultOutboundQueueDir).Count()

		fmt.Printf(" Active Print Jobs: %d | Outbound Queue: %d | Failed Jobs: %d\n",
			health.PrintQueueDepth, outboundCount, failedCount)
		if failedCount > 0 {
			fmt.Printf(" [!] %d failed print job(s) in %s\n", failedCount, failedDir)
		}
		if health.UptimeSeconds > 0 && !serviceStopped {
			uptimeDur := time.Duration(health.UptimeSeconds) * time.Second
			fmt.Printf(" Service Uptime:   %s\n", uptimeDur)
		}

		currentExe, _ := os.Executable()
		isExternal := currentExe != "" && !strings.EqualFold(filepath.Clean(currentExe), filepath.Clean(TargetExe))
		if isExternal {
			fmt.Println("----------------------------------------------------------------------")
			fmt.Printf(" [!] Running from external binary: %s\n", currentExe)
			fmt.Printf("     Installed binary location:    %s\n", TargetExe)
			curHash, curErr := fileSHA256(currentExe)
			instHash, instErr := fileSHA256(TargetExe)
			if curErr == nil && instErr == nil {
				shortCur := curHash
				if len(shortCur) > 12 {
					shortCur = shortCur[:12]
				}
				shortInst := instHash
				if len(shortInst) > 12 {
					shortInst = shortInst[:12]
				}
				if curHash == instHash {
					fmt.Printf("     Binary SHA256: %s (matches installed binary)\n", shortCur)
				} else {
					fmt.Printf(" [!] Binary SHA256 DIFFERS (running: %s vs installed: %s)\n", shortCur, shortInst)
					fmt.Println("     Select [3] below to upgrade the installed service to this binary.")
				}
			}
		}

		fmt.Println("----------------------------------------------------------------------")
		fmt.Println(" Quick Actions:")
		fmt.Println("   [1] Connect / Pair a New Device")
		if serviceStopped {
			fmt.Println("   [2] Start Background Service")
		} else {
			fmt.Println("   [2] Restart Background Service")
		}
		if isExternal {
			fmt.Println("   [3] Upgrade Bridge Installation to this executable")
		} else {
			fmt.Println("   [3] Upgrade / Refresh Bridge Installation")
		}
		fmt.Println("   [4] Uninstall Bridge Completely")
		fmt.Println("   [5] Remove a Paired Device")
		fmt.Println("   [6] Open Shared Folder")
		fmt.Println("   [0] Exit Dashboard")
		fmt.Println("======================================================================")
		fmt.Print("Select an option [0-6]: ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			if !IsElevated() {
				fmt.Println("[*] Administrator elevation required to pair devices & register virtual printers. Elevating...")
				time.Sleep(500 * time.Millisecond)
				ReleaseInteractiveLock()
				if err := SelfElevate([]string{"-pair"}); err != nil {
					fmt.Printf("[!] Elevation failed or canceled: %v\n", err)
					_ = AcquireInteractiveLock()
					time.Sleep(1500 * time.Millisecond)
					continue
				}
				return nil
			}
			pairNewDevice(cfg, reader, localIP)
		case "2":
			if serviceStopped {
				fmt.Println("[*] Starting Bridge background service...")
				if err := StartService(); err != nil {
					fmt.Printf("[!] Failed to start background service: %v\n", err)
				} else {
					fmt.Println("[✓] Background service started in taskbar tray.")
				}
			} else {
				fmt.Println("[*] Restarting Bridge background service...")
				if err := RestartService(); err != nil {
					fmt.Printf("[!] Failed to restart background service: %v\n", err)
				} else {
					fmt.Println("[✓] Background service restarted.")
				}
			}
			time.Sleep(2 * time.Second)
		case "3":
			targetSource := currentExe
			if !isExternal {
				fmt.Println("\nEnter path to new bridge.exe to install,")
				fmt.Print("or press Enter to refresh/repair the current installation: ")
				inputPath, _ := reader.ReadString('\n')
				inputPath = strings.TrimSpace(inputPath)
				if inputPath != "" {
					inputPath = strings.Trim(inputPath, "\"'")
					if _, err := os.Stat(inputPath); err != nil {
						fmt.Printf("[!] Specified file does not exist: %v\n", err)
						time.Sleep(2 * time.Second)
						continue
					}
					targetSource = inputPath
				}
			}
			fmt.Println()
			fmt.Printf("Are you sure you want to upgrade Bridge installation using: \n%s? [y/N]: ", targetSource)
			conf, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(conf)) == "y" {
				if !IsElevated() {
					fmt.Println("[*] Administrator elevation required for upgrade. Elevating...")
					time.Sleep(500 * time.Millisecond)
					ReleaseInteractiveLock()
					if err := SelfElevate([]string{"-upgrade", targetSource}); err != nil {
						fmt.Printf("[!] Elevation failed or canceled: %v\n", err)
						_ = AcquireInteractiveLock()
						time.Sleep(1500 * time.Millisecond)
						continue
					}
					return nil
				}
				if err := runUpgradeWithDiscoverySource(targetSource, cfg, reader); err != nil {
					fmt.Printf("[!] Upgrade failed: %v\n", err)
				}
				fmt.Println("Press Enter to continue...")
				_, _ = reader.ReadString('\n')
			}
		case "4":
			fmt.Println()
			fmt.Println("======================================================================")
			fmt.Println("                    HARD UNINSTALL CONFIRMATION                       ")
			fmt.Println("======================================================================")
			fmt.Println("This will permanently remove:")
			fmt.Println("  - Bridge background service and taskbar tray icon")
			fmt.Println("  - All saved printer names and Windows virtual printer ports")
			fmt.Println("  - Autostart entries from Windows startup and registry")
			fmt.Println("  - Application files in C:\\BridgeApp")
			fmt.Println("  (Your synchronized files in C:\\Bridge\\Shared will NOT be deleted)")
			fmt.Println("======================================================================")
			fmt.Print("Are you sure you want to completely uninstall Bridge? [y/N]: ")
			conf, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(conf)) == "y" {
				if !IsElevated() {
					fmt.Println("[*] Administrator elevation required for uninstallation. Elevating...")
					time.Sleep(500 * time.Millisecond)
					ReleaseInteractiveLock()
					if err := SelfElevate([]string{"-uninstall"}); err != nil {
						fmt.Printf("[!] Elevation failed or canceled: %v\n", err)
						_ = AcquireInteractiveLock()
						time.Sleep(1500 * time.Millisecond)
						continue
					}
					return nil
				}
				err := UninstallService()
				if err != nil {
					fmt.Printf("[!] Uninstall encountered an error: %v\n", err)
					time.Sleep(2 * time.Second)
				}
				// Exit immediately without pausing so C:\BridgeApp\bridge.exe file lock releases for deletion.
				return err
			}
		case "5":
			removePairedDevice(cfg, reader, localIP)
		case "6":
			sharedDir := cfg.SharedDir
			if strings.TrimSpace(sharedDir) == "" {
				sharedDir = `C:\Bridge\Shared`
			}
			_ = os.MkdirAll(sharedDir, 0755)
			if runtime.GOOS == "windows" {
				_ = exec.Command("explorer.exe", sharedDir).Start()
			} else {
				_ = OpenBrowser(sharedDir)
			}
			fmt.Printf("[✓] Opened shared folder: %s\n", sharedDir)
			time.Sleep(1 * time.Second)
		case "0", "exit", "q":
			return nil
		default:
			fmt.Printf("\n[!] Invalid option '%s'. Please enter 0-6.\n", choice)
			time.Sleep(1500 * time.Millisecond)
		}
	}
}

func queryHealth(port int) server.HealthResponse {
	client := http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health?probe=sync", port)
	resp, err := client.Get(url)
	if err != nil {
		return server.HealthResponse{
			Status: "[SERVICE STOPPED] / [OFFLINE]",
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return server.HealthResponse{
			Status: fmt.Sprintf("[SERVICE STOPPED] / [OFFLINE] (HTTP %d)", resp.StatusCode),
		}
	}

	var hr server.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil || strings.TrimSpace(hr.Status) == "" {
		return server.HealthResponse{
			Status: "[SERVICE STOPPED] / [OFFLINE]",
		}
	}
	hr.Status = "[RUNNING] (" + strings.ToUpper(hr.Status) + ")"
	return hr
}

func removePairedDevice(cfg *config.Config, reader *bufio.Reader, localIP string) {
	peers := cfg.GetPeers()
	if len(peers) == 0 {
		fmt.Println("\n[*] No paired devices to remove.")
		time.Sleep(1200 * time.Millisecond)
		return
	}

	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                      REMOVE A PAIRED DEVICE                          ")
	fmt.Println("======================================================================")
	for i, p := range peers {
		fmt.Printf("  [%d] %-15s (IP: %s, Port: %d)\n", i+1, p.Name, p.IP, p.SpoolPort)
	}
	fmt.Print("Enter device number to remove (or 0/Enter to cancel): ")
	ans, _ := reader.ReadString('\n')
	ans = strings.TrimSpace(ans)
	if ans == "" || ans == "0" || isNegativeReply(ans) {
		return
	}

	var idx int
	if _, err := fmt.Sscanf(ans, "%d", &idx); err != nil || idx < 1 || idx > len(peers) {
		fmt.Printf("[!] Invalid selection %q.\n", ans)
		time.Sleep(1200 * time.Millisecond)
		return
	}

	targetPeer := peers[idx-1]
	fmt.Printf("Remove '%s' (%s) and its virtual printer? [y/N]: ", targetPeer.Name, targetPeer.IP)
	conf, _ := reader.ReadString('\n')
	if strings.TrimSpace(strings.ToLower(conf)) != "y" {
		return
	}

	// 1. Best-effort notify remote peer to remove THIS device via DELETE /api/peers
	selfPayload := buildSelfDeregisterPayload(cfg, localIP)
	if err := deregisterFromPeer(targetPeer, selfPayload, cfg.AuthToken, nil); err != nil {
		fmt.Printf("[!] Note: could not notify remote peer '%s' (%v); continuing local removal.\n", targetPeer.Name, err)
	} else {
		fmt.Printf("[✓] Notified '%s' to remove this device.\n", targetPeer.Name)
	}

	// 2. Notify local running service via DELETE /api/peers so spooler/sync/tray update immediately
	listenPort := cfg.ListenPort
	if listenPort <= 0 {
		listenPort = 8765
	}
	peerPayload, _ := json.Marshal(struct {
		URL  string `json:"url,omitempty"`
		IP   string `json:"ip,omitempty"`
		Name string `json:"name,omitempty"`
	}{
		URL:  targetPeer.URL,
		IP:   targetPeer.IP,
		Name: targetPeer.Name,
	})
	client := http.Client{Timeout: 2500 * time.Millisecond}
	if req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("http://127.0.0.1:%d/api/peers", listenPort), bytes.NewReader(peerPayload)); err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Bridge-Token", cfg.AuthToken)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}

	// 3. Remove from config and persist
	targetKey := targetPeer.URL
	if targetKey == "" {
		targetKey = targetPeer.IP
	}
	if targetKey == "" {
		targetKey = targetPeer.Name
	}
	cfg.RemovePeer(targetKey)

	// 4. Remove SendTo shortcut and Windows virtual printer/port
	cleanPeer := SanitizeFileName(targetPeer.Name)
	sendToName := fmt.Sprintf("Print on %s.lnk", cleanPeer)
	for _, st := range collectSendToDirs() {
		_ = os.Remove(filepath.Join(st, sendToName))
	}
	if !IsElevated() {
		fmt.Printf("[!] Note: Not running as Administrator; run 'bridge.exe -sync-printers' elevated to remove 'Print on %s' from Windows Printers.\n", cleanPeer)
	} else if err := RemoveVirtualPrinter(cleanPeer); err != nil {
		fmt.Printf("[!] Warning removing virtual printer 'Print on %s': %v\n", cleanPeer, err)
	} else {
		fmt.Printf("[✓] Removed Windows Virtual Printer 'Print on %s'.\n", cleanPeer)
	}

	fmt.Printf("[✓] Device '%s' removed.\n", targetPeer.Name)
	time.Sleep(1500 * time.Millisecond)
}

// IntroduceToRemote introduces this node to a remote peer via /api/peers.
func IntroduceToRemote(targetURL, myName, myIP, token string) {
	if targetURL == "" || myIP == "" {
		return
	}
	cleanTarget := strings.TrimRight(strings.TrimSpace(targetURL), "/")
	dev := config.PeerDevice{
		Name: myName,
		IP:   myIP,
		URL:  fmt.Sprintf("http://%s:8765", myIP),
	}
	data, _ := json.Marshal(dev)
	client := http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/peers", cleanTarget), bytes.NewReader(data))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Bridge-Token", token)
		req.Header.Set("X-Bridge-Node-Name", myName)
		req.Header.Set("X-Bridge-Peer-URL", fmt.Sprintf("http://%s:8765", myIP))
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
}

type discoveryCandidate struct {
	name         string
	ip           string
	url          string
	hostName     string
	osName       string
	printerReady bool
	printerName  string
	meshPeers    []config.PeerDevice
}

func collectUnlinkedDiscoveryCandidates(cfg *config.Config, localIP string, includeIgnored bool) []discoveryCandidate {
	tsPeers, _ := discoverPeersFunc()
	if len(tsPeers) == 0 {
		return nil
	}

	var candidates []discoveryCandidate
	var wg sync.WaitGroup
	var mu sync.Mutex
	seenIPs := make(map[string]bool)
	seenNames := make(map[string]bool)

	for _, p := range tsPeers {
		if !p.BridgeActive || !p.Online {
			continue
		}
		if localIP != "" && p.IP == localIP {
			continue
		}
		peerURL := fmt.Sprintf("http://%s:8765", p.IP)
		if cfg.FindPeer(p.IP) != nil || cfg.FindPeer(peerURL) != nil {
			continue
		}
		if !includeIgnored && cfg.IsPeerIgnored(p.BridgeName, p.IP, peerURL) {
			continue
		}

		wg.Add(1)
		go func(dp DiscoveredPeer, pURL string) {
			defer wg.Done()
			client := http.Client{Timeout: 4 * time.Second}
			req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", pURL), nil)
			if err != nil {
				return
			}
			req.Header.Set("X-Bridge-Token", cfg.AuthToken)
			req.Header.Set("X-Bridge-Node-Name", cfg.NodeName)
			req.Header.Set("X-Bridge-Probe", "1")
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}

			var hr struct {
				Status         string              `json:"status"`
				NodeName       string              `json:"node_name"`
				PrinterReady   bool                `json:"printer_ready"`
				DefaultPrinter string              `json:"default_printer"`
				Peers          []config.PeerDevice `json:"peers"`
			}
			if json.NewDecoder(resp.Body).Decode(&hr) != nil ||
				!strings.EqualFold(strings.TrimSpace(hr.Status), "healthy") ||
				strings.TrimSpace(hr.NodeName) == "" {
				return
			}

			cleanName := strings.TrimSpace(hr.NodeName)
			normName := strings.ToLower(cleanName)
			normIP := strings.TrimSpace(dp.IP)

			if config.IsSelfTarget(cleanName, dp.IP, pURL, cfg.NodeName, cfg.ListenPort) {
				return
			}
			if !includeIgnored && cfg.IsPeerIgnored(cleanName, dp.IP, pURL) {
				return
			}

			mu.Lock()
			if !seenIPs[normIP] && !seenNames[normName] {
				seenIPs[normIP] = true
				seenNames[normName] = true
				candidates = append(candidates, discoveryCandidate{
					name:         cleanName,
					ip:           dp.IP,
					url:          pURL,
					hostName:     dp.HostName,
					osName:       dp.OS,
					printerReady: hr.PrinterReady,
					printerName:  strings.TrimSpace(hr.DefaultPrinter),
					meshPeers:    hr.Peers,
				})
			}
			mu.Unlock()
		}(p, peerURL)
	}
	wg.Wait()

	return candidates
}

// RunUpgradeWithDiscovery upgrades the installed Bridge service to the current executable,
// verifies health, and prompts to connect any newly discovered Bridge devices or repair missing virtual printers.
func RunUpgradeWithDiscovery(cfg *config.Config, reader *bufio.Reader) error {
	exe, err := os.Executable()
	if err != nil || strings.TrimSpace(exe) == "" {
		exe = TargetExe
	}
	return runUpgradeWithDiscoverySource(exe, cfg, reader)
}

func runUpgradeWithDiscoverySource(sourceExe string, cfg *config.Config, reader *bufio.Reader) error {
	if err := UpgradeInstallation(sourceExe); err != nil {
		return err
	}
	if loaded, err := config.LoadConfig(TargetConfig); err == nil && loaded != nil {
		cfg = loaded
	}
	return PromptNewDiscoveries(cfg, reader)
}

// PromptNewDiscoveries scans for unlinked active Bridge devices on Tailscale (excluding IgnoredPeers)
// and verifies local physical / virtual printer health, offering interactive pairing.
func PromptNewDiscoveries(cfg *config.Config, reader *bufio.Reader) error {
	if cfg == nil {
		loaded, err := config.LoadConfig(TargetConfig)
		if err != nil {
			return err
		}
		cfg = loaded
	}
	if reader == nil {
		reader = bufio.NewReader(os.Stdin)
	}

	// 1. Check local physical printer & existing peers' native virtual printers
	reportLocalPrinterStatus(os.Stdout)

	existingPeers := cfg.GetPeers()
	if len(existingPeers) > 0 {
		vpStatus := CheckVirtualPrinters(existingPeers)
		var missing []string
		for _, p := range existingPeers {
			if !vpStatus[p.Name] {
				missing = append(missing, p.Name)
			}
		}
		if len(missing) > 0 {
			fmt.Printf("[*] Repairing missing virtual printer(s) for: %s\n", strings.Join(missing, ", "))
			res := SyncBridgeShortcutsForPeers(existingPeers)
			for _, name := range res.SucceededPrinters {
				fmt.Printf("[✓] Windows Virtual Printer ready: 'Print on %s'\n", name)
			}
			for name, pErr := range res.PrinterErrors {
				fmt.Printf("[!] Windows Virtual Printer 'Print on %s' failed: %v\n", name, pErr)
			}
		}
	}

	// 2. Scan Tailscale for newly discovered unlinked active Bridge devices (excluding IgnoredPeers)
	localIP := GetTailscaleIP()
	candidates := collectUnlinkedDiscoveryCandidates(cfg, localIP, false)
	if len(candidates) == 0 {
		return nil
	}

	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("               NEW BRIDGE DEVICES DISCOVERED ON NETWORK               ")
	fmt.Println("======================================================================")
	for i, c := range candidates {
		fmt.Printf("  [%d] %-15s (IP: %s, Host: %s) %s\n", i+1, c.name, c.ip, c.hostName, formatDiscoveredPrinterTag(c.printerReady))
	}
	fmt.Println("======================================================================")
	fmt.Print("Connect to discovered device(s)? [Y = Connect all / 1-N = Pick / S = Skip this time / I = Ignore permanently]: ")

	ans, _ := reader.ReadString('\n')
	ans = strings.TrimSpace(ans)
	low := strings.ToLower(ans)

	connectCandidate := func(c discoveryCandidate) {
		confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, c.ip, c.url, c.name, cfg.NodeName)
		connectDetectedPeer(cfg, confirmedName, isCustom, c.ip, c.url, c.meshPeers, localIP)
		notifyRunningServiceOfPeer(cfg)
		if IsVirtualPrinterRegistered(confirmedName) {
			fmt.Printf("[✓] Verified native Windows printer: 'Print on %s'\n", SanitizeFileName(confirmedName))
		}
	}

	switch {
	case low == "i" || low == "ignore":
		for _, c := range candidates {
			if err := cfg.AddIgnoredPeer(c.name, c.ip, c.url); err != nil {
				fmt.Printf("[!] Failed to ignore %s: %v\n", c.name, err)
			} else {
				fmt.Printf("[✓] Permanently ignored '%s' (%s). You can re-enable it from [1] Connect / Pair a New Device.\n", c.name, c.ip)
			}
		}
	case low == "s" || low == "skip" || low == "n" || low == "no":
		fmt.Println("[*] Skipped connecting new devices for now.")
	case ans == "" || low == "y" || low == "yes":
		for _, c := range candidates {
			confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, c.ip, c.url, c.name, cfg.NodeName)
			connectDetectedPeer(cfg, confirmedName, isCustom, c.ip, c.url, c.meshPeers, localIP)
		}
		notifyRunningServiceOfPeer(cfg)
		for _, p := range cfg.GetPeers() {
			if IsVirtualPrinterRegistered(p.Name) {
				fmt.Printf("[✓] Verified native Windows printer: 'Print on %s'\n", SanitizeFileName(p.Name))
			}
		}
	default:
		var idx int
		if _, err := fmt.Sscanf(ans, "%d", &idx); err == nil && idx >= 1 && idx <= len(candidates) {
			connectCandidate(candidates[idx-1])
		} else {
			fmt.Printf("[!] Unrecognized choice %q; skipping.\n", ans)
		}
	}

	return nil
}

// RunPairNewDevice runs the interactive pairing flow for an already-loaded config (used by -pair).
func RunPairNewDevice(cfg *config.Config) {
	if cfg == nil {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	pairNewDevice(cfg, reader, GetTailscaleIP())
}

func pairNewDevice(cfg *config.Config, reader *bufio.Reader, localIP string) {
	// Detach ConfigPath while staging new peers so we sync authoritative ports from
	// the running service before persisting config.json or creating virtual printers.
	origConfigPath := cfg.ConfigPath
	cfg.ConfigPath = ""
	defer func() {
		if cfg.ConfigPath == "" {
			cfg.ConfigPath = origConfigPath
		}
	}()

	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                      CONNECT / PAIR A NEW DEVICE                     ")
	fmt.Println("======================================================================")
	fmt.Print("[*] Scanning your Tailscale network for active Bridge devices... ")

	newPeers := collectUnlinkedDiscoveryCandidates(cfg, localIP, false)
	fmt.Printf("Done! (Found %d)\n", len(newPeers))

	ignoredList := cfg.GetIgnoredPeers()
	if len(ignoredList) > 0 {
		fmt.Printf("[*] Note: %d previously ignored device(s) hidden (", len(ignoredList))
		var igNames []string
		for _, ig := range ignoredList {
			label := ig.Name
			if label == "" {
				label = ig.IP
			}
			igNames = append(igNames, label)
		}
		fmt.Printf("%s). Enter 'U' to un-ignore & connect one.\n", strings.Join(igNames, ", "))
	}

	pairedAny := false
	handleUnignore := func() bool {
		if len(ignoredList) == 0 {
			return false
		}
		fmt.Println("\nPreviously ignored devices:")
		for i, ig := range ignoredList {
			fmt.Printf("  [%d] %-15s IP: %-15s URL: %s\n", i+1, ig.Name, ig.IP, ig.URL)
		}
		fmt.Print("Enter number to un-ignore and connect (or 0/Enter to cancel): ")
		uAns, _ := reader.ReadString('\n')
		uAns = strings.TrimSpace(uAns)
		var uIdx int
		if _, err := fmt.Sscanf(uAns, "%d", &uIdx); err == nil && uIdx >= 1 && uIdx <= len(ignoredList) {
			ig := ignoredList[uIdx-1]
			target := ig.URL
			if target == "" {
				target = ig.IP
			}
			if target == "" {
				target = ig.Name
			}
			// Temporarily restore ConfigPath so RemoveIgnoredPeer persists
			cfg.ConfigPath = origConfigPath
			_ = cfg.RemoveIgnoredPeer(target)
			cfg.ConfigPath = ""
			if ig.IP != "" {
				return connectManualPeer(cfg, reader, ig.IP, localIP)
			}
			if ig.URL != "" {
				return connectManualPeer(cfg, reader, ig.URL, localIP)
			}
			if ig.Name != "" {
				return connectManualPeer(cfg, reader, ig.Name, localIP)
			}
		}
		return false
	}

	if len(newPeers) == 1 {
		p := newPeers[0]
		fmt.Println()
		fmt.Printf("[*] Current active device found: '%s' (IP: %s) %s\n", p.name, p.ip, formatDiscoveredPrinterTag(p.printerReady))
		fmt.Printf("Connect to '%s' (%s)? [Y = Connect, N = Skip, U = Un-ignore, or enter another IP/name]: ", p.name, p.ip)
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if strings.EqualFold(ans, "u") && len(ignoredList) > 0 {
			pairedAny = handleUnignore()
		} else if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, p.ip, p.url, p.name, cfg.NodeName)
			connectDetectedPeer(cfg, confirmedName, isCustom, p.ip, p.url, p.meshPeers, localIP)
			pairedAny = true
		} else if !isNegativeReply(ans) {
			pairedAny = connectManualPeer(cfg, reader, ans, localIP)
		}
	} else if len(newPeers) > 1 {
		fmt.Println()
		fmt.Println("Active Bridge devices found on your network:")
		for i, p := range newPeers {
			fmt.Printf("  [%d] %-15s (IP: %s) %s\n", i+1, p.name, p.ip, formatDiscoveredPrinterTag(p.printerReady))
		}
		fmt.Print("\nConnect to these devices? [Y = Connect all, 1-N = Pick one, N = Skip, U = Un-ignore, or enter IP/name]: ")
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if strings.EqualFold(ans, "u") && len(ignoredList) > 0 {
			pairedAny = handleUnignore()
		} else if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			linkedMap := make(map[string]bool)
			for _, p := range newPeers {
				normIP := strings.TrimSpace(p.ip)
				normName := strings.ToLower(strings.TrimSpace(p.name))
				if linkedMap[normIP] || linkedMap[normName] {
					continue
				}
				linkedMap[normIP] = true
				linkedMap[normName] = true
				confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, p.ip, p.url, p.name, cfg.NodeName)
				connectDetectedPeer(cfg, confirmedName, isCustom, p.ip, p.url, p.meshPeers, localIP)
				pairedAny = true
			}
		} else if !isNegativeReply(ans) {
			idx := 0
			if _, scanErr := fmt.Sscanf(ans, "%d", &idx); scanErr == nil && idx >= 1 && idx <= len(newPeers) {
				sel := newPeers[idx-1]
				confirmedName, isCustom := confirmPeerNameWithConfig(reader, cfg, sel.ip, sel.url, sel.name, cfg.NodeName)
				connectDetectedPeer(cfg, confirmedName, isCustom, sel.ip, sel.url, sel.meshPeers, localIP)
				pairedAny = true
			} else {
				pairedAny = connectManualPeer(cfg, reader, ans, localIP)
			}
		}
	} else {
		fmt.Println()
		fmt.Println("[*] No new unlinked devices automatically detected on Tailscale.")
		if len(ignoredList) > 0 {
			fmt.Print("Enter IP address / device name, 'U' to un-ignore a device, or press Enter to cancel: ")
		} else {
			fmt.Print("Enter IP address or device name of peer to connect (or press Enter to cancel): ")
		}
		inputIP, _ := reader.ReadString('\n')
		inputIP = strings.TrimSpace(inputIP)
		if strings.EqualFold(inputIP, "u") && len(ignoredList) > 0 {
			pairedAny = handleUnignore()
		} else if inputIP != "" && !isNegativeReply(inputIP) {
			pairedAny = connectManualPeer(cfg, reader, inputIP, localIP)
		}
	}

	if pairedAny {
		if origConfigPath != "" {
			cfg.ConfigPath = origConfigPath
		}
		notifyRunningServiceOfPeer(cfg)
		time.Sleep(1500 * time.Millisecond)
	}
}

func syncPeersWithRunningService(cfg *config.Config) {
	listenPort := cfg.ListenPort
	if listenPort <= 0 {
		listenPort = 8765
	}
	savePath := cfg.ConfigPath
	if savePath == "" {
		savePath = TargetConfig
	}

	client := http.Client{Timeout: 3 * time.Second}
	localURL := fmt.Sprintf("http://127.0.0.1:%d/api/peers", listenPort)

	localPeers := cfg.GetPeers()
	localCustomByURL := make(map[string]config.PeerDevice)
	for _, p := range localPeers {
		if p.CustomName {
			key := strings.ToLower(strings.TrimRight(p.URL, "/"))
			if key != "" {
				localCustomByURL[key] = p
			}
		}
	}

	serviceReachable := false
	syncedByURL := make(map[string]config.PeerDevice)
	for _, p := range localPeers {
		devJSON, _ := json.Marshal(p)
		if req, err := http.NewRequest("POST", localURL, bytes.NewReader(devJSON)); err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Bridge-Token", cfg.AuthToken)
			if resp, err := client.Do(req); err == nil {
				if resp.StatusCode == http.StatusOK {
					serviceReachable = true
					var authDev config.PeerDevice
					if json.NewDecoder(resp.Body).Decode(&authDev) == nil && authDev.SpoolPort > 0 {
						syncedByURL[strings.ToLower(strings.TrimRight(authDev.URL, "/"))] = authDev
					}
				}
				resp.Body.Close()
			}
		}
	}

	if serviceReachable {
		var authPeers []config.PeerDevice
		if req, err := http.NewRequest("GET", localURL, nil); err == nil {
			req.Header.Set("X-Bridge-Token", cfg.AuthToken)
			if resp, err := client.Do(req); err == nil {
				if resp.StatusCode == http.StatusOK {
					var listResp struct {
						Peers []config.PeerDevice `json:"peers"`
					}
					if json.NewDecoder(resp.Body).Decode(&listResp) == nil && len(listResp.Peers) > 0 {
						authPeers = listResp.Peers
					}
				}
				resp.Body.Close()
			}
		}

		if len(authPeers) == 0 && len(syncedByURL) > 0 {
			cur := cfg.GetPeers()
			for i := range cur {
				key := strings.ToLower(strings.TrimRight(cur[i].URL, "/"))
				if ad, ok := syncedByURL[key]; ok {
					cur[i].SpoolPort = ad.SpoolPort
					if ad.Name != "" {
						cur[i].Name = ad.Name
					}
					cur[i].CustomName = cur[i].CustomName || ad.CustomName
				}
			}
			authPeers = cur
		}

		if len(authPeers) > 0 {
			for i := range authPeers {
				key := strings.ToLower(strings.TrimRight(authPeers[i].URL, "/"))
				if prev, ok := localCustomByURL[key]; ok && !authPeers[i].CustomName && strings.EqualFold(authPeers[i].Name, prev.Name) {
					authPeers[i].CustomName = true
				}
			}
			cfg.ConfigPath = ""
			_ = cfg.SetPeers(authPeers)
		}
	}

	cfg.ConfigPath = savePath
	_ = config.SaveConfig(savePath, cfg)
}

func notifyRunningServiceOfPeer(cfg *config.Config) {
	syncPeersWithRunningService(cfg)

	res := SyncBridgeShortcutsForPeers(cfg.GetPeers())
	for _, name := range res.SucceededPrinters {
		fmt.Printf("[✓] Windows Virtual Printer created: 'Print on %s'\n", name)
	}
	for name, pErr := range res.PrinterErrors {
		fmt.Printf("[!] Windows Virtual Printer 'Print on %s' failed: %v\n", name, pErr)
	}
}

// SuggestNextDeviceName determines an intuitive unit name (e.g. U1, U2, U3) based on existing mesh peers.
func SuggestNextDeviceName(peerNames []string, currentName string) string {
	hasName := func(name string) bool {
		for _, p := range peerNames {
			if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(name)) {
				return true
			}
		}
		return false
	}
	// If current name is already valid and NOT in use by another device on the network, keep it
	if currentName != "" && currentName != "Bridge-PC" && !strings.HasPrefix(currentName, "Desktop ") && !strings.HasPrefix(currentName, "DESKTOP-") && !hasName(currentName) {
		return currentName
	}
	for i := 1; i <= 50; i++ {
		candidate := fmt.Sprintf("U%d", i)
		if !hasName(candidate) {
			return candidate
		}
	}
	return "U1"
}
