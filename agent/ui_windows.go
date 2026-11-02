//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var procMessageBox = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")

const (
	mbOK              = 0x0
	mbYesNo           = 0x4
	mbIconQuestion    = 0x20
	mbIconInformation = 0x40
	mbDefButton2      = 0x100
	mbSetForeground   = 0x10000
	mbTopMost         = 0x40000
	idYes             = 6
)

func messageBox(title, text string, flags uintptr) uintptr {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	r, _, _ := procMessageBox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags)
	return r
}

// askConsent shows a Yes/No dialog; "No" is the default button.
func askConsent(title, msg string) bool {
	return messageBox(title, msg, mbYesNo|mbIconQuestion|mbDefButton2|mbSetForeground|mbTopMost) == idYes
}

func showInfo(title, msg string) {
	messageBox(title, msg, mbOK|mbIconInformation|mbSetForeground|mbTopMost)
}
