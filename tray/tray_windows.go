//go:build windows

package tray

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"bridge/config"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procShowWindow            = user32.NewProc("ShowWindow")

	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procLoadImageW             = user32.NewProc("LoadImageW")
	procDestroyIcon            = user32.NewProc("DestroyIcon")
	procMessageBoxW            = user32.NewProc("MessageBoxW")
	procPostMessageW           = user32.NewProc("PostMessageW")

	procShell_NotifyIconW      = shell32.NewProc("Shell_NotifyIconW")
	procGetOpenFileNameW       = comdlg32.NewProc("GetOpenFileNameW")
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")

	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")

	wmTaskbarCreated uint32
)

const (
	NIM_ADD        = 0x00000000
	NIM_MODIFY     = 0x00000001
	NIM_DELETE     = 0x00000002
	NIM_SETVERSION = 0x00000004

	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004
	NIF_INFO    = 0x00000010

	NIIF_NONE    = 0x00000000
	NIIF_INFO    = 0x00000001
	NIIF_WARNING = 0x00000002
	NIIF_ERROR   = 0x00000003

	WM_DESTROY     = 0x0002
	WM_USER        = 0x0400
	WM_TRAYMESSAGE = WM_USER + 100
	WM_COMMAND     = 0x0111
	WM_UPDATE_TRAY = WM_USER + 101

	WM_LBUTTONUP     = 0x0202
	WM_RBUTTONUP     = 0x0205
	WM_LBUTTONDBLCLK = 0x0203

	MF_STRING    = 0x00000000
	MF_SEPARATOR = 0x00000800
	MF_GRAYED    = 0x00000001

	TPM_BOTTOMALIGN = 0x0020
	TPM_LEFTALIGN   = 0x0000

	IMAGE_ICON      = 1
	LR_LOADFROMFILE = 0x00000010
	LR_DEFAULTSIZE  = 0x00000040

	MB_OK              = 0x00000000
	MB_YESNO           = 0x00000004
	MB_ICONWARNING     = 0x00000030
	MB_ICONINFORMATION = 0x00000040
	MB_SETFOREGROUND   = 0x00010000
	IDYES              = 6
)

const printFileFilter = "PDF, XPS & Images (*.pdf;*.xps;*.oxps;*.png;*.jpg;*.jpeg;*.bmp;*.tif;*.tiff)\x00*.pdf;*.xps;*.oxps;*.png;*.jpg;*.jpeg;*.bmp;*.tif;*.tiff\x00" +
	"PDF Documents (*.pdf)\x00*.pdf\x00" +
	"XPS Documents (*.xps;*.oxps)\x00*.xps;*.oxps\x00" +
	"Images (*.png;*.jpg;*.jpeg;*.bmp;*.tif;*.tiff)\x00*.png;*.jpg;*.jpeg;*.bmp;*.tif;*.tiff\x00" +
	"All Files (*.*)\x00*.*\x00\x00"

type POINT struct {
	X int32
	Y int32
}

type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type NOTIFYICONDATAW struct {
	CbSize            uint32
	HWnd              uintptr
	UID               uint32
	UFlags            uint32
	UCallbackMessage  uint32
	HIcon             uintptr
	SzTip             [128]uint16
	DwState           uint32
	DwStateMask       uint32
	SzInfo            [256]uint16
	UTimeoutOrVersion uint32
	SzInfoTitle       [64]uint16
	DwInfoFlags       uint32
	GuidItem          [16]byte
	HBalloonIcon      uintptr
}

type OPENFILENAMEW struct {
	LStructSize       uint32
	HwndOwner         uintptr
	HInstance         uintptr
	LpstrFilter       *uint16
	LpstrCustomFilter *uint16
	NMaxCustFilter    uint32
	NFilterIndex      uint32
	LpstrFile         *uint16
	NMaxFile          uint32
	LpstrFileTitle    *uint16
	NMaxFileTitle     uint32
	LpstrInitialDir   *uint16
	LpstrTitle        *uint16
	Flags             uint32
	NFileOffset       uint16
	NFileExtension    uint16
	LpstrDefExt       *uint16
	LCustData         uintptr
	LpfnHook          uintptr
	LpTemplateName    *uint16
	PvReserved        uintptr
	DwReserved        uint32
	FlagsEx           uint32
}

var globalTray *TrayManager

