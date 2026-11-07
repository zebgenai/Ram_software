//go:build windows

package main

import (
	"log"
	"syscall"
	"unsafe"
)

// serve runs the poll loop in the background and shows a system-tray icon on
// the main thread. The tray lets the user open the dashboard or disconnect.
func serve(cfg config, cfgPath string) {
	go run(cfg, cfgPath)
	runTray(cfg, cfgPath)
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32w = syscall.NewLazyDLL("shell32.dll")
	// kernel32 is already declared in drives_windows.go; reuse it.

	pRegisterClassEx  = user32.NewProc("RegisterClassExW")
	pCreateWindowEx   = user32.NewProc("CreateWindowExW")
	pDefWindowProc    = user32.NewProc("DefWindowProcW")
	pGetMessage       = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessage  = user32.NewProc("DispatchMessageW")
	pLoadIcon         = user32.NewProc("LoadIconW")
	pLoadImage        = user32.NewProc("LoadImageW")
	pLoadCursor       = user32.NewProc("LoadCursorW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	pAppendMenu       = user32.NewProc("AppendMenuW")
	pTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	pDestroyMenu      = user32.NewProc("DestroyMenu")
	pGetCursorPos     = user32.NewProc("GetCursorPos")
	pSetForegroundWin = user32.NewProc("SetForegroundWindow")
	pGetSystemMetrics = user32.NewProc("GetSystemMetrics")

	pShellNotifyIcon = shell32w.NewProc("Shell_NotifyIconW")
	pShellExecute    = shell32w.NewProc("ShellExecuteW")

	pGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy   = 0x0002
	wmCommand   = 0x0111
	wmRButtonUp = 0x0205
	wmLButtonUp = 0x0202
	wmApp       = 0x8000
	trayMessage = wmApp + 1

	nimAdd     = 0x0
	nimDelete  = 0x2
	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4

	idiApplication = 32512
	idcArrow       = 32512

	// Embedded application icon (see rsrc.rc) — used everywhere: exe file icon,
	// window, and the system tray.
	idiAppIcon = 1

	imageIcon      = 1
	lrDefaultColor = 0x0000
	lrShared       = 0x8000
	smCXSmall      = 49 // SM_CXSMICON
	smCYSmall      = 50 // SM_CYSMICON
	smCXIcon       = 11 // SM_CXICON
	smCYIcon       = 12 // SM_CYICON

	mfString       = 0x0
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	swShowNormal = 1

	idOpen       = 1
	idDisconnect = 2
	idQuit       = 3
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type wndClassEx struct {
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

type point struct{ X, Y int32 }

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// tray state shared with the window procedure.
var (
	trayCfgPath string
	trayServer  string
	trayHWnd    uintptr
	trayNID     notifyIconData
)

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func sysMetric(i int) int {
	r, _, _ := pGetSystemMetrics.Call(uintptr(i))
	return int(r)
}

// loadIconRes loads an embedded icon (rsrc.rc) by resource id at the given size,
// falling back to the generic application icon if it can't be loaded.
func loadIconRes(hInst uintptr, resID, cx, cy int) uintptr {
	h, _, _ := pLoadImage.Call(hInst, uintptr(resID), imageIcon,
		uintptr(cx), uintptr(cy), lrDefaultColor|lrShared)
	if h == 0 {
		h, _, _ = pLoadIcon.Call(0, uintptr(idiApplication))
	}
	return h
}

func runTray(cfg config, cfgPath string) {
	trayCfgPath = cfgPath
	trayServer = cfg.Server

	hInst, _, _ := pGetModuleHandle.Call(0)
	// One icon everywhere: window, taskbar, and the system tray.
	appBig := loadIconRes(hInst, idiAppIcon, sysMetric(smCXIcon), sysMetric(smCYIcon))
	appSmall := loadIconRes(hInst, idiAppIcon, sysMetric(smCXSmall), sysMetric(smCYSmall))
	trayIcon := loadIconRes(hInst, idiAppIcon, sysMetric(smCXSmall), sysMetric(smCYSmall))
	cursor, _, _ := pLoadCursor.Call(0, uintptr(idcArrow))
	className := utf16("RemoteFilesTrayClass")

	wc := wndClassEx{
		Style:         0,
		LpfnWndProc:   syscall.NewCallback(wndProc),
		HInstance:     hInst,
		HIcon:         appBig,
		HCursor:       cursor,
		LpszClassName: className,
		HIconSm:       appSmall,
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	if ret, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		log.Printf("tray: RegisterClassEx failed: %v", err)
		return
	}

	// A normal (hidden) overlapped window; we never call ShowWindow so it stays invisible.
	hwnd, _, err := pCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16(appName))),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		log.Printf("tray: CreateWindowEx failed: %v", err)
		return
	}
	trayHWnd = hwnd

	trayNID = notifyIconData{
		HWnd:             hwnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: trayMessage,
		HIcon:            trayIcon,
	}
	trayNID.CbSize = uint32(unsafe.Sizeof(trayNID))
	copy(trayNID.SzTip[:], syscall.StringToUTF16(appName+" — connected"))
	if ret, _, err := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&trayNID))); ret == 0 {
		log.Printf("tray: Shell_NotifyIcon add failed: %v", err)
	}

	// Message loop.
	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&trayNID)))
}

func wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case trayMessage:
		// lParam low word is the mouse event.
		switch uint32(lParam) & 0xffff {
		case wmRButtonUp, wmLButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	case wmCommand:
		switch uint32(wParam) & 0xffff {
		case idOpen:
			openDashboard()
		case idDisconnect:
			if !askConsent(appName, "Disconnect this computer from "+companyName+" IT?\n\n"+
				"This removes IT access until your admin sends a new invite.") {
				return 0 // user cancelled
			}
			pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&trayNID)))
			disconnectAndExit(trayCfgPath)
		}
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := pDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return ret
}

func showTrayMenu(hwnd uintptr) {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	pAppendMenu.Call(menu, mfString, idOpen, uintptr(unsafe.Pointer(utf16("Open dashboard"))))
	pAppendMenu.Call(menu, mfString, idDisconnect, uintptr(unsafe.Pointer(utf16("Disconnect this computer"))))

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Required so the menu closes when the user clicks elsewhere.
	pSetForegroundWin.Call(hwnd)
	// With TPM_RETURNCMD the selected id is returned; dispatch it like WM_COMMAND.
	cmd, _, _ := pTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	if cmd != 0 {
		wndProc(hwnd, wmCommand, cmd&0xffff, 0)
	}
}

func openDashboard() {
	if trayServer == "" {
		return
	}
	pShellExecute.Call(0, uintptr(unsafe.Pointer(utf16("open"))),
		uintptr(unsafe.Pointer(utf16(trayServer))), 0, 0, swShowNormal)
}
