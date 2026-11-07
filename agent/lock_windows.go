//go:build windows

package main

import (
	"strings"
	"syscall"
	"unsafe"
)

var (
	lockUser32                 = syscall.NewLazyDLL("user32.dll")
	pOpenInputDesktop          = lockUser32.NewProc("OpenInputDesktop")
	pCloseDesktop              = lockUser32.NewProc("CloseDesktop")
	pGetUserObjectInformationW = lockUser32.NewProc("GetUserObjectInformationW")
)

const (
	desktopSwitchDesktop = 0x0100
	uoiName              = 2
)

// isWorkstationLocked reports whether the screen is locked. When locked, the
// input desktop is the secure/lock-screen desktop, which a normal user process
// can't open (or whose name isn't "Default").
func isWorkstationLocked() bool {
	h, _, _ := pOpenInputDesktop.Call(0, 0, desktopSwitchDesktop)
	if h == 0 {
		return true
	}
	defer pCloseDesktop.Call(h)
	var buf [256]uint16
	var needed uint32
	r, _, _ := pGetUserObjectInformationW.Call(h, uoiName,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		return false // couldn't read the name; assume unlocked to avoid false alarms
	}
	return !strings.EqualFold(syscall.UTF16ToString(buf[:]), "Default")
}