// NewTrayManager initializes icons and prepares the tray manager.
func NewTrayManager(iconDir, nodeName, peerName string, cb TrayCallbacks) (*TrayManager, error) {
	icons, err := EnsureIcons(iconDir)
	if err != nil {
		return nil, err
	}

	tm := &TrayManager{
		icons:        icons,
		hIcons:       make(map[TrayState]uintptr),
		currentState: StateOffline,
		currentTip:   "Bridge: Starting up...",
		callbacks:    cb,
		nodeName:     nodeName,
		peerName:     peerName,
		peers:        make([]config.PeerDevice, 0),
	}
	globalTray = tm

	tm.loadIconHandles()
	return tm, nil
}

func (tm *TrayManager) loadIconHandles() {
	// Query current system small icon metrics to scale crisply across 100%, 125%, 150%, 200% High-DPI taskbars
	cx, _, _ := procGetSystemMetrics.Call(49) // SM_CXSMICON
	cy, _, _ := procGetSystemMetrics.Call(50) // SM_CYSMICON
	if cx == 0 {
		cx = 16
	}
	if cy == 0 {
		cy = 16
	}

	loadIco := func(path string) uintptr {
		p, _ := syscall.UTF16PtrFromString(path)
		h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)), IMAGE_ICON, cx, cy, LR_LOADFROMFILE)
		return h
	}

	tm.hIcons[StateDisconnected] = loadIco(tm.icons.PrinterDisconnected)
	tm.hIcons[StatePartial] = loadIco(tm.icons.PrinterGray)
	tm.hIcons[StateConnected] = loadIco(tm.icons.PrinterGreen)
	tm.hIcons[StatePrinting] = loadIco(tm.icons.PrinterLoading)
	tm.hIcons[StatePrintCompleted] = loadIco(tm.icons.PrinterBlue)
	tm.hIcons[StateSyncing] = loadIco(tm.icons.CloudLoading)
	tm.hIcons[StateSyncCompleted] = loadIco(tm.icons.CloudBlue)
}

// Start begins the Win32 window and tray icon event loop in a dedicated thread.
func (tm *TrayManager) Start() error {
	readyChan := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		tbCreatedStr, _ := syscall.UTF16PtrFromString("TaskbarCreated")
		rMsg, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(tbCreatedStr)))
		wmTaskbarCreated = uint32(rMsg)

		className, _ := syscall.UTF16PtrFromString("BridgeTrayWndClass")
		wndProcPtr := syscall.NewCallback(trayWndProc)

		var wc WNDCLASSEXW
		wc.CbSize = uint32(unsafe.Sizeof(wc))
		wc.LpfnWndProc = wndProcPtr
		wc.LpszClassName = className

		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			errno, ok := err.(syscall.Errno)
			if !ok || errno != 1410 { // 1410 = ERROR_CLASS_ALREADY_EXISTS
				readyChan <- fmt.Errorf("failed to register tray window class: %v", err)
				return
			}
		}

		wndTitle, _ := syscall.UTF16PtrFromString("BridgeTrayWindow")
		hwnd, _, err := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(wndTitle)),
			0, 0, 0, 0, 0, 0, 0, 0, 0,
		)
		if hwnd == 0 {
			readyChan <- fmt.Errorf("failed to create tray window: %v", err)
			return
		}

		tm.hwnd = hwnd

		// Add tray icon to taskbar
		nid := tm.buildNotifyData(tm.currentState, tm.currentTip)
		if r, _, _ := procShell_NotifyIconW.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid))); r == 0 {
			readyChan <- fmt.Errorf("failed to add icon to system tray")
			return
		}

		readyChan <- nil

		// Message loop
		var msg struct {
			HWnd    uintptr
			Message uint32
			WParam  uintptr
			LParam  uintptr
			Time    uint32
			Pt      POINT
		}

		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}()

	return <-readyChan
}

// Stop removes the icon from the system tray, cancels timers, and destroys the window.
func (tm *TrayManager) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped {
		return
	}
	tm.stopped = true
	if tm.syncDebounceTimer != nil {
		tm.syncDebounceTimer.Stop()
		tm.syncDebounceTimer = nil
	}
	tm.syncDebounceCount = 0
	tm.syncDebounceToken++

	if tm.hwnd != 0 {
		var nid NOTIFYICONDATAW
		nid.CbSize = uint32(unsafe.Sizeof(nid))
		nid.HWnd = tm.hwnd
		nid.UID = 1
		procShell_NotifyIconW.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))
		procPostMessageW.Call(tm.hwnd, WM_DESTROY, 0, 0)
	}

	for st, h := range tm.hIcons {
		if h != 0 {
			procDestroyIcon.Call(h)
		}
		delete(tm.hIcons, st)
	}
}

