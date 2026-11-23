//go:build windows

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	u16 "unicode/utf16"

	"remotefiles/internal/proto"
)

// recentActivity reads the Windows "Recent" shortcut folder — the OS's own list
// of recently opened files — and returns each resolved target path with the time
// it was last opened (the shortcut's modification time).
func recentActivity() []proto.ActivityEntry {
	base, err := os.UserConfigDir() // %AppData% (Roaming)
	if err != nil {
		return nil
	}
	recent := filepath.Join(base, "Microsoft", "Windows", "Recent")
	ents, err := os.ReadDir(recent)
	if err != nil {
		return nil
	}
	var out []proto.ActivityEntry
	for _, de := range ents {
		if de.IsDir() || !strings.EqualFold(filepath.Ext(de.Name()), ".lnk") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		target := resolveLnkTarget(filepath.Join(recent, de.Name()))
		if target == "" {
			continue
		}
		out = append(out, proto.ActivityEntry{At: info.ModTime().UTC(), Path: target})
	}
	return out
}

// resolveLnkTarget parses a Windows .lnk (Shell Link) file far enough to recover
// the local target path from its LinkInfo structure. It prefers the Unicode base
// path when present (for non-ASCII filenames) and falls back to the ANSI one.
// Returns "" if the link has no usable local path.
func resolveLnkTarget(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 76 {
		return ""
	}
	// ShellLinkHeader: HeaderSize(4), LinkCLSID(16), LinkFlags(4) at offset 20.
	flags := binary.LittleEndian.Uint32(b[20:24])
	const (
		hasLinkTargetIDList = 0x1
		hasLinkInfo         = 0x2
	)
	off := 76 // past the fixed header
	if flags&hasLinkTargetIDList != 0 {
		if off+2 > len(b) {
			return ""
		}
		idListSize := int(binary.LittleEndian.Uint16(b[off : off+2]))
		off += 2 + idListSize
	}
	if flags&hasLinkInfo == 0 {
		return ""
	}
	li := off
	if li+20 > len(b) {
		return ""
	}
	liHeaderSize := binary.LittleEndian.Uint32(b[li+4 : li+8])
	liFlags := binary.LittleEndian.Uint32(b[li+8 : li+12])
	const volumeIDAndLocalBasePath = 0x1
	if liFlags&volumeIDAndLocalBasePath == 0 {
		return ""
	}

	// Prefer the Unicode local base path if this LinkInfo carries one.
	if liHeaderSize >= 0x24 && li+32 <= len(b) {
		uOff := int(binary.LittleEndian.Uint32(b[li+28 : li+32]))
		if s := readUTF16Z(b, li+uOff); s != "" {
			return s
		}
	}
	lbpOff := int(binary.LittleEndian.Uint32(b[li+16 : li+20]))
	return readANSIZ(b, li+lbpOff)
}

// readANSIZ reads a null-terminated single-byte string starting at off.
func readANSIZ(b []byte, off int) string {
	if off < 0 || off >= len(b) {
		return ""
	}
	end := off
	for end < len(b) && b[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(b[off:end]))
}

// readUTF16Z reads a null-terminated UTF-16LE string starting at off.
func readUTF16Z(b []byte, off int) string {
	if off < 0 || off+1 >= len(b) {
		return ""
	}
	var u []uint16
	for i := off; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i : i+2])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return strings.TrimSpace(string(u16.Decode(u)))
}
