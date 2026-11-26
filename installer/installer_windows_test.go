//go:build windows

package installer

import (
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"bridge/config"
)

func encodeUTF16LEWithBOM(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, 2+len(u16)*2)
	out[0] = 0xFF
	out[1] = 0xFE
	for i, v := range u16 {
		out[2+i*2] = byte(v)
		out[2+i*2+1] = byte(v >> 8)
	}
	return out
}

func TestIsBridgeLogonTaskXMLCurrent(t *testing.T) {
	validXML := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.3" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>DESKTOP-TEST\User</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal>
      <UserId>DESKTOP-TEST\User</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions>
    <Exec>
      <Command>C:\BridgeApp\bridge.exe</Command>
      <Arguments>-autostart</Arguments>
    </Exec>
  </Actions>
</Task>`

	if !isBridgeLogonTaskXMLCurrent([]byte(validXML)) {
		t.Fatal("expected UTF-8 valid BridgeAppLogon XML to be recognized as current")
	}
	if !isBridgeLogonTaskXMLCurrent(encodeUTF16LEWithBOM(validXML)) {
		t.Fatal("expected UTF-16LE BOM valid BridgeAppLogon XML to be recognized as current")
	}

	legacySchtasksXML := strings.NewReplacer(
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>", "<DisallowStartIfOnBatteries>true</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>", "<StopIfGoingOnBatteries>true</StopIfGoingOnBatteries>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<ExecutionTimeLimit>PT72H</ExecutionTimeLimit>",
	).Replace(validXML)
	if isBridgeLogonTaskXMLCurrent(encodeUTF16LEWithBOM(legacySchtasksXML)) {
		t.Fatal("expected legacy schtasks XML (batteries/72h limit) to NOT be recognized as current")
	}

	cases := []struct {
		name string
		xml  string
	}{
		{"empty", ""},
		{"wrong_exe", strings.ReplaceAll(validXML, `C:\BridgeApp\bridge.exe`, `C:\Temp\other.exe`)},
		{"missing_autostart", strings.ReplaceAll(validXML, "-autostart", "-tray")},
		{"missing_logontrigger", strings.ReplaceAll(validXML, "<LogonTrigger>", "<BootTrigger>")},
		{"wrong_runlevel", strings.ReplaceAll(validXML, "<RunLevel>HighestAvailable</RunLevel>", "<RunLevel>LeastPrivilege</RunLevel>")},
		{"wrong_logontype", strings.ReplaceAll(validXML, "<LogonType>InteractiveToken</LogonType>", "<LogonType>Password</LogonType>")},
		{"disallow_batteries_true", strings.ReplaceAll(validXML, "<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>", "<DisallowStartIfOnBatteries>true</DisallowStartIfOnBatteries>")},
		{"stop_on_batteries_true", strings.ReplaceAll(validXML, "<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>", "<StopIfGoingOnBatteries>true</StopIfGoingOnBatteries>")},
		{"timeout_72h", strings.ReplaceAll(validXML, "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<ExecutionTimeLimit>PT72H</ExecutionTimeLimit>")},
		{"omitted_battery_and_timeout", strings.NewReplacer(
			"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>", "",
			"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>", "",
			"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "",
		).Replace(validXML)},
	}

	for _, tc := range cases {
		if isBridgeLogonTaskXMLCurrent([]byte(tc.xml)) {
			t.Errorf("case %s: expected isBridgeLogonTaskXMLCurrent to return false", tc.name)
		}
	}
}

func TestBuildRegisterLogonTaskScript(t *testing.T) {
	script := buildRegisterLogonTaskScript(`C:\BridgeApp\bridge.exe`, LogonTaskName)
	required := []string{
		"$ErrorActionPreference = 'Stop'",
		"New-ScheduledTaskAction -Execute $exe -Argument '-autostart'",
		"New-ScheduledTaskTrigger -AtLogOn -User $user",
		"New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Highest",
		"New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -MultipleInstances IgnoreNew",
		"Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force -ErrorAction Stop",
	}
	for _, want := range required {
		if !strings.Contains(script, want) {
			t.Errorf("generated script missing %q", want)
		}
	}

	// Verify single-quote escaping
	quotedScript := buildRegisterLogonTaskScript(`C:\O'Brien\bridge.exe`, `Bridge'Task`)
	if !strings.Contains(quotedScript, `'C:\O''Brien\bridge.exe'`) || !strings.Contains(quotedScript, `'Bridge''Task'`) {
		t.Fatalf("expected single quotes to be escaped in script:\n%s", quotedScript)
	}

	// Verify PowerShell AST syntax validity
	astCheck := `$errs = $null; $null = [System.Management.Automation.Language.Parser]::ParseInput(@'
` + script + `
'@, [ref]$null, [ref]$errs); if ($errs.Count -gt 0) { $errs | ForEach-Object { Write-Error $_.Message }; exit 1 }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", astCheck)
	hideCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PowerShell AST parse failed for generated script: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func assertValidPowerShellAST(t *testing.T, script string) {
	t.Helper()
	astCheck := `$errs = $null; $null = [System.Management.Automation.Language.Parser]::ParseInput(@'
` + script + `
'@, [ref]$null, [ref]$errs); if ($errs.Count -gt 0) { $errs | ForEach-Object { Write-Error $_.Message }; exit 1 }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", astCheck)
	hideCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PowerShell AST parse failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func TestBuildInstallVirtualPrinterScriptNoSNMPFlagAndVerified(t *testing.T) {
	portName := BridgePortName("Unit 2", 9101)
	script := buildInstallVirtualPrinterScript("Print on Unit 2", portName, 9101)

	if strings.Contains(script, "-SNMP 0") || strings.Contains(script, "-SNMP") {
		t.Fatalf("generated printer install script must NOT pass -SNMP to Add-PrinterPort:\n%s", script)
	}
	required := []string{
		"Add-PrinterPort -Name $portName -PrinterHostAddress '127.0.0.1' -PortNumber $portNumber -ErrorAction Stop",
		`Set-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\$portName" -Name "SNMP Enabled" -Value 0`,
		"Add-Printer -Name $printerName -DriverName $driver -PortName $portName -ErrorAction Stop",
		"Get-Printer -Name $printerName -ErrorAction Stop",
		"Bridge - Unit 2 (9101)",
		"Bridge_Port_$portNumber",
		"generic / text only",
		"snagit",
		"openxps",
	}
	for _, want := range required {
		if !strings.Contains(script, want) {
			t.Errorf("install virtual printer script missing %q", want)
		}
	}
	if !strings.Contains(script, `$preferredDrivers = @('Microsoft Print To PDF', 'Microsoft XPS Document Writer v4',`) {
		t.Errorf("install virtual printer script must prefer 'Microsoft Print To PDF' first for 1:1 PDF spooling")
	}
	assertValidPowerShellAST(t, script)

	if !shouldUpgradeVirtualPrinterDriver("Microsoft XPS Document Writer v4", true, true) {
		t.Errorf("expected elevated upgrade from XPS driver when Microsoft Print To PDF is available")
	}
	if shouldUpgradeVirtualPrinterDriver("Microsoft Print To PDF", true, true) {
		t.Errorf("must not re-upgrade when already using Microsoft Print To PDF")
	}
	if shouldUpgradeVirtualPrinterDriver("Microsoft XPS Document Writer v4", true, false) {
		t.Errorf("must not require re-install when unelevated")
	}

	// Peer names containing brackets [1] must generate a safe script using -LiteralPath
	bracketPort := BridgePortName("Unit [1]", 9102)
	bracketScript := buildInstallVirtualPrinterScript("Print on Unit [1]", bracketPort, 9102)
	if !strings.Contains(bracketScript, `Set-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\$portName"`) {
		t.Fatalf("bracket peer script must use -LiteralPath for Set-ItemProperty")
	}
	if !strings.Contains(bracketScript, "'Bridge - Unit [1] (9102)'") || !strings.Contains(bracketScript, "'Print on Unit [1]'") {
		t.Fatalf("bracket peer script missing expected port or printer name:\n%s", bracketScript)
	}
	assertValidPowerShellAST(t, bracketScript)
}

func TestBuildSafePortAndPrinterRemovalScripts(t *testing.T) {
	pruneScript := buildPruneObsoleteVirtualPrintersScript([]config.PeerDevice{
		{Name: "Unit 2", SpoolPort: 9101},
	})
	if !strings.Contains(pruneScript, "Bridge - Unit 2 (9101)") {
		t.Errorf("prune script missing canonical port name")
	}
	if !strings.Contains(pruneScript, "$_.Name -like 'Bridge_Port_*'") || !strings.Contains(pruneScript, "$_.Name -like 'Bridge - *'") {
		t.Errorf("prune script must handle both legacy Bridge_Port_* and Bridge - * ports")
	}
	if !strings.Contains(pruneScript, "Get-Service -Name spooler") {
		t.Errorf("prune script must check spooler service status before removing ports")
	}
	assertValidPowerShellAST(t, pruneScript)

	removeScript := buildRemoveVirtualPrinterScript("Unit 2")
	if !strings.Contains(removeScript, "Get-PrintJob -PrinterName $pName") || !strings.Contains(removeScript, "Remove-PrintJob") {
		t.Errorf("remove script must clear print jobs first")
	}
	if !strings.Contains(removeScript, "Get-Service -Name spooler") {
		t.Errorf("remove script must wait for spooler Running before Remove-PrinterPort")
	}
	assertValidPowerShellAST(t, removeScript)

	uninstallScript := buildUninstallAllVirtualPrintersScript()
	if strings.Contains(uninstallScript, `Name -like '*Bridge*'`) || strings.Contains(uninstallScript, `Name -notlike '*Bridge*'`) {
		t.Fatalf("uninstall printer script must NOT match unrelated *Bridge* printer names:\n%s", uninstallScript)
	}
	requiredUninstall := []string{
		"Get-PrintJob -PrinterName $pName",
		"Restart-Service spooler -Force",
		"$svc.Status -eq 'Running'",
		"$_.Name -like 'Bridge_Port_*'",
		"$_.Name -like 'Bridge - *'",
	}
	for _, want := range requiredUninstall {
		if !strings.Contains(uninstallScript, want) {
			t.Errorf("uninstall printer script missing %q", want)
		}
	}
	assertValidPowerShellAST(t, uninstallScript)
}

func TestVirtualPrinterPruneAndRemovalFastPathHelpers(t *testing.T) {
	validPrinters, validPorts := buildValidPrintersAndPortsMaps([]config.PeerDevice{
		{Name: "Unit [1]", SpoolPort: 9101},
	})

	if shouldPruneBridgePrinter("Print on Unit [1]", "Bridge - Unit [1] (9101)", validPrinters) {
		t.Errorf("active peer printer must not be pruned")
	}
	if !shouldPruneBridgePrinter("Print on OldPeer", "Bridge - OldPeer (9102)", validPrinters) {
		t.Errorf("obsolete Print on OldPeer printer must be pruned")
	}
	if !shouldPruneBridgePrinter("Legacy Bridge Printer", "Bridge_Port_9102", validPrinters) {
		t.Errorf("printer bound to Bridge_Port_* must be pruned")
	}
	if shouldPruneBridgePrinter("Bridge Office Jet", "USB001", validPrinters) {
		t.Errorf("unrelated physical printer with 'Bridge' in name must NOT be pruned")
	}

	if shouldPruneBridgePort("Bridge - Unit [1] (9101)", validPorts) {
		t.Errorf("active peer port must not be pruned")
	}
	for _, obs := range []string{"Bridge_Port_9101", "Bridge_TCP_Port", "Bridge - OldPeer (9102)"} {
		if !shouldPruneBridgePort(obs, validPorts) {
			t.Errorf("obsolete Bridge port %q must be pruned", obs)
		}
	}
	if shouldPruneBridgePort("USB001", validPorts) {
		t.Errorf("non-Bridge port USB001 must NOT be pruned")
	}

	// Fast-path: removing a non-existent peer printer must return nil in <500ms without spawning PowerShell
	start := time.Now()
	if err := RemoveVirtualPrinter("NonExistentPeer_FastPath_99999"); err != nil {
		t.Fatalf("RemoveVirtualPrinter fast-path failed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("RemoveVirtualPrinter fast-path took %v (expected <500ms without PowerShell spawn)", elapsed)
	}
}

func TestInstallPS1SyntaxAndPrinterFix(t *testing.T) {
	psPath := `..\install.ps1`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		`$raw = Get-Content -LiteralPath '`+psPath+`' -Raw; $errs = $null; $null = [System.Management.Automation.Language.Parser]::ParseInput($raw, [ref]$null, [ref]$errs); if ($errs.Count -gt 0) { $errs | ForEach-Object { Write-Error $_.Message }; exit 1 }; if ($raw -match '-SNMP\s+0') { Write-Error 'Found forbidden -SNMP 0'; exit 2 }; if ($raw -notmatch '-H windowsgui') { Write-Error 'Missing windowsgui build documentation'; exit 3 }; if ($raw -notmatch '-WindowStyle Hidden') { Write-Error 'Missing -WindowStyle Hidden'; exit 4 }; if ($raw -notlike '*Set-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Control\Print\Monitors\Standard TCP/IP Port\Ports\$portName"*') { Write-Error 'Missing -LiteralPath on registry Set-ItemProperty'; exit 5 }`)
	hideCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.ps1 validation failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}
