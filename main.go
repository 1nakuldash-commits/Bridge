package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	"regexp"
	"sort"
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

const upgradeSelfMarker = "__self__"

var (
	errPrintQueuedOffline = errors.New("print queued for offline peer")
	errPrintHandled       = errors.New("print error already handled")
)

func normalizeUpgradeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-upgrade" || arg == "--upgrade" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				out = append(out, "-upgrade="+upgradeSelfMarker)
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

func main() {
	fs := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	serviceFlag := fs.Bool("service", false, "Run silently as background service without tray icon")
	autostartFlag := fs.Bool("autostart", false, "Run in background with taskbar tray icon (used at system boot / startup)")
	trayFlag := fs.Bool("tray", false, "Run in background with taskbar tray icon")
	installFlag := fs.Bool("install", false, "Install Bridge background service")
	setupFlag := fs.Bool("setup", false, "Run interactive setup wizard")
	pairFlag := fs.Bool("pair", false, "Connect / pair a new device (elevated)")
	syncPrintersFlag := fs.Bool("sync-printers", false, "Sync desktop shortcuts and Windows virtual printers")
	uninstallFlag := fs.Bool("uninstall", false, "Uninstall Bridge background service")
	yesFlag := fs.Bool("yes", false, "Skip interactive confirmation prompts (e.g. for -uninstall)")
	yFlag := fs.Bool("y", false, "Alias for -yes")
	statusFlag := fs.Bool("status", false, "Check status of Bridge background service")
	versionFlag := fs.Bool("version", false, "Print application version")
	sendPrintFlag := fs.Bool("send-print", false, "Send file to remote printer (used by Windows Explorer Send-To)")

	upgradeFlag := fs.String("upgrade", "", "Upgrade Bridge installation using specified binary")
	nameFlag := fs.String("name", "", "Name of this PC for automated install")
	peerFlag := fs.String("peer", "", "Peer Tailscale IP/URL for automated install")
	tokenFlag := fs.String("token", "", "Auth token for automated install")
	targetFlag := fs.String("target", "", "Target peer device name or IP for print dispatch")

	configPath := fs.String("config", "", "Path to config.json")

	if err := fs.Parse(normalizeUpgradeArgs(os.Args[1:])); err != nil {
		EnsureConsole()
		fs.SetOutput(os.Stderr)
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "Usage of %s:\n", filepath.Base(os.Args[0]))
			fs.PrintDefaults()
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n\nUsage of %s:\n", err, filepath.Base(os.Args[0]))
		fs.PrintDefaults()
		os.Exit(2)
	}

	resolvedConfig := *configPath
	if resolvedConfig == "" {
		resolvedConfig = installer.TargetConfig
	}
	skipConfirm := *yesFlag || *yFlag

	// 1. Version Mode
	if *versionFlag {
		EnsureConsole()
		fmt.Println("Bridge v2.3.0 (Zero-Defect 1:1 Scale & Instant Mesh Sync Edition)")
		return
	}

	// 2. Right-Click SendTo Handler ("Print on <PeerName>") or verified existing positional file
	if *sendPrintFlag {
		targetFile := ""
		if len(fs.Args()) > 0 {
			targetFile = fs.Args()[0]
		}
		if targetFile != "" {
			handleSendPrint(targetFile, *targetFlag, resolvedConfig)
		}
		return
	}
	if len(fs.Args()) > 0 && !*serviceFlag && !*autostartFlag && !*trayFlag && !*installFlag && !*setupFlag && !*pairFlag && !*syncPrintersFlag && !*uninstallFlag && !*statusFlag && *upgradeFlag == "" {
		firstArg := fs.Args()[0]
		if info, statErr := os.Stat(firstArg); statErr == nil && !info.IsDir() {
			handleSendPrint(firstArg, *targetFlag, resolvedConfig)
			return
		}
		EnsureConsole()
		fmt.Fprintf(os.Stderr, "Error: unrecognized argument or file not found: %q\n", firstArg)
		fmt.Fprintf(os.Stderr, "Run '%s -help' for valid options (for example: -status, -setup, -pair, -upgrade, -uninstall, -sync-printers).\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}

	// 3. Query Status Mode
	if *statusFlag {
		EnsureConsole()
		printStatus(resolvedConfig)
		return
	}

	// 4. Uninstall Mode
	if *uninstallFlag {
		if !installer.IsElevated() {
			requireElevation(os.Args[1:])
			return
		}
		EnsureConsole()
		if !skipConfirm {
			fmt.Print("Are you sure you want to completely uninstall Bridge? [y/N]: ")
			ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			trimmed := strings.TrimSpace(strings.ToLower(ans))
			if trimmed != "y" && trimmed != "yes" {
				fmt.Println("Uninstall canceled.")
				return
			}
		}
		if err := installer.UninstallService(); err != nil {
			fmt.Fprintf(os.Stderr, "Uninstall failed: %v\n", err)
			if !isRunningInsideAppDir() {
				if fi, statErr := os.Stdin.Stat(); statErr == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
					fmt.Println("Press Enter to close...")
					_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
				}
			}
			os.Exit(1)
		}
		// Exit immediately without blocking so C:\BridgeApp\bridge.exe releases its lock cleanly.
		return
	}

	// 5. Sync Printers Mode
	if *syncPrintersFlag {
		if !installer.IsElevated() {
			requireElevation(os.Args[1:])
			return
		}
		EnsureConsole()
		cfg, err := config.LoadConfig(resolvedConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load configuration from %s: %v\n", resolvedConfig, err)
			os.Exit(1)
		}
		peers := cfg.GetPeers()
		res := installer.SyncBridgeShortcutsForPeers(peers)
		if len(peers) == 0 {
			fmt.Println("[*] No peer devices configured; desktop shared shortcut and Send-To shortcuts synchronized.")
		}
		for _, name := range res.SucceededPrinters {
			fmt.Printf("[OK] Windows Virtual Printer ready: 'Print on %s'\n", name)
		}
		for name, pErr := range res.PrinterErrors {
			fmt.Fprintf(os.Stderr, "[!] Windows Virtual Printer 'Print on %s' failed: %v\n", name, pErr)
		}
		if res.ElevationRequired {
			fmt.Fprintln(os.Stderr, "[!] Administrator privileges are required to register virtual printers.")
		}
		if len(res.PrinterErrors) > 0 || res.ElevationRequired {
			os.Exit(1)
		}
		return
	}

	// 6. Pair Mode (elevated pairing + printer creation, then enters Dashboard)
	if *pairFlag {
		if !installer.IsElevated() {
			requireElevation(os.Args[1:])
			return
		}
		if !installer.AcquireInteractiveLock() {
			return
		}
		defer installer.ReleaseInteractiveLock()
		EnsureConsole()

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
		res := installer.SyncBridgeShortcutsForPeers(cfg.GetPeers())
		for name, pErr := range res.PrinterErrors {
			fmt.Fprintf(os.Stderr, "[!] Virtual printer 'Print on %s' warning: %v\n", name, pErr)
		}
		if err := installer.RunDashboard(); err != nil {
			fmt.Fprintf(os.Stderr, "Dashboard error: %v\n", err)
		}
		return
	}

	// 7. Upgrade Mode
	if *upgradeFlag != "" {
		if !installer.IsElevated() {
			requireElevation(os.Args[1:])
			return
		}
		EnsureConsole()
		installer.UnregisterInteractiveStartup()

		cfg, _ := config.LoadConfig(resolvedConfig)
		reader := bufio.NewReader(os.Stdin)
		currentExe, _ := os.Executable()

		var upgradeErr error
		if *upgradeFlag == upgradeSelfMarker || (currentExe != "" && strings.EqualFold(filepath.Clean(*upgradeFlag), filepath.Clean(currentExe))) {
			upgradeErr = installer.RunUpgradeWithDiscovery(cfg, reader)
		} else {
			upgradeErr = installer.UpgradeInstallation(*upgradeFlag)
			if upgradeErr == nil {
				if loaded, loadErr := config.LoadConfig(resolvedConfig); loadErr == nil && loaded != nil {
					cfg = loaded
				}
				upgradeErr = installer.PromptNewDiscoveries(cfg, reader)
			}
		}

		if upgradeErr != nil {
			fmt.Fprintf(os.Stderr, "Upgrade failed: %v\n", upgradeErr)
			fmt.Println("Press Enter to close...")
			_, _ = reader.ReadString('\n')
			os.Exit(1)
		}
		fmt.Println("Press Enter to close...")
		_, _ = reader.ReadString('\n')
		return
	}

	// 8. Automated / Headless Install Mode
	if *installFlag && *peerFlag != "" {
		if !installer.IsElevated() {
			requireElevation(os.Args[1:])
			return
		}
		EnsureConsole()
		installer.UnregisterInteractiveStartup()
		runHeadlessInstall(*nameFlag, *peerFlag, *tokenFlag)
		return
	}

	// 9. Explicit Setup Mode (-setup or -install without peer)
	if *setupFlag || (*installFlag && *peerFlag == "") {
		if !installer.IsElevated() {
			requireElevation(os.Args[1:])
			return
		}
		if !installer.AcquireInteractiveLock() {
			return
		}
		defer installer.ReleaseInteractiveLock()
		EnsureConsole()
		installer.SetDashboardConsoleTitle("Bridge Multi-Device Setup")

		installer.UnregisterInteractiveStartup()
		if err := installer.RunInteractiveSetup(); err != nil {
			fmt.Fprintf(os.Stderr, "Setup error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// 10. Background / Autostart Mode (System boot or background daemon - NEVER allocate a console)
	if *autostartFlag || *trayFlag || *serviceFlag {
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

	// 11. Interactive Launch Mode (User double-clicked bridge.exe or ran from terminal)
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
		requireElevation(os.Args[1:])
		return
	}

	// Enforce single interactive terminal: if already open, bring existing window to front
	if !installer.AcquireInteractiveLock() {
		return
	}
	defer installer.ReleaseInteractiveLock()
	EnsureConsole()

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

func requireElevation(args []string) {
	elevateOrExit(args, installer.SelfElevate, EnsureConsole, os.Stderr, os.Exit)
}

func elevateOrExit(args []string, elevateFn func([]string) error, ensureConsoleFn func(), errOut io.Writer, exitFn func(int)) {
	if err := elevateFn(args); err != nil {
		if ensureConsoleFn != nil {
			ensureConsoleFn()
		}
		if errOut != nil {
			fmt.Fprintf(errOut, "Elevation cancelled or failed: %v\n", err)
		}
		if exitFn != nil {
			exitFn(1)
		}
	}
}

func isRunningInsideAppDir() bool {
	currExe, err := os.Executable()
	if err != nil || strings.TrimSpace(currExe) == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(filepath.Clean(currExe)), strings.ToLower(filepath.Clean(installer.TargetAppDir)))
}

func enqueueOfflineSendPrint(queueDir, peerName, peerURL, absPath string) bool {
	if strings.TrimSpace(queueDir) == "" {
		queueDir = printer.DefaultOutboundQueueDir
	}
	oq := printer.NewOutboundQueue(queueDir)
	if _, qErr := oq.Enqueue(peerName, peerURL, absPath, printer.OutboundJobMeta{JobName: filepath.Base(absPath)}); qErr == nil {
		fmt.Fprintf(os.Stderr, "Peer %s is currently offline; queued %s in %s for automatic delivery.\n", peerName, filepath.Base(absPath), oq.Dir())
		return true
	}
	return false
}

func handleSendPrint(filePath, targetPeer, cfgPath string) {
	if err := handleSendPrintWithQueue(filePath, targetPeer, cfgPath, printer.DefaultOutboundQueueDir); err != nil {
		os.Exit(1)
	}
}

func handleSendPrintWithQueue(filePath, targetPeer, cfgPath, queueDir string) error {
	if strings.TrimSpace(cfgPath) == "" {
		cfgPath = installer.TargetConfig
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration from %s: %v\n", cfgPath, err)
		return nil
	}

	// Determine destination peer URL
	peerURL := ""
	peerName := targetPeer
	if targetPeer != "" {
		if p := cfg.FindPeer(targetPeer); p != nil {
			peerURL = p.URL
			peerName = p.Name
		} else {
			fmt.Fprintf(os.Stderr, "Target peer %q was not found in Bridge configuration.\n", targetPeer)
			return nil
		}
	} else {
		peerURL = cfg.GetPeerURL()
		peerName = cfg.GetPeerName()
	}

	// Try sending via local running bridge instance first
	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/print-local", cfg.ListenPort), nil)
	if err == nil {
		req.Header.Set("X-Bridge-Token", cfg.AuthToken)
		req.Header.Set("X-Bridge-FilePath", absPath)
		if targetPeer != "" {
			req.Header.Set("X-Bridge-Target", targetPeer)
		}
		resp, doErr := client.Do(req)
		if doErr == nil {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			bodyMsg := strings.TrimSpace(string(bodyBytes))
			serviceErr := fmt.Errorf("local bridge service returned HTTP %d: %s", resp.StatusCode, bodyMsg)
			if resp.StatusCode == http.StatusPreconditionFailed || isNoRemotePrinterError(serviceErr) {
				fmt.Fprintf(os.Stderr, "Printer Not Connected: No physical printer is connected on %s.\n", peerName)
				return fmt.Errorf("printer not connected on %s: %w", peerName, serviceErr)
			}
			if isPeerUnreachableError(serviceErr) {
				if peerURL != "" && enqueueOfflineSendPrint(queueDir, peerName, peerURL, absPath) {
					return nil
				}
			}
			// Avoid a duplicate 120s direct send attempt when the local service already tried the peer or rejected the job.
			if resp.StatusCode == http.StatusInternalServerError || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusServiceUnavailable {
				if bodyMsg != "" {
					fmt.Fprintf(os.Stderr, "Print to %s failed: %s\n", peerName, bodyMsg)
				} else {
					fmt.Fprintf(os.Stderr, "Print to %s failed: %v\n", peerName, serviceErr)
				}
				return nil
			}
		}
	}

	if peerURL == "" {
		fmt.Fprintf(os.Stderr, "No target peer available to print.\n")
		return nil
	}

	// Direct send fallback when local service is not running or did not handle the request
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	err = printer.SendDirectPrintWithOptions(ctx, peerURL, cfg.AuthToken, absPath, printer.PrintOptions{},
		"X-Bridge-Node-Name", cfg.GetNodeName(),
		"X-Bridge-Origin", cfg.GetNodeName(),
		"X-Bridge-Job-Name", filepath.Base(absPath),
	)
	if err != nil {
		if isNoRemotePrinterError(err) {
			fmt.Fprintf(os.Stderr, "Printer Not Connected: No physical printer is connected on %s.\n", peerName)
			return fmt.Errorf("printer not connected on %s: %w", peerName, err)
		}
		if isPeerUnreachableError(err) && enqueueOfflineSendPrint(queueDir, peerName, peerURL, absPath) {
			return nil
		}
		fmt.Fprintf(os.Stderr, "Print to %s failed: %v\n", peerName, err)
		return nil
	}
	return nil
}

func isNoRemotePrinterError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, printer.ErrNoPhysicalPrinter) {
		return true
	}
	msg := strings.ToLower(err.Error())
	noPrinterHints := []string{
		"http 412",
		"printer not connected",
		"no physical printer",
		"offline or disconnected (workoffline)",
	}
	for _, hint := range noPrinterHints {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

func isPeerUnreachableError(err error) bool {
	if err == nil || isNoRemotePrinterError(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "peer returned http 4") {
		return false
	}
	if strings.Contains(msg, "unsupported") || strings.Contains(msg, "unrecognized binary") {
		return false
	}
	unreachableHints := []string{
		"failed to deliver print job",
		"connection refused",
		"actively refused",
		"timed out",
		"timeout",
		"context deadline exceeded",
		"unreachable",
		"no such host",
		"not connected yet",
		"eof",
		"connection reset",
		"broken pipe",
		"peer returned http 502",
		"peer returned http 503",
		"peer returned http 504",
	}
	for _, hint := range unreachableHints {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

func notifyShortcutSyncResult(res installer.ShortcutSyncResult, tm *tray.TrayManager) {
	if len(res.PrinterErrors) == 0 && !res.ElevationRequired {
		return
	}
	failedNames := make([]string, 0, len(res.PrinterErrors))
	for name, pErr := range res.PrinterErrors {
		log.Printf("[VirtualPrinter] Virtual printer setup failed for 'Print on %s': %v", name, pErr)
		failedNames = append(failedNames, name)
	}
	sort.Strings(failedNames)

	if tm == nil {
		return
	}
	var msg string
	if res.ElevationRequired {
		if len(failedNames) > 0 {
			msg = fmt.Sprintf("Admin privileges required to configure virtual printer(s) (%s). Run 'bridge.exe -sync-printers' as Administrator.", strings.Join(failedNames, ", "))
		} else {
			msg = "Admin privileges required to configure Windows virtual printers. Run 'bridge.exe -sync-printers' as Administrator."
		}
	} else {
		msg = fmt.Sprintf("Virtual printer setup failed for: %s. Run 'bridge.exe -sync-printers' as Administrator to repair.", strings.Join(failedNames, ", "))
	}
	tm.ShowNotificationLevel("Virtual Printer Setup", msg, tray.NotifyWarning)
}

func syncVirtualPrintersAndNotify(peers []config.PeerDevice, tm *tray.TrayManager) {
	res := installer.SyncBridgeShortcutsForPeers(peers)
	notifyShortcutSyncResult(res, tm)
}

var autoGeneratedJobNameRe = regexp.MustCompile(`(?i)^print_\d+(\.[a-z0-9]+)?$`)

func isAutoGeneratedJobName(name string) bool {
	trimmed := strings.TrimSpace(name)
	return trimmed == "" || strings.EqualFold(trimmed, "print_job.pdf") || autoGeneratedJobNameRe.MatchString(trimmed)
}

type printNotifier interface {
	SetPrintingState(tooltip string)
	FlashPrintComplete(title, message string)
	ShowNotificationLevel(title, message string, level tray.NotifyLevel)
	RestoreNetworkStatus()
}

func resolveTargetPrinterLabel(resolveFn func() (string, error)) string {
	if resolveFn == nil {
		return "local printer"
	}
	ch := make(chan string, 1)
	go func() {
		name, err := resolveFn()
		if err == nil {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				ch <- trimmed
				return
			}
		}
		ch <- "local printer"
	}()
	select {
	case name := <-ch:
		return name
	case <-time.After(1 * time.Second):
		return "local printer"
	}
}

func wirePrintServiceNotifications(ps *printer.PrintService, getNotifier func() printNotifier, resolvePrinter func() (string, error)) {
	if ps == nil {
		return
	}
	ps.OnJobStarted = func(job printer.PrintJob) {
		if tm := getNotifier(); tm != nil {
			tm.SetPrintingState(fmt.Sprintf("Bridge: Printing %s...", job.FriendlyLabel()))
		}
	}
	ps.OnJobCompleted = func(job printer.PrintJob) {
		if tm := getNotifier(); tm != nil {
			targetPrinter := resolveTargetPrinterLabel(resolvePrinter)
			tm.FlashPrintComplete("Print Complete", fmt.Sprintf("Printed %s on %s", job.FriendlyLabel(), targetPrinter))
		}
	}
	ps.OnJobFailed = func(job printer.PrintJob, reason string) {
		if tm := getNotifier(); tm != nil {
			tm.ShowNotificationLevel("Print Failed", fmt.Sprintf("Failed to print %s: %s", job.FriendlyLabel(), reason), tray.NotifyError)
			tm.RestoreNetworkStatus()
		}
	}
}

func handleMultiSpoolerSendResult(
	peerName string,
	peerOnline bool,
	targetURL string,
	filePath string,
	sendErr error,
	outboundQueue *printer.OutboundQueue,
	tm *tray.TrayManager,
	notifier printNotifier,
) error {
	if sendErr == nil {
		return nil
	}
	if isNoRemotePrinterError(sendErr) {
		log.Printf("[MultiSpooler] Peer %q has no physical printer connected (%v); skipping queue.", peerName, sendErr)
		if notifier != nil {
			notifier.ShowNotificationLevel("Printer Not Connected", fmt.Sprintf("No physical printer is connected on %s.", peerName), tray.NotifyError)
			notifier.RestoreNetworkStatus()
		}
		return errPrintHandled
	}
	if !peerOnline || isPeerUnreachableError(sendErr) {
		if qErr := queueOfflinePrintJob(outboundQueue, peerName, targetURL, filePath, tm); qErr == nil {
			log.Printf("[MultiSpooler] Peer %q unreachable (%v); queued %s into outbound queue", peerName, sendErr, filepath.Base(filePath))
			return errPrintQueuedOffline
		} else {
			log.Printf("[MultiSpooler] Failed to enqueue offline print for %q: %v", peerName, qErr)
		}
	}
	return sendErr
}

func wireMultiSpoolerNotifications(ms *printer.MultiSpoolerListener, getNotifier func() printNotifier) {
	if ms == nil {
		return
	}
	ms.OnJobStarted = func(peer config.PeerDevice, fileName string) {
		if tm := getNotifier(); tm != nil {
			tm.SetPrintingState(fmt.Sprintf("Bridge: Sending print to %s...", peer.Name))
		}
	}
	ms.OnJobCompleted = func(peer config.PeerDevice, fileName string) {
		if tm := getNotifier(); tm != nil {
			tm.FlashPrintComplete("Print Sent", fmt.Sprintf("Document sent to %s for printing.", peer.Name))
		}
	}
	ms.OnJobFailed = func(peer config.PeerDevice, fileName string, err error) {
		if errors.Is(err, errPrintQueuedOffline) || errors.Is(err, errPrintHandled) {
			return
		}
		if tm := getNotifier(); tm != nil {
			tm.ShowNotificationLevel("Print Error", fmt.Sprintf("Failed to print on %s: %v", peer.Name, err), tray.NotifyError)
			tm.RestoreNetworkStatus()
		}
	}
}

func wireServerPrintRejectedNotification(srv *server.Server, getNotifier func() printNotifier) {
	if srv == nil {
		return
	}
	srv.SetOnPrintRejected(func(sourceNode, reason string) {
		sourceLabel := strings.TrimSpace(sourceNode)
		if sourceLabel == "" {
			sourceLabel = "a remote device"
		}
		log.Printf("[Server] Print from %s rejected (no physical printer): %s", sourceLabel, reason)
		if tm := getNotifier(); tm != nil {
			tm.ShowNotificationLevel(
				"Print Rejected — No Printer",
				fmt.Sprintf("Remote print from %s skipped: no physical printer connected on this PC.", sourceLabel),
				tray.NotifyWarning,
			)
		}
	})
}

func notifyPrintDispatched(tm printNotifier, fileName, peerName string) {
	if tm == nil {
		return
	}
	tm.FlashPrintComplete("Print Sent", fmt.Sprintf("Sent %s to %s for printing.", fileName, peerName))
}

func sendTrayPrintFile(cfg *config.Config, outboundQueue *printer.OutboundQueue, peerName, peerURL, filePath string, tm *tray.TrayManager) {
	var notifier printNotifier
	if tm != nil {
		notifier = tm
	}
	sendTrayPrintFileWithNotifier(cfg, outboundQueue, peerName, peerURL, filePath, tm, notifier)
}

func sendTrayPrintFileWithNotifier(cfg *config.Config, outboundQueue *printer.OutboundQueue, peerName, peerURL, filePath string, tm *tray.TrayManager, notifier printNotifier) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	fileName := filepath.Base(filePath)
	err := printer.SendDirectPrintWithOptions(ctx, peerURL, cfg.AuthToken, filePath, printer.PrintOptions{},
		"X-Bridge-Node-Name", cfg.GetNodeName(),
		"X-Bridge-Origin", cfg.GetNodeName(),
		"X-Bridge-Job-Name", fileName,
	)
	if err == nil {
		if notifier != nil {
			notifyPrintDispatched(notifier, fileName, peerName)
		}
		return
	}
	if isNoRemotePrinterError(err) {
		if notifier != nil {
			notifier.ShowNotificationLevel("Printer Not Connected", fmt.Sprintf("No physical printer is connected on %s.", peerName), tray.NotifyError)
			notifier.RestoreNetworkStatus()
		}
		return
	}
	if isPeerUnreachableError(err) {
		if qErr := queueOfflinePrintJob(outboundQueue, peerName, peerURL, filePath, tm); qErr != nil && notifier != nil {
			notifier.ShowNotificationLevel("Print Error", fmt.Sprintf("Failed to queue print for %s: %v", peerName, qErr), tray.NotifyError)
			notifier.RestoreNetworkStatus()
		}
		return
	}
	if notifier != nil {
		notifier.ShowNotificationLevel("Print Error", fmt.Sprintf("Failed to print on %s: %v", peerName, err), tray.NotifyError)
		notifier.RestoreNetworkStatus()
	}
}

func flushOutboundQueueForPeer(oq *printer.OutboundQueue, cfg *config.Config, peer config.PeerDevice, tm *tray.TrayManager) {
	var notifier printNotifier
	if tm != nil {
		notifier = tm
	}
	flushOutboundQueueForPeerWithNotifier(oq, cfg, peer, tm, notifier)
}

func flushOutboundQueueForPeerWithNotifier(oq *printer.OutboundQueue, cfg *config.Config, peer config.PeerDevice, tm *tray.TrayManager, notifier printNotifier) {
	if oq == nil || cfg == nil {
		return
	}
	totalSent := 0
	for {
		var skippedJob *printer.OutboundJobMeta
		sent, err := oq.FlushPeer(context.Background(), peer.Name, peer.URL, func(ctx context.Context, job printer.OutboundJobMeta) error {
			sendCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()
			opts := printer.PrintOptions{
				PaperSize:   job.PaperSize,
				Orientation: job.Orientation,
				PageCount:   job.PageCount,
			}
			extraHeaders := []string{
				"X-Bridge-Node-Name", cfg.GetNodeName(),
				"X-Bridge-Origin", cfg.GetNodeName(),
			}
			if jn := strings.TrimSpace(job.JobName); jn != "" && !isAutoGeneratedJobName(jn) {
				extraHeaders = append(extraHeaders, "X-Bridge-Job-Name", jn)
			}
			sendErr := printer.SendDirectPrintWithOptions(sendCtx, job.PeerURL, cfg.AuthToken, job.FilePath, opts, extraHeaders...)
			if sendErr != nil && isNoRemotePrinterError(sendErr) {
				jobCopy := job
				skippedJob = &jobCopy
			}
			return sendErr
		})
		totalSent += sent
		if err == nil {
			break
		}
		if isNoRemotePrinterError(err) && skippedJob != nil {
			remErr := oq.Remove(skippedJob.ID)
			if remErr != nil && !errors.Is(remErr, os.ErrNotExist) {
				log.Printf("[OutboundQueue] Warning removing skipped job %s for peer %s: %v", skippedJob.ID, peer.Name, remErr)
				break
			}
			log.Printf("[OutboundQueue] Removed queued print %s for %s: peer has no physical printer connected (%v)", skippedJob.ID, peer.Name, err)
			if notifier != nil {
				jobLabel := strings.TrimSpace(skippedJob.JobName)
				if jobLabel == "" {
					jobLabel = filepath.Base(skippedJob.FilePath)
				}
				notifier.ShowNotificationLevel(
					"Printer Not Connected",
					fmt.Sprintf("Queued print %s skipped: no physical printer is connected on %s.", jobLabel, peer.Name),
					tray.NotifyWarning,
				)
			}
			continue
		}
		log.Printf("[OutboundQueue] Flush for peer %s (%s) stopped after %d sent: %v", peer.Name, peer.URL, totalSent, err)
		break
	}
	if tm != nil {
		tm.SetQueuedPrintCount(oq.Count())
	}
	if notifier != nil && totalSent > 0 {
		notifier.ShowNotificationLevel("Queued Prints Sent", fmt.Sprintf("Sent %d Queued Print(s) to %s", totalSent, peer.Name), tray.NotifyInfo)
	}
}

func queueOfflinePrintJob(oq *printer.OutboundQueue, peerName, peerURL, filePath string, tm *tray.TrayManager) error {
	if oq == nil {
		return errors.New("outbound queue unavailable")
	}
	meta := printer.OutboundJobMeta{
		JobName: filepath.Base(filePath),
	}
	if _, err := oq.Enqueue(peerName, peerURL, filePath, meta); err != nil {
		return err
	}
	if tm != nil {
		tm.SetQueuedPrintCount(oq.Count())
		tm.ShowNotificationLevel(
			fmt.Sprintf("Peer %s Offline — Print Queued", peerName),
			fmt.Sprintf("Queued %s. It will print automatically when %s comes online.", filepath.Base(filePath), peerName),
			tray.NotifyWarning,
		)
		tm.RestoreNetworkStatus()
	}
	return nil
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

	// Ensure desktop shortcuts and virtual printers are configured for all peers (errors surfaced once tray initializes)
	startupPrinterRes := installer.SyncBridgeShortcutsForPeers(cfg.GetPeers())

	stateMgr, err := bridgesync.NewStateManager(cfg.StateFile)
	if err != nil {
		log.Fatalf("Failed to initialize state manager: %v", err)
	}

	// 1. Outbound Print Queue (holds jobs when peers are offline, flushes automatically on recovery)
	outboundQueue := printer.NewOutboundQueue(printer.DefaultOutboundQueueDir)

	// 2. Silent Print Service (Receives incoming print jobs from any peer)
	printService := printer.NewPrintService(cfg.SumatraPath, cfg.PrintTimeoutSec, cfg.PrintQueueDir, cfg.PrintFailedDir)

	// 3. Multi-Peer Mesh Sync Engine
	syncEngine := bridgesync.NewSyncEngine(cfg.SharedDir, "", cfg.AuthToken, stateMgr)
	syncEngine.SetNodeName(cfg.GetNodeName())
	syncEngine.UpdatePeerURLs(cfg.GetPeerURLs())
	initialLocalTS := installer.GetTailscaleIP()
	if initialLocalTS != "" {
		syncEngine.SetLocalURL(fmt.Sprintf("http://%s:%d", initialLocalTS, cfg.ListenPort))
	}

	// 4. HTTP API Server & thread-safe TrayManager reference
	var trayMgr atomic.Pointer[tray.TrayManager]
	getTray := func() *tray.TrayManager {
		return trayMgr.Load()
	}
	getPrintNotifier := func() printNotifier {
		if tm := getTray(); tm != nil {
			return tm
		}
		return nil
	}

	quitChan := make(chan struct{})
	var quitOnce sync.Once
	triggerShutdown := func() {
		quitOnce.Do(func() {
			close(quitChan)
		})
	}
	var bgWg sync.WaitGroup

	httpServer := server.NewServer(cfg, stateMgr, printService)
	httpServer.SetSyncEngine(syncEngine)
	httpServer.SetOnSyncReceived(func(relPath string) {
		if tm := getTray(); tm != nil {
			tm.FlashSyncCompleteDebounced(relPath)
		}
	})

	// Dispatch feedback hook for SendTo right-click printing
	httpServer.SetOnPrintDispatched(func(fileName, peerName string) {
		notifyPrintDispatched(getPrintNotifier(), fileName, peerName)
	})
	wireServerPrintRejectedNotification(httpServer, getPrintNotifier)

	// 5. Windows Spooler Multi-Listener (One listener per peer virtual printer port)
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

		host := config.ExtractIP(targetURL)
		if parsedIP := net.ParseIP(host); (parsedIP != nil && parsedIP.IsLoopback()) ||
			strings.EqualFold(host, "127.0.0.1") ||
			strings.EqualFold(host, "localhost") ||
			host == "::1" ||
			config.IsSelfTarget(targetPeer.Name, targetPeer.IP, targetURL, cfg.GetNodeName(), cfg.ListenPort) {
			return fmt.Errorf("refusing to route virtual print to self/loopback target (%s)", targetURL)
		}

		if targetURL == "" {
			if qErr := queueOfflinePrintJob(outboundQueue, targetPeer.Name, targetURL, filePath, getTray()); qErr == nil {
				return errPrintQueuedOffline
			}
			return fmt.Errorf("peer %s is not connected yet (no reachable URL or IP)", targetPeer.Name)
		}

		spoolPort := targetPeer.SpoolPort
		if spoolPort <= 0 {
			spoolPort = peer.SpoolPort
		}
		log.Printf("[MultiSpooler] Routing virtual print from port %d to peer %q (%s)", spoolPort, targetPeer.Name, targetURL)

		timeout := 60 * time.Second
		if !targetPeer.Online {
			timeout = 4 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		sendErr := printer.SendDirectPrintWithOptions(ctx, targetURL, cfg.AuthToken, filePath, printer.PrintOptions{},
			"X-Bridge-Node-Name", cfg.GetNodeName(),
			"X-Bridge-Origin", cfg.GetNodeName(),
		)
		return handleMultiSpoolerSendResult(targetPeer.Name, targetPeer.Online, targetURL, filePath, sendErr, outboundQueue, getTray(), getPrintNotifier())
	})

	wireMultiSpoolerNotifications(multiSpooler, getPrintNotifier)

	// Dynamic peer update wiring: when a peer connects or registers
	httpServer.SetPeerUpdater(func(peer config.PeerDevice, isNew bool) {
		if config.IsSelfTarget(peer.Name, peer.IP, peer.URL, cfg.GetNodeName(), cfg.ListenPort) {
			return // Strictly ignore self
		}
		cfg.SetPeerOnlineStatus(peer.URL, true)
		latestPeers := cfg.GetPeers()
		if err := multiSpooler.SyncListeners(latestPeers); err != nil {
			log.Printf("[PeerUpdater] Warning syncing spooler listeners: %v", err)
		}
		syncEngine.UpdatePeerURLs(cfg.GetPeerURLs())
		if tm := getTray(); tm != nil {
			tm.UpdateNetworkStatus(cfg.GetNodeName(), latestPeers)
		}
		_ = config.SaveConfig(configPath, cfg)

		if peer.URL != "" {
			bgWg.Add(1)
			go func(pURL string) {
				defer bgWg.Done()
				healthy, _, printerReady, printerName, _ := checkPeerHealthy(pURL, cfg.AuthToken, cfg.GetNodeName())
				if !healthy {
					return
				}
				if cfg.SetPeerPrinterStatus(pURL, printerReady, printerName) {
					if tm := getTray(); tm != nil {
						tm.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
					}
				}
			}(peer.URL)
		}

		go func(peersSnap []config.PeerDevice) {
			syncVirtualPrintersAndNotify(peersSnap, getTray())
		}(latestPeers)

		// Catch-up synchronize files & flush queued prints when a brand new peer connects
		if isNew {
			syncEngine.ReconcileWithPeer(peer.URL)
			go flushOutboundQueueForPeer(outboundQueue, cfg, peer, getTray())

			// Mesh Gossip Propagation: announce newly discovered peer to all other known peers
			localTailscale := installer.GetTailscaleIP()
			for _, other := range latestPeers {
				if other.URL != "" && other.URL != peer.URL && !strings.EqualFold(other.Name, peer.Name) {
					go installer.IntroduceToRemote(other.URL, peer.Name, peer.IP, cfg.AuthToken)
				}
			}
			if localTailscale != "" && peer.URL != "" {
				go installer.IntroduceToRemote(peer.URL, cfg.GetNodeName(), localTailscale, cfg.AuthToken)
			}
		}
	})

	// Dynamic peer removal wiring: when a peer uninstalls or is deregistered via DELETE /api/peers
	httpServer.SetPeerRemover(func(peer config.PeerDevice) {
		latestPeers := cfg.GetPeers()
		if err := multiSpooler.SyncListeners(latestPeers); err != nil {
			log.Printf("[PeerRemover] Warning syncing spooler listeners: %v", err)
		}
		if peer.URL != "" {
			syncEngine.RemovePeerURL(peer.URL)
		}
		if tm := getTray(); tm != nil {
			tm.UpdateNetworkStatus(cfg.GetNodeName(), latestPeers)
			if peer.Name != "" {
				tm.ShowNotificationLevel("Device Removed", fmt.Sprintf("%s was removed from your Bridge network.", peer.Name), tray.NotifyInfo)
			}
		}
		go func(removedPeer config.PeerDevice, peersSnap []config.PeerDevice) {
			if removedPeer.Name != "" {
				if err := installer.RemoveVirtualPrinter(removedPeer.Name); err != nil {
					log.Printf("[PeerRemover] Warning removing virtual printer for %s: %v", removedPeer.Name, err)
				}
			}
			syncVirtualPrintersAndNotify(peersSnap, getTray())
		}(peer, latestPeers)
	})

	// Connect callbacks to update tray state before starting services
	wirePrintServiceNotifications(printService, getPrintNotifier, printer.GetTargetPhysicalPrinter)

	syncEngine.SetOnSyncStarted(func(relPath string) {
		if tm := getTray(); tm != nil {
			tm.SetSyncingState(fmt.Sprintf("Bridge: Uploading/Syncing %s...", relPath))
		}
	})
	syncEngine.SetOnSyncCompleted(func(relPath string) {
		if tm := getTray(); tm != nil {
			tm.FlashSyncCompleteDebounced(relPath)
		}
	})
	syncEngine.SetOnSyncFailed(func(relPath string, syncErr error) {
		if tm := getTray(); tm != nil {
			tm.ShowNotificationLevel("Sync Error", fmt.Sprintf("Failed to sync %s: %v", filepath.Base(relPath), syncErr), tray.NotifyError)
			tm.RestoreNetworkStatus()
		}
	})

	// 6. System Tray Icon ("Status Dot" in Windows Taskbar) initialized before service goroutines start
	if !headless {
		resolveCurrentExe := func() string {
			if exe, exeErr := os.Executable(); exeErr == nil && strings.TrimSpace(exe) != "" {
				return exe
			}
			return installer.TargetExe
		}

		cb := tray.TrayCallbacks{
			OnOpenShared: func() {
				_ = exec.Command("explorer.exe", cfg.SharedDir).Start()
			},
			OnOpenDashboard: func() {
				exe := resolveCurrentExe()
				if err := exec.Command(exe).Start(); err != nil {
					log.Printf("[Tray] Failed to open dashboard (%s): %v", exe, err)
				}
			},
			OnPairDevice: func() {
				exe := resolveCurrentExe()
				if err := exec.Command(exe, "-pair").Start(); err != nil {
					log.Printf("[Tray] Failed to launch pairing wizard (%s -pair): %v", exe, err)
				}
			},
			OnConnectDiscovered: func(device tray.DiscoveredDevice) {
				tm := getTray()
				peerURL := strings.TrimRight(strings.TrimSpace(device.URL), "/")
				if peerURL == "" && device.IP != "" {
					port := device.Port
					if port <= 0 {
						port = 8765
					}
					peerURL = fmt.Sprintf("http://%s:%d", device.IP, port)
				}
				if peerURL == "" {
					return
				}

				healthy, learnedName, printerReady, printerName, _ := checkPeerHealthy(peerURL, cfg.AuthToken, cfg.GetNodeName())
				if !healthy {
					if tm != nil {
						tm.ShowNotificationLevel("Connection Failed", fmt.Sprintf("Could not reach %s (%s). Verify Bridge is running on that device.", device.Name, device.IP), tray.NotifyWarning)
					}
					return
				}
				peerName := learnedName
				if peerName == "" {
					peerName = device.Name
				}
				if config.IsSelfTarget(peerName, device.IP, peerURL, cfg.GetNodeName(), cfg.ListenPort) {
					return
				}

				added, isNew := cfg.AddOrUpdatePeer(peerName, device.IP, peerURL)
				if added.Name == "" {
					return
				}
				cfg.SetPeerPrinterStatus(added.URL, printerReady, printerName)
				if updated := cfg.FindPeer(added.URL); updated != nil {
					added = *updated
				}
				_ = config.SaveConfig(configPath, cfg)

				localTS := installer.GetTailscaleIP()
				if localTS != "" {
					go installer.IntroduceToRemote(added.URL, cfg.GetNodeName(), localTS, cfg.AuthToken)
				}

				latestPeers := cfg.GetPeers()
				_ = multiSpooler.SyncListeners(latestPeers)
				syncEngine.UpdatePeerURLs(cfg.GetPeerURLs())
				if isNew {
					syncEngine.ReconcileWithPeer(added.URL)
					go flushOutboundQueueForPeer(outboundQueue, cfg, added, getTray())
				}

				if tm != nil {
					remaining := make([]tray.DiscoveredDevice, 0)
					for _, d := range tm.GetDiscoveredPeers() {
						if !strings.EqualFold(d.IP, device.IP) && !strings.EqualFold(strings.TrimRight(d.URL, "/"), peerURL) {
							remaining = append(remaining, d)
						}
					}
					tm.SetDiscoveredPeers(remaining)
					tm.UpdateNetworkStatus(cfg.GetNodeName(), latestPeers)
					tm.ShowNotificationLevel("Device Connected", fmt.Sprintf("Connected to %s (%s)!", added.Name, added.IP), tray.NotifyInfo)
				}

				go syncVirtualPrintersAndNotify(latestPeers, getTray())
			},
			OnPrintFile: func(filePath string) {
				primaryURL := cfg.GetPeerURL()
				primaryName := cfg.GetPeerName()
				if primaryURL == "" {
					if tm := getTray(); tm != nil {
						tm.ShowNotificationLevel("No Peer Connected", "Cannot print: no peer device is connected yet.", tray.NotifyWarning)
						tm.RestoreNetworkStatus()
					}
					return
				}
				sendTrayPrintFile(cfg, outboundQueue, primaryName, primaryURL, filePath, getTray())
			},
			OnPrintFileToPeer: func(targetPeerName, filePath string) {
				p := cfg.FindPeer(targetPeerName)
				if p == nil || p.URL == "" {
					if tm := getTray(); tm != nil {
						tm.ShowNotificationLevel("Peer Unavailable", fmt.Sprintf("Cannot print: peer %q is not connected yet.", targetPeerName), tray.NotifyWarning)
						tm.RestoreNetworkStatus()
					}
					return
				}
				sendTrayPrintFile(cfg, outboundQueue, p.Name, p.URL, filePath, getTray())
			},
			OnQueueOfflinePrint: func(targetPeerName, filePath string) {
				tm := getTray()
				if err := printer.ValidatePrintableFormat(filePath); err != nil {
					if tm != nil {
						tm.ShowNotificationLevel("Print Error", fmt.Sprintf("Cannot queue %s: %v", filepath.Base(filePath), err), tray.NotifyError)
						tm.RestoreNetworkStatus()
					}
					return
				}
				peerName := targetPeerName
				peerURL := ""
				if p := cfg.FindPeer(targetPeerName); p != nil {
					peerName = p.Name
					peerURL = p.URL
				}
				if err := queueOfflinePrintJob(outboundQueue, peerName, peerURL, filePath, tm); err != nil {
					if tm != nil {
						tm.ShowNotificationLevel("Print Error", fmt.Sprintf("Failed to queue print for %s: %v", peerName, err), tray.NotifyError)
						tm.RestoreNetworkStatus()
					}
				}
			},
			OnShowStatus: func() string {
				targetPrinter, pErr := printer.GetTargetPhysicalPrinter()
				return buildNetworkStatusText(
					cfg.GetNodeName(),
					installer.GetTailscaleIP(),
					targetPrinter,
					pErr,
					printService.QueueDepth(),
					outboundQueue.Count(),
					cfg.SharedDir,
					cfg.GetPeers(),
				)
			},
			OnResync: func() {
				go func() {
					if tm := getTray(); tm != nil {
						tm.SetSyncingState("Bridge: Syncing shared files across devices...")
					}
					syncEngine.ResyncNow()
					if tm := getTray(); tm != nil {
						tm.FlashSyncComplete("Sync Complete", "Shared folder synchronized across devices!")
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
				tm.SetQueuedPrintCount(outboundQueue.Count())
				tm.UpdateNetworkStatus(cfg.GetNodeName(), cfg.GetPeers())
				notifyShortcutSyncResult(startupPrinterRes, tm)
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
			triggerShutdown()
			bgWg.Wait()
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

	// Periodic connection watcher runs in BOTH -service (headless) and -autostart/-tray modes
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()

		peerFailCounts := make(map[string]int)
		lastLocalTS := initialLocalTS
		var notifiedMu sync.Mutex
		notifiedDiscovered := make(map[string]bool)
		var discoveryInFlight atomic.Bool
		tick := 0

		for {
			waitDur := 3 * time.Second
			if tick == 0 {
				waitDur = 200 * time.Millisecond
			}
			select {
			case <-quitChan:
				return
			case <-time.After(waitDur):
			}
			tick++

			localTailscale := installer.GetTailscaleIP()
			if localTailscale != "" && localTailscale != lastLocalTS {
				lastLocalTS = localTailscale
				syncEngine.SetLocalURL(fmt.Sprintf("http://%s:%d", localTailscale, cfg.ListenPort))
			}

			peers := cfg.GetPeers()
			if len(peers) > 0 {
				var wg sync.WaitGroup
				var mu sync.Mutex
				statusChanged := false
				var newlyOnline []config.PeerDevice
				type learnedNameUpdate struct {
					url  string
					name string
				}
				var learnedUpdates []learnedNameUpdate
				var gossipCandidates []config.PeerDevice

				for i := range peers {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						p := peers[idx]
						if p.URL == "" {
							return
						}
						healthy, learnedName, printerReady, printerName, hr, _ := checkPeerHealthDetail(p.URL, cfg.AuthToken, cfg.GetNodeName())

						mu.Lock()
						defer mu.Unlock()

						wasOnline := peers[idx].Online
						if healthy {
							peerFailCounts[p.URL] = 0
							prevReadyKnown := peers[idx].PrinterReady != nil
							prevReady := prevReadyKnown && *peers[idx].PrinterReady
							if !prevReadyKnown || prevReady != printerReady || peers[idx].PrinterName != printerName {
								readyCopy := printerReady
								peers[idx].PrinterReady = &readyCopy
								peers[idx].PrinterName = printerName
								cfg.SetPeerPrinterStatus(p.URL, printerReady, printerName)
								statusChanged = true
							}
							if !wasOnline {
								peers[idx].Online = true
								cfg.SetPeerOnlineStatus(p.URL, true)
								statusChanged = true
								newlyOnline = append(newlyOnline, peers[idx])
							}
						} else {
							peerFailCounts[p.URL]++
							// Debounce ping jitter: Require 3 consecutive failed health checks (~9s)
							// before marking a connected peer as offline.
							if peerFailCounts[p.URL] >= 3 && wasOnline {
								peers[idx].Online = false
								cfg.SetPeerOnlineStatus(p.URL, false)
								statusChanged = true
							}
						}

						if healthy && hr != nil {
							peers[idx].LastSeen = time.Now()
							if learnedName != "" && learnedName != p.Name && !p.CustomName {
								learnedUpdates = append(learnedUpdates, learnedNameUpdate{url: p.URL, name: learnedName})
							}
							gossipCandidates = append(gossipCandidates, hr.Peers...)
						}
					}(i)
				}
				wg.Wait()

				// Apply config writes, sync reconciliation, and outbound print flushes OUTSIDE mu.Lock()
				namesChanged := false
				for _, lu := range learnedUpdates {
					log.Printf("[HealthWatcher] Learned peer name for %s: %s", lu.url, lu.name)
					if cfg.SetPeerName(lu.url, lu.name) {
						namesChanged = true
					}
				}
				for _, p := range newlyOnline {
					syncEngine.ReconcileWithPeer(p.URL)
					peerCopy := p
					bgWg.Add(1)
					go func() {
						defer bgWg.Done()
						flushOutboundQueueForPeer(outboundQueue, cfg, peerCopy, getTray())
					}()
				}

				// Mesh Gossip: validate candidates against ignore list and active health probe before adding
				hasNewPeers := false
				seenGossip := make(map[string]bool)
				for _, mp := range gossipCandidates {
					mpIP := strings.TrimSpace(mp.IP)
					mpURL := strings.TrimRight(strings.TrimSpace(mp.URL), "/")
					if mpIP == "" && mpURL != "" {
						mpIP = config.ExtractIP(mpURL)
					}
					if mpURL == "" && mpIP != "" {
						mpURL = fmt.Sprintf("http://%s:8765", mpIP)
					}
					if mpURL == "" {
						continue
					}
					key := strings.ToLower(mpURL)
					if seenGossip[key] {
						continue
					}
					seenGossip[key] = true

					if config.IsSelfTarget(mp.Name, mpIP, mpURL, cfg.GetNodeName(), cfg.ListenPort) || (localTailscale != "" && mpIP == localTailscale) {
						continue
					}
					if cfg.FindPeer(mp.Name) != nil || cfg.FindPeer(mpIP) != nil || cfg.FindPeer(mpURL) != nil {
						continue
					}
					if cfg.IsPeerIgnored(mp.Name, mpIP, mpURL) {
						continue
					}

					// Actively probe mpURL via checkPeerHealthy before trusting gossip
					gHealthy, gName, gPrinterReady, gPrinterName, _ := checkPeerHealthy(mpURL, cfg.AuthToken, cfg.GetNodeName())
					if !gHealthy {
						continue
					}
					candidateName := gName
					if candidateName == "" {
						candidateName = mp.Name
					}
					if config.IsSelfTarget(candidateName, mpIP, mpURL, cfg.GetNodeName(), cfg.ListenPort) {
						continue
					}
					if cfg.IsPeerIgnored(candidateName, mpIP, mpURL) {
						continue
					}
					if cfg.FindPeer(mpIP) != nil || cfg.FindPeer(mpURL) != nil {
						continue
					}

					added, isNew := cfg.AddOrUpdatePeer(candidateName, mpIP, mpURL)
					if isNew {
						cfg.SetPeerPrinterStatus(added.URL, gPrinterReady, gPrinterName)
						log.Printf("[MeshGossip] Verified & added new mesh peer: %s (%s)", added.Name, added.URL)
						hasNewPeers = true
						syncEngine.UpdatePeerURLs(cfg.GetPeerURLs())
						_ = multiSpooler.EnsurePeerListener(added)
						if localTailscale != "" {
							go installer.IntroduceToRemote(added.URL, cfg.GetNodeName(), localTailscale, cfg.AuthToken)
						}
					}
				}

				latestPeers := cfg.GetPeers()
				if statusChanged || namesChanged || hasNewPeers {
					if namesChanged || hasNewPeers {
						syncVirtualPrintersAndNotify(latestPeers, getTray())
					}
					_ = multiSpooler.SyncListeners(latestPeers)
				}
				if tm := getTray(); tm != nil {
					tm.SetQueuedPrintCount(outboundQueue.Count())
					tm.UpdateNetworkStatus(cfg.GetNodeName(), latestPeers)
				}
			} else if tm := getTray(); tm != nil {
				tm.SetQueuedPrintCount(outboundQueue.Count())
				tm.UpdateNetworkStatus(cfg.GetNodeName(), nil)
			}

			// Non-intrusive runtime discovery: scan Tailscale every ~30s for unlinked, non-ignored Bridge nodes
			if !headless && (tick == 2 || tick%10 == 0) && discoveryInFlight.CompareAndSwap(false, true) {
				bgWg.Add(1)
				go func(localTS string) {
					defer bgWg.Done()
					defer discoveryInFlight.Store(false)

					select {
					case <-quitChan:
						return
					default:
					}

					tsPeers, dErr := installer.DiscoverTailscalePeers()
					if dErr != nil {
						return
					}

					var discovered []tray.DiscoveredDevice
					var newlySeen []tray.DiscoveredDevice
					nodeName := cfg.GetNodeName()

					for _, dp := range tsPeers {
						if !dp.BridgeActive || !dp.Online || dp.IP == "" {
							continue
						}
						if localTS != "" && dp.IP == localTS {
							continue
						}
						peerURL := fmt.Sprintf("http://%s:8765", dp.IP)
						dispName := strings.TrimSpace(dp.BridgeName)
						if dispName == "" {
							dispName = strings.TrimSpace(dp.HostName)
						}
						if dispName == "" {
							dispName = dp.IP
						}
						if config.IsSelfTarget(dispName, dp.IP, peerURL, nodeName, cfg.ListenPort) {
							continue
						}
						if cfg.FindPeer(dp.IP) != nil || cfg.FindPeer(peerURL) != nil {
							continue
						}
						if cfg.IsPeerIgnored(dispName, dp.IP, peerURL) {
							continue
						}
						dev := tray.DiscoveredDevice{
							Name: dispName,
							IP:   dp.IP,
							URL:  peerURL,
							Port: 8765,
						}
						discovered = append(discovered, dev)
						notifiedMu.Lock()
						alreadyNotified := notifiedDiscovered[dp.IP]
						if !alreadyNotified {
							notifiedDiscovered[dp.IP] = true
						}
						notifiedMu.Unlock()
						if !alreadyNotified {
							newlySeen = append(newlySeen, dev)
						}
					}

					if tm := getTray(); tm != nil {
						tm.SetDiscoveredPeers(discovered)
						for _, d := range newlySeen {
							tm.ShowNotificationLevel(
								"New Bridge Device Found",
								fmt.Sprintf("%s (%s) is running Bridge on Tailscale. Right-click the tray icon to connect.", d.Name, d.IP),
								tray.NotifyInfo,
							)
						}
					}
				}(localTailscale)
			}
		}
	}()

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

func formatPeerStatusTag(p config.PeerDevice) string {
	if !p.Online {
		return "[Offline]"
	}
	if p.IsPrinterKnownMissing() {
		return "[Online | No Printer]"
	}
	if name := strings.TrimSpace(p.PrinterName); name != "" {
		return fmt.Sprintf("[Online | Printer: %s]", name)
	}
	if p.PrinterReady != nil && *p.PrinterReady {
		return "[Online | Printer Ready]"
	}
	return "[Online]"
}

func buildNetworkStatusText(nodeName, localIP, targetPrinter string, printerErr error, activeJobs, queuedCount int, sharedDir string, peers []config.PeerDevice) string {
	localPrinter := strings.TrimSpace(targetPrinter)
	if printerErr != nil || localPrinter == "" {
		localPrinter = "None (No physical printer connected)"
	}

	onlineCount := 0
	for _, p := range peers {
		if p.Online {
			onlineCount++
		}
	}

	var b strings.Builder
	b.WriteString("Bridge Multi-Device Mesh Status\n\n")
	b.WriteString(fmt.Sprintf("This Device:     %s (%s)\n", nodeName, localIP))
	b.WriteString(fmt.Sprintf("Local Printer:   %s\n", localPrinter))
	b.WriteString(fmt.Sprintf("Active Jobs:     %d\n", activeJobs))
	b.WriteString(fmt.Sprintf("Queued Prints:   %d\n", queuedCount))
	b.WriteString(fmt.Sprintf("Shared Folder:   %s (Global Mesh)\n\n", sharedDir))

	b.WriteString(fmt.Sprintf("Connected Devices (%d/%d Online):\n", onlineCount, len(peers)))
	if len(peers) == 0 {
		b.WriteString("  (Waiting for other devices to connect)\n")
	} else {
		for _, p := range peers {
			b.WriteString(fmt.Sprintf("  - %-12s IP: %-15s Port: %-5d %s\n", p.Name, p.IP, p.SpoolPort, formatPeerStatusTag(p)))
		}
	}
	return b.String()
}

func checkPeerHealthy(peerURL, token, nodeName string) (healthy bool, learnedName string, printerReady bool, printerName string, err error) {
	healthy, learnedName, printerReady, printerName, _, err = checkPeerHealthDetail(peerURL, token, nodeName)
	return
}

func checkPeerHealthDetail(peerURL, token, nodeName string) (bool, string, bool, string, *server.HealthResponse, error) {
	if strings.TrimSpace(peerURL) == "" {
		return false, "", false, "", nil, errors.New("empty peer URL")
	}
	client := http.Client{Timeout: 3500 * time.Millisecond}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/health", strings.TrimRight(peerURL, "/")), nil)
	if err != nil {
		return false, "", false, "", nil, err
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
		return false, "", false, "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "", false, "", nil, fmt.Errorf("peer health returned HTTP %d", resp.StatusCode)
	}

	var hr server.HealthResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&hr); err != nil {
		return false, "", false, "", nil, err
	}
	learnedName := strings.TrimSpace(hr.NodeName)
	if !strings.EqualFold(strings.TrimSpace(hr.Status), "healthy") || learnedName == "" {
		return false, "", false, "", nil, errors.New("unhealthy or invalid Bridge health response")
	}
	return true, learnedName, hr.PrinterReady, strings.TrimSpace(hr.DefaultPrinter), &hr, nil
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
	cfg.Configured = true

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

	fmt.Println("[OK] Headless installation completed successfully.")
}

func printStatus(cfgPath string) {
	if cfgPath == "" {
		cfgPath = installer.TargetConfig
	}
	cfg, err := config.LoadConfig(cfgPath)
	port := 8765
	if err == nil && cfg.ListenPort > 0 {
		port = cfg.ListenPort
	}

	err = installer.VerifyHealth(port)
	if err == nil {
		fmt.Printf("Bridge Service: RUNNING on port %d (Healthy)\n", port)
		if targetPrinter, pErr := printer.GetTargetPhysicalPrinter(); pErr == nil && targetPrinter != "" {
			fmt.Printf("Local Printer:  %s\n", targetPrinter)
		} else {
			fmt.Println("Local Printer:  None (No physical printer connected)")
		}
		if cfg != nil {
			peers := cfg.GetPeers()
			fmt.Printf("Node: %s (Total Peers: %d)\n", cfg.GetNodeName(), len(peers))
			for i, p := range peers {
				fmt.Printf("  [%d] %s (%s) [Port %d]\n", i+1, p.Name, p.URL, p.SpoolPort)
			}
		}
		queued := printer.NewOutboundQueue(printer.DefaultOutboundQueueDir).Count()
		if queued > 0 {
			fmt.Printf("Outbound Queue: %d queued print(s)\n", queued)
		}
	} else {
		fmt.Printf("Bridge Service: STOPPED / UNREACHABLE (%v)\n", err)
	}
}

// safeLogWriter writes to primary unconditionally first, then best-effort to secondary
// without allowing secondary write failures (such as an invalid GUI-subsystem stdout handle)
// to abort logging to primary.
type safeLogWriter struct {
	primary   io.Writer
	secondary io.Writer
}

func (w *safeLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	var primaryErr error
	if w.primary != nil {
		n, primaryErr = w.primary.Write(p)
	}
	if w.secondary != nil {
		_, _ = w.secondary.Write(p)
	}
	return n, primaryErr
}

func setupLogging(logDir string) {
	var console io.Writer
	if os.Stdout != nil {
		console = os.Stdout
	}
	_ = setupLoggingWithOutput(logDir, console)
}

func setupLoggingWithOutput(logDir string, console io.Writer) *os.File {
	if logDir == "" {
		return nil
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
		log.SetOutput(&safeLogWriter{
			primary:   f,
			secondary: console,
		})
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
		return f
	}
	return nil
}
