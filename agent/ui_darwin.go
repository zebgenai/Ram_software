//go:build darwin

package main

import (
	"os/exec"
	"strings"
)

// Text is passed as osascript arguments, never spliced into the script.
func askConsent(title, msg string) bool {
	out, err := exec.Command("osascript",
		"-e", "on run argv",
		"-e", `display dialog (item 1 of argv) with title (item 2 of argv) buttons {"Deny", "Allow"} default button "Deny" with icon caution`,
		"-e", "end run", msg, title).Output()
	return err == nil && strings.Contains(string(out), "button returned:Allow")
}

func showInfo(title, msg string) {
	_ = exec.Command("osascript",
		"-e", "on run argv",
		"-e", `display dialog (item 1 of argv) with title (item 2 of argv) buttons {"OK"} default button "OK"`,
		"-e", "end run", msg, title).Run()
}
