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
	"bridge/tray"
)

var (
	tsIPMu      sync.RWMutex
	cachedTSIP  string
	lastTSCheck time.Time
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
	TaskName     = "BridgeSystemService"
	FirewallRule = "BridgeSystemService_8765"
	TargetAppDir = `C:\BridgeApp`
	TargetExe    = `C:\BridgeApp\bridge.exe`
	TargetConfig = `C:\BridgeApp\config.json`
	TargetIcons  = `C:\BridgeApp\icons`
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

		// Probe peer's /api/health with a short timeout to retrieve friendly Bridge names
		if p.Online {
			client := http.Client{Timeout: 1500 * time.Millisecond}
			resp, err := client.Get(fmt.Sprintf("http://%s:8765/api/health", ip))
			if err == nil {
				var hr struct {
					NodeName string `json:"node_name"`
					PeerName string `json:"peer_name"`
				}
				if json.NewDecoder(resp.Body).Decode(&hr) == nil {
					if hr.NodeName != "" {
						dp.BridgeName = hr.NodeName
					}
					if hr.PeerName != "" {
						dp.MyName = hr.PeerName
					}
				}
				resp.Body.Close()
			}
		}

		peers = append(peers, dp)
	}

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
	_ = exec.Command("net", "start", "Tailscale").Run()
	_ = exec.Command("net", "start", "Tailscale IPN").Run()

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
			_ = exec.Command(bin, "up").Start()
		}
	} else {
		_ = exec.Command(bin, "up").Start()
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

// CreateBridgeShortcutsForPeers configures Windows Virtual Printers and SendTo shortcuts
// for all connected peer devices in the multi-device network, plus the single global shared folder.
func CreateBridgeShortcutsForPeers(peers []config.PeerDevice) error {
	// 1. Desktop Shortcuts: ONLY ONE global shared folder shortcut
	sharedLnkName := "Bridge Shared Folder.lnk"
	for _, d := range GetAllDesktopDirs() {
		// Clean up legacy drop-to-print or old shortcuts
		_ = os.Remove(filepath.Join(d, "Bridge Print Folder.lnk"))

		files, err := os.ReadDir(d)
		if err == nil {
			for _, f := range files {
				name := f.Name()
				if strings.HasSuffix(name, " - Drop to Print.lnk") {
					_ = os.Remove(filepath.Join(d, name))
				}
			}
		}

		_ = CreateDesktopShortcut(`C:\Bridge\Shared`, filepath.Join(d, sharedLnkName), "Bridge Multi-Device Two-Way Shared Folder (Global Mesh)", "")
	}

	// 2. Windows Explorer "Send To" native right-click shortcuts for EACH peer
	sendToDirs := []string{
		filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "SendTo"),
	}
	entries, err := os.ReadDir(`C:\Users`)
	if err == nil {
		for _, e := range entries {
			st := filepath.Join(`C:\Users`, e.Name(), "AppData", "Roaming", "Microsoft", "Windows", "SendTo")
			if _, err := os.Stat(st); err == nil {
				sendToDirs = append(sendToDirs, st)
			}
		}
	}

	// Purge stale SendTo shortcuts that no longer match any peer in peers
	validLnkNames := make(map[string]bool)
	for _, p := range peers {
		validLnkNames[fmt.Sprintf("Print on %s.lnk", SanitizeFileName(p.Name))] = true
	}
	for _, st := range sendToDirs {
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
		}
	}

	return nil
}

