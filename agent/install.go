package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// stableDir is where the agent copies itself so autostart always points at a
// fixed path (the first run may be from Downloads under a one-time name).
func stableDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, appName)
}

func stableExe() string {
	name := appName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(stableDir(), name)
}

// copySelfTo copies the running executable to dst (unless already there).
func copySelfTo(dst string) (bool, error) {
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	self, _ = filepath.EvalSymlinks(self)
	if d, _ := filepath.EvalSymlinks(dst); d == self {
		return false, nil // already running from the stable path
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(self)
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return false, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}
