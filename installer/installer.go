package installer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bridge/config"
	"bridge/printer"
	"bridge/tray"
)

var (
	tsIPMu      sync.RWMutex
	cachedTSIP  string
	lastTSCheck time.Time
	shortcutsMu sync.Mutex
)

// FindLocalTailscaleIP inspects system network interfaces for an active 100.x.y.z IP.
// This executes in <1ms and requires 0 subprocess spawns.
func FindLocalTailscaleIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
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
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip4 := ip.To4()
			// Tailscale addresses fall into the CGNAT range 100.64.0.0/10 (100.64.x.x - 100.127.x.x)
			if ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
				return ip4.String()
			}
		}
	}
	return ""
}

// InvalidateTailscaleCache clears the cached IP.
func InvalidateTailscaleCache() {
	tsIPMu.Lock()
	cachedTSIP = ""
	lastTSCheck = time.Time{}
	tsIPMu.Unlock()
}

const (
	TaskName      = "BridgeSystemService"
	LogonTaskName = "BridgeAppLogon"
	FirewallRule  = "BridgeSystemService_8765"
	TargetAppDir  = `C:\BridgeApp`
	TargetExe     = `C:\BridgeApp\bridge.exe`
	TargetConfig  = `C:\BridgeApp\config.json`
	TargetIcons   = `C:\BridgeApp\icons`
)

// DiscoveredPeer represents an auto-discovered peer on Tailscale.
type DiscoveredPeer struct {
	HostName   string `json:"hostname"`
	IP         string `json:"ip"`
	Online     bool   `json:"online"`
	OS         string `json:"os"`
	BridgeName string `json:"bridge_name,omitempty"`
	MyName     string `json:"my_name,omitempty"`
}

