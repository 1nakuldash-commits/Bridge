//go:build windows

package tray

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
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

	NIIF_INFO = 0x00000001

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
	MB_ICONINFORMATION = 0x00000040
)

const (
	CmdHeader = 1000 + iota
	CmdOpenShared
	CmdPrintFileDefault
	CmdStatus
	CmdResync
	CmdCopyIP
	CmdOpenLogs
	CmdExit
	CmdPrintFilePeerBase = 2000
)

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

// Stop removes the icon from the system tray and destroys the window.
func (tm *TrayManager) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped || tm.hwnd == 0 {
		return
	}
	tm.stopped = true

	var nid NOTIFYICONDATAW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = tm.hwnd
	nid.UID = 1
	procShell_NotifyIconW.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))

	procPostMessageW.Call(tm.hwnd, WM_DESTROY, 0, 0)
}

// SetState updates the dot color and tooltip.
func (tm *TrayManager) SetState(state TrayState, tooltip string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped || tm.hwnd == 0 {
		return
	}
	tm.currentState = state
	tm.currentTip = tooltip

	nid := tm.buildNotifyData(state, tooltip)
	procShell_NotifyIconW.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
}

// ShowBalloon displays a native Windows taskbar notification bubble.
func (tm *TrayManager) ShowBalloon(title, message string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.stopped || tm.hwnd == 0 {
		return
	}

	nid := tm.buildNotifyData(tm.currentState, tm.currentTip)
	nid.UFlags |= NIF_INFO
	nid.DwInfoFlags = NIIF_INFO

	title16, _ := syscall.UTF16FromString(title)
	copy(nid.SzInfoTitle[:], title16)

	msg16, _ := syscall.UTF16FromString(message)
	copy(nid.SzInfo[:], msg16)

	procShell_NotifyIconW.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
}

func (tm *TrayManager) buildNotifyData(state TrayState, tip string) NOTIFYICONDATAW {
	var nid NOTIFYICONDATAW
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = tm.hwnd
	nid.UID = 1
	nid.UFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP
	nid.UCallbackMessage = WM_TRAYMESSAGE
	nid.HIcon = tm.hIcons[state]

	tip16, _ := syscall.UTF16FromString(tip)
	copy(nid.SzTip[:], tip16)
	return nid
}

// UpdateNames updates the friendly names shown in menu items and tooltips (backward compatibility).
func (tm *TrayManager) UpdateNames(nodeName, peerName string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.nodeName = nodeName
	tm.peerName = peerName
}

