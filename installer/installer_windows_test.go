//go:build windows

package installer

import (
	"os/exec"
	"strings"
	"testing"
	"unicode/utf16"
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
