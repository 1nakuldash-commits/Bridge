package installer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		_ = printer.CreateSumatraShortcut(sumatra)
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
		name      string
		ip        string
		url       string
		meshPeers []config.PeerDevice
	}
	var detectedPeers []activePeerInfo
	var detectedNames []string

	tsPeers, err := DiscoverTailscalePeers()
	if err == nil && len(tsPeers) > 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		seenIPs := make(map[string]bool)
		seenNames := make(map[string]bool)

		for _, p := range tsPeers {
			if p.IP == localIP {
				continue
			}
			wg.Add(1)
			go func(peer DiscoveredPeer) {
				defer wg.Done()
				peerURL := fmt.Sprintf("http://%s:8765", peer.IP)
				// Probe peer's /api/health with probe header (strictly read-only, does not trigger auto-pair)
				client := http.Client{Timeout: 2 * time.Second}
				req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", peerURL), nil)
				if err == nil {
					req.Header.Set("X-Bridge-Token", cfg.AuthToken)
					req.Header.Set("X-Bridge-Probe", "1")
					resp, err := client.Do(req)
					if err == nil && resp.StatusCode == http.StatusOK {
						var hr struct {
							NodeName string              `json:"node_name"`
							Peers    []config.PeerDevice `json:"peers"`
						}
						nodeName := peer.BridgeName
						var meshPeers []config.PeerDevice
						if json.NewDecoder(resp.Body).Decode(&hr) == nil && hr.NodeName != "" {
							nodeName = hr.NodeName
							meshPeers = hr.Peers
						}
						resp.Body.Close()

						if nodeName == "" {
							nodeName = peer.HostName
						}
						if nodeName == "" {
							nodeName = fmt.Sprintf("Device (%s)", peer.IP)
						}

						cleanName := strings.TrimSpace(nodeName)
						normName := strings.ToLower(cleanName)
						normIP := strings.TrimSpace(peer.IP)

						mu.Lock()
						if !seenIPs[normIP] && !seenNames[normName] && !config.IsSelfTarget(cleanName, peer.IP, peerURL, cfg.NodeName, cfg.ListenPort) {
							seenIPs[normIP] = true
							seenNames[normName] = true
							detectedPeers = append(detectedPeers, activePeerInfo{
								name:      cleanName,
								ip:        peer.IP,
								url:       peerURL,
								meshPeers: meshPeers,
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

	// 5. Connect and pair devices with clear confirmation
	if len(detectedPeers) == 0 {
		// Zero peers: This is Device 1 (first device on the network)
		fmt.Println("[*] No other active Bridge devices detected on your network.")
		fmt.Printf("[✓] '%s' will act as your first network device.\n", cfg.NodeName)
		fmt.Println()
		fmt.Println("When setting up your next device (e.g. U2), start Bridge.")
		fmt.Printf("It will automatically detect this device ('%s' - %s) and prompt to link them!\n", cfg.NodeName, localIP)
		fmt.Println()
		fmt.Println("Enter peer IP address if connecting to a specific IP,")
		fmt.Print("or press Enter if this is Device 1 (or setting up other devices later): ")
		manualInput, _ := reader.ReadString('\n')
		manualInput = strings.TrimSpace(manualInput)
		if manualInput != "" {
			connectManualPeer(cfg, manualInput, localIP)
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
		fmt.Printf("[*] Current active device found: '%s' (IP: %s)\n", p.name, p.ip)
		fmt.Printf("Connect to '%s' (%s)? [Y = Connect, or enter another IP]: ", p.name, p.ip)
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			connectDetectedPeer(cfg, p.name, p.ip, p.url, p.meshPeers, localIP)
		} else {
			cleanManual := config.ExtractIP(ans)
			if cleanManual != "" {
				connectManualPeer(cfg, cleanManual, localIP)
			} else {
				fmt.Print("Enter peer IP address to connect manually, or press Enter to skip: ")
				manual, _ := reader.ReadString('\n')
				manual = strings.TrimSpace(manual)
				if manual != "" {
					connectManualPeer(cfg, manual, localIP)
				}
			}
		}
	} else {
		// Multiple active devices found (e.g. setting up Device 3, found U1 and U2)
		fmt.Println()
		fmt.Println("======================================================================")
		fmt.Println("                  REGISTERED DEVICES ON YOUR NETWORK                  ")
		fmt.Println("======================================================================")
		fmt.Println("Active Bridge devices already registered:")
		for i, p := range detectedPeers {
			fmt.Printf("    [%d] %-15s (IP: %s)\n", i+1, p.name, p.ip)
		}
		fmt.Println("======================================================================")
		fmt.Print("Connect to these devices? [Y = Connect all, or enter another IP]: ")
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
				connectDetectedPeer(cfg, p.name, p.ip, p.url, p.meshPeers, localIP)
			}
		} else {
			idx := 0
			if _, scanErr := fmt.Sscanf(ans, "%d", &idx); scanErr == nil && idx >= 1 && idx <= len(detectedPeers) {
				sel := detectedPeers[idx-1]
				connectDetectedPeer(cfg, sel.name, sel.ip, sel.url, sel.meshPeers, localIP)
			} else if ans != "n" && ans != "N" {
				connectManualPeer(cfg, ans, localIP)
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

	if err := InstallService(cfg); err != nil {
		fmt.Printf("\n[!] Setup warning: %v\n", err)
	}

	fmt.Println("[*] Launching Bridge background service in Windows Taskbar tray...")
	if err := RestartService(); err != nil {
		fmt.Printf("[!] Note starting background service: %v\n", err)
	}

	printSetupSuccess(cfg, localIP, reader)
	return nil
}

func connectDetectedPeer(cfg *config.Config, name, ip, targetURL string, meshPeers []config.PeerDevice, localIP string) {
	cleanName := strings.TrimSpace(name)
	if config.IsSelfTarget(cleanName, ip, targetURL, cfg.NodeName, cfg.ListenPort) {
		return // Strictly ignore self device
	}
	if localIP != "" && ip == localIP {
		return // Strictly ignore self IP
	}

	addedDev, isNew := cfg.AddOrUpdatePeer(cleanName, ip, targetURL)
	if isNew {
		fmt.Printf("[*] Linking with '%s' (%s)...\n", cleanName, ip)
		fmt.Printf("[✓] Linked with '%s'! Virtual Printer prepared: 'Print on %s' (Port %d)\n", addedDev.Name, addedDev.Name, addedDev.SpoolPort)
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

func connectManualPeer(cfg *config.Config, inputIP, localIP string) {
	cleanIP := config.ExtractIP(inputIP)
	if cleanIP == "" {
		return
	}
	targetURL := fmt.Sprintf("http://%s:8765", cleanIP)
	fmt.Printf("[*] Connecting to peer at %s...\n", cleanIP)

	client := http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", targetURL), nil)
	if err == nil {
		req.Header.Set("X-Bridge-Token", cfg.AuthToken)
		req.Header.Set("X-Bridge-Node-Name", cfg.NodeName)
		if localIP != "" {
			req.Header.Set("X-Bridge-Peer-URL", fmt.Sprintf("http://%s:8765", localIP))
		}
		req.Header.Set("X-Bridge-Probe", "1")

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var hr struct {
				NodeName string              `json:"node_name"`
				Peers    []config.PeerDevice `json:"peers"`
			}
			if json.NewDecoder(resp.Body).Decode(&hr) == nil && hr.NodeName != "" {
				connectDetectedPeer(cfg, hr.NodeName, cleanIP, targetURL, hr.Peers, localIP)
				resp.Body.Close()
				return
			}
			resp.Body.Close()
		}
	}

	placeholderName := fmt.Sprintf("Device (%s)", cleanIP)
	addedDev, _ := cfg.AddOrUpdatePeer(placeholderName, cleanIP, targetURL)
	fmt.Printf("[*] Peer at %s added. Bridge will auto-pair as soon as it goes online.\n", cleanIP)
	fmt.Printf("[✓] Virtual Printer registered: 'Print on %s' (Port %d)\n", addedDev.Name, addedDev.SpoolPort)
	if localIP != "" {
		IntroduceToRemote(targetURL, cfg.NodeName, localIP, cfg.AuthToken)
	}
}

func printSetupSuccess(cfg *config.Config, localIP string, reader *bufio.Reader) {
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                   SETUP COMPLETED SUCCESSFULLY!                      ")
	fmt.Println("======================================================================")
	fmt.Println("[✓] Bridge multi-device service is active in your user session.")
	fmt.Println("[✓] Multifunctional Status Dot is active in your Windows Taskbar!")
	fmt.Println("[✓] Global Shared Folder created on Desktop: 'Bridge Shared Folder' (C:\\Bridge\\Shared)")

	peers := cfg.GetPeers()
	if len(peers) > 0 {
		for _, p := range peers {
			fmt.Printf("[✓] Windows Virtual Printer created: 'Print on %s' (Select in Ctrl+P!)\n", p.Name)
			fmt.Printf("[✓] Right-Click Send-To added: 'Print on %s'\n", p.Name)
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

// RunDashboard displays status and actions when bridge.exe is launched interactively.
func RunDashboard() error {
	reader := bufio.NewReader(os.Stdin)

	for {
		cfg, err := config.LoadConfig(TargetConfig)
		if err != nil {
			return RunInteractiveSetup()
		}

		health := queryHealth(cfg.ListenPort)

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

		targetPrinter, _ := printer.GetTargetPhysicalPrinter()
		if targetPrinter != "" {
			fmt.Printf(" Local Printer:    %s (Physical Ready)\n", targetPrinter)
		} else {
			fmt.Println(" Local Printer:    (No physical printer connected)")
		}

		peers := cfg.GetPeers()
		// Build lookup for live peer online status reported by running server
		peerOnlineMap := make(map[string]bool)
		for _, hp := range health.Peers {
			if hp.Online {
				if hp.Name != "" {
					peerOnlineMap[strings.ToLower(strings.TrimSpace(hp.Name))] = true
				}
				if hp.IP != "" {
					peerOnlineMap[strings.TrimSpace(hp.IP)] = true
				}
				if hp.URL != "" {
					peerOnlineMap[strings.TrimSpace(hp.URL)] = true
				}
			}
		}

		onlineCount := 0
		for i := range peers {
			p := &peers[i]
			isOnline := p.Online ||
				peerOnlineMap[strings.ToLower(strings.TrimSpace(p.Name))] ||
				peerOnlineMap[strings.TrimSpace(p.IP)] ||
				(p.URL != "" && peerOnlineMap[strings.TrimSpace(p.URL)])
			if isOnline {
				p.Online = true
				onlineCount++
			}
		}
		if health.OnlineCount > onlineCount {
			onlineCount = health.OnlineCount
		}

		fmt.Printf(" Connected Peers:  %d total (%d online)\n", len(peers), onlineCount)
		if len(peers) > 0 {
			for i, p := range peers {
				statusStr := "⚪ Offline"
				if p.Online {
					statusStr = "🟢 Online"
				}
				fmt.Printf("   [%d] %-15s IP: %-15s Port: %-5d Status: %s\n", i+1, p.Name, p.IP, p.SpoolPort, statusStr)
			}
		} else {
			fmt.Println("   (No peers connected yet - waiting for other devices)")
		}

		fmt.Printf(" Active Print Jobs: %d\n", health.PrintQueueDepth)
		if health.UptimeSeconds > 0 {
			uptimeDur := time.Duration(health.UptimeSeconds) * time.Second
			fmt.Printf(" Service Uptime:   %s\n", uptimeDur)
		}
		currentExe, _ := os.Executable()
		isExternal := currentExe != "" && !strings.EqualFold(filepath.Clean(currentExe), filepath.Clean(TargetExe))
		if isExternal {
			fmt.Println("----------------------------------------------------------------------")
			fmt.Printf(" [!] Executable Location: %s\n", currentExe)
			fmt.Printf("     Installed Location:  %s\n", TargetExe)
		}

		fmt.Println("----------------------------------------------------------------------")
		fmt.Println(" Quick Actions:")
		fmt.Println("   [1] Connect / Pair a New Device")
		if strings.Contains(strings.ToUpper(health.Status), "STOPPED") || strings.Contains(strings.ToUpper(health.Status), "OFFLINE") {
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
		fmt.Println("   [0] Exit Dashboard")
		fmt.Println("======================================================================")
		fmt.Print("Select an option [0-4]: ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			pairNewDevice(cfg, reader, localIP)
		case "2", "5":
			if strings.Contains(strings.ToUpper(health.Status), "STOPPED") || strings.Contains(strings.ToUpper(health.Status), "OFFLINE") {
				fmt.Println("[*] Starting Bridge background service...")
				cmd := exec.Command(TargetExe, "-autostart")
				hideCmd(cmd)
				if err := cmd.Start(); err != nil {
					fmt.Printf("[!] Failed to start background service: %v\n", err)
				} else {
					fmt.Println("[✓] Background service started in taskbar tray.")
				}
			} else {
				fmt.Println("[*] Restarting Bridge background service...")
				_ = RestartService()
				fmt.Println("[✓] Background service restarted.")
			}
			time.Sleep(2 * time.Second)
		case "3", "8":
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
					time.Sleep(1 * time.Second)
					_ = SelfElevate([]string{"-upgrade", targetSource})
					return nil
				}
				if err := UpgradeInstallation(targetSource); err != nil {
					fmt.Printf("[!] Upgrade failed: %v\n", err)
				}
				fmt.Println("Press Enter to continue...")
				_, _ = reader.ReadString('\n')
			}
		case "4", "7":
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
					time.Sleep(1 * time.Second)
					return SelfElevate([]string{"-uninstall"})
				}
				err := UninstallService()
				if err == nil {
					time.Sleep(2 * time.Second)
				}
				return err
			}
		case "6":
			return RunInteractiveSetup()
		case "0", "exit", "q":
			return nil
		default:
			fmt.Printf("\n[!] Invalid option '%s'. Please enter 0, 1, 2, 3, or 4.\n", choice)
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
			Status: "STOPPED / OFFLINE",
		}
	}
	defer resp.Body.Close()

	var hr server.HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&hr)
	if hr.Status == "" {
		hr.Status = "RUNNING"
	} else {
		hr.Status = "RUNNING (" + strings.ToUpper(hr.Status) + ")"
	}
	return hr
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

func pairNewDevice(cfg *config.Config, reader *bufio.Reader, localIP string) {
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                      CONNECT / PAIR A NEW DEVICE                     ")
	fmt.Println("======================================================================")
	fmt.Print("[*] Scanning your Tailscale network for active Bridge devices... ")

	tsPeers, _ := DiscoverTailscalePeers()
	type peerCandidate struct {
		name      string
		ip        string
		url       string
		meshPeers []config.PeerDevice
	}
	var newPeers []peerCandidate

	if len(tsPeers) > 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex
		seenCandidateIPs := make(map[string]bool)
		seenCandidateNames := make(map[string]bool)

		for _, p := range tsPeers {
			if p.IP == localIP {
				continue
			}
			if cfg.FindPeer(p.IP) != nil {
				continue
			}

			wg.Add(1)
			go func(dp DiscoveredPeer) {
				defer wg.Done()
				peerURL := fmt.Sprintf("http://%s:8765", dp.IP)
				client := http.Client{Timeout: 2 * time.Second}
				req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", peerURL), nil)
				if err == nil {
					req.Header.Set("X-Bridge-Token", cfg.AuthToken)
					req.Header.Set("X-Bridge-Node-Name", cfg.NodeName)
					req.Header.Set("X-Bridge-Probe", "1")
					resp, err := client.Do(req)
					if err == nil && resp.StatusCode == http.StatusOK {
						var hr struct {
							NodeName string              `json:"node_name"`
							Peers    []config.PeerDevice `json:"peers"`
						}
						nodeName := dp.BridgeName
						var meshPeers []config.PeerDevice
						if json.NewDecoder(resp.Body).Decode(&hr) == nil && hr.NodeName != "" {
							nodeName = hr.NodeName
							meshPeers = hr.Peers
						}
						resp.Body.Close()
						if nodeName == "" {
							nodeName = dp.HostName
						}
						if nodeName == "" {
							nodeName = fmt.Sprintf("Device (%s)", dp.IP)
						}

						cleanName := strings.TrimSpace(nodeName)
						normName := strings.ToLower(cleanName)
						normIP := strings.TrimSpace(dp.IP)

						mu.Lock()
						if !seenCandidateIPs[normIP] && !seenCandidateNames[normName] && !strings.EqualFold(cleanName, cfg.NodeName) {
							seenCandidateIPs[normIP] = true
							seenCandidateNames[normName] = true
							newPeers = append(newPeers, peerCandidate{
								name:      cleanName,
								ip:        dp.IP,
								url:       peerURL,
								meshPeers: meshPeers,
							})
						}
						mu.Unlock()
					}
					if resp != nil {
						resp.Body.Close()
					}
				}
			}(p)
		}
		wg.Wait()
	}

	fmt.Printf("Done! (Found %d)\n", len(newPeers))

	if len(newPeers) == 1 {
		p := newPeers[0]
		fmt.Println()
		fmt.Printf("[*] Current active device found: '%s' (IP: %s)\n", p.name, p.ip)
		fmt.Printf("Connect to '%s' (%s)? [Y = Connect, or enter another IP]: ", p.name, p.ip)
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			connectDetectedPeer(cfg, p.name, p.ip, p.url, p.meshPeers, localIP)
			notifyRunningServiceOfPeer(cfg)
			time.Sleep(1500 * time.Millisecond)
		} else {
			cleanManual := config.ExtractIP(ans)
			if cleanManual != "" {
				connectManualPeer(cfg, cleanManual, localIP)
				notifyRunningServiceOfPeer(cfg)
				time.Sleep(1500 * time.Millisecond)
			} else {
				fmt.Print("Enter peer IP address if connecting to another device, or press Enter to skip: ")
				manual, _ := reader.ReadString('\n')
				manual = strings.TrimSpace(manual)
				if manual != "" {
					connectManualPeer(cfg, manual, localIP)
					notifyRunningServiceOfPeer(cfg)
					time.Sleep(1500 * time.Millisecond)
				}
			}
		}
	} else if len(newPeers) > 1 {
		fmt.Println()
		fmt.Println("Active Bridge devices found on your network:")
		for i, p := range newPeers {
			fmt.Printf("  [%d] %-15s (IP: %s)\n", i+1, p.name, p.ip)
		}
		fmt.Print("\nConnect to these devices? [Y = Connect all, or select 1-N]: ")
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			linkedMap := make(map[string]bool)
			for _, p := range newPeers {
				normIP := strings.TrimSpace(p.ip)
				normName := strings.ToLower(strings.TrimSpace(p.name))
				if linkedMap[normIP] || linkedMap[normName] {
					continue
				}
				linkedMap[normIP] = true
				linkedMap[normName] = true
				connectDetectedPeer(cfg, p.name, p.ip, p.url, p.meshPeers, localIP)
			}
			notifyRunningServiceOfPeer(cfg)
			time.Sleep(1500 * time.Millisecond)
		} else {
			idx := 0
			if _, scanErr := fmt.Sscanf(ans, "%d", &idx); scanErr == nil && idx >= 1 && idx <= len(newPeers) {
				sel := newPeers[idx-1]
				connectDetectedPeer(cfg, sel.name, sel.ip, sel.url, sel.meshPeers, localIP)
				notifyRunningServiceOfPeer(cfg)
				time.Sleep(1500 * time.Millisecond)
			} else if ans != "n" && ans != "N" {
				connectManualPeer(cfg, ans, localIP)
				notifyRunningServiceOfPeer(cfg)
				time.Sleep(1500 * time.Millisecond)
			}
		}
	} else {
		fmt.Println()
		fmt.Println("[*] No new unlinked devices automatically detected on Tailscale.")
		fmt.Print("Enter IP address of peer device to connect (or press Enter to cancel): ")
		inputIP, _ := reader.ReadString('\n')
		inputIP = strings.TrimSpace(inputIP)
		if inputIP != "" {
			connectManualPeer(cfg, inputIP, localIP)
			notifyRunningServiceOfPeer(cfg)
			time.Sleep(1500 * time.Millisecond)
		}
	}
}

func notifyRunningServiceOfPeer(cfg *config.Config) {
	_ = CreateBridgeShortcutsForPeers(cfg.GetPeers())
	_ = config.SaveConfig(TargetConfig, cfg)

	client := http.Client{Timeout: 1 * time.Second}
	localURL := fmt.Sprintf("http://127.0.0.1:%d/api/peers", cfg.ListenPort)
	for _, p := range cfg.GetPeers() {
		devJSON, _ := json.Marshal(p)
		if req, err := http.NewRequest("POST", localURL, bytes.NewReader(devJSON)); err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Bridge-Token", cfg.AuthToken)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
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
