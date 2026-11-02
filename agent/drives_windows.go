//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"remotefiles/internal/proto"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	procDriveType     = kernel32.NewProc("GetDriveTypeW")
)

func listDrives() ([]proto.Drive, error) {
	mask, _, err := procLogicalDrives.Call()
	if mask == 0 {
		return nil, err
	}
	kinds := map[uintptr]string{2: "Removable", 3: "Local disk", 4: "Network", 5: "CD/DVD", 6: "RAM disk"}
	var out []proto.Drive
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := syscall.UTF16PtrFromString(root)
		t, _, _ := procDriveType.Call(uintptr(unsafe.Pointer(p)))
		if t == 5 { // skip optical drives, they usually just error
			continue
		}
		label := root
		if k, ok := kinds[t]; ok {
			label = root + " (" + k + ")"
		}
		out = append(out, proto.Drive{Path: root, Label: label})
	}
	return out, nil
}
