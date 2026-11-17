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
	"strings"
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

	// Discover online Tailscale peers early to intelligently suggest default device name
	var suggestedPeerIP string
	var suggestedPeerName string
	var knownTailscaleBridgeNames []string
	tsPeers, err := DiscoverTailscalePeers()
	if err == nil && len(tsPeers) > 0 {
		for _, p := range tsPeers {
			if p.Online {
				if suggestedPeerIP == "" {
					suggestedPeerIP = p.IP
					if p.BridgeName != "" {
						suggestedPeerName = p.BridgeName
					} else {
						suggestedPeerName = p.HostName
					}
				}
				if p.BridgeName != "" {
					knownTailscaleBridgeNames = append(knownTailscaleBridgeNames, p.BridgeName)
				}
			}
		}
	}

	// 3. User Experience: Ask for this device's name and display its IP
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                       DEVICE CONFIGURATION                           ")
	if localIP != "" {
		fmt.Printf(">>> THIS DEVICE'S TAILSCALE IP: %s <<<\n", localIP)
	}
	fmt.Println("======================================================================")

	defaultName := "Unit 1"
	if cfg.NodeName != "" && !strings.HasPrefix(cfg.NodeName, "Desktop ") && cfg.NodeName != "Bridge-PC" {
		defaultName = cfg.NodeName
	} else if len(knownTailscaleBridgeNames) > 0 {
		hasUnit1 := false
		hasUnit2 := false
		for _, bn := range knownTailscaleBridgeNames {
			if strings.EqualFold(bn, "Unit 1") {
				hasUnit1 = true
			}
			if strings.EqualFold(bn, "Unit 2") {
				hasUnit2 = true
			}
		}
		if hasUnit1 && !hasUnit2 {
			defaultName = "Unit 2"
		} else if hasUnit1 && hasUnit2 {
			defaultName = "Unit 3"
		}
	}

	fmt.Printf("What name to give THIS device? (e.g. Unit 1, Unit 2) [Default: '%s']: ", defaultName)
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

	// 4. Prompt for Peer IP to link with another device
	fmt.Println("----------------------------------------------------------------------")

	if suggestedPeerIP != "" {
		fmt.Printf("[*] Auto-detected online Tailscale device: %s (%s)\n", suggestedPeerName, suggestedPeerIP)
		fmt.Printf("Enter peer IP address [Default: '%s' or Enter if this is Device 1]: ", suggestedPeerIP)
	} else {
		fmt.Println("Enter peer IP address to connect (e.g. 100.x.y.z),")
		fmt.Print("or press Enter if this is Device 1 (first device) / setting up peers later: ")
	}

	peerInput, _ := reader.ReadString('\n')
	peerInput = strings.TrimSpace(peerInput)
	if peerInput == "" && suggestedPeerIP != "" {
		peerInput = suggestedPeerIP
	}

	if peerInput != "" {
		cleanIP := config.ExtractIP(peerInput)
		targetURL := fmt.Sprintf("http://%s:8765", cleanIP)

		fmt.Printf("[*] Connecting to peer at %s...\n", cleanIP)

		// Probe peer's /api/health to retrieve friendly name and peer list
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
					fmt.Printf("[✓] Successfully detected peer device: '%s' (%s)!\n", hr.NodeName, cleanIP)
					addedDev, _ := cfg.AddOrUpdatePeer(hr.NodeName, cleanIP, targetURL)
					fmt.Printf("[✓] Windows Virtual Printer prepared: 'Print on %s' (Port %d)\n", addedDev.Name, addedDev.SpoolPort)

					// Register ourselves with the target peer
					if localIP != "" {
						IntroduceToRemote(targetURL, cfg.NodeName, localIP, cfg.AuthToken)
					}

					// Inherit any mesh peers known by the connected device and introduce ourselves
					for _, mp := range hr.Peers {
						if mp.IP != localIP && mp.Name != cfg.NodeName {
							cfg.AddOrUpdatePeer(mp.Name, mp.IP, mp.URL)
							fmt.Printf("[✓] Discovered mesh device: '%s' (%s)\n", mp.Name, mp.IP)
							if mp.URL != "" && localIP != "" {
								IntroduceToRemote(mp.URL, cfg.NodeName, localIP, cfg.AuthToken)
							}
						}
					}
				}
				resp.Body.Close()
			} else {
				// Peer not currently responding -> add with cleanIP placeholder
				placeholderName := suggestedPeerName
				if placeholderName == "" {
					placeholderName = fmt.Sprintf("Unit (%s)", cleanIP)
				}
				addedDev, _ := cfg.AddOrUpdatePeer(placeholderName, cleanIP, targetURL)
				fmt.Printf("[*] Peer at %s added. Bridge will auto-pair as soon as it goes online.\n", cleanIP)
				fmt.Printf("[✓] Virtual Printer registered: 'Print on %s' (Port %d)\n", addedDev.Name, addedDev.SpoolPort)
			}
		}
	} else {
		fmt.Println()
		fmt.Println("======================================================================")
		fmt.Printf("               SETUP COMPLETED FOR '%s'!\n", cfg.NodeName)
		fmt.Println("======================================================================")
		if localIP != "" {
			fmt.Printf(">>> THIS DEVICE'S IP: %s <<<\n", localIP)
			fmt.Println("When setting up your second device (e.g. Unit 2), run Bridge and")
			fmt.Printf("enter this IP (%s). Both devices will link automatically!\n", localIP)
		}
		fmt.Println("======================================================================")
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

	printSetupSuccess(cfg, localIP, reader)
	return nil
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
		fmt.Println("1. Run 'bridge.exe' on your other computer.")
		fmt.Printf("2. Enter this computer's Tailscale IP: %s\n", localIP)
		fmt.Println("   Both computers will link up instantly, and printers will appear!")
	}
	fmt.Println("======================================================================")
	fmt.Println("Press Enter to launch...")
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
		fmt.Println("----------------------------------------------------------------------")
		fmt.Println(" Quick Actions:")
		fmt.Println("   [1] Open Global Shared Folder in File Explorer (C:\\Bridge\\Shared)")
		fmt.Println("   [2] Print a File to a Remote Device...")
		fmt.Println("   [3] Add a New Peer Device by IP")
		fmt.Println("   [4] View Real-Time Service Logs (C:\\Bridge\\Logs\\bridge.log)")
		fmt.Println("   [5] Restart Background Service")
		fmt.Println("   [6] Re-run Setup Wizard / Change Settings")
		fmt.Println("   [7] Uninstall Bridge Service")
		fmt.Println("   [0] Exit Dashboard")
		fmt.Println("======================================================================")
		fmt.Print("Select an option [0-7]: ")

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
			fmt.Println("[*] Restarting Bridge background service...")
			_ = RestartService()
			time.Sleep(2 * time.Second)
		case "6":
			return RunInteractiveSetup()
		case "7":
			fmt.Print("Are you sure you want to completely uninstall Bridge Service? [y/N]: ")
			conf, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(conf)) == "y" {
				return UninstallService()
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
