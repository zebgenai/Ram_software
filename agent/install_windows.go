//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// startupCmd is a launcher placed in the user's Startup folder so the agent
// runs at every logon. A .cmd avoids needing COM to build a .lnk shortcut.
func startupCmdPath() (string, error) {
	appData, err := os.UserConfigDir() // %APPDATA%
	if err != nil {
		return "", err
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", appName+".cmd"), nil
}

func ensureInstalled() error {
	dst := stableExe()
	if _, err := copySelfTo(dst); err != nil {
		return err
	}
	cmdPath, err := startupCmdPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cmdPath), 0o755); err != nil {
		return err
	}
	// `start "" /B` launches the GUI exe detached without a console window.
	content := "@echo off\r\nstart \"\" /B \"" + dst + "\"\r\n"
	return os.WriteFile(cmdPath, []byte(content), 0o644)
}

func removeAutostart() error {
	if cmdPath, err := startupCmdPath(); err == nil {
		os.Remove(cmdPath)
	}
	// Leave the copied exe in place: a running process can't delete its own
	// image on Windows, and it's harmless once the Startup entry is gone.
	return nil
}
