package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"remotefiles/internal/proto"
)

const backupDir = ".rf-backups"

// validName rejects names that could escape a directory or name the backup folder.
func validName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("invalid name")
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return errors.New("name cannot contain path separators")
	}
	if name == backupDir {
		return errors.New("reserved name")
	}
	return nil
}

func mkdir(p proto.MkdirParams) (proto.OpResult, error) {
	dir, err := cleanAbs(p.Dir)
	if err != nil {
		return proto.OpResult{}, err
	}
	if err := validName(p.Name); err != nil {
		return proto.OpResult{}, err
	}
	full := filepath.Join(dir, p.Name)
	if _, err := os.Lstat(full); err == nil {
		return proto.OpResult{}, errors.New("a file or folder with that name already exists")
	}
	if err := os.Mkdir(full, 0o755); err != nil {
		return proto.OpResult{}, friendlyErr(err)
	}
	return proto.OpResult{Path: full}, nil
}

func rename(p proto.RenameParams) (proto.OpResult, error) {
	src, err := cleanAbs(p.Path)
	if err != nil {
		return proto.OpResult{}, err
	}
	if err := validName(p.NewName); err != nil {
		return proto.OpResult{}, err
	}
	if _, err := os.Lstat(src); err != nil {
		return proto.OpResult{}, friendlyErr(err)
	}
	dst := filepath.Join(filepath.Dir(src), p.NewName)
	if _, err := os.Lstat(dst); err == nil {
		return proto.OpResult{}, errors.New("a file or folder with that name already exists")
	}
	if err := os.Rename(src, dst); err != nil {
		return proto.OpResult{}, friendlyErr(err)
	}
	return proto.OpResult{Path: dst}, nil
}

func moveItem(p proto.TransferParams) (proto.OpResult, error) {
	src, dstDir, err := transferPaths(p)
	if err != nil {
		return proto.OpResult{}, err
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	if sameOrInside(src, dst) {
		return proto.OpResult{}, errors.New("cannot move a folder into itself")
	}
	if _, err := os.Lstat(dst); err == nil {
		return proto.OpResult{}, errors.New("an item with that name already exists in the destination")
	}
	// Rename works across folders on the same volume; fall back to copy+delete.
	if err := os.Rename(src, dst); err != nil {
		if err := copyTree(src, dst); err != nil {
			return proto.OpResult{}, friendlyErr(err)
		}
		if err := os.RemoveAll(src); err != nil {
			return proto.OpResult{}, fmt.Errorf("copied but could not remove original: %w", err)
		}
	}
	return proto.OpResult{Path: dst}, nil
}

func copyItem(p proto.TransferParams) (proto.OpResult, error) {
	src, dstDir, err := transferPaths(p)
	if err != nil {
		return proto.OpResult{}, err
	}
	dst := filepath.Join(dstDir, filepath.Base(src))
	if sameOrInside(src, dst) {
		return proto.OpResult{}, errors.New("cannot copy a folder into itself")
	}
	if _, err := os.Lstat(dst); err == nil {
		return proto.OpResult{}, errors.New("an item with that name already exists in the destination")
	}
	if err := copyTree(src, dst); err != nil {
		_ = os.RemoveAll(dst) // clean up a half-done copy
		return proto.OpResult{}, friendlyErr(err)
	}
	return proto.OpResult{Path: dst}, nil
}

func transferPaths(p proto.TransferParams) (src, dstDir string, err error) {
	src, err = cleanAbs(p.Src)
	if err != nil {
		return "", "", err
	}
	dstDir, err = cleanAbs(p.DstDir)
	if err != nil {
		return "", "", err
	}
	if _, err := os.Lstat(src); err != nil {
		return "", "", friendlyErr(err)
	}
	st, err := os.Stat(dstDir)
	if err != nil {
		return "", "", friendlyErr(err)
	}
	if !st.IsDir() {
		return "", "", errors.New("destination is not a folder")
	}
	return src, dstDir, nil
}

// sameOrInside reports whether dst equals src or sits inside src (which would
// make a recursive copy/move loop).
func sameOrInside(src, dst string) bool {
	if src == dst {
		return true
	}
	rel, err := filepath.Rel(src, dst)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func deleteItem(p proto.DeleteParams) (proto.OpResult, error) {
	path, err := cleanAbs(p.Path)
	if err != nil {
		return proto.OpResult{}, err
	}
	// Refuse to delete a whole drive root.
	if path == filepath.Dir(path) {
		return proto.OpResult{}, errors.New("refusing to delete a drive root")
	}
	if _, err := os.Lstat(path); err != nil {
		return proto.OpResult{}, friendlyErr(err)
	}
	if err := os.RemoveAll(path); err != nil {
		return proto.OpResult{}, friendlyErr(err)
	}
	return proto.OpResult{Path: path}, nil
}

// backupExisting moves an existing file to <dir>/.rf-backups/<name>.<ts> before
// it is overwritten. Returns "" if there was nothing to back up.
func backupExisting(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		return "", nil // nothing there
	}
	dir := filepath.Join(filepath.Dir(path), backupDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ts := time.Now().Format("2006-01-02_15-04-05")
	name := filepath.Base(path)
	bak := filepath.Join(dir, name+"."+ts)
	for i := 1; ; i++ {
		if _, err := os.Lstat(bak); err != nil {
			break
		}
		bak = filepath.Join(dir, fmt.Sprintf("%s.%s.%d", name, ts, i))
	}
	if err := os.Rename(path, bak); err != nil {
		// Cross-device or locked: copy instead of move.
		if cerr := copyFile(path, bak); cerr != nil {
			return "", err
		}
	}
	return bak, nil
}

// ---- tree copy helpers ----

func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if info, err = os.Stat(src); err != nil {
			return err
		}
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".rf-partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
