package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"bridge/config"
	"bridge/installer"
	"bridge/printer"
	"bridge/server"
	bridgesync "bridge/sync"
	"bridge/tray"
)

func main() {
	// Early console suppression for background / autostart launches
	for _, arg := range os.Args[1:] {
		if arg == "-autostart" || arg == "-tray" || arg == "-service" || arg == "-silent" {
			tray.HideConsole()
			break
		}
	}

	serviceFlag := flag.Bool("service", false, "Run silently as background service without tray icon")
	autostartFlag := flag.Bool("autostart", false, "Run in background with taskbar tray icon (used at system boot / startup)")
	trayFlag := flag.Bool("tray", false, "Run in background with taskbar tray icon")
	installFlag := flag.Bool("install", false, "Install Bridge background service")
	setupFlag := flag.Bool("setup", false, "Run interactive setup wizard")
	uninstallFlag := flag.Bool("uninstall", false, "Uninstall Bridge background service")
	statusFlag := flag.Bool("status", false, "Check status of Bridge background service")
	sendPrintFlag := flag.Bool("send-print", false, "Send file to remote printer (used by Windows Explorer Send-To)")

	upgradeFlag := flag.String("upgrade", "", "Upgrade Bridge installation using specified binary")
	nameFlag := flag.String("name", "", "Name of this PC for automated install")
	peerFlag := flag.String("peer", "", "Peer Tailscale IP/URL for automated install")
	tokenFlag := flag.String("token", "", "Auth token for automated install")
	targetFlag := flag.String("target", "", "Target peer device name or IP for print dispatch")

	configPath := flag.String("config", "", "Path to config.json")
	flag.Parse()

	// 1. Right-Click SendTo Handler ("Print on <PeerName>")
	if *sendPrintFlag || (len(flag.Args()) > 0 && !*serviceFlag && !*autostartFlag && !*trayFlag && !*installFlag && !*setupFlag && !*uninstallFlag && !*statusFlag && *upgradeFlag == "") {
		targetFile := ""
		if len(flag.Args()) > 0 {
			targetFile = flag.Args()[0]
		}
		if targetFile != "" {
			handleSendPrint(targetFile, *targetFlag)
			return
		}
	}

	// 2. Query Status Mode
	if *statusFlag {
		printStatus()
		return
	}

	// 3. Uninstall Mode
	if *uninstallFlag {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		if err := installer.UninstallService(); err != nil {
			fmt.Fprintf(os.Stderr, "Uninstall failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 4. Upgrade Mode
	if *upgradeFlag != "" {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		if err := installer.UpgradeInstallation(*upgradeFlag); err != nil {
			fmt.Fprintf(os.Stderr, "Upgrade failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 5. Automated / Headless Install Mode
	if *installFlag && *peerFlag != "" {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		runHeadlessInstall(*nameFlag, *peerFlag, *tokenFlag)
		return
	}

	// 6. Explicit Setup Mode (-setup or -install without peer)
	if *setupFlag || (*installFlag && *peerFlag == "") {
		if !installer.AcquireInteractiveLock() {
			return
		}
		defer installer.ReleaseInteractiveLock()
		installer.SetDashboardConsoleTitle("Bridge Multi-Device Setup")

		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		if err := installer.RunInteractiveSetup(); err != nil {
			fmt.Fprintf(os.Stderr, "Setup error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	resolvedConfig := *configPath
	if resolvedConfig == "" {
		resolvedConfig = installer.TargetConfig
	}

	// 7. Background / Autostart Mode (System boot or background daemon)
	if *autostartFlag || *trayFlag || *serviceFlag {
		tray.HideConsole()

		// Ensure only one background service instance runs
		if !installer.AcquireServiceLock() {
			return
		}
		defer installer.ReleaseServiceLock()

		// If config does not exist or is not yet configured, exit silently at boot (do not spawn setup terminal)
		existingCfg, err := config.LoadConfig(resolvedConfig)
		if err != nil || !existingCfg.Configured {
			log.Printf("[Autostart] Config not ready at %s. Exiting background start.", resolvedConfig)
			return
		}

		// Ensure duplicate startup registrations are cleaned up
		_ = installer.RegisterInteractiveStartup()

		// If an un-mutexed legacy process is currently bound to port, kill it to take over
		if installer.VerifyHealth(existingCfg.ListenPort) == nil {
			installer.KillOtherBridgeProcesses()
			time.Sleep(300 * time.Millisecond)
		}

		// Run unified background service (headless=true if -service, false if -autostart/-tray with taskbar tray)
		runUnifiedService(resolvedConfig, *serviceFlag)
		return
	}

	// 8. Interactive Launch Mode (User double-clicked bridge.exe or ran from terminal)
	// Enforce single interactive terminal: if already open, bring existing window to front
	if !installer.AcquireInteractiveLock() {
		return
	}
	defer installer.ReleaseInteractiveLock()
	installer.SetDashboardConsoleTitle("Bridge Multi-Device Dashboard")

	needsSetup := false
	if _, err := os.Stat(resolvedConfig); os.IsNotExist(err) {
		needsSetup = true
	} else {
		existingCfg, err := config.LoadConfig(resolvedConfig)
		if err != nil || !existingCfg.Configured {
			needsSetup = true
		}
	}

	// First-Time Setup Wizard (only if never configured before)
	if needsSetup {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		if err := installer.RunInteractiveSetup(); err != nil {
			fmt.Fprintf(os.Stderr, "Setup error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Ensure clean autostart registration
	_ = installer.RegisterInteractiveStartup()

	// Already configured: Launch Control Dashboard with options (NO blind auto-updating)
	if err := installer.RunDashboard(); err != nil {
		fmt.Fprintf(os.Stderr, "Dashboard error: %v\n", err)
	}
}

func handleSendPrint(filePath, targetPeer string) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	cfg, err := config.LoadConfig(installer.TargetConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		return
	}

	// Determine destination peer URL
	peerURL := ""
	peerName := targetPeer
	if targetPeer != "" {
		if p := cfg.FindPeer(targetPeer); p != nil {
			peerURL = p.URL
			peerName = p.Name
		}
	}
	if peerURL == "" {
		peerURL = cfg.GetPeerURL()
		if peerName == "" {
			peerName = cfg.GetPeerName()
		}
	}

	// Try sending via local running bridge instance first
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/print-local", cfg.ListenPort), nil)
	if err == nil {
		req.Header.Set("X-Bridge-Token", cfg.AuthToken)
		req.Header.Set("X-Bridge-FilePath", absPath)
		if targetPeer != "" {
			req.Header.Set("X-Bridge-Target", targetPeer)
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	if peerURL == "" {
		fmt.Fprintf(os.Stderr, "No target peer available to print.\n")
		return
	}

	// Direct send fallback
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err = printer.SendDirectPrint(ctx, peerURL, cfg.AuthToken, absPath,
		"X-Bridge-Node-Name", cfg.GetNodeName(),
		"X-Bridge-Origin", cfg.GetNodeName(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Print to %s failed: %v\n", peerName, err)
		return
	}
}

func runUnifiedService(configPath string, headless bool) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config from %s: %v\n", configPath, err)
		os.Exit(1)
	}

	setupLogging(cfg.LogDir)
	log.Printf("==================================================")
	log.Printf("Bridge multi-device starting: Node='%s'", cfg.GetNodeName())
	log.Printf("ListenPort: %d | Total Peers: %d", cfg.ListenPort, len(cfg.GetPeers()))
	log.Printf("SharedDir:  %s", cfg.SharedDir)
	log.Printf("==================================================")

	// Ensure desktop shortcuts and virtual printers are configured for all peers
	_ = installer.CreateBridgeShortcutsForPeers(cfg.GetPeers())

	stateMgr, err := bridgesync.NewStateManager(cfg.StateFile)
	if err != nil {
		log.Fatalf("Failed to initialize state manager: %v", err)
	}

	// 1. Silent Print Service (Receives incoming print jobs from any peer)
	printService := printer.NewPrintService(cfg.SumatraPath, cfg.PrintTimeoutSec, cfg.PrintQueueDir, cfg.PrintFailedDir)
	if err := printService.Start(); err != nil {
		log.Printf("[Printer] Warning starting print service: %v", err)
	}
	defer printService.Stop()

	// 2. Multi-Peer Mesh Sync Engine
	syncEngine := bridgesync.NewSyncEngine(cfg.SharedDir, "", cfg.AuthToken, stateMgr)
	syncEngine.UpdatePeerURLs(cfg.GetPeerURLs())
	if localTS := installer.GetTailscaleIP(); localTS != "" {
		syncEngine.SetLocalURL(fmt.Sprintf("http://%s:%d", localTS, cfg.ListenPort))
	}
	if err := syncEngine.Start(); err != nil {
		log.Fatalf("Failed to start sync engine: %v", err)
	}
	defer syncEngine.Stop()

	// 3. HTTP API Server
	httpServer := server.NewServer(cfg, stateMgr, printService)
	httpServer.SetSyncEngine(syncEngine)
	go func() {
		if err := httpServer.Start(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server terminated: %v", err)
		}
	}()
	defer httpServer.Stop()

	var trayMgr *tray.TrayManager

	// Dispatch feedback hook for SendTo right-click printing
	httpServer.SetOnPrintDispatched(func(fileName, peerName string) {
		if trayMgr != nil {
			trayMgr.FlashComplete("Print Dispatched", fmt.Sprintf("Sent %s to print on %s!", fileName, peerName))
		}
	})

	// 4. Windows Spooler Multi-Listener (One listener per peer virtual printer port)
	multiSpooler := printer.NewMultiSpoolerListener(func(peer config.PeerDevice, filePath string) error {
		if trayMgr != nil {
			trayMgr.SetState(tray.StatePrinting, fmt.Sprintf("Bridge: Sending print to %s...", peer.Name))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return printer.SendDirectPrint(ctx, peer.URL, cfg.AuthToken, filePath,
			"X-Bridge-Node-Name", cfg.GetNodeName(),
			"X-Bridge-Origin", cfg.GetNodeName(),
		)
	})

	multiSpooler.OnJobStarted = func(peer config.PeerDevice, fileName string) {
		if trayMgr != nil {
			trayMgr.SetState(tray.StatePrinting, fmt.Sprintf("Bridge: Transmitting %s to %s...", fileName, peer.Name))
		}
	}
	multiSpooler.OnJobCompleted = func(peer config.PeerDevice, fileName string) {
		if trayMgr != nil {
			trayMgr.FlashComplete("Print Complete", fmt.Sprintf("Sent print job to %s!", peer.Name))
		}
	}
	multiSpooler.OnJobFailed = func(peer config.PeerDevice, fileName string, err error) {
		if trayMgr != nil {
			trayMgr.ShowBalloon("Print Error", fmt.Sprintf("Failed to print on %s: %v", peer.Name, err))
			trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
		}
	}

	// Start listeners for all known peers
	for _, p := range cfg.GetPeers() {
		if err := multiSpooler.EnsurePeerListener(p); err != nil {
			log.Printf("[MultiSpooler] Warning starting spooler listener for %s: %v", p.Name, err)
		}
	}
	defer multiSpooler.StopAll()

	// Dynamic peer update wiring: when a peer connects or registers
	httpServer.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		syncEngine.UpdatePeerURL(peer.URL)
		_ = multiSpooler.EnsurePeerListener(peer)
		cfg.SetPeerOnlineStatus(peer.URL, true)
		if trayMgr != nil {
			trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
		}
		// Catch-up synchronize files immediately ONLY when a brand new peer connects (not on repeated pings)
		if isNew {
			syncEngine.ReconcileWithPeer(peer.URL)

			go func() {
				_ = installer.CreateBridgeShortcutsForPeers(cfg.GetPeers())
			}()

			// Mesh Gossip Propagation: announce newly discovered peer to all other known peers
			localTailscale := installer.GetTailscaleIP()
			allPeers := cfg.GetPeers()
			for _, other := range allPeers {
				if other.URL != "" && other.URL != peer.URL && !strings.EqualFold(other.Name, peer.Name) {
					go installer.IntroduceToRemote(other.URL, peer.Name, peer.IP, cfg.AuthToken)
				}
			}
			if localTailscale != "" && peer.URL != "" {
				go installer.IntroduceToRemote(peer.URL, cfg.GetNodeName(), localTailscale, cfg.AuthToken)
			}
		}
	})

	// 5. System Tray Icon ("Status Dot" in Windows Taskbar)
	if !headless {
		tray.HideConsoleIfDedicated()

		cb := tray.TrayCallbacks{
			OnOpenShared: func() {
				_ = exec.Command("explorer.exe", cfg.SharedDir).Start()
			},
			OnPrintFile: func(filePath string) {
				primaryURL := cfg.GetPeerURL()
				primaryName := cfg.GetPeerName()
				if primaryURL == "" {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				err := printer.SendDirectPrint(ctx, primaryURL, cfg.AuthToken, filePath,
					"X-Bridge-Node-Name", cfg.GetNodeName(),
					"X-Bridge-Origin", cfg.GetNodeName(),
				)
				if err == nil {
					if trayMgr != nil {
						trayMgr.FlashComplete("Print Complete", fmt.Sprintf("Printed %s on %s!", filepath.Base(filePath), primaryName))
					}
				} else {
					if trayMgr != nil {
						trayMgr.ShowBalloon("Print Error", fmt.Sprintf("Failed to print on %s: %v", primaryName, err))
						trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
					}
				}
			},
			OnPrintFileToPeer: func(targetPeerName, filePath string) {
				p := cfg.FindPeer(targetPeerName)
				if p == nil || p.URL == "" {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				err := printer.SendDirectPrint(ctx, p.URL, cfg.AuthToken, filePath,
					"X-Bridge-Node-Name", cfg.GetNodeName(),
					"X-Bridge-Origin", cfg.GetNodeName(),
				)
				if err == nil {
					if trayMgr != nil {
						trayMgr.FlashComplete("Print Complete", fmt.Sprintf("Printed %s on %s!", filepath.Base(filePath), p.Name))
					}
				} else {
					if trayMgr != nil {
						trayMgr.ShowBalloon("Print Error", fmt.Sprintf("Failed to print on %s: %v", p.Name, err))
						trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
					}
				}
			},
			OnShowStatus: func() string {
				defaultPrinter, _ := printer.GetDefaultWindowsPrinter()
				if defaultPrinter == "" {
					defaultPrinter = "None"
				}

				peers := cfg.GetPeers()
				onlineCount := 0
				for _, p := range peers {
					if p.Online {
						onlineCount++
					}
				}

				localIP := installer.GetTailscaleIP()

				var b strings.Builder
				b.WriteString("Bridge Multi-Device Mesh Status\n\n")
				b.WriteString(fmt.Sprintf("This Device:     %s (%s)\n", cfg.GetNodeName(), localIP))
				b.WriteString(fmt.Sprintf("Local Printer:   %s\n", defaultPrinter))
				b.WriteString(fmt.Sprintf("Active Jobs:     %d\n", printService.QueueDepth()))
				b.WriteString(fmt.Sprintf("Shared Folder:   %s (Global Mesh)\n\n", cfg.SharedDir))

				b.WriteString(fmt.Sprintf("Connected Devices (%d/%d Online):\n", onlineCount, len(peers)))
				if len(peers) == 0 {
					b.WriteString("  (Waiting for other devices to connect)\n")
				} else {
					for _, p := range peers {
						statusTag := "⚪ Offline"
						if p.Online {
							statusTag = "🟢 Online"
						}
						b.WriteString(fmt.Sprintf("  ● %-12s IP: %-15s Port: %-5d %s\n", p.Name, p.IP, p.SpoolPort, statusTag))
					}
				}
				return b.String()
			},
			OnResync: func() {
				if trayMgr != nil {
					trayMgr.SetState(tray.StateSyncing, "Bridge: Syncing shared files across devices...")
					time.Sleep(1 * time.Second)
					trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
				}
			},
			OnCopyIP: func() string {
				return installer.GetTailscaleIP()
			},
			OnOpenLogs: func() {
				_ = exec.Command("explorer.exe", cfg.LogDir).Start()
			},
			OnExit: func() {
				if trayMgr != nil {
					trayMgr.Stop()
				}
				os.Exit(0)
			},
		}

		tm, err := tray.NewTrayManager(installer.TargetIcons, cfg.GetNodeName(), cfg.GetPeerName(), cb)
		if err == nil {
			trayMgr = tm
			_ = trayMgr.Start()
			trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
			defer trayMgr.Stop()
		}

		// Connect callbacks to update tray dot state:
		printService.OnJobStarted = func(job printer.PrintJob) {
			if trayMgr != nil {
				trayMgr.SetState(tray.StatePrinting, fmt.Sprintf("Bridge: Printing %s...", job.FileName))
			}
		}
		printService.OnJobCompleted = func(job printer.PrintJob) {
			if trayMgr != nil {
				trayMgr.FlashComplete("Print Succeeded", fmt.Sprintf("%s was printed successfully!", job.FileName))
			}
		}

		syncEngine.OnSyncStarted = func(relPath string) {
			if trayMgr != nil {
				trayMgr.SetState(tray.StateSyncing, fmt.Sprintf("Bridge: Syncing %s...", relPath))
			}
		}
		syncEngine.OnSyncCompleted = func(relPath string) {
			if trayMgr != nil {
				trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
			}
		}

		// Periodic connection watcher to keep tray icon dot updated and maintain transitive mesh discovery
		go func() {
			peerFailCounts := make(map[string]int)

			for {
				time.Sleep(3 * time.Second)
				if trayMgr == nil {
					continue
				}

				peers := cfg.GetPeers()
				if len(peers) == 0 {
					trayMgr.SetState(tray.StateOffline, fmt.Sprintf("Bridge: %s (Waiting for devices to connect)", cfg.GetNodeName()))
					continue
				}

				var wg sync.WaitGroup
				var mu sync.Mutex
				statusChanged := false
				namesChanged := false
				var newDiscoveredPeers []config.PeerDevice
				localTailscale := installer.GetTailscaleIP()

				for i := range peers {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						p := peers[idx]
						if p.URL == "" {
							return
						}
						healthy, learnedName, hr := checkPeerHealthy(p.URL, cfg.AuthToken, cfg.GetNodeName())
						mu.Lock()
						defer mu.Unlock()

						wasOnline := peers[idx].Online
						if healthy {
							peerFailCounts[p.URL] = 0
							if !wasOnline {
								peers[idx].Online = true
								cfg.SetPeerOnlineStatus(p.URL, true)
								statusChanged = true
								// If peer transitioned from offline to online, trigger catch-up sync!
								syncEngine.ReconcileWithPeer(p.URL)
							}
						} else {
							peerFailCounts[p.URL]++
							// Debounce ping jitter: Require 3 consecutive failed health checks (~9-12s of sustained downtime)
							// before marking a connected peer as offline. This eliminates flickering between yellow and green!
							if peerFailCounts[p.URL] >= 3 && wasOnline {
								peers[idx].Online = false
								cfg.SetPeerOnlineStatus(p.URL, false)
								statusChanged = true
							}
						}

						if healthy && hr != nil {
							peers[idx].LastSeen = time.Now()
							if learnedName != "" && learnedName != p.Name {
								log.Printf("[HealthWatcher] Learned peer name for %s: %s", p.IP, learnedName)
								peers[idx].Name = learnedName
								cfg.SetPeerName(p.URL, learnedName)
								namesChanged = true
							}
							// Transitive Mesh Peer Discovery: check peers returned by healthy peer
							for _, mp := range hr.Peers {
								if mp.Name == cfg.GetNodeName() || (localTailscale != "" && mp.IP == localTailscale) {
									continue
								}
								if cfg.FindPeer(mp.Name) == nil && cfg.FindPeer(mp.IP) == nil && cfg.FindPeer(mp.URL) == nil {
									newDiscoveredPeers = append(newDiscoveredPeers, mp)
								}
							}
						}
					}(i)
				}
				wg.Wait()

				// Add any newly discovered mesh peers
				hasNewPeers := false
				for _, np := range newDiscoveredPeers {
					if cfg.FindPeer(np.Name) == nil && cfg.FindPeer(np.IP) == nil && cfg.FindPeer(np.URL) == nil {
						added, isNew := cfg.AddOrUpdatePeer(np.Name, np.IP, np.URL)
						if isNew {
							log.Printf("[MeshGossip] Automatically discovered new mesh peer: %s (%s)", added.Name, added.URL)
							hasNewPeers = true
							syncEngine.UpdatePeerURL(added.URL)
							_ = multiSpooler.EnsurePeerListener(added)
							if localTailscale != "" {
								go installer.IntroduceToRemote(added.URL, cfg.GetNodeName(), localTailscale, cfg.AuthToken)
							}
						}
					}
				}

				latestPeers := cfg.GetPeers()
				if statusChanged || namesChanged || hasNewPeers {
					// Only re-run shortcut/printer creation when names change or new peers join (never on simple online ping)
					if namesChanged || hasNewPeers {
						_ = installer.CreateBridgeShortcutsForPeers(latestPeers)
					}
					for _, p := range latestPeers {
						_ = multiSpooler.EnsurePeerListener(p)
					}
				}
				trayMgr.UpdateNetworkStatus(cfg.GetNodeName(), latestPeers)
			}
		}()
	}

	// Wait for OS shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	sig := <-sigChan

	log.Printf("Received signal %v. Initiating graceful shutdown...", sig)
}

func checkPeerHealthy(peerURL, token, nodeName string) (bool, string, *server.HealthResponse) {
	if peerURL == "" {
		return false, "", nil
	}
	client := http.Client{Timeout: 3500 * time.Millisecond}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", peerURL), nil)
	if err != nil {
		return false, "", nil
	}
	req.Header.Set("X-Bridge-Token", token)
	req.Header.Set("X-Bridge-Node-Name", nodeName)
	req.Header.Set("X-Bridge-Probe", "1")
	localTailscale := installer.GetTailscaleIP()
	if localTailscale != "" {
		req.Header.Set("X-Bridge-Peer-URL", fmt.Sprintf("http://%s:8765", localTailscale))
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, "", nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "", nil
	}

	var hr server.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err == nil && hr.NodeName != "" {
		return true, hr.NodeName, &hr
	}
	return true, "", nil
}

func runHeadlessInstall(name, peer, token string) {
	cfg := config.DefaultConfig()
	if name != "" {
		cfg.NodeName = name
	}
	if peer != "" {
		cleanIP := config.ExtractIP(peer)
		peerURL := peer
		if !strings.HasPrefix(peer, "http://") && !strings.HasPrefix(peer, "https://") {
			peerURL = fmt.Sprintf("http://%s:8765", cleanIP)
		}
		cfg.AddOrUpdatePeer(cleanIP, cleanIP, peerURL)
	}
	if token != "" {
		cfg.AuthToken = token
	}

	sumatra := installer.FindSumatraPDF()
	if sumatra == "" {
		var err error
		sumatra, err = installer.InstallSumatraPDF()
		if err != nil {
			fmt.Printf("[!] SumatraPDF install warning: %v\n", err)
		}
	}
	cfg.SumatraPath = sumatra

	if err := installer.InstallService(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Installation failed: %v\n", err)
		os.Exit(1)
	}

	_ = installer.RestartService()

	fmt.Println("[✓] Headless installation completed successfully.")
}

func printStatus() {
	cfg, err := config.LoadConfig(installer.TargetConfig)
	port := 8765
	if err == nil && cfg.ListenPort > 0 {
		port = cfg.ListenPort
	}

	err = installer.VerifyHealth(port)
	if err == nil {
		fmt.Printf("Bridge Service: RUNNING on port %d (Healthy)\n", port)
		if cfg != nil {
			peers := cfg.GetPeers()
			fmt.Printf("Node: %s (Total Peers: %d)\n", cfg.GetNodeName(), len(peers))
			for i, p := range peers {
				fmt.Printf("  [%d] %s (%s)\n", i+1, p.Name, p.URL)
			}
		}
	} else {
		fmt.Printf("Bridge Service: STOPPED / UNREACHABLE (%v)\n", err)
	}
}

func setupLogging(logDir string) {
	if logDir == "" {
		return
	}
	_ = os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "bridge.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		mw := io.MultiWriter(os.Stdout, f)
		log.SetOutput(mw)
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	}
}