// DiscoverTailscalePeers queries tailscale status and probes online peers for running Bridge instances.
func DiscoverTailscalePeers() ([]DiscoveredPeer, error) {
	bin := FindTailscale()
	if bin == "" {
		return nil, errors.New("tailscale binary not found")
	}

	cmd := exec.Command(bin, "status", "--json")
	hideCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status failed: %w", err)
	}

	var status struct {
		Peer map[string]struct {
			HostName     string   `json:"HostName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
			OS           string   `json:"OS"`
		} `json:"Peer"`
	}

	if err := json.Unmarshal(out, &status); err != nil {
		return nil, fmt.Errorf("failed to parse tailscale status json: %w", err)
	}

	var peers []DiscoveredPeer
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, p := range status.Peer {
		ip := ""
		for _, tip := range p.TailscaleIPs {
			if strings.HasPrefix(tip, "100.") {
				ip = tip
				break
			}
		}
		if ip == "" {
			continue
		}

		dp := DiscoveredPeer{
			HostName: p.HostName,
			IP:       ip,
			Online:   p.Online,
			OS:       p.OS,
		}

		wg.Add(1)
		go func(d DiscoveredPeer) {
			defer wg.Done()
			client := http.Client{Timeout: 4 * time.Second}
			req, err := http.NewRequest("GET", fmt.Sprintf("http://%s:8765/api/health", d.IP), nil)
			if err == nil {
				req.Header.Set("X-Bridge-Probe", "1")
				resp, err := client.Do(req)
				if err == nil {
					if resp.StatusCode == http.StatusOK {
						var hr struct {
							NodeName string `json:"node_name"`
							PeerName string `json:"peer_name"`
						}
						if json.NewDecoder(resp.Body).Decode(&hr) == nil {
							if hr.NodeName != "" {
								d.BridgeName = hr.NodeName
								d.Online = true
							}
							if hr.PeerName != "" {
								d.MyName = hr.PeerName
							}
						}
					}
					resp.Body.Close()
				}
			}
			mu.Lock()
			peers = append(peers, d)
			mu.Unlock()
		}(dp)
	}
	wg.Wait()

	return peers, nil
}

// SanitizeFileName cleans invalid Windows filename characters.
func SanitizeFileName(name string) string {
	invalid := []string{"\\", "/", ":", "*", "?", "\"", "<", ">", "|"}
	res := name
	for _, inv := range invalid {
		res = strings.ReplaceAll(res, inv, "_")
	}
	res = strings.TrimSpace(res)
	if res == "" {
		res = "Remote PC"
	}
	return res
}

// EnsureTailscaleConnected automatically verifies Tailscale installation and connectivity.
// If Tailscale is not logged in, it initiates authentication, automatically opens the browser,
// and waits for successful login before returning the private IP.
func EnsureTailscaleConnected() (string, error) {
	fmt.Print("[*] Checking Tailscale network environment... ")
	bin := FindTailscale()
	if bin == "" {
		fmt.Println("Not found!")
		fmt.Println("[*] Tailscale is required for private zero-config networking between your devices.")
		fmt.Println("[*] Installing Tailscale silently via winget...")
		var err error
		bin, err = InstallTailscale()
		if err != nil {
			fmt.Printf("[!] Note on install: %v\n", err)
		}
	} else {
		fmt.Printf("Found: %s\n", bin)
	}

	if bin == "" {
		bin = FindTailscale()
	}
	if bin == "" {
		return "", errors.New("tailscale binary not available")
	}

	// Ensure Windows service is active if applicable
	cmdNet1 := exec.Command("net", "start", "Tailscale")
	hideCmd(cmdNet1)
	_ = cmdNet1.Run()

	cmdNet2 := exec.Command("net", "start", "Tailscale IPN")
	hideCmd(cmdNet2)
	_ = cmdNet2.Run()

	// 1. Check if already connected with a valid 100.x.y.z IP
	localIP := GetTailscaleIP()
	if localIP != "" {
		fmt.Printf("[✓] Tailscale connected! Local IP: %s\n", localIP)
		return localIP, nil
	}

	// 2. Not connected: initiate automated login
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                 TAILSCALE AUTHENTICATION REQUIRED                    ")
	fmt.Println("======================================================================")
	fmt.Println("[*] Launching Tailscale login. Opening your web browser automatically...")
	fmt.Println("[*] Please complete sign-in in the browser window.")
	fmt.Println("======================================================================")

	loginCmd := exec.Command(bin, "login")
	hideCmd(loginCmd)
	stdout, err1 := loginCmd.StdoutPipe()
	stderr, err2 := loginCmd.StderrPipe()

	if err1 == nil && err2 == nil && loginCmd.Start() == nil {
		go func() {
			_ = loginCmd.Wait()
		}()

		urlFound := make(chan string, 1)
		readForURL := func(r io.Reader) {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(line, "https://login.tailscale.com/") || strings.Contains(line, "https://") {
					for _, word := range strings.Fields(line) {
						if strings.HasPrefix(word, "https://login.tailscale.com/") || strings.HasPrefix(word, "https://") {
							cleanURL := strings.Trim(word, "\"'<>[](),.")
							select {
							case urlFound <- cleanURL:
							default:
							}
							return
						}
					}
				}
			}
		}
		go readForURL(stdout)
		go readForURL(stderr)

		select {
		case url := <-urlFound:
			fmt.Printf("\n>>> Login URL: %s <<<\n", url)
			fmt.Println("Opening browser now...")
			_ = OpenBrowser(url)
		case <-time.After(6 * time.Second):
			// Fallback: trigger general tailscale up
			cmdUp := exec.Command(bin, "up")
			hideCmd(cmdUp)
			_ = cmdUp.Start()
		}
	} else {
		cmdUp := exec.Command(bin, "up")
		hideCmd(cmdUp)
		_ = cmdUp.Start()
	}

	// 3. Poll for successful login in the browser
	fmt.Print("[*] Waiting for browser login completion")
	for i := 0; i < 120; i++ {
		time.Sleep(1500 * time.Millisecond)
		ip := GetTailscaleIP()
		if ip != "" {
			fmt.Printf("\n[✓] Tailscale login successful! Local IP: %s\n\n", ip)
			return ip, nil
		}
		fmt.Print(".")
	}

	return "", errors.New("timed out waiting for Tailscale login")
}

// ShortcutSyncResult reports virtual printer registration outcomes per peer.
type ShortcutSyncResult struct {
	SucceededPrinters []string
	PrinterErrors     map[string]error
}

func removeSumatraShortcuts() {
	_ = os.Remove(filepath.Join(TargetAppDir, "SumatraPDF.lnk"))
	for _, d := range GetAllDesktopDirs() {
		_ = os.Remove(filepath.Join(d, "SumatraPDF.lnk"))
	}
}

func collectSendToDirs() []string {
	seen := make(map[string]bool)
	var sendToDirs []string
	addSendTo := func(st string) {
		if strings.TrimSpace(st) == "" {
			return
		}
		clean := filepath.Clean(st)
		low := strings.ToLower(clean)
		if info, err := os.Stat(clean); err == nil && info.IsDir() && !seen[low] {
			seen[low] = true
			sendToDirs = append(sendToDirs, clean)
		}
	}

	if appData := os.Getenv("APPDATA"); appData != "" {
		addSendTo(filepath.Join(appData, "Microsoft", "Windows", "SendTo"))
	}
	if entries, err := os.ReadDir(`C:\Users`); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				addSendTo(filepath.Join(`C:\Users`, e.Name(), "AppData", "Roaming", "Microsoft", "Windows", "SendTo"))
			}
		}
	}
	return sendToDirs
}

// CreateBridgeShortcuts creates desktop shortcut and virtual printer for a single peer (backward compatibility).
func CreateBridgeShortcuts(peerName string) error {
	cleanPeer := SanitizeFileName(peerName)
	return CreateBridgeShortcutsForPeers([]config.PeerDevice{
		{
			Name:      cleanPeer,
			SpoolPort: 9101,
		},
	})
}

// SyncBridgeShortcutsForPeers configures Windows Virtual Printers and SendTo shortcuts
// for all connected peer devices plus a single global shared folder shortcut on the primary Desktop,
// and returns per-peer virtual printer registration results.
func SyncBridgeShortcutsForPeers(peers []config.PeerDevice) ShortcutSyncResult {
	shortcutsMu.Lock()
	defer shortcutsMu.Unlock()

	result := ShortcutSyncResult{
		PrinterErrors: make(map[string]error),
	}

	// Prune any obsolete virtual printers and ports from older versions
	_ = PruneObsoleteVirtualPrinters(peers)

	// 0. Determine local node name to strictly prevent self virtual printers or shortcuts
	localNodeName := ""
	if cfg, err := config.LoadConfig(TargetConfig); err == nil && cfg.NodeName != "" {
		localNodeName = strings.TrimSpace(cfg.NodeName)
	}

	// If a virtual printer for THIS device was previously created, remove it immediately
	if localNodeName != "" {
		_ = RemoveVirtualPrinter(localNodeName)
	}

	// 1. Desktop Shortcuts: Create Bridge Shared Folder.lnk ONLY ONCE in GetPrimaryDesktopDir(),
	// delete duplicates from all other Desktop directories, and remove SumatraPDF.lnk everywhere.
	sharedLnkName := "Bridge Shared Folder.lnk"
	primaryDesktop := GetPrimaryDesktopDir()
	desktopDirs := GetAllDesktopDirs()

	_ = os.Remove(filepath.Join(TargetAppDir, "SumatraPDF.lnk"))

	for _, d := range desktopDirs {
		// Clean up legacy drop-to-print, old shared folder shortcuts, and unwanted SumatraPDF shortcuts
		_ = os.Remove(filepath.Join(d, "Bridge Print Folder.lnk"))
		_ = os.Remove(filepath.Join(d, "SumatraPDF.lnk"))

		if primaryDesktop != "" && !strings.EqualFold(filepath.Clean(d), filepath.Clean(primaryDesktop)) {
			_ = os.Remove(filepath.Join(d, sharedLnkName))
		}

		files, err := os.ReadDir(d)
		if err == nil {
			for _, f := range files {
				name := f.Name()
				if strings.HasSuffix(name, " - Drop to Print.lnk") || strings.HasSuffix(name, " - Shared Folder.lnk") {
					_ = os.Remove(filepath.Join(d, name))
				}
			}
		}
	}

	if primaryDesktop != "" {
		_ = CreateDesktopShortcut(`C:\Bridge\Shared`, filepath.Join(primaryDesktop, sharedLnkName), "Bridge Multi-Device Two-Way Shared Folder (Global Mesh)", "")
	}

	// 2. Windows Explorer "Send To" native right-click shortcuts for EACH peer
	sendToDirs := collectSendToDirs()

	// Purge stale SendTo shortcuts that no longer match any peer in peers (and strictly purge self shortcut)
	validLnkNames := make(map[string]bool)
	for _, p := range peers {
		if config.IsSelfTarget(p.Name, p.IP, p.URL, localNodeName, 0) {
			continue // Exclude self
		}
		validLnkNames[fmt.Sprintf("Print on %s.lnk", SanitizeFileName(p.Name))] = true
	}
	for _, st := range sendToDirs {
		// Explicitly purge self SendTo shortcut
		if localNodeName != "" {
			_ = os.Remove(filepath.Join(st, fmt.Sprintf("Print on %s.lnk", SanitizeFileName(localNodeName))))
		}
		stEntries, err := os.ReadDir(st)
		if err == nil {
			for _, e := range stEntries {
				if strings.HasPrefix(e.Name(), "Print on ") && strings.HasSuffix(e.Name(), ".lnk") {
					if !validLnkNames[e.Name()] {
						_ = os.Remove(filepath.Join(st, e.Name()))
					}
				}
			}
		}
	}

	for _, p := range peers {
		// Strictly never install a virtual printer or SendTo shortcut for THIS device
		if config.IsSelfTarget(p.Name, p.IP, p.URL, localNodeName, 0) {
			continue
		}

		cleanPeer := SanitizeFileName(p.Name)
		sendToLnkName := fmt.Sprintf("Print on %s.lnk", cleanPeer)
		sendToArgs := fmt.Sprintf("-send-print -target \"%s\"", cleanPeer)

		for _, st := range sendToDirs {
			_ = CreateDesktopShortcut(TargetExe, filepath.Join(st, sendToLnkName), fmt.Sprintf("Send document to print on %s", cleanPeer), sendToArgs)
		}

		// 3. Register native Windows Virtual Printer for Ctrl+P dropdown support
		port := p.SpoolPort
		if port <= 0 {
			port = 9101
		}
		if err := InstallVirtualPrinter(cleanPeer, port); err != nil {
			log.Printf("[Installer] Warning: Virtual printer registration failed for %s on port %d: %v", cleanPeer, port, err)
			result.PrinterErrors[cleanPeer] = err
		} else {
			result.SucceededPrinters = append(result.SucceededPrinters, cleanPeer)
		}
	}

	return result
}

// CreateBridgeShortcutsForPeers configures Windows Virtual Printers and SendTo shortcuts
// for all connected peer devices in the multi-device network, plus the single global shared folder.
func CreateBridgeShortcutsForPeers(peers []config.PeerDevice) error {
	_ = SyncBridgeShortcutsForPeers(peers)
	return nil
}

// UpgradeInstallation cleanly upgrades the installed Bridge service to the specified source executable.
// It is explicitly triggered by user confirmation, preventing unwanted auto-upgrades when opening bridge.exe.
func UpgradeInstallation(sourceExe string) error {
	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("                      UPGRADING BRIDGE SERVICE                        ")
	fmt.Println("======================================================================")

	// Verify source executable exists
	srcInfo, err := os.Stat(sourceExe)
	if err != nil {
		return fmt.Errorf("source binary not accessible: %w", err)
	}
	if srcInfo.IsDir() {
		return fmt.Errorf("source path is a directory, not an executable file")
	}

	fmt.Println("[1/5] Stopping active Bridge background services and print engine...")
	KillOtherBridgeProcesses()
	cmdSumatra := exec.Command("taskkill", "/F", "/IM", "SumatraPDF.exe")
	hideCmd(cmdSumatra)
	_ = cmdSumatra.Run()
	time.Sleep(500 * time.Millisecond)

	// Replace TargetExe if source is different
	if !strings.EqualFold(filepath.Clean(sourceExe), filepath.Clean(TargetExe)) {
		fmt.Printf("[2/5] Deploying updated binary to %s...\n", TargetExe)
		src, err := os.Open(sourceExe)
		if err != nil {
			return fmt.Errorf("failed to open source executable: %w", err)
		}
		defer src.Close()

		copied := false
		for attempt := 0; attempt < 5; attempt++ {
			_, _ = src.Seek(0, io.SeekStart)
			dst, err := os.OpenFile(TargetExe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err == nil {
				_, _ = io.Copy(dst, src)
				dst.Close()
				copied = true
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if !copied {
			return fmt.Errorf("failed to overwrite %s: file may still be in use", TargetExe)
		}
	} else {
		fmt.Printf("[2/5] Verified installation at %s.\n", TargetExe)
	}

	fmt.Println("[3/5] Refreshing system tray icons and configuration...")
	_, _ = tray.EnsureIcons(TargetIcons)
	removeSumatraShortcuts()

	cfg, _ := config.LoadConfig(TargetConfig)
	if cfg != nil {
		if sumatra := FindSumatraPDF(); sumatra != "" {
			cfg.SumatraPath = sumatra
			printer.ConfigureSumatraDefaults(sumatra)
		} else {
			if sumatraPath, err := printer.EnsureSumatraPDF(); err == nil && sumatraPath != "" {
				cfg.SumatraPath = sumatraPath
				printer.ConfigureSumatraDefaults(sumatraPath)
			}
		}
		removeSumatraShortcuts()
		_ = config.SaveConfig(TargetConfig, cfg)
	}

	fmt.Println("[4/5] Updating startup registration (Session 1 taskbar tray)...")
	UnregisterInteractiveStartup()
	if err := RegisterInteractiveStartup(); err != nil {
		fmt.Printf("[!] Warning updating startup registration: %v\n", err)
	}

	if cfg != nil {
		res := SyncBridgeShortcutsForPeers(cfg.GetPeers())
		for peerName, pErr := range res.PrinterErrors {
			fmt.Printf("[!] Warning: Virtual printer 'Print on %s' failed: %v\n", peerName, pErr)
		}
	} else {
		_ = SyncBridgeShortcutsForPeers(nil)
	}

	port := 8765
	if cfg != nil && cfg.ListenPort > 0 {
		port = cfg.ListenPort
	}
	fmt.Printf("Refreshing Windows Firewall rule (%s) for TCP ports %d and 9100-9120...\n", FirewallRule, port)
	ensureFirewallRule(port)

	fmt.Println("[5/5] Restarting Bridge background service...")
	cmd := exec.Command(TargetExe, "-autostart")
	hideCmd(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start Bridge background service: %w", err)
	}

	var healthErr error
	for i := 0; i < 15; i++ {
		time.Sleep(350 * time.Millisecond)
		healthErr = VerifyHealth(port)
		if healthErr == nil {
			break
		}
	}
	if healthErr != nil {
		fmt.Printf("[!] Note: Bridge restarted, but health check on port %d did not respond yet (%v).\n", port, healthErr)
	} else {
		fmt.Printf("[✓] Verified Bridge service health on port %d.\n", port)
	}

	fmt.Println()
	fmt.Println("[✓] Bridge has been successfully upgraded and restarted in the background!")
	fmt.Println("======================================================================")
	return nil
}

// AutoUpgradeExisting detects if an older version of Bridge is installed and runs upgrade.
// Deprecated: Opening bridge.exe now presents explicit options instead of auto-upgrading without consent.
func AutoUpgradeExisting() bool {
	if _, err := os.Stat(TargetConfig); os.IsNotExist(err) {
		return false
	}
	currentExe, err := os.Executable()
	if err != nil {
		currentExe = TargetExe
	}
	return UpgradeInstallation(currentExe) == nil
}

func installServiceWithReport(cfg *config.Config) (ShortcutSyncResult, error) {
	var res ShortcutSyncResult
	fmt.Println("[1/5] Setting up directories and icons...")

	// Kill legacy Session 0 scheduled task and any other old bridge processes
	for _, tn := range []string{TaskName, LogonTaskName} {
		cmdTask := exec.Command("schtasks", "/Delete", "/TN", tn, "/F")
		hideCmd(cmdTask)
		_ = cmdTask.Run()
	}
	KillOtherBridgeProcesses()

	dirs := []string{
		TargetAppDir,
		TargetIcons,
		cfg.SharedDir,
		cfg.PrintQueueDir,
		cfg.PrintFailedDir,
		cfg.LogDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return res, fmt.Errorf("failed to create directory %s: %w", d, err)
		}
	}

	_ = os.RemoveAll(`C:\Bridge\Print`)
	removeSumatraShortcuts()
	cmdAcl := exec.Command("icacls", TargetAppDir, "/grant", "*S-1-5-32-545:(OI)(CI)M", "/T", "/Q")
	hideCmd(cmdAcl)
	_ = cmdAcl.Run()

	_, _ = tray.EnsureIcons(TargetIcons)

	fmt.Println("[2/5] Deploying application binary and saving configuration...")
	currentExe, err := os.Executable()
	if err == nil && !strings.EqualFold(filepath.Clean(currentExe), filepath.Clean(TargetExe)) {
		src, err := os.Open(currentExe)
		if err == nil {
			defer src.Close()
			for attempt := 0; attempt < 5; attempt++ {
				_, _ = src.Seek(0, io.SeekStart)
				dst, err := os.OpenFile(TargetExe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
				if err == nil {
					_, _ = io.Copy(dst, src)
					dst.Close()
					break
				}
				time.Sleep(300 * time.Millisecond)
			}
		}
	}

	cfg.ConfigPath = TargetConfig
	if err := config.SaveConfig(TargetConfig, cfg); err != nil {
		return res, fmt.Errorf("failed to save config to %s: %w", TargetConfig, err)
	}

	listenPort := cfg.ListenPort
	if listenPort <= 0 {
		listenPort = 8765
	}
	fmt.Printf("[3/5] Configuring Windows Firewall for TCP ports %d and 9100-9120...\n", listenPort)
	ensureFirewallRule(listenPort)

	fmt.Println("[4/5] Registering Interactive Startup (Session 1 Taskbar Tray Dot)...")
	if err := RegisterInteractiveStartup(); err != nil {
		fmt.Printf("[!] Warning registering startup: %v\n", err)
	}

	fmt.Println("[5/5] Creating Global Desktop Shortcut, SendTo Print, and Virtual Printers...")
	res = SyncBridgeShortcutsForPeers(cfg.GetPeers())

	return res, nil
}

func firewallRuleDeleteArgs() []string {
	return []string{"advfirewall", "firewall", "delete", "rule", fmt.Sprintf("name=%s", FirewallRule)}
}

func firewallRuleAddArgs(listenPort int) []string {
	if listenPort <= 0 {
		listenPort = 8765
	}
	return []string{
		"advfirewall", "firewall", "add", "rule",
		fmt.Sprintf("name=%s", FirewallRule),
		"dir=in",
		"action=allow",
		"protocol=TCP",
		fmt.Sprintf("localport=%d,9100-9120", listenPort),
		"profile=any",
		"description=Bridge multi-device file sync and virtual print communication",
	}
}

func ensureFirewallRule(listenPort int) {
	cmdFwDel := exec.Command("netsh", firewallRuleDeleteArgs()...)
	hideCmd(cmdFwDel)
	_ = cmdFwDel.Run()

	cmdFwAdd := exec.Command("netsh", firewallRuleAddArgs(listenPort)...)
	hideCmd(cmdFwAdd)
	_ = cmdFwAdd.Run()
}

// InstallService deploys the executable, directories, firewall rule, and interactive startup.
func InstallService(cfg *config.Config) error {
	_, err := installServiceWithReport(cfg)
	return err
}

// VerifyHealth checks if the local Bridge service HTTP API responds 200 OK.
func VerifyHealth(port int) error {
	client := http.Client{Timeout: 3 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("health check request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// UninstallService cleanly removes startup entries, virtual printers, firewall rule, shortcuts, and application binaries.
func UninstallService() error {
	fmt.Println("[1/6] Stopping Bridge background processes, print engine, and scheduled tasks...")
	for _, tn := range []string{LogonTaskName, TaskName} {
		cmdSch := exec.Command("schtasks", "/Delete", "/TN", tn, "/F")
		hideCmd(cmdSch)
		_ = cmdSch.Run()
	}

	KillOtherBridgeProcesses()
	cmdSumatra := exec.Command("taskkill", "/F", "/IM", "SumatraPDF.exe")
	hideCmd(cmdSumatra)
	_ = cmdSumatra.Run()

	fmt.Println("[2/6] Permanently removing startup entries across all user hives and registry locations...")
	UnregisterInteractiveStartup()

	fmt.Println("[3/6] Removing Windows Virtual Printers, printer names, and RAW TCP ports...")
	_ = UninstallAllVirtualPrinters()

	fmt.Println("[4/6] Removing Windows Firewall communication rules...")
	cmdFwDel2 := exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", fmt.Sprintf("name=%s", FirewallRule))
	hideCmd(cmdFwDel2)
	_ = cmdFwDel2.Run()

	fmt.Println("[5/6] Cleaning up Desktop and Send-To shortcuts across all user profiles...")
	removeSumatraShortcuts()
	for _, d := range GetAllDesktopDirs() {
		_ = os.Remove(filepath.Join(d, "Bridge Shared Folder.lnk"))
		_ = os.Remove(filepath.Join(d, "Bridge Print Folder.lnk"))
		_ = os.Remove(filepath.Join(d, "SumatraPDF.lnk"))

		files, err := os.ReadDir(d)
		if err == nil {
			for _, f := range files {
				name := f.Name()
				if strings.HasSuffix(name, " - Shared Folder.lnk") || strings.HasSuffix(name, " - Drop to Print.lnk") || (strings.Contains(strings.ToLower(name), "bridge") || strings.Contains(strings.ToLower(name), "sumatra")) && strings.HasSuffix(name, ".lnk") {
					_ = os.Remove(filepath.Join(d, name))
				}
			}
		}
	}

	for _, st := range collectSendToDirs() {
		if stEntries, err := os.ReadDir(st); err == nil {
			for _, se := range stEntries {
				low := strings.ToLower(se.Name())
				if (strings.HasPrefix(se.Name(), "Print on ") || strings.Contains(low, "bridge")) && strings.HasSuffix(low, ".lnk") {
					_ = os.Remove(filepath.Join(st, se.Name()))
				}
			}
		}
	}

	fmt.Println("[6/6] Purging internal queues, logs, and application installation files...")
	_ = os.RemoveAll(`C:\Bridge\Logs`)
	_ = os.RemoveAll(`C:\Bridge\PrintQueue`)
	_ = os.RemoveAll(`C:\Bridge\PrintFailed`)
	_ = os.RemoveAll(`C:\Bridge\Print`)

	// 1. Remove non-locked files inside TargetAppDir immediately
	if entries, err := os.ReadDir(TargetAppDir); err == nil {
		for _, e := range entries {
			if !strings.EqualFold(e.Name(), "bridge.exe") {
				_ = os.RemoveAll(filepath.Join(TargetAppDir, e.Name()))
			}
		}
	}

	// 2. Schedule reliable deletion of TargetAppDir with CWD = C:\ and retry loop via PowerShell
	psClean := fmt.Sprintf(`Start-Sleep -Seconds 1; 1..15 | ForEach-Object { Remove-Item -LiteralPath '%s' -Recurse -Force -ErrorAction SilentlyContinue; if (-not (Test-Path -LiteralPath '%s')) { exit 0 }; Start-Sleep -Seconds 1 }`, TargetAppDir, TargetAppDir)
	cleanupCmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psClean)
	cleanupCmd.Dir = `C:\`
	hideCmd(cleanupCmd)
	_ = cleanupCmd.Start()

	// If current process is not running inside TargetAppDir, delete it directly now
	currExe, _ := os.Executable()
	isInsideAppDir := strings.HasPrefix(strings.ToLower(filepath.Clean(currExe)), strings.ToLower(filepath.Clean(TargetAppDir)))
	if !isInsideAppDir {
		_ = os.RemoveAll(TargetAppDir)
	}

	fmt.Println()
	fmt.Println("======================================================================")
	fmt.Println("[✓] Bridge has been completely and permanently uninstalled.")
	fmt.Println("    - All Bridge virtual printers and ports were removed.")
	fmt.Println("    - All autostart entries across all accounts were eradicated.")
	fmt.Println("    - Application binaries (C:\\BridgeApp) have been deleted.")
	fmt.Println("    - Your synchronized files in C:\\Bridge\\Shared were safely preserved.")
	fmt.Println("======================================================================")
	return nil
}

// RestartService restarts Bridge in the current interactive session with taskbar tray icon.
func RestartService() error {
	cmdSch := exec.Command("schtasks", "/Delete", "/TN", TaskName, "/F")
	hideCmd(cmdSch)
	_ = cmdSch.Run()

	KillOtherBridgeProcesses()
	time.Sleep(500 * time.Millisecond)
	cmd := exec.Command(TargetExe, "-autostart")
	hideCmd(cmd)
	return cmd.Start()
}
