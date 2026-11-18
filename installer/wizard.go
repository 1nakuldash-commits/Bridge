package installer

import (
	"bufio"
	"bytes"
	"context"
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
	"bridge/tray"
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
		fmt.Println("[*] Installing SumatraPDF silently via winget for silent background printing...")
		installedPath, err := InstallSumatraPDF()
		if err != nil {
			fmt.Printf("[!] Warning: %v\n", err)
		}
		sumatra = installedPath
	} else {
		fmt.Printf("Found: %s\n", sumatra)
	}

	cfg := config.DefaultConfig()
	cfg.SumatraPath = sumatra

	// Load existing configuration if present to offer previous name as default
	if existingCfg, err := config.LoadConfig(TargetConfig); err == nil && existingCfg.NodeName != "" {
		cfg.NodeName = existingCfg.NodeName
		cfg.Peers = existingCfg.GetPeers()
		if existingCfg.AuthToken != "" {
			cfg.AuthToken = existingCfg.AuthToken
		}
	}

	// 3. User Experience: Ask for THIS device's name first
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                       DEVICE CONFIGURATION                           ")
	if localIP != "" {
		fmt.Printf(">>> THIS DEVICE'S TAILSCALE IP: %s <<<\n", localIP)
	}
	fmt.Println("======================================================================")

	defaultName := "U1"
	if cfg.NodeName != "" && !strings.HasPrefix(cfg.NodeName, "Desktop ") && cfg.NodeName != "Bridge-PC" {
		defaultName = cfg.NodeName
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

	fmt.Println()
	fmt.Printf("[✓] Configured this device as: '%s'\n", cfg.NodeName)
	if localIP != "" {
		fmt.Printf("[✓] Device IP: %s\n", localIP)
	}

	// 4. Auto-detect active Bridge devices on Tailscale
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("               AUTO-DETECTING ACTIVE NETWORK DEVICES                  ")
	fmt.Println("======================================================================")
	fmt.Print("[*] Scanning your Tailscale network for active Bridge devices... ")

	type activePeerInfo struct {
		name      string
		ip        string
		url       string
		meshPeers []config.PeerDevice
	}
	var detectedPeers []activePeerInfo

	tsPeers, err := DiscoverTailscalePeers()
	if err == nil && len(tsPeers) > 0 {
		var wg sync.WaitGroup
		var mu sync.Mutex

		for _, p := range tsPeers {
			if !p.Online || p.IP == localIP {
				continue
			}
			wg.Add(1)
			go func(peer DiscoveredPeer) {
				defer wg.Done()
				peerURL := fmt.Sprintf("http://%s:8765", peer.IP)
				// Probe peer's /api/health to verify running Bridge node and retrieve friendly name + mesh peer list
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

						mu.Lock()
						detectedPeers = append(detectedPeers, activePeerInfo{
							name:      nodeName,
							ip:        peer.IP,
							url:       peerURL,
							meshPeers: meshPeers,
						})
						mu.Unlock()
						return
					}
					if resp != nil {
						resp.Body.Close()
					}
				}
				// Note: If /api/health probe failed or timed out, the machine is not running Bridge
				// and is deliberately excluded from active Bridge peers.
			}(p)
		}
		wg.Wait()
	}
	fmt.Printf("Done! (Found %d)\n", len(detectedPeers))

	if len(detectedPeers) == 0 {
		// Zero peers: This is Device 1 (first device on the network)
		fmt.Println("[*] No other active Bridge devices detected on your network.")
		fmt.Printf("[✓] '%s' will act as your first network device.\n", cfg.NodeName)
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
				fmt.Println("When setting up your next device (e.g. U2), run Bridge.")
				fmt.Printf("It will automatically detect this active device ('%s') and link them!\n", cfg.NodeName)
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
			connectManualPeer(cfg, ans, localIP)
		}
	} else {
		// Multiple active devices found (e.g. setting up Device 3, found U1 and U2)
		fmt.Println()
		fmt.Println("[*] Current active devices found on your network:")
		for i, p := range detectedPeers {
			fmt.Printf("    [%d] %-15s (IP: %s)\n", i+1, p.name, p.ip)
		}
		fmt.Println()
		fmt.Print("Connect to these devices? [Y = Connect all, 1-N = Connect single, or enter another IP]: ")
		ans, _ := reader.ReadString('\n')
		ans = strings.TrimSpace(ans)
		if ans == "" || strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes") {
			for _, p := range detectedPeers {
				connectDetectedPeer(cfg, p.name, p.ip, p.url, p.meshPeers, localIP)
			}
		} else {
			idx := 0
			if _, scanErr := fmt.Sscanf(ans, "%d", &idx); scanErr == nil && idx >= 1 && idx <= len(detectedPeers) {
				sel := detectedPeers[idx-1]
				connectDetectedPeer(cfg, sel.name, sel.ip, sel.url, sel.meshPeers, localIP)
			} else {
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
	fmt.Printf("[*] Linking with '%s' (%s)...\n", name, ip)
	addedDev, _ := cfg.AddOrUpdatePeer(name, ip, targetURL)
	fmt.Printf("[✓] Linked with '%s'! Virtual Printer prepared: 'Print on %s' (Port %d)\n", addedDev.Name, addedDev.Name, addedDev.SpoolPort)

	// Introduce ourselves to the remote peer
	if localIP != "" {
		IntroduceToRemote(targetURL, cfg.NodeName, localIP, cfg.AuthToken)
	}

	// Inherit and link with any other mesh peers discovered from this device
	for _, mp := range meshPeers {
		if mp.IP != localIP && mp.Name != cfg.NodeName && mp.IP != ip {
			cfg.AddOrUpdatePeer(mp.Name, mp.IP, mp.URL)
			fmt.Printf("[✓] Discovered mesh device: '%s' (%s)\n", mp.Name, mp.IP)
			if mp.URL != "" && localIP != "" {
				IntroduceToRemote(mp.URL, cfg.NodeName, localIP, cfg.AuthToken)
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
		fmt.Printf(" Connected Peers:  %d total (%d online)\n", len(peers), health.OnlineCount)
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
		fmt.Println("   [1] Open Global Shared Folder in File Explorer (C:\\Bridge\\Shared)")
		fmt.Println("   [2] Print a File to a Remote Device...")
		fmt.Println("   [3] Add a New Peer Device by IP")
		fmt.Println("   [4] View Real-Time Service Logs (C:\\Bridge\\Logs\\bridge.log)")
		if strings.Contains(strings.ToUpper(health.Status), "STOPPED") || strings.Contains(strings.ToUpper(health.Status), "OFFLINE") {
			fmt.Println("   [5] Start Background Service")
		} else {
			fmt.Println("   [5] Restart Background Service")
		}
		fmt.Println("   [6] Re-run Setup Wizard / Change Settings")
		fmt.Println("   [7] Uninstall Bridge Service")
		if isExternal {
			fmt.Println("   [8] Upgrade Bridge Installation to this executable")
		} else {
			fmt.Println("   [8] Upgrade / Refresh Bridge Installation")
		}
		fmt.Println("   [0] Exit Dashboard")
		fmt.Println("======================================================================")
		fmt.Print("Select an option [0-8]: ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			_ = exec.Command("explorer.exe", cfg.SharedDir).Start()
		case "2":
			if len(peers) == 0 {
				fmt.Println("[!] No peer devices connected yet.")
				time.Sleep(1 * time.Second)
				continue
			}
			targetPeer := peers[0]
			if len(peers) > 1 {
				fmt.Println("\nSelect target device to print on:")
				for i, p := range peers {
					fmt.Printf("  [%d] %s (%s)\n", i+1, p.Name, p.IP)
				}
				fmt.Print("Choose device [1-N]: ")
				pChoice, _ := reader.ReadString('\n')
				pChoice = strings.TrimSpace(pChoice)
				idx := 0
				_, _ = fmt.Sscanf(pChoice, "%d", &idx)
				if idx >= 1 && idx <= len(peers) {
					targetPeer = peers[idx-1]
				}
			}

			filePath, err := tray.OpenFilePicker("Select Document to Print on " + targetPeer.Name)
			if err == nil && filePath != "" {
				fmt.Printf("[*] Sending %s to %s for silent printing...\n", filePath, targetPeer.Name)
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				err := printer.SendDirectPrint(ctx, targetPeer.URL, cfg.AuthToken, filePath,
					"X-Bridge-Node-Name", cfg.GetNodeName(),
					"X-Bridge-Origin", cfg.GetNodeName(),
				)
				cancel()
				if err == nil {
					fmt.Println("[✓] Print job delivered successfully!")
				} else {
					fmt.Printf("[!] Print failed: %v\n", err)
				}
				time.Sleep(2 * time.Second)
			}
		case "3":
			fmt.Print("\nEnter IP address of new peer device: ")
			newIP, _ := reader.ReadString('\n')
			newIP = strings.TrimSpace(newIP)
			if newIP != "" {
				cleanIP := config.ExtractIP(newIP)
				targetURL := fmt.Sprintf("http://%s:8765", cleanIP)
				peerName := cleanIP

				// Probe peer to fetch friendly name
				client := http.Client{Timeout: 2 * time.Second}
				if resp, err := client.Get(fmt.Sprintf("%s/api/health", targetURL)); err == nil {
					var hr struct {
						NodeName string `json:"node_name"`
					}
					if json.NewDecoder(resp.Body).Decode(&hr) == nil && hr.NodeName != "" {
						peerName = hr.NodeName
					}
					resp.Body.Close()
				}

				addedDev, _ := cfg.AddOrUpdatePeer(peerName, cleanIP, targetURL)
				_ = CreateBridgeShortcutsForPeers(cfg.GetPeers())

				// Notify running local service so it registers in memory immediately
				localServiceURL := fmt.Sprintf("http://127.0.0.1:%d/api/peers", cfg.ListenPort)
				devJSON, _ := json.Marshal(addedDev)
				if req, err := http.NewRequest("POST", localServiceURL, bytes.NewReader(devJSON)); err == nil {
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Bridge-Token", cfg.AuthToken)
					if resp, err := client.Do(req); err == nil {
						resp.Body.Close()
					}
				}

				// Introduce ourselves to the new peer
				if localIP := GetTailscaleIP(); localIP != "" {
					go IntroduceToRemote(targetURL, cfg.GetNodeName(), localIP, cfg.AuthToken)
				}

				fmt.Printf("[✓] Added peer device: %s (Port %d)\n", addedDev.Name, addedDev.SpoolPort)
				time.Sleep(1500 * time.Millisecond)
			}
		case "4":
			logFile := `C:\Bridge\Logs\bridge.log`
			if data, err := os.ReadFile(logFile); err == nil {
				fmt.Println("\n--- RECENT LOG ENTRIES ---")
				lines := strings.Split(string(data), "\n")
				start := 0
				if len(lines) > 25 {
					start = len(lines) - 25
				}
				for _, line := range lines[start:] {
					fmt.Println(line)
				}
				fmt.Println("--------------------------")
			} else {
				fmt.Println("\nNo log entries found yet.")
			}
			fmt.Println("Press Enter to continue...")
			_, _ = reader.ReadString('\n')
		case "5":
			if strings.Contains(strings.ToUpper(health.Status), "STOPPED") || strings.Contains(strings.ToUpper(health.Status), "OFFLINE") {
				fmt.Println("[*] Starting Bridge background service...")
				cmd := exec.Command(TargetExe, "-autostart")
				hideCmd(cmd)
				if err := cmd.Start(); err != nil {
					fmt.Printf("[!] Failed to start background service: %v\n", err)
				} else {
					fmt.Println("[✓] Background service started.")
				}
			} else {
				fmt.Println("[*] Restarting Bridge background service...")
				_ = RestartService()
			}
			time.Sleep(2 * time.Second)
		case "6":
			return RunInteractiveSetup()
		case "7":
			fmt.Print("Are you sure you want to completely uninstall Bridge Service? [y/N]: ")
			conf, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(conf)) == "y" {
				if !IsElevated() {
					fmt.Println("[*] Administrator elevation required for uninstallation. Elevating...")
					time.Sleep(1 * time.Second)
					return SelfElevate([]string{"-uninstall"})
				}
				return UninstallService()
			}
		case "8":
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
		case "0", "exit", "q":
			return nil
		}
	}
}

func queryHealth(port int) server.HealthResponse {
	client := http.Client{Timeout: 1 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
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
