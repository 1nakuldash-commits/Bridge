package printer

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func buildMockSumatraWithArgsLog(t *testing.T, tempDir string) (string, string) {
	t.Helper()
	mockSrc := filepath.Join(tempDir, "mock_sumatra_opts.go")
	mockExe := filepath.Join(tempDir, "mock_sumatra_opts.exe")
	logFile := filepath.Join(tempDir, "sumatra_opts.log")

	code := fmt.Sprintf(`package main
import (
	"os"
	"strings"
)
func main() {
	f, _ := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer f.Close()
	_, _ = f.WriteString(strings.Join(os.Args[1:], "|") + "\n")
}
`, logFile)

	if err := os.WriteFile(mockSrc, []byte(code), 0644); err != nil {
		t.Fatalf("write mock src: %v", err)
	}
	if out, err := exec.Command("go", "build", "-o", mockExe, mockSrc).CombinedOutput(); err != nil {
		t.Fatalf("compile mock sumatra: %v\n%s", err, string(out))
	}
	return mockExe, logFile
}

func TestPrintFileWithOptions_ForwardsPaperSizeAndOrientation(t *testing.T) {
	tempDir := t.TempDir()
	mockExe, logFile := buildMockSumatraWithArgsLog(t, tempDir)

	pdfPath := filepath.Join(tempDir, "doc.pdf")
	writeSampleA4PDF(t, pdfPath)

	mono := false
	err := PrintFileWithOptions(pdfPath, "HP_Office_Printer", PrintOptions{
		PaperSize:   "Letter",
		Orientation: "landscape",
		Copies:      3,
		Color:       &mono,
		SumatraPath: mockExe,
	})
	if err != nil {
		t.Fatalf("PrintFileWithOptions failed: %v", err)
	}

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read mock log: %v", err)
	}
	args := string(raw)
	if !strings.Contains(args, "-print-to|HP_Office_Printer") {
		t.Errorf("Expected target printer in args, got %q", args)
	}
	if !strings.Contains(args, "paper=Letter") {
		t.Errorf("Expected forwarded paper=Letter in args, got %q", args)
	}
	if !strings.Contains(args, "landscape") {
		t.Errorf("Expected forwarded landscape orientation in args, got %q", args)
	}
	if !strings.Contains(args, "3x") || !strings.Contains(args, "monochrome") {
		t.Errorf("Expected copies=3x and monochrome in args, got %q", args)
	}
}

func TestValidatePrintableFormat_UnsupportedDiagnostics(t *testing.T) {
	tempDir := t.TempDir()

	docx := filepath.Join(tempDir, "report.docx")
	_ = os.WriteFile(docx, []byte("PK\x03\x04word/document.xml"), 0644)
	if err := ValidatePrintableFormat(docx); err == nil || !strings.Contains(err.Error(), "Ctrl+P") {
		t.Errorf("Expected actionable Ctrl+P hint for .docx, got %v", err)
	}

	psFile := filepath.Join(tempDir, "spool.prn")
	_ = os.WriteFile(psFile, []byte("%!PS-Adobe-3.0\nshowpage\n"), 0644)
	if err := ValidatePrintableFormat(psFile); err == nil || !strings.Contains(strings.ToLower(err.Error()), "postscript") {
		t.Errorf("Expected clear PostScript diagnostic, got %v", err)
	}

	pclFile := filepath.Join(tempDir, "raw_pcl.bin")
	_ = os.WriteFile(pclFile, []byte("\x1b%-12345X@PJL JOB\r\n"), 0644)
	if err := ValidatePrintableFormat(pclFile); err == nil || !strings.Contains(strings.ToLower(err.Error()), "pcl") {
		t.Errorf("Expected clear PCL diagnostic, got %v", err)
	}

	pdfFile := filepath.Join(tempDir, "valid.pdf")
	writeSampleA4PDF(t, pdfFile)
	if err := ValidatePrintableFormat(pdfFile); err != nil {
		t.Errorf("Expected valid PDF to pass validation, got %v", err)
	}
}

