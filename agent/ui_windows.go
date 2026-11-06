//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	procMessageBox         = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	procMessageBoxIndirect = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxIndirectW")
	procConsentModule      = syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

const (
	mbOK              = 0x0
	mbYesNo           = 0x4
	mbIconInformation = 0x40
	mbUserIcon        = 0x80
	mbDefButton2      = 0x100
	mbSetForeground   = 0x10000
	mbTopMost         = 0x40000
	idYes             = 6
)

// msgBoxParams matches MSGBOXPARAMSW. lpszIcon is an icon resource id
// (MAKEINTRESOURCE), not a string.
type msgBoxParams struct {
	cbSize             uint32
	hwndOwner          uintptr
	hInstance          uintptr
	lpszText           *uint16
	lpszCaption        *uint16
	dwStyle            uint32
	lpszIcon           uintptr
	dwContextHelpId    uintptr
	lpfnMsgBoxCallback uintptr
	dwLanguageId       uint32
}

func messageBox(title, text string, flags uintptr) uintptr {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := procMessageBox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags)
	return r
}

// askConsent shows a Yes/No dialog; "No" is the default button.
// The icon is the embedded WPS logo (rsrc.rc IDI_APPICON). Other dialogs stay on the system icon.
func askConsent(title, msg string) bool {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(msg)
	hInst, _, _ := procConsentModule.Call(0)
	p := msgBoxParams{
		hInstance:   hInst,
		lpszText:    m,
		lpszCaption: t,
		dwStyle:     mbYesNo | mbUserIcon | mbDefButton2 | mbSetForeground | mbTopMost,
		lpszIcon:    idiAppIcon,
	}
	p.cbSize = uint32(unsafe.Sizeof(p))
	r, _, _ := procMessageBoxIndirect.Call(uintptr(unsafe.Pointer(&p)))
	if r == 0 {
		return messageBox(title, msg, mbYesNo|mbDefButton2|mbSetForeground|mbTopMost) == idYes
	}
	return r == idYes
}

func showInfo(title, msg string) {
	messageBox(title, msg, mbOK|mbIconInformation|mbSetForeground|mbTopMost)
}
