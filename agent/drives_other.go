//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"runtime"

	"remotefiles/internal/proto"
)

func listDrives() ([]proto.Drive, error) {
	out := []proto.Drive{{Path: "/", Label: "/ (System)"}}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, proto.Drive{Path: home, Label: "Home"})
	}
	if runtime.GOOS == "darwin" {
		// External and extra volumes. "Macintosh HD" is a link back to "/".
		if des, err := os.ReadDir("/Volumes"); err == nil {
			for _, de := range des {
				p := filepath.Join("/Volumes", de.Name())
				if target, err := filepath.EvalSymlinks(p); err == nil && target == "/" {
					continue
				}
				out = append(out, proto.Drive{Path: p, Label: de.Name()})
			}
		}
	}
	return out, nil
}