func TestConvertOXPStoXPS_RewritesNamespaces(t *testing.T) {
	tempDir := t.TempDir()
	oxpsPath := filepath.Join(tempDir, "sample.oxps")
	if err := os.WriteFile(oxpsPath, buildTestXPSArchive(t, true), 0644); err != nil {
		t.Fatalf("write sample.oxps: %v", err)
	}

	geom := DetectDocumentGeometry(oxpsPath)
	if geom.Format != ".oxps" {
		t.Errorf("Expected .oxps format, got %q", geom.Format)
	}

	xpsPath, err := convertOXPStoXPS(oxpsPath)
	if err != nil {
		t.Fatalf("convertOXPStoXPS failed: %v", err)
	}
	defer os.Remove(xpsPath)

	zr, err := zip.OpenReader(xpsPath)
	if err != nil {
		t.Fatalf("open converted xps: %v", err)
	}
	defer zr.Close()

	var combined strings.Builder
	for _, f := range zr.File {
		rc, _ := f.Open()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		_ = rc.Close()
		combined.WriteString(buf.String())
	}
	allXML := combined.String()
	if strings.Contains(allXML, "schemas.openxps.org") {
		t.Errorf("Converted XPS still contains OpenXPS namespace")
	}
	if !strings.Contains(allXML, "schemas.microsoft.com/xps/2005/06") {
		t.Errorf("Converted XPS missing MS-XPS 2005/06 namespace")
	}
}

func TestIsVirtualPrinterName_AllVirtualEntries(t *testing.T) {
	virtual := []struct {
		name string
		port string
	}{
		{"Print on OfficePC", "Bridge - OfficePC (9101)"},
		{"Generic / Text Only", "FILE:"},
		{"Snagit 2024", "Snagit24"},
		{"Microsoft OpenXPS Class Driver", "PORTPROMPT:"},
		{"Any Printer", "FILE:"},
		{"PDF24", "PDF24Port"},
	}
	for _, v := range virtual {
		if !IsVirtualPrinterName(v.name, v.port) {
			t.Errorf("Expected IsVirtualPrinterName(%q, %q) == true", v.name, v.port)
		}
	}
	if IsVirtualPrinterName("HP Smart Tank 580 series", "USB001") {
		t.Errorf("Expected physical HP printer not to be flagged virtual")
	}
}

func assertPowerShellASTValid(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	t.Setenv("BRIDGE_TEST_PS_SCRIPT", script)
	psCheck := `$errors = $null; $tokens = $null; [System.Management.Automation.Language.Parser]::ParseInput($env:BRIDGE_TEST_PS_SCRIPT, [ref]$tokens, [ref]$errors) | Out-Null; if ($errors.Count -gt 0) { $errors | ForEach-Object { Write-Error $_.Message }; exit 1 }`
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psCheck).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell AST parse failed: %v\nOutput: %s\nScript:\n%s", err, string(out), script)
	}
}