// UpdateNetworkStatus updates multi-device status, tooltip, and dot color in real-time.
func (tm *TrayManager) UpdateNetworkStatus(nodeName string, peers []config.PeerDevice) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.nodeName = nodeName
	tm.peers = make([]config.PeerDevice, len(peers))
	copy(tm.peers, peers)

	onlineCount := 0
	for _, p := range peers {
		if p.Online {
			onlineCount++
		}
	}

	tip := ""
	state := StateDisconnected
	total := len(peers)

	// User-specified icon state machine:
	// 1. If 0 devices connected/online -> Printer icon with red cross line (Disconnected)
	// 2. If 1 or more devices connected, but not all -> Gray printer icon (cross removed)
	// 3. If all devices connected -> Green printer icon
	if total == 0 || onlineCount == 0 {
		state = StateDisconnected
		if total == 0 {
			tip = fmt.Sprintf("Bridge: %s (No other device connected)", nodeName)
		} else {
			tip = fmt.Sprintf("Bridge: %s (0/%d devices connected - Disconnected)", nodeName, total)
		}
	} else if onlineCount < total {
		state = StatePartial
		tip = fmt.Sprintf("Bridge: %s (%d/%d devices connected)", nodeName, onlineCount, total)
	} else {
		state = StateConnected
		tip = fmt.Sprintf("Bridge: %s (All %d devices connected & ready)", nodeName, total)
	}

	// Avoid Windows taskbar re-render flickering: Only update if state or tip changed!
	if !tm.stopped && tm.hwnd != 0 {
		if tm.currentState != state || tm.currentTip != tip {
			tm.currentState = state
			tm.currentTip = tip
			nid := tm.buildNotifyData(state, tip)
			procShell_NotifyIconW.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
		}
	}
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
		switch {
		case cmdID == CmdOpenShared:
			if tm.callbacks.OnOpenShared != nil {
				go tm.callbacks.OnOpenShared()
			}
		case cmdID == CmdPrintFileDefault:
			go tm.handlePrintFileDialog("")
		case cmdID >= CmdPrintFilePeerBase && cmdID < CmdPrintFilePeerBase+100:
			peerIdx := cmdID - CmdPrintFilePeerBase
			tm.mu.Lock()
			peerName := ""
			if peerIdx < len(tm.peers) {
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
		return 0

	case WM_DESTROY:
		procPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
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

	tm.mu.Lock()
	nodeName := tm.nodeName
	peers := make([]config.PeerDevice, len(tm.peers))
	copy(peers, tm.peers)
	fallbackPeer := tm.peerName
	tm.mu.Unlock()

	onlineCount := 0
	for _, p := range peers {
		if p.Online {
			onlineCount++
		}
	}

	// 1. Header with dynamic online count
	headerText := fmt.Sprintf("Bridge: %s (%d/%d Online)", nodeName, onlineCount, len(peers))
	if len(peers) == 0 {
		headerText = fmt.Sprintf("Bridge: %s (Standby)", nodeName)
	}
	appendMenuItem(CmdHeader, headerText, MF_STRING|MF_GRAYED)
	procAppendMenuW.Call(menu, MF_SEPARATOR, 0, 0)

	// 2. Open Shared Folder
	appendMenuItem(CmdOpenShared, "📁  Open Shared Folder", MF_STRING)

	// 3. Print Actions: Dynamic list of peers
	if len(peers) == 0 {
		target := fallbackPeer
		if target == "" {
			target = "Remote PC"
		}
		appendMenuItem(CmdPrintFileDefault, fmt.Sprintf("🖨️  Print on %s...", target), MF_STRING)
	} else if len(peers) == 1 {
		p := peers[0]
		statusTag := "Online"
		if !p.Online {
			statusTag = "Offline"
		}
		appendMenuItem(CmdPrintFilePeerBase, fmt.Sprintf("🖨️  Print on %s (%s)...", p.Name, statusTag), MF_STRING)
	} else {
		for i, p := range peers {
			if i >= 50 {
				break
			}
			statusTag := "🟢 Online"
			if !p.Online {
				statusTag = "⚪ Offline"
			}
			itemText := fmt.Sprintf("🖨️  Print on %s (%s)...", p.Name, statusTag)
			appendMenuItem(CmdPrintFilePeerBase+i, itemText, MF_STRING)
		}
	}

	// 4. Multi-Function Quick Actions
	appendMenuItem(CmdCopyIP, "📋  Copy My Tailscale IP", MF_STRING)
	appendMenuItem(CmdStatus, "📊  Device Network Status", MF_STRING)
	appendMenuItem(CmdResync, "🔄  Sync Now", MF_STRING)
	appendMenuItem(CmdOpenLogs, "📂  Open Service Logs", MF_STRING)
	procAppendMenuW.Call(menu, MF_SEPARATOR, 0, 0)
	appendMenuItem(CmdExit, "❌  Exit Bridge", MF_STRING)

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
		tm.ShowBalloon("Tailscale IP", "Tailscale IP is not yet connected or available.")
		return
	}
	if err := SetClipboardText(ip); err == nil {
		tm.ShowBalloon("IP Copied", fmt.Sprintf("Copied %s to clipboard!", ip))
	} else {
		tm.ShowBalloon("Tailscale IP", fmt.Sprintf("Local IP: %s", ip))
	}
}

func (tm *TrayManager) handleOpenLogs() {
	if tm.callbacks.OnOpenLogs != nil {
		tm.callbacks.OnOpenLogs()
	} else {
		_ = exec.Command("explorer.exe", `C:\Bridge\Logs`).Start()
	}
}

