//go:build !windows && !darwin

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Terminal prompts, used on Linux (development/testing).
func askConsent(title, msg string) bool {
	fmt.Fprintf(os.Stderr, "\n== %s ==\n%s\nType 'yes' to allow: ", title, msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}

func showInfo(title, msg string) { fmt.Fprintf(os.Stderr, "\n== %s ==\n%s\n", title, msg) }