func TestBuildNativeXPSScript_TicketValidationAndAST(t *testing.T) {
	t.Run("LandscapeA4MultiCopy", func(t *testing.T) {
		geom := DocumentGeometry{
			Format:      ".oxps",
			PaperName:   "A4",
			Orientation: "portrait",
			PageCount:   2,
		}
		opts := PrintOptions{
			Orientation: "landscape",
			Copies:      3,
		}
		script := buildNativeXPSScript("HP Smart Tank 580", `C:\Temp\doc.xps`, geom, opts)
		assertPowerShellASTValid(t, script)

		required := []string{
			`$ticket.PageOrientation = [System.Printing.PageOrientation]::Landscape`,
			`$ticket.PageMediaSize = New-Object System.Printing.PageMediaSize([System.Printing.PageMediaSizeName]::ISOA4)`,
			`$ticket.CopyCount = 3`,
			`$validatedRes = $pq.MergeAndValidatePrintTicket($pq.DefaultPrintTicket, $ticket)`,
			`$writer.Write($xpsDoc.GetFixedDocumentSequence(), $validatedRes.ValidatedPrintTicket)`,
			`try {`,
			`} finally {`,
			`if ($xpsDoc) { $xpsDoc.Close() }`,
		}
		for _, want := range required {
			if !strings.Contains(script, want) {
				t.Errorf("Expected script to contain %q, got:\n%s", want, script)
			}
		}
		if strings.Contains(script, "PageScalingFactor") {
			t.Errorf("Script must NOT force PageScalingFactor, got:\n%s", script)
		}
	})

	t.Run("MixedOrientationAndSizeDoesNotForcePortrait", func(t *testing.T) {
		geom := DocumentGeometry{
			Format:            ".xps",
			PaperName:         "",
			Orientation:       "",
			MixedSizes:        true,
			MixedOrientations: true,
			PageCount:         4,
		}
		script := buildNativeXPSScript("HP 'Office' Printer", `C:\Users\o'neil\mixed.xps`, geom, PrintOptions{Copies: 1})
		assertPowerShellASTValid(t, script)

		if strings.Contains(script, "PageOrientation") {
			t.Errorf("Mixed orientation document must NOT force PageOrientation, got:\n%s", script)
		}
		if strings.Contains(script, "PageMediaSize") {
			t.Errorf("Mixed/unknown paper size must NOT force PageMediaSize, got:\n%s", script)
		}
		if strings.Contains(script, "CopyCount") {
			t.Errorf("Single copy must NOT force CopyCount, got:\n%s", script)
		}
		if strings.Contains(script, "PageScalingFactor") {
			t.Errorf("Script must NOT set PageScalingFactor, got:\n%s", script)
		}
		if !strings.Contains(script, `'HP ''Office'' Printer'`) || !strings.Contains(script, `'C:\Users\o''neil\mixed.xps'`) {
			t.Errorf("Expected single quotes escaped in PowerShell literals, got:\n%s", script)
		}
		if !strings.Contains(script, "MergeAndValidatePrintTicket($pq.DefaultPrintTicket, $ticket)") {
			t.Errorf("Expected MergeAndValidatePrintTicket call, got:\n%s", script)
		}
	})

	t.Run("AllSupportedPaperSizes", func(t *testing.T) {
		cases := map[string]string{
			"A4":        "ISOA4",
			"A3":        "ISOA3",
			"A2":        "ISOA2",
			"A1":        "ISOA1",
			"A0":        "ISOA0",
			"A5":        "ISOA5",
			"A6":        "ISOA6",
			"Letter":    "NorthAmericaLetter",
			"Legal":     "NorthAmericaLegal",
			"Tabloid":   "NorthAmericaTabloid",
			"Statement": "NorthAmericaStatement",
		}
		for paper, enumName := range cases {
			script := buildNativeXPSScript("Printer", `C:\doc.xps`, DocumentGeometry{PaperName: paper, Orientation: "portrait"}, PrintOptions{})
			if !strings.Contains(script, "::"+enumName+")") {
				t.Errorf("Paper %q expected enum %q in script:\n%s", paper, enumName, script)
			}
		}
	})
}