func (tm *TrayManager) modifyNotifyLocked() {
	if tm.stopped || tm.hwnd == 0 {
		return
	}
	nid := tm.buildNotifyData(tm.currentState, tm.currentTip)
	procShell_NotifyIconW.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
}

func notifyInfoFlag(level NotifyLevel) uint32 {
	switch level {
	case NotifyError:
		return NIIF_ERROR
	case NotifyWarning:
		return NIIF_WARNING
	default:
		return NIIF_INFO
	}
}

func (tm *TrayManager) showBalloonLocked(title, message string, level NotifyLevel) {
	if tm.stopped || tm.hwnd == 0 {
		return
	}
	nid := tm.buildNotifyData(tm.currentState, tm.currentTip)
	nid.UFlags |= NIF_INFO
	nid.DwInfoFlags = notifyInfoFlag(level)
	nid.UTimeoutOrVersion = 10000

	copyUTF16Z(nid.SzInfoTitle[:], title)
	copyUTF16Z(nid.SzInfo[:], message)

	procShell_NotifyIconW.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
}

// ShowBalloon displays a native Windows taskbar notification bubble, inferring severity from title.
func (tm *TrayManager) ShowBalloon(title, message string) {
	tm.ShowNotification(title, message)
}

func copyUTF16Z(dst []uint16, s string) {
	if len(dst) == 0 {
		return
	}
	clear(dst)
	u16, err := syscall.UTF16FromString(s)
	if err != nil {
		u16 = utf16.Encode([]rune(strings.ReplaceAll(s, "\x00", "")))
	}
	maxChars := len(dst) - 1
	n := copy(dst[:maxChars], u16)
	if len(u16) > maxChars && n == maxChars && n > 0 && dst[n-1] >= 0xD800 && dst[n-1] <= 0xDBFF {
		dst[n-1] = 0
	}
	dst[maxChars] = 0
}

func (tm *TrayManager) buildNotifyData(state TrayState, tip string) NOTIFYICONDATAW {
	var nid NOTIFYICONDATAW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = tm.hwnd
	nid.UID = 1
	nid.UFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP
	nid.UCallbackMessage = WM_TRAYMESSAGE
	nid.HIcon = tm.hIcons[state]

	copyUTF16Z(nid.SzTip[:], tip)
	return nid
}

func trayWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	tm := globalTray
	if tm == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
		return r
	}

	if wmTaskbarCreated != 0 && msg == wmTaskbarCreated {
		tm.mu.Lock()
		nid := tm.buildNotifyData(tm.currentState, tm.currentTip)
		tm.mu.Unlock()
		procShell_NotifyIconW.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid)))
		return 0
	}

	switch msg {
	case WM_TRAYMESSAGE:
		switch lparam {
		case WM_LBUTTONUP, WM_RBUTTONUP:
			tm.showContextMenu()
		case WM_LBUTTONDBLCLK:
			if tm.callbacks.OnOpenShared != nil {
				tm.callbacks.OnOpenShared()
			}
		}
		return 0

	case WM_COMMAND:
		cmdID := int(wparam & 0xFFFF)
		tm.dispatchMenuCommand(cmdID)
		return 0

	case WM_DESTROY:
		procPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}

func (tm *TrayManager) dispatchMenuCommand(cmdID int) {
	switch {
	case cmdID == CmdOpenShared:
		if tm.callbacks.OnOpenShared != nil {
			go tm.callbacks.OnOpenShared()
		}
	case cmdID == CmdOpenDashboard:
		if tm.callbacks.OnOpenDashboard != nil {
			go tm.callbacks.OnOpenDashboard()
		}
	case cmdID == CmdPairDevice:
		if tm.callbacks.OnPairDevice != nil {
			go tm.callbacks.OnPairDevice()
		}
	case cmdID >= CmdConnectDiscoveredBase && cmdID < CmdConnectDiscoveredBase+100:
		idx := cmdID - CmdConnectDiscoveredBase
		tm.mu.Lock()
		var dev DiscoveredDevice
		found := false
		if idx >= 0 && idx < len(tm.menuDiscovered) {
			dev = tm.menuDiscovered[idx]
			found = true
		} else if idx >= 0 && idx < len(tm.discoveredPeers) {
			dev = tm.discoveredPeers[idx]
			found = true
		}
		tm.mu.Unlock()
		if found {
			if tm.callbacks.OnConnectDiscovered != nil {
				go tm.callbacks.OnConnectDiscovered(dev)
			} else if tm.callbacks.OnPairDevice != nil {
				go tm.callbacks.OnPairDevice()
			}
		}
	case cmdID == CmdPrintFileDefault:
		go tm.handlePrintFileDialog("")
	case cmdID >= CmdPrintFilePeerBase && cmdID < CmdPrintFilePeerBase+100:
		peerIdx := cmdID - CmdPrintFilePeerBase
		tm.mu.Lock()
		peerName := ""
		if peerIdx >= 0 && peerIdx < len(tm.menuPeers) {
			peerName = tm.menuPeers[peerIdx].Name
		} else if peerIdx >= 0 && peerIdx < len(tm.peers) {
			peerName = tm.peers[peerIdx].Name
		}
		tm.mu.Unlock()
		if peerName != "" {
			go tm.handlePrintFileDialog(peerName)
		}
	case cmdID == CmdStatus:
		go tm.handleShowStatus()
	case cmdID == CmdResync:
		if tm.callbacks.OnResync != nil {
			go tm.callbacks.OnResync()
		}
	case cmdID == CmdCopyIP:
		go tm.handleCopyIP()
	case cmdID == CmdOpenLogs:
		go tm.handleOpenLogs()
	case cmdID == CmdExit:
		if tm.callbacks.OnExit != nil {
			go tm.callbacks.OnExit()
		}
	}
}

