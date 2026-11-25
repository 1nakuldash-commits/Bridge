package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bridge/config"
	"bridge/installer"
	"bridge/printer"
	"bridge/server"
	bridgesync "bridge/sync"
	"bridge/tray"
)

func init() {
	// Earliest possible console suppression for background / autostart launches
	for _, arg := range os.Args[1:] {
		if arg == "-autostart" || arg == "-tray" || arg == "-service" || arg == "-silent" {
			tray.HideConsole()
			break
		}
	}
}

func main() {
	// Double check early console suppression
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
	pairFlag := flag.Bool("pair", false, "Connect / pair a new device (elevated)")
	syncPrintersFlag := flag.Bool("sync-printers", false, "Sync desktop shortcuts and Windows virtual printers")
	uninstallFlag := flag.Bool("uninstall", false, "Uninstall Bridge background service")
	statusFlag := flag.Bool("status", false, "Check status of Bridge background service")
	versionFlag := flag.Bool("version", false, "Print application version")
	sendPrintFlag := flag.Bool("send-print", false, "Send file to remote printer (used by Windows Explorer Send-To)")

	upgradeFlag := flag.String("upgrade", "", "Upgrade Bridge installation using specified binary")
	nameFlag := flag.String("name", "", "Name of this PC for automated install")
	peerFlag := flag.String("peer", "", "Peer Tailscale IP/URL for automated install")
	tokenFlag := flag.String("token", "", "Auth token for automated install")
	targetFlag := flag.String("target", "", "Target peer device name or IP for print dispatch")

	configPath := flag.String("config", "", "Path to config.json")
	flag.Parse()

	resolvedConfig := *configPath
	if resolvedConfig == "" {
		resolvedConfig = installer.TargetConfig
	}

	// 1. Version Mode
	if *versionFlag {
		fmt.Println("Bridge v2.3.0 (Zero-Defect 1:1 Scale & Instant Mesh Sync Edition)")
		return
	}

	// 2. Right-Click SendTo Handler ("Print on <PeerName>")
	if *sendPrintFlag || (len(flag.Args()) > 0 && !*serviceFlag && !*autostartFlag && !*trayFlag && !*installFlag && !*setupFlag && !*pairFlag && !*syncPrintersFlag && !*uninstallFlag && !*statusFlag && *upgradeFlag == "") {
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
			if fi, statErr := os.Stdin.Stat(); statErr == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
				fmt.Println("Press Enter to close...")
				_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			}
			os.Exit(1)
		}
		if fi, statErr := os.Stdin.Stat(); statErr == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			fmt.Println("Press Enter to close...")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		return
	}

	// 4. Sync Printers Mode
	if *syncPrintersFlag {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		cfg, err := config.LoadConfig(resolvedConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load configuration from %s: %v\n", resolvedConfig, err)
			os.Exit(1)
		}
		res := installer.SyncBridgeShortcutsForPeers(cfg.GetPeers())
		for _, name := range res.SucceededPrinters {
			fmt.Printf("[✓] Windows Virtual Printer ready: 'Print on %s'\n", name)
		}
		for name, pErr := range res.PrinterErrors {
			fmt.Fprintf(os.Stderr, "[!] Windows Virtual Printer 'Print on %s' failed: %v\n", name, pErr)
		}
		return
	}

	// 5. Pair Mode (elevated pairing + printer creation, then enters Dashboard)
	if *pairFlag {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		if !installer.AcquireInteractiveLock() {
			return
		}
		defer installer.ReleaseInteractiveLock()

		cfg, err := config.LoadConfig(resolvedConfig)
		if err != nil || !cfg.Configured {
			installer.SetDashboardConsoleTitle("Bridge Multi-Device Setup")
			installer.UnregisterInteractiveStartup()
			if err := installer.RunInteractiveSetup(); err != nil {
				fmt.Fprintf(os.Stderr, "Setup error: %v\n", err)
				os.Exit(1)
			}
			return
		}
		installer.SetDashboardConsoleTitle("Bridge Multi-Device Dashboard")
		_ = installer.RegisterInteractiveStartup()
		installer.RunPairNewDevice(cfg)
		_ = installer.CreateBridgeShortcutsForPeers(cfg.GetPeers())
		if err := installer.RunDashboard(); err != nil {
			fmt.Fprintf(os.Stderr, "Dashboard error: %v\n", err)
		}
		return
	}

	// 6. Upgrade Mode
	if *upgradeFlag != "" {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		installer.UnregisterInteractiveStartup()
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
		installer.UnregisterInteractiveStartup()
		runHeadlessInstall(*nameFlag, *peerFlag, *tokenFlag)
		return
	}

	// 6. Explicit Setup Mode (-setup or -install without peer)
	if *setupFlag || (*installFlag && *peerFlag == "") {
		if !installer.IsElevated() {
			_ = installer.SelfElevate(os.Args[1:])
			return
		}
		if !installer.AcquireInteractiveLock() {
			return
		}
		defer installer.ReleaseInteractiveLock()
		installer.SetDashboardConsoleTitle("Bridge Multi-Device Setup")

		installer.UnregisterInteractiveStartup()
		if err := installer.RunInteractiveSetup(); err != nil {
			fmt.Fprintf(os.Stderr, "Setup error: %v\n", err)
			os.Exit(1)
		}
		return
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

		// Ensure clean autostart registration (fast in-process check)
		_ = installer.RegisterInteractiveStartup()

		// If an un-mutexed legacy process is currently bound to port, kill it and wait briefly for port release
		if installer.VerifyHealth(existingCfg.ListenPort) == nil {
			installer.KillOtherBridgeProcesses()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if installer.VerifyHealth(existingCfg.ListenPort) != nil {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
		}

		// Run unified background service (headless=true if -service, false if -autostart/-tray with taskbar tray)
		runUnifiedService(resolvedConfig, *serviceFlag)
		return
	}

	// 8. Interactive Launch Mode (User double-clicked bridge.exe or ran from terminal)
	needsSetup := false
	if _, err := os.Stat(resolvedConfig); os.IsNotExist(err) {
		needsSetup = true
	} else {
		existingCfg, err := config.LoadConfig(resolvedConfig)
		if err != nil || !existingCfg.Configured {
			needsSetup = true
		}
	}

	// Elevate BEFORE acquiring interactive mutex so elevated child never races with exiting parent's lock
	if needsSetup && !installer.IsElevated() {
		_ = installer.SelfElevate(os.Args[1:])
		return
	}

	// Enforce single interactive terminal: if already open, bring existing window to front
	if !installer.AcquireInteractiveLock() {
		return
	}
	defer installer.ReleaseInteractiveLock()

	// First-Time Setup Wizard (only if never configured before)
	if needsSetup {
		installer.SetDashboardConsoleTitle("Bridge Multi-Device Setup")
		installer.UnregisterInteractiveStartup()
		if err := installer.RunInteractiveSetup(); err != nil {
			fmt.Fprintf(os.Stderr, "Setup error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	installer.SetDashboardConsoleTitle("Bridge Multi-Device Dashboard")

	// Ensure clean autostart registration (fast in-process check)
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

	// 2. Multi-Peer Mesh Sync Engine
	syncEngine := bridgesync.NewSyncEngine(cfg.SharedDir, "", cfg.AuthToken, stateMgr)
	syncEngine.UpdatePeerURLs(cfg.GetPeerURLs())
	initialLocalTS := installer.GetTailscaleIP()
	if initialLocalTS != "" {
		syncEngine.SetLocalURL(fmt.Sprintf("http://%s:%d", initialLocalTS, cfg.ListenPort))
	}

	// 3. HTTP API Server & thread-safe TrayManager reference
	var trayMgr atomic.Pointer[tray.TrayManager]
	getTray := func() *tray.TrayManager {
		return trayMgr.Load()
	}

	quitChan := make(chan struct{})
	var quitOnce sync.Once
	triggerShutdown := func() {
		quitOnce.Do(func() {
			close(quitChan)
		})
	}

	httpServer := server.NewServer(cfg, stateMgr, printService)
	httpServer.SetSyncEngine(syncEngine)
	httpServer.SetOnSyncReceived(func(relPath string) {
		if tm := getTray(); tm != nil {
			tm.FlashSyncComplete("File Received", fmt.Sprintf("Received %s from network!", filepath.Base(relPath)))
		}
	})

	// Dispatch feedback hook for SendTo right-click printing
	httpServer.SetOnPrintDispatched(func(fileName, peerName string) {
		if tm := getTray(); tm != nil {
			tm.FlashPrintComplete("Print Dispatched", fmt.Sprintf("Sent %s to print on %s!", fileName, peerName))
		}
	})

	// 4. Windows Spooler Multi-Listener (One listener per peer virtual printer port)
	multiSpooler := printer.NewMultiSpoolerListener(func(peer config.PeerDevice, filePath string) error {
		if tm := getTray(); tm != nil {
			tm.SetPrintingState(fmt.Sprintf("Bridge: Sending print to %s...", peer.Name))
		}

		targetPeer := peer
		if p := cfg.FindPeer(peer.Name); p != nil {
			targetPeer = *p
		} else if p := cfg.FindPeer(peer.IP); p != nil {
			targetPeer = *p
		}
		targetURL := targetPeer.URL
		if targetURL == "" && targetPeer.IP != "" {
			targetURL = fmt.Sprintf("http://%s:%d", targetPeer.IP, cfg.ListenPort)
		}
		if targetURL == "" {
			return fmt.Errorf("peer %s is not connected yet (no reachable URL or IP)", targetPeer.Name)
		}

		host := config.ExtractIP(targetURL)
		if parsedIP := net.ParseIP(host); (parsedIP != nil && parsedIP.IsLoopback()) ||
			strings.EqualFold(host, "127.0.0.1") ||
			strings.EqualFold(host, "localhost") ||
			host == "::1" ||
			config.IsSelfTarget(targetPeer.Name, targetPeer.IP, targetURL, cfg.GetNodeName(), cfg.ListenPort) {
			return fmt.Errorf("refusing to route virtual print to self/loopback target (%s)", targetURL)
		}

		spoolPort := targetPeer.SpoolPort
		if spoolPort <= 0 {
			spoolPort = peer.SpoolPort
		}
		log.Printf("[MultiSpooler] Routing virtual print from port %d to peer %q (%s)", spoolPort, targetPeer.Name, targetURL)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return printer.SendDirectPrint(ctx, targetURL, cfg.AuthToken, filePath,
			"X-Bridge-Node-Name", cfg.GetNodeName(),
			"X-Bridge-Origin", cfg.GetNodeName(),
		)
	})

	multiSpooler.OnJobStarted = func(peer config.PeerDevice, fileName string) {
		if tm := getTray(); tm != nil {
			tm.SetPrintingState(fmt.Sprintf("Bridge: Transmitting %s to %s...", fileName, peer.Name))
		}
	}
	multiSpooler.OnJobCompleted = func(peer config.PeerDevice, fileName string) {
		if tm := getTray(); tm != nil {
			tm.FlashPrintComplete("Print Complete", fmt.Sprintf("Sent print job to %s!", peer.Name))
		}
	}
	multiSpooler.OnJobFailed = func(peer config.PeerDevice, fileName string, err error) {
		if tm := getTray(); tm != nil {
			tm.ShowBalloon("Print Error", fmt.Sprintf("Failed to print on %s: %v", peer.Name, err))
			tm.RestoreNetworkStatus()
		}
	}

	// Dynamic peer update wiring: when a peer connects or registers
	httpServer.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		if strings.EqualFold(strings.TrimSpace(peer.Name), strings.TrimSpace(cfg.GetNodeName())) {
			return // Strictly ignore self
		}
		syncEngine.UpdatePeerURL(peer.URL)
		_ = multiSpooler.EnsurePeerListener(peer)
		cfg.SetPeerOnlineStatus(peer.URL, true)
		if tm := getTray(); tm != nil {
			tm.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
		}
		// Catch-up synchronize files immediately ONLY when a brand new peer connects (not on repeated pings)
		if isNew {
			_ = config.SaveConfig(configPath, cfg)
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

	// Connect callbacks to update tray dot state before starting services
	printService.OnJobStarted = func(job printer.PrintJob) {
		if tm := getTray(); tm != nil {
			tm.SetPrintingState(fmt.Sprintf("Bridge: Printing %s...", job.FileName))
		}
	}
	printService.OnJobCompleted = func(job printer.PrintJob) {
		if tm := getTray(); tm != nil {
			tm.FlashPrintComplete("Print Succeeded", fmt.Sprintf("%s was printed successfully!", job.FileName))
		}
	}
	printService.OnJobFailed = func(job printer.PrintJob, reason string) {
		if tm := getTray(); tm != nil {
			tm.ShowBalloon("Print Failed", fmt.Sprintf("Failed to print %s: %s", job.FileName, reason))
			tm.RestoreNetworkStatus()
		}
	}

	syncEngine.OnSyncStarted = func(relPath string) {
		if tm := getTray(); tm != nil {
			tm.SetSyncingState(fmt.Sprintf("Bridge: Uploading/Syncing %s...", relPath))
		}
	}
	syncEngine.OnSyncCompleted = func(relPath string) {
		if tm := getTray(); tm != nil {
			tm.FlashSyncComplete("Sync Complete", fmt.Sprintf("Uploaded %s to network!", relPath))
		}
	}

	// 5. System Tray Icon ("Status Dot" in Windows Taskbar) initialized before service goroutines start
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
					if tm := getTray(); tm != nil {
						tm.ShowBalloon("No Peer Connected", "Cannot print: no peer device is connected yet.")
						tm.RestoreNetworkStatus()
					}
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				err := printer.SendDirectPrint(ctx, primaryURL, cfg.AuthToken, filePath,
					"X-Bridge-Node-Name", cfg.GetNodeName(),
					"X-Bridge-Origin", cfg.GetNodeName(),
				)
				if err == nil {
					if tm := getTray(); tm != nil {
						tm.FlashPrintComplete("Print Complete", fmt.Sprintf("Printed %s on %s!", filepath.Base(filePath), primaryName))
					}
				} else {
					if tm := getTray(); tm != nil {
						tm.ShowBalloon("Print Error", fmt.Sprintf("Failed to print on %s: %v", primaryName, err))
						tm.RestoreNetworkStatus()
					}
				}
			},
			OnPrintFileToPeer: func(targetPeerName, filePath string) {
				p := cfg.FindPeer(targetPeerName)
				if p == nil || p.URL == "" {
					if tm := getTray(); tm != nil {
						tm.ShowBalloon("Peer Unavailable", fmt.Sprintf("Cannot print: peer %q is not connected yet.", targetPeerName))
						tm.RestoreNetworkStatus()
					}
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				err := printer.SendDirectPrint(ctx, p.URL, cfg.AuthToken, filePath,
					"X-Bridge-Node-Name", cfg.GetNodeName(),
					"X-Bridge-Origin", cfg.GetNodeName(),
				)
				if err == nil {
					if tm := getTray(); tm != nil {
						tm.FlashPrintComplete("Print Complete", fmt.Sprintf("Printed %s on %s!", filepath.Base(filePath), p.Name))
					}
				} else {
					if tm := getTray(); tm != nil {
						tm.ShowBalloon("Print Error", fmt.Sprintf("Failed to print on %s: %v", p.Name, err))
						tm.RestoreNetworkStatus()
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
				go func() {
					if tm := getTray(); tm != nil {
						tm.SetState(tray.StateSyncing, "Bridge: Syncing shared files across devices...")
					}
					for _, p := range cfg.GetPeers() {
						if p.URL != "" {
							syncEngine.ReconcileWithPeer(p.URL)
						}
					}
					time.Sleep(1500 * time.Millisecond)
					if tm := getTray(); tm != nil {
						tm.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
					}
				}()
			},
			OnCopyIP: func() string {
				return installer.GetTailscaleIP()
			},
			OnOpenLogs: func() {
				_ = exec.Command("explorer.exe", cfg.LogDir).Start()
			},
			OnExit: func() {
				log.Printf("[Tray] Exit requested by user. Initiating graceful shutdown...")
				if tm := getTray(); tm != nil {
					tm.Stop()
				}
				triggerShutdown()
			},
		}

		tm, err := tray.NewTrayManager(installer.TargetIcons, cfg.GetNodeName(), cfg.GetPeerName(), cb)
		if err == nil {
			if startErr := tm.Start(); startErr == nil {
				trayMgr.Store(tm)
				tm.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
			} else {
				log.Printf("[Tray] Warning starting tray manager: %v", startErr)
			}
		}
	}

	// Start services after trayMgr and all callbacks are initialized
	if err := printService.Start(); err != nil {
		log.Printf("[Printer] Warning starting print service: %v", err)
	}
	syncStarted := false
	if err := syncEngine.Start(); err != nil {
		if tm := getTray(); tm != nil {
			tm.Stop()
		}
		printService.Stop()
		log.Fatalf("Failed to start sync engine: %v", err)
	}
	syncStarted = true

	var shutdownOnce sync.Once
	shutdownAll := func() {
		shutdownOnce.Do(func() {
			if tm := getTray(); tm != nil {
				tm.Stop()
			}
			multiSpooler.StopAll()
			_ = httpServer.Stop()
			if syncStarted {
				syncEngine.Stop()
			}
			printService.Stop()
		})
	}
	defer shutdownAll()

	go func() {
		if err := httpServer.Start(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server terminated: %v", err)
		}
	}()

	// Start spooler listeners for all known peers (with brief retry for TIME_WAIT after restart)
	for _, p := range cfg.GetPeers() {
		var listenErr error
		for attempt := 0; attempt < 3; attempt++ {
			listenErr = multiSpooler.EnsurePeerListener(p)
			if listenErr == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if listenErr != nil {
			log.Printf("[MultiSpooler] Warning starting spooler listener for %s: %v", p.Name, listenErr)
		}
	}

	if !headless {
		// Periodic connection watcher to keep tray icon dot updated and maintain transitive mesh discovery
		go func() {
			peerFailCounts := make(map[string]int)
			lastLocalTS := initialLocalTS

			for {
				select {
				case <-quitChan:
					return
				case <-time.After(3 * time.Second):
				}

				localTailscale := installer.GetTailscaleIP()
				if localTailscale != "" && localTailscale != lastLocalTS {
					lastLocalTS = localTailscale
					syncEngine.SetLocalURL(fmt.Sprintf("http://%s:%d", localTailscale, cfg.ListenPort))
				}

				tm := getTray()
				if tm == nil {
					continue
				}

				peers := cfg.GetPeers()
				if len(peers) == 0 {
					tm.SetState(tray.StateDisconnected, fmt.Sprintf("Bridge: %s (No other device connected)", cfg.GetNodeName()))
					continue
				}

				var wg sync.WaitGroup
				var mu sync.Mutex
				statusChanged := false
				namesChanged := false
				var newDiscoveredPeers []config.PeerDevice

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
								if config.IsSelfTarget(mp.Name, mp.IP, mp.URL, cfg.GetNodeName(), cfg.ListenPort) || (localTailscale != "" && mp.IP == localTailscale) {
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
					if config.IsSelfTarget(np.Name, np.IP, np.URL, cfg.GetNodeName(), cfg.ListenPort) {
						continue
					}
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
				tm.UpdateNetworkStatus(cfg.GetNodeName(), latestPeers)
			}
		}()
	}

	// Wait for OS shutdown signal or Tray exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-sigChan:
		log.Printf("Received signal %v. Initiating graceful shutdown...", sig)
	case <-quitChan:
		log.Printf("Exit requested via tray. Initiating graceful shutdown...")
	}
	triggerShutdown()
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
	if sumatra != "" {
		cfg.SumatraPath = sumatra
	}

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

	const maxLogBytes = 10 * 1024 * 1024 // 10 MB
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogBytes {
		backupPath := logPath + ".1"
		_ = os.Remove(backupPath)
		_ = os.Rename(logPath, backupPath)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		mw := io.MultiWriter(os.Stdout, f)
		log.SetOutput(mw)
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	}
}