func TestExecutePrint_NativeXPSFirstOnWindowsAndFallback(t *testing.T) {
	tempDir := t.TempDir()
	realSumatra := filepath.Join(tempDir, "SumatraPDF.exe")
	dummyExe := make([]byte, 1024*1024+16)
	copy(dummyExe, "MZ")
	if err := os.WriteFile(realSumatra, dummyExe, 0755); err != nil {
		t.Fatalf("write dummy SumatraPDF.exe: %v", err)
	}
	mockSumatra := filepath.Join(tempDir, "mock_sumatra.exe")

	oxpsPath := filepath.Join(tempDir, "spool.oxps")
	if err := os.WriteFile(oxpsPath, buildTestXPSArchive(t, true), 0644); err != nil {
		t.Fatalf("write spool.oxps: %v", err)
	}
	xpsPath := filepath.Join(tempDir, "spool.xps")
	if err := os.WriteFile(xpsPath, buildTestXPSArchive(t, false), 0644); err != nil {
		t.Fatalf("write spool.xps: %v", err)
	}
	pdfPath := filepath.Join(tempDir, "doc.pdf")
	writeSampleA4PDF(t, pdfPath)

	t.Run("Windows_OXPS_RoutesNativeFirst", func(t *testing.T) {
		var order []string
		out, err := executePrintWithRunners(
			context.Background(),
			"windows",
			realSumatra,
			"HP Smart Tank 580",
			oxpsPath,
			DocumentGeometry{Format: ".oxps", PaperName: "A4", Orientation: "portrait", PageCount: 1},
			PrintOptions{},
			func(ctx context.Context, bin, printer, settings, file string) ([]byte, error) {
				order = append(order, "sumatra")
				return []byte("sumatra ok"), nil
			},
			func(ctx context.Context, printer, file string, geom DocumentGeometry, opts PrintOptions) ([]byte, error) {
				order = append(order, "native")
				return []byte("native ok"), nil
			},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(out) != "native ok" || len(order) != 1 || order[0] != "native" {
			t.Errorf("expected native XPS first and no Sumatra fallback on success, got order=%v out=%q", order, string(out))
		}
	})

	t.Run("Windows_XPS_NativeErrorFallsBackToSumatra", func(t *testing.T) {
		var order []string
		out, err := executePrintWithRunners(
			context.Background(),
			"windows",
			realSumatra,
			"HP Smart Tank 580",
			xpsPath,
			DocumentGeometry{Format: ".xps", PaperName: "A4", Orientation: "portrait", PageCount: 1},
			PrintOptions{},
			func(ctx context.Context, bin, printer, settings, file string) ([]byte, error) {
				order = append(order, "sumatra")
				return []byte("sumatra fallback ok"), nil
			},
			func(ctx context.Context, printer, file string, geom DocumentGeometry, opts PrintOptions) ([]byte, error) {
				order = append(order, "native")
				return []byte("native failed"), errors.New("print queue unavailable")
			},
		)
		if err != nil {
			t.Fatalf("expected Sumatra fallback to succeed, got error: %v", err)
		}
		if string(out) != "sumatra fallback ok" || len(order) != 2 || order[0] != "native" || order[1] != "sumatra" {
			t.Errorf("expected [native sumatra] order, got order=%v out=%q", order, string(out))
		}
	})

	t.Run("Windows_OXPS_CanceledContextSkipsFallback", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var order []string
		_, err := executePrintWithRunners(
			ctx,
			"windows",
			realSumatra,
			"HP Smart Tank 580",
			oxpsPath,
			DocumentGeometry{Format: ".oxps", PaperName: "A4", Orientation: "portrait", PageCount: 1},
			PrintOptions{},
			func(ctx context.Context, bin, printer, settings, file string) ([]byte, error) {
				order = append(order, "sumatra")
				return nil, nil
			},
			func(ctx context.Context, printer, file string, geom DocumentGeometry, opts PrintOptions) ([]byte, error) {
				order = append(order, "native")
				cancel()
				return nil, context.Canceled
			},
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if len(order) != 1 || order[0] != "native" {
			t.Errorf("expected no fallback after context cancellation, got order=%v", order)
		}
	})

	t.Run("Windows_MockSumatra_StillUsesMock", func(t *testing.T) {
		var order []string
		_, err := executePrintWithRunners(
			context.Background(),
			"windows",
			mockSumatra,
			"MockPrinter",
			oxpsPath,
			DocumentGeometry{Format: ".oxps", PaperName: "A4", Orientation: "portrait", PageCount: 1},
			PrintOptions{},
			func(ctx context.Context, bin, printer, settings, file string) ([]byte, error) {
				order = append(order, "sumatra")
				return []byte("mock ok"), nil
			},
			func(ctx context.Context, printer, file string, geom DocumentGeometry, opts PrintOptions) ([]byte, error) {
				order = append(order, "native")
				return nil, nil
			},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(order) != 1 || order[0] != "sumatra" {
			t.Errorf("expected mock sumatra to run directly for tests, got order=%v", order)
		}
	})

	t.Run("Windows_PDF_RoutesToSumatra", func(t *testing.T) {
		var order []string
		_, err := executePrintWithRunners(
			context.Background(),
			"windows",
			realSumatra,
			"HP Smart Tank 580",
			pdfPath,
			DocumentGeometry{Format: ".pdf", PaperName: "A4", Orientation: "portrait", PageCount: 1},
			PrintOptions{},
			func(ctx context.Context, bin, printer, settings, file string) ([]byte, error) {
				order = append(order, "sumatra")
				return []byte("sumatra pdf ok"), nil
			},
			func(ctx context.Context, printer, file string, geom DocumentGeometry, opts PrintOptions) ([]byte, error) {
				order = append(order, "native")
				return nil, nil
			},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(order) != 1 || order[0] != "sumatra" {
			t.Errorf("expected PDF to route through SumatraPDF, got order=%v", order)
		}
	})

	t.Run("ExecutePrint_RealMockExeOnOXPS", func(t *testing.T) {
		builtMock, logFile := buildMockSumatraWithArgsLog(t, tempDir)
		out, err := executePrint(
			context.Background(),
			builtMock,
			"MockPrinter",
			oxpsPath,
			DocumentGeometry{Format: ".oxps", PaperName: "A4", Orientation: "portrait", PageCount: 1},
			PrintOptions{},
		)
		if err != nil {
			t.Fatalf("executePrint with mock failed: %v (%s)", err, string(out))
		}
		logged, _ := os.ReadFile(logFile)
		if !strings.Contains(string(logged), "-print-to|MockPrinter") {
			t.Errorf("expected mock exe log to record print call, got %q", string(logged))
		}
	})
}

func TestPrintJob_FriendlyLabelAndMetadata(t *testing.T) {
	cases := []struct {
		name string
		job  PrintJob
		want string
	}{
		{
			name: "SpoolOXPSWithSourceNode",
			job:  PrintJob{FileName: "print_1700000000123.oxps", SourceNode: "OfficePC"},
			want: "Document from OfficePC",
		},
		{
			name: "SpoolPDFWithoutSourceNode",
			job:  PrintJob{FileName: "print_1700000000123.pdf"},
			want: "Remote Document",
		},
		{
			name: "DefaultSpoolNameWithOptionsSourceNode",
			job:  PrintJob{FileName: "print_job.pdf", Options: PrintOptions{SourceNode: "Laptop"}},
			want: "Document from Laptop",
		},
		{
			name: "NamedFileWithSourceNode",
			job:  PrintJob{FileName: "Quarterly_Report.pdf", SourceNode: "FinancePC"},
			want: "Quarterly_Report.pdf from FinancePC",
		},
		{
			name: "NamedFileWithoutSourceNode",
			job:  PrintJob{FileName: "invoice.pdf"},
			want: "invoice.pdf",
		},
		{
			name: "SpoolFileWithJobNameOption",
			job:  PrintJob{FileName: "print_999.oxps", SourceNode: "DesignMac", Options: PrintOptions{JobName: "Brochure.pdf"}},
			want: "Brochure.pdf from DesignMac",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.job.FriendlyLabel(); got != tc.want {
				t.Errorf("FriendlyLabel() = %q, want %q", got, tc.want)
			}
		})
	}

	// Verify SaveIncomingJobWithOptions propagates SourceNode and JobName while preserving disk extension
	tempDir := t.TempDir()
	qDir := filepath.Join(tempDir, "queue")
	fDir := filepath.Join(tempDir, "failed")
	ps := NewPrintService("mock_sumatra.exe", 5, qDir, fDir)
	_ = os.MkdirAll(qDir, 0755)
	_ = os.MkdirAll(fDir, 0755)

	oxpsBytes := buildTestXPSArchive(t, true)
	job, err := ps.SaveIncomingJobWithOptions("print_1700000000999.oxps", bytes.NewReader(oxpsBytes), PrintOptions{
		SourceNode: "TestPeer",
		JobName:    "Annual Budget",
	})
	if err != nil {
		t.Fatalf("SaveIncomingJobWithOptions failed: %v", err)
	}
	if job.SourceNode != "TestPeer" {
		t.Errorf("expected job.SourceNode == %q, got %q", "TestPeer", job.SourceNode)
	}
	if job.FileName != "Annual Budget" {
		t.Errorf("expected job.FileName == %q, got %q", "Annual Budget", job.FileName)
	}
	if filepath.Ext(job.FilePath) != ".oxps" {
		t.Errorf("expected disk FilePath to retain .oxps extension, got %q", job.FilePath)
	}
	if got := job.FriendlyLabel(); got != "Annual Budget from TestPeer" {
		t.Errorf("expected FriendlyLabel() == %q, got %q", "Annual Budget from TestPeer", got)
	}
	_ = time.Now()
}

func TestIsPhysicalPrinterUnavailable(t *testing.T) {
	cases := []struct {
		name string
		p    WindowsPrinter
		want bool
	}{
		{
			name: "OnlineReadyPrinter",
			p:    WindowsPrinter{Name: "HP Smart Tank 580", PortName: "USB001", WorkOffline: false, Status: 0},
			want: false,
		},
		{
			name: "PrintingActiveStatusIsAvailable",
			p:    WindowsPrinter{Name: "HP Smart Tank 580", PortName: "USB001", WorkOffline: false, Status: 0x00000400},
			want: false,
		},
		{
			name: "WorkOfflineAttribute",
			p:    WindowsPrinter{Name: "HP Smart Tank 580", PortName: "USB001", WorkOffline: true, Status: 0},
			want: true,
		},
		{
			name: "StatusOfflineBit",
			p:    WindowsPrinter{Name: "Canon LBP2900", PortName: "USB002", WorkOffline: false, Status: 0x00000080},
			want: true,
		},
		{
			name: "StatusNotAvailableBit",
			p:    WindowsPrinter{Name: "Epson L3150", PortName: "USB003", WorkOffline: false, Status: 0x00001000},
			want: true,
		},
		{
			name: "WorkOfflineAndStatusCombined",
			p:    WindowsPrinter{Name: "Brother HL-L2350DW", PortName: "WSD-1", WorkOffline: true, Status: 0x00001080},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPhysicalPrinterUnavailable(tc.p); got != tc.want {
				t.Errorf("isPhysicalPrinterUnavailable(%+v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

func TestResolvePhysicalPrinterFromList(t *testing.T) {
	t.Run("DefaultPhysicalAndOnline", func(t *testing.T) {
		printers := []WindowsPrinter{
			{Name: "Microsoft Print to PDF", PortName: "PORTPROMPT:"},
			{Name: "HP OfficeJet", PortName: "USB001", Default: true},
		}
		got, err := resolvePhysicalPrinterFromList("HP OfficeJet", printers)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "HP OfficeJet" {
			t.Errorf("got %q, want %q", got, "HP OfficeJet")
		}
	})

	t.Run("DefaultOfflineFallsBackToOnlinePhysical", func(t *testing.T) {
		printers := []WindowsPrinter{
			{Name: "HP Offline USB", PortName: "USB001", Default: true, WorkOffline: true},
			{Name: "Canon Network", PortName: "192.168.1.50"},
		}
		got, err := resolvePhysicalPrinterFromList("HP Offline USB", printers)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "Canon Network" {
			t.Errorf("got %q, want %q", got, "Canon Network")
		}
	})

	t.Run("OnlyOfflinePhysicalPrintersReturnsWorkOfflineError", func(t *testing.T) {
		printers := []WindowsPrinter{
			{Name: "Microsoft Print to PDF", PortName: "PORTPROMPT:", Default: true},
			{Name: "HP Smart Tank 580", PortName: "USB001", WorkOffline: true},
			{Name: "Epson L3150", PortName: "USB002", Status: 0x00000080},
		}
		got, err := resolvePhysicalPrinterFromList("Microsoft Print to PDF", printers)
		if err == nil {
			t.Fatalf("expected error, got printer %q", got)
		}
		if !strings.Contains(err.Error(), "HP Smart Tank 580") || !strings.Contains(err.Error(), "WorkOffline") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("OnlyVirtualPrintersReturnsNoPhysicalPrinter", func(t *testing.T) {
		printers := []WindowsPrinter{
			{Name: "Microsoft Print to PDF", PortName: "PORTPROMPT:", Default: true},
			{Name: "Fax", PortName: "SHRFAX:"},
		}
		got, err := resolvePhysicalPrinterFromList("Microsoft Print to PDF", printers)
		if err == nil {
			t.Fatalf("expected error, got printer %q", got)
		}
		if !strings.Contains(err.Error(), "no physical printer connected on this computer") {
			t.Errorf("unexpected error message: %v", err)
		}
	})
}

func TestPrintService_CheckPrinterReady(t *testing.T) {
	t.Run("SetTargetPrinterOverride", func(t *testing.T) {
		ps := NewPrintService("", 10, t.TempDir(), t.TempDir())
		ps.SetTargetPrinter("HP_LaserJet_400")
		ps.resolveTargetPrinter = func() (string, error) {
			return "", errors.New("should not be called when targetPrinter is set")
		}
		got, err := ps.CheckPrinterReady()
		if err != nil {
			t.Fatalf("CheckPrinterReady failed: %v", err)
		}
		if got != "HP_LaserJet_400" {
			t.Errorf("got %q, want %q", got, "HP_LaserJet_400")
		}
	})

	t.Run("ResolveTargetPrinterSuccess", func(t *testing.T) {
		ps := NewPrintService("", 10, t.TempDir(), t.TempDir())
		ps.resolveTargetPrinter = func() (string, error) {
			return "Canon_PIXMA", nil
		}
		got, err := ps.CheckPrinterReady()
		if err != nil {
			t.Fatalf("CheckPrinterReady failed: %v", err)
		}
		if got != "Canon_PIXMA" {
			t.Errorf("got %q, want %q", got, "Canon_PIXMA")
		}
	})

	t.Run("MockSumatraPathReturnsMockPrinter", func(t *testing.T) {
		ps := NewPrintService(`C:\temp\Mock_SumatraPDF.exe`, 10, t.TempDir(), t.TempDir())
		got, err := ps.CheckPrinterReady()
		if err != nil {
			t.Fatalf("CheckPrinterReady failed: %v", err)
		}
		if got != "MockPrinter" {
			t.Errorf("got %q, want %q", got, "MockPrinter")
		}
	})

	t.Run("FailingPrinterCheckWrapsErrNoPhysicalPrinter", func(t *testing.T) {
		ps := NewPrintService("", 10, t.TempDir(), t.TempDir())
		ps.resolveTargetPrinter = func() (string, error) {
			return "", errors.New("physical printer 'HP Smart Tank' is offline or disconnected (WorkOffline)")
		}
		got, err := ps.CheckPrinterReady()
		if err == nil {
			t.Fatalf("expected error, got printer %q", got)
		}
		if !errors.Is(err, ErrNoPhysicalPrinter) {
			t.Errorf("expected errors.Is(err, ErrNoPhysicalPrinter) == true, got %v", err)
		}
	})
}

func TestSanitizeXPSForNativePrintStripsSourcePrintTickets(t *testing.T) {
	tempDir := t.TempDir()
	oxpsPath := filepath.Join(tempDir, "mxdw_sample.oxps")

	f, err := os.Create(oxpsPath)
	if err != nil {
		t.Fatalf("create oxps: %v", err)
	}
	zw := zip.NewWriter(f)
	entries := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml" /><Default Extension="fdseq" ContentType="application/vnd.ms-package.xps-fixeddocumentsequence+xml" /><Default Extension="fdoc" ContentType="application/vnd.ms-package.xps-fixeddocument+xml" /><Default Extension="fpage" ContentType="application/vnd.ms-package.xps-fixedpage+xml" /><Override PartName="/Metadata/Job_PT.xml" ContentType="application/vnd.ms-printing.printticket+xml" /><Override PartName="/Documents/1/Metadata/Page1_PT.xml" ContentType="application/vnd.ms-printing.printticket+xml" /></Types>`,
		"_rels/.rels":          `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Target="FixedDocumentSequence.fdseq" Id="R0" Type="http://schemas.openxps.org/oxps/v1.0/fixedrepresentation" /></Relationships>`,
		"FixedDocumentSequence.fdseq": `<?xml version="1.0" encoding="UTF-8"?><FixedDocumentSequence xmlns="http://schemas.openxps.org/oxps/v1.0"><DocumentReference Source="Documents/1/FixedDocument.fdoc" /></FixedDocumentSequence>`,
		"_rels/FixedDocumentSequence.fdseq.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Target="Metadata/Job_PT.xml" Id="R0" Type="http://schemas.openxps.org/oxps/v1.0/printticket" /></Relationships>`,
		"Documents/1/FixedDocument.fdoc": `<?xml version="1.0" encoding="UTF-8"?><FixedDocument xmlns="http://schemas.openxps.org/oxps/v1.0"><PageContent Source="Pages/1.fpage" /></FixedDocument>`,
		"Documents/1/_rels/FixedDocument.fdoc.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Target="../../Metadata/MXDC_Empty_PT.xml" Id="R0" Type="http://schemas.openxps.org/oxps/v1.0/printticket" /></Relationships>`,
		"Documents/1/Pages/1.fpage": `<FixedPage Width="793.76" Height="1122.56" xmlns="http://schemas.openxps.org/oxps/v1.0" xml:lang="und"><Path Data="F1 M 48,48 L 720,48 720,1008 48,1008 z" Stroke="#ff0000ff" /></FixedPage>`,
		"Documents/1/Pages/_rels/1.fpage.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Target="../Metadata/Page1_PT.xml" Id="R1" Type="http://schemas.openxps.org/oxps/v1.0/printticket" /></Relationships>`,
		"Metadata/Job_PT.xml":              `<psf:PrintTicket xmlns:psf="http://schemas.microsoft.com/windows/2003/08/printing/printschemaframework"><psf:ParameterInit name="ns0000:PageDevmodeSnapshot"><psf:Value>MXDW</psf:Value></psf:ParameterInit></psf:PrintTicket>`,
		"Metadata/MXDC_Empty_PT.xml":       `<psf:PrintTicket xmlns:psf="http://schemas.microsoft.com/windows/2003/08/printing/printschemaframework"/>`,
		"Documents/1/Metadata/Page1_PT.xml": `<psf:PrintTicket xmlns:psf="http://schemas.microsoft.com/windows/2003/08/printing/printschemaframework"><psf:Feature name="psk:PageResolution"/></psf:PrintTicket>`,
	}
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	_ = f.Close()

	cleanPath, err := convertOXPStoXPS(oxpsPath)
	if err != nil {
		t.Fatalf("convertOXPStoXPS failed: %v", err)
	}
	defer os.Remove(cleanPath)

	zr, err := zip.OpenReader(cleanPath)
	if err != nil {
		t.Fatalf("open sanitized xps: %v", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		lower := strings.ToLower(entry.Name)
		if strings.Contains(lower, "_pt.xml") {
			t.Errorf("source PrintTicket part %q should have been stripped from sanitized XPS", entry.Name)
		}
		rc, err := entry.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", entry.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read entry %s: %v", entry.Name, err)
		}
		txt := string(data)
		if strings.Contains(txt, "schemas.openxps.org/oxps/v1.0") {
			t.Errorf("entry %s still contains OpenXPS namespace", entry.Name)
		}
		if strings.Contains(strings.ToLower(txt), "/printticket") {
			t.Errorf("entry %s still contains printticket relationship", entry.Name)
		}
	}

	if runtime.GOOS == "windows" {
		psCmd := fmt.Sprintf(`Add-Type -AssemblyName ReachFramework, System.Printing, PresentationCore, PresentationFramework, WindowsBase
$doc = New-Object System.Windows.Xps.Packaging.XpsDocument('%s', [System.IO.FileAccess]::Read)
try {
    $r = $doc.FixedDocumentSequenceReader
    $fd = $r.FixedDocuments[0]
    $fp = $fd.FixedPages[0]
    if ($r.PrintTicket -ne $null -or $fd.PrintTicket -ne $null -or $fp.PrintTicket -ne $null) {
        throw "Expected null cloned PrintTickets"
    }
    $pag = $doc.GetFixedDocumentSequence().DocumentPaginator
    if ($pag.PageCount -ne 1) { throw "Expected 1 page" }
} finally {
    $doc.Close()
}`, strings.ReplaceAll(cleanPath, `'`, `''`))
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psCmd)
		hideCmd(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ReachFramework XpsDocument failed to load sanitized XPS: %v (%s)", err, string(out))
		}
	}
}
