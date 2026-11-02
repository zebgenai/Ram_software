//go:build !windows

package main

// On non-Windows the agent runs headless in the foreground (on macOS it is
// kept alive by the LaunchAgent). A menu-bar icon needs Cocoa bindings and is
// added in the macOS-specific build.
func serve(cfg config, cfgPath string) {
	run(cfg, cfgPath)
}
