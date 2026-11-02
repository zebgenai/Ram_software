package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"remotefiles/internal/proto"
)

const maxEntries = 5000

func cleanAbs(p string) (string, error) {
	if p == "" || !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	return filepath.Clean(p), nil
}

func listDir(p string) (proto.ListResult, error) {
	p, err := cleanAbs(p)
	if err != nil {
		return proto.ListResult{}, err
	}
	des, err := os.ReadDir(p)
	if err != nil {
		return proto.ListResult{}, friendlyErr(err)
	}
	res := proto.ListResult{Path: p, Entries: make([]proto.Entry, 0, min(len(des), maxEntries))}
	if parent := filepath.Dir(p); parent != p {
		res.Parent = parent
	}
	for _, de := range des {
		if len(res.Entries) >= maxEntries {
			res.Truncated = true
			break
		}
		full := filepath.Join(p, de.Name())
		e := proto.Entry{Name: de.Name(), Path: full, Dir: de.IsDir()}
		info, err := de.Info()
		if de.Type()&fs.ModeSymlink != 0 {
			// Follow links so linked folders can be opened.
			if st, serr := os.Stat(full); serr == nil {
				info, err = st, nil
				e.Dir = st.IsDir()
			}
		}
		if err == nil {
			e.ModTime = info.ModTime().UTC()
			if !e.Dir {
				e.Size = info.Size()
			}
		}
		res.Entries = append(res.Entries, e)
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i], res.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return res, nil
}

func friendlyErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return errors.New("permission denied on this computer (on Mac, grant Full Disk Access to the agent)")
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("folder not found")
	}
	return err
}