// AutoUpgradeExisting detects if an older version of Bridge is installed, migrates it, and updates binary/shortcuts.
func AutoUpgradeExisting() bool {
	if _, err := os.Stat(TargetConfig); os.IsNotExist(err) {
		return false
	}

	log.Printf("[AutoUpgrade] Existing Bridge installation detected. Performing automatic upgrade...")

	cfg, err := config.LoadConfig(TargetConfig)
	if err != nil {
		return false
	}

	// Ensure SumatraPDF exists
	if sumatra := FindSumatraPDF(); sumatra != "" {
		cfg.SumatraPath = sumatra
	} else {
		if installed, err := InstallSumatraPDF(); err == nil {
			cfg.SumatraPath = installed
		}
	}

	// Ensure icons exist
	_, _ = tray.EnsureIcons(TargetIcons)

	// Save migrated configuration
	_ = config.SaveConfig(TargetConfig, cfg)

	// Kill legacy Session 0 scheduled task so it won't block Session 1 tray dot
	_ = exec.Command("schtasks", "/Delete", "/TN", TaskName, "/F").Run()

	// Register interactive Session 1 startup
	_ = RegisterInteractiveStartup()

	// Replace binary if running from another location
	currentExe, err := os.Executable()
	if err == nil && !strings.EqualFold(filepath.Clean(currentExe), filepath.Clean(TargetExe)) {
		KillOtherBridgeProcesses()
		time.Sleep(500 * time.Millisecond)

		src, err := os.Open(currentExe)
		if err == nil {
			defer src.Close()
			for attempt := 0; attempt < 5; attempt++ {
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

	// Decommission legacy print folders if present
	_ = os.RemoveAll(`C:\Bridge\Print`)

	// Refresh shortcuts and virtual printers for all peers
	_ = CreateBridgeShortcutsForPeers(cfg.GetPeers())

	log.Printf("[AutoUpgrade] Upgrade complete! Service configured for interactive Session 1.")
	return true
}

// InstallService deploys the executable, directories, firewall rule, and interactive startup.
func InstallService(cfg *config.Config) error {
	fmt.Println("[1/5] Setting up directories and icons...")

	// Kill legacy Session 0 scheduled task and any other old bridge processes
	_ = exec.Command("schtasks", "/Delete", "/TN", TaskName, "/F").Run()
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
			return fmt.Errorf("failed to create directory %s: %w", d, err)
		}
	}

	_ = os.RemoveAll(`C:\Bridge\Print`)
	_, _ = tray.EnsureIcons(TargetIcons)

	fmt.Println("[2/5] Deploying application binary and saving configuration...")
	currentExe, err := os.Executable()
	if err == nil && !strings.EqualFold(filepath.Clean(currentExe), filepath.Clean(TargetExe)) {
		src, err := os.Open(currentExe)
		if err == nil {
			defer src.Close()
			for attempt := 0; attempt < 5; attempt++ {
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
		return fmt.Errorf("failed to save config to %s: %w", TargetConfig, err)
	}

	fmt.Println("[3/5] Configuring Windows Firewall for TCP ports 8765 and 9100-9120...")
	_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", fmt.Sprintf("name=%s", FirewallRule)).Run()
	_ = exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		fmt.Sprintf("name=%s", FirewallRule),
		"dir=in",
		"action=allow",
		"protocol=TCP",
		fmt.Sprintf("localport=%d,9100-9120", cfg.ListenPort),
		"profile=any",
		"description=Bridge multi-device file sync and virtual print communication").Run()

	fmt.Println("[4/5] Registering Interactive Startup (Session 1 Taskbar Tray Dot)...")
	_ = RegisterInteractiveStartup()

	fmt.Println("[5/5] Creating Global Desktop Shortcut, SendTo Print, and Virtual Printers...")
	_ = CreateBridgeShortcutsForPeers(cfg.GetPeers())

	return nil
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

// UninstallService cleanly removes startup entries, virtual printers, firewall rule, and shortcuts.
func UninstallService() error {
	fmt.Println("[*] Stopping Bridge processes and removing Scheduled Task...")
	_ = exec.Command("schtasks", "/Delete", "/TN", TaskName, "/F").Run()
	_ = exec.Command("taskkill", "/F", "/IM", "bridge.exe").Run()

	fmt.Println("[*] Removing interactive startup registry keys...")
	UnregisterInteractiveStartup()

	fmt.Println("[*] Removing Windows Virtual Printers and RAW TCP Ports...")
	_ = UninstallAllVirtualPrinters()

	fmt.Println("[*] Removing Windows Firewall rule...")
	_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", fmt.Sprintf("name=%s", FirewallRule)).Run()

	fmt.Println("[*] Cleaning up Desktop shortcuts...")
	for _, d := range GetAllDesktopDirs() {
		_ = os.Remove(filepath.Join(d, "Bridge Shared Folder.lnk"))
		_ = os.Remove(filepath.Join(d, "Bridge Print Folder.lnk"))

		files, err := os.ReadDir(d)
		if err == nil {
			for _, f := range files {
				name := f.Name()
				if strings.HasSuffix(name, " - Shared Folder.lnk") || strings.HasSuffix(name, " - Drop to Print.lnk") {
					_ = os.Remove(filepath.Join(d, name))
				}
			}
		}
	}

	sendToDir := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "SendTo")
	entries, _ := os.ReadDir(sendToDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "Print on ") && strings.HasSuffix(e.Name(), ".lnk") {
			_ = os.Remove(filepath.Join(sendToDir, e.Name()))
		}
	}

	fmt.Println("[*] Bridge service has been successfully uninstalled.")
	fmt.Println("    (Your synchronized files in C:\\Bridge\\Shared were preserved).")
	return nil
}

// RestartService restarts Bridge in the current interactive session.
func RestartService() error {
	_ = exec.Command("schtasks", "/Delete", "/TN", TaskName, "/F").Run()
	KillOtherBridgeProcesses()
	time.Sleep(500 * time.Millisecond)
	cmd := exec.Command(TargetExe)
	return cmd.Start()
}