func (tm *TrayManager) showContextMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendMenuItem := func(id int, text string, flags uint32) {
		t, _ := syscall.UTF16PtrFromString(text)
		procAppendMenuW.Call(menu, uintptr(flags), uintptr(id), uintptr(unsafe.Pointer(t)))
	}

	items := tm.snapshotAndBuildMenuItems()
	for _, item := range items {
		if item.separator {
			procAppendMenuW.Call(menu, MF_SEPARATOR, 0, 0)
			continue
		}
		flags := uint32(MF_STRING)
		if item.disabled {
			flags |= MF_GRAYED
		}
		appendMenuItem(item.id, item.text, flags)
	}

	var pt POINT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(tm.hwnd)
	procTrackPopupMenu.Call(menu, TPM_BOTTOMALIGN|TPM_LEFTALIGN, uintptr(pt.X), uintptr(pt.Y), 0, tm.hwnd, 0)
}

func (tm *TrayManager) handleCopyIP() {
	ip := ""
	if tm.callbacks.OnCopyIP != nil {
		ip = tm.callbacks.OnCopyIP()
	}
	if ip == "" {
		tm.ShowNotificationLevel("Tailscale IP", "Tailscale IP is not yet connected or available.", NotifyWarning)
		return
	}
	if err := SetClipboardText(ip); err == nil {
		tm.ShowNotificationLevel("IP Copied", fmt.Sprintf("Copied %s to clipboard!", ip), NotifyInfo)
	} else {
		tm.ShowNotificationLevel("Tailscale IP", fmt.Sprintf("Local IP: %s", ip), NotifyInfo)
	}
}

func (tm *TrayManager) handleOpenLogs() {
	if tm.callbacks.OnOpenLogs != nil {
		tm.callbacks.OnOpenLogs()
	} else {
		_ = exec.Command("explorer.exe", `C:\Bridge\Logs`).Start()
	}
}

func (tm *TrayManager) alertWarning(title, message string) {
	if tm.alertFn != nil {
		tm.alertFn(title, message)
		return
	}
	textPtr, _ := syscall.UTF16PtrFromString(message)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	procMessageBoxW.Call(
		tm.hwnd,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		MB_OK|MB_ICONWARNING|MB_SETFOREGROUND,
	)
}

func (tm *TrayManager) confirmAction(title, message string) bool {
	if tm.confirmFn != nil {
		return tm.confirmFn(title, message)
	}
	textPtr, _ := syscall.UTF16PtrFromString(message)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	r, _, _ := procMessageBoxW.Call(
		tm.hwnd,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		MB_YESNO|MB_ICONWARNING|MB_SETFOREGROUND,
	)
	return r == IDYES
}

func (tm *TrayManager) pickFile(title string) (string, error) {
	if tm.filePickerFn != nil {
		return tm.filePickerFn(title)
	}
	return OpenFilePicker(title)
}