func (tm *TrayManager) handlePrintFileDialog(targetPeer string) {
	title := "Select a Document to Print"
	if targetPeer != "" {
		title = fmt.Sprintf("Select a Document to Print on %s", targetPeer)
	}

	filePath, err := OpenFilePicker(title)
	if err != nil || filePath == "" {
		return
	}

	if targetPeer != "" && tm.callbacks.OnPrintFileToPeer != nil {
		tm.SetState(StatePrinting, fmt.Sprintf("Bridge: Printing %s on %s...", filepath.Base(filePath), targetPeer))
		tm.callbacks.OnPrintFileToPeer(targetPeer, filePath)
	} else if tm.callbacks.OnPrintFile != nil {
		tm.SetState(StatePrinting, fmt.Sprintf("Bridge: Printing %s...", filepath.Base(filePath)))
		tm.callbacks.OnPrintFile(filePath)
	}
}

func (tm *TrayManager) handleShowStatus() {
	statusMsg := "Bridge Multi-Device Mesh is active."
	if tm.callbacks.OnShowStatus != nil {
		statusMsg = tm.callbacks.OnShowStatus()
	}

	textPtr, _ := syscall.UTF16PtrFromString(statusMsg)
	titlePtr, _ := syscall.UTF16PtrFromString("Bridge Multi-Device Network Status")
	procMessageBoxW.Call(tm.hwnd, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), MB_OK|MB_ICONINFORMATION)
}

// OpenFilePicker opens the native Windows Open File Dialog.
func OpenFilePicker(title string) (string, error) {
	var ofn OPENFILENAMEW
	ofn.LStructSize = uint32(unsafe.Sizeof(ofn))

	filter, _ := syscall.UTF16FromString("All Supported Files\x00*.pdf;*.txt;*.png;*.jpg;*.docx;*.doc\x00PDF Documents (*.pdf)\x00*.pdf\x00All Files (*.*)\x00*.*\x00\x00")
	ofn.LpstrFilter = &filter[0]

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

// SetPrintingState displays the loading printer icon during print spooling/transmission.
func (tm *TrayManager) SetPrintingState(tooltip string) {
	tm.SetState(StatePrinting, tooltip)
}

// SetSyncingState displays the loading cloud icon during file upload/synchronization.
func (tm *TrayManager) SetSyncingState(tooltip string) {
	tm.SetState(StateSyncing, tooltip)
}

// FlashPrintComplete displays the blue printer icon for 4 seconds, then reverts to baseline network status.
func (tm *TrayManager) FlashPrintComplete(title, message string) {
	tm.SetState(StatePrintCompleted, message)
	tm.ShowBalloon(title, message)

	go func() {
		time.Sleep(4 * time.Second)
		tm.RestoreNetworkStatus()
	}()
}

// FlashSyncComplete displays the blue cloud icon for 4 seconds, then reverts to baseline network status.
func (tm *TrayManager) FlashSyncComplete(title, message string) {
	tm.SetState(StateSyncCompleted, message)
	tm.ShowBalloon(title, message)

	go func() {
		time.Sleep(4 * time.Second)
		tm.RestoreNetworkStatus()
	}()
}

// RestoreNetworkStatus re-evaluates current peer counts and restores the baseline printer icon.
func (tm *TrayManager) RestoreNetworkStatus() {
	tm.mu.Lock()
	nodeName := tm.nodeName
	peers := make([]config.PeerDevice, len(tm.peers))
	copy(peers, tm.peers)
	tm.mu.Unlock()
	tm.UpdateNetworkStatus(nodeName, peers)
}

// FlashComplete backward-compatible alias for print completion.
func (tm *TrayManager) FlashComplete(title, message string) {
	tm.FlashPrintComplete(title, message)
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
	copy((*[1 << 30]byte)(unsafe.Pointer(ptr))[:cb], (*[1 << 30]byte)(unsafe.Pointer(&text16[0]))[:cb])
	procGlobalUnlock.Call(hMem)

	procSetClipboardData.Call(13 /* CF_UNICODETEXT */, hMem)
	return nil
}