func (tm *TrayManager) handlePrintFileDialog(targetPeer string) {
	if targetPeer != "" {
		if peer, ok := tm.lookupPeer(targetPeer); ok {
			if peer.Online && peer.IsPrinterKnownMissing() {
				msg := fmt.Sprintf(
					"No physical printer is connected on %s.\n\n%s is currently online, so file sharing works, but you cannot print to it until a printer is connected on %s.",
					peer.Name, peer.Name, peer.Name,
				)
				tm.alertWarning("Printer Not Connected", msg)
				return
			}
			if !peer.Online {
				var prompt string
				if tm.callbacks.OnQueueOfflinePrint != nil {
					prompt = fmt.Sprintf("%s is currently offline.\n\nQueue the document to print automatically when it comes online?", targetPeer)
				} else {
					prompt = fmt.Sprintf("%s is currently offline.\n\nAttempts to print now may fail unless it comes back online. Select a document anyway?", targetPeer)
				}
				if !tm.confirmAction("Device Offline", prompt) {
					return
				}
			}
		}
	}

	title := "Select a Document to Print"
	if targetPeer != "" {
		title = fmt.Sprintf("Select a Document to Print on %s", targetPeer)
	}

	filePath, err := tm.pickFile(title)
	if err != nil || filePath == "" {
		return
	}

	if targetPeer != "" && tm.isPeerOffline(targetPeer) && tm.callbacks.OnQueueOfflinePrint != nil {
		tm.callbacks.OnQueueOfflinePrint(targetPeer, filePath)
		return
	}

	if targetPeer != "" && tm.callbacks.OnPrintFileToPeer != nil {
		tm.SetPrintingState(fmt.Sprintf("Bridge: Printing %s on %s...", filepath.Base(filePath), targetPeer))
		tm.callbacks.OnPrintFileToPeer(targetPeer, filePath)
	} else if tm.callbacks.OnPrintFile != nil {
		tm.SetPrintingState(fmt.Sprintf("Bridge: Printing %s...", filepath.Base(filePath)))
		tm.callbacks.OnPrintFile(filePath)
	}
}

func (tm *TrayManager) handleShowStatus() {
	var statusMsg string
	if tm.callbacks.OnShowStatus != nil {
		statusMsg = tm.callbacks.OnShowStatus()
	} else {
		statusMsg = tm.buildDefaultStatusText()
	}
	formatted := FormatStatusText(statusMsg)

	tm.mu.Lock()
	queued := tm.queuedPrints
	tm.mu.Unlock()
	if queued > 0 && !strings.Contains(strings.ToLower(formatted), "queued") {
		formatted = fmt.Sprintf("%s\n\nQueued Prints: %d (waiting for offline peer)", formatted, queued)
	}

	textPtr, _ := syscall.UTF16PtrFromString(formatted)
	titlePtr, _ := syscall.UTF16PtrFromString("Bridge Multi-Device Network Status")
	procMessageBoxW.Call(tm.hwnd, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), MB_OK|MB_ICONINFORMATION|MB_SETFOREGROUND)
}

// OpenFilePicker opens the native Windows Open File Dialog filtered for supported direct-print formats.
func OpenFilePicker(title string) (string, error) {
	var ofn OPENFILENAMEW
	ofn.LStructSize = uint32(unsafe.Sizeof(ofn))

	filter := utf16.Encode([]rune(printFileFilter))
	ofn.LpstrFilter = &filter[0]
	ofn.NFilterIndex = 1

	fileBuf := make([]uint16, 1024)
	ofn.LpstrFile = &fileBuf[0]
	ofn.NMaxFile = 1024

	t, _ := syscall.UTF16PtrFromString(title)
	ofn.LpstrTitle = t
	ofn.Flags = 0x00080000 | 0x00001000 // OFN_EXPLORER | OFN_FILEMUSTEXIST

	r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return "", nil // canceled
	}

	return syscall.UTF16ToString(fileBuf), nil
}

// HideConsole unconditionally hides and detaches any console window for background execution.
func HideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, 0) // SW_HIDE
	}
	procFreeConsole.Call()
}

// HideConsoleIfDedicated hides the console window if this process was spawned in a dedicated console.
func HideConsoleIfDedicated() {
	HideConsole()
}

// SetClipboardText places plain unicode text onto the Windows system clipboard.
func SetClipboardText(text string) error {
	text16, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	r, _, _ := procOpenClipboard.Call(0)
	if r == 0 {
		return fmt.Errorf("failed to open clipboard")
	}
	defer procCloseClipboard.Call()

	procEmptyClipboard.Call()

	cb := len(text16) * 2
	hMem, _, _ := procGlobalAlloc.Call(0x0002 /* GMEM_MOVEABLE */, uintptr(cb))
	if hMem == 0 {
		return fmt.Errorf("global memory allocation failed")
	}

	ptr, _, _ := procGlobalLock.Call(hMem)
	if ptr == 0 {
		return fmt.Errorf("global memory lock failed")
	}
	procRtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&text16[0])), uintptr(cb))
	procGlobalUnlock.Call(hMem)

	procSetClipboardData.Call(13 /* CF_UNICODETEXT */, hMem)
	return nil
}
