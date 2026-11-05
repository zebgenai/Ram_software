package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"remotefiles/internal/proto"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (a *App) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var p proto.MkdirParams
	if err := readJSON(w, r, 8192, &p); err != nil || p.Dir == "" || p.Name == "" {
		writeErr(w, http.StatusBadRequest, "folder and name required")
		return
	}
	a.callDevice(w, r, "mkdir", proto.MethodMkdir, path.Join(p.Dir, p.Name), p)
}

func (a *App) handleRename(w http.ResponseWriter, r *http.Request) {
	var p proto.RenameParams
	if err := readJSON(w, r, 8192, &p); err != nil || p.Path == "" || p.NewName == "" {
		writeErr(w, http.StatusBadRequest, "path and new name required")
		return
	}
	a.callDevice(w, r, "rename", proto.MethodRename, p.Path+" → "+p.NewName, p)
}

func (a *App) handleMove(w http.ResponseWriter, r *http.Request) {
	var p proto.TransferParams
	if err := readJSON(w, r, 8192, &p); err != nil || p.Src == "" || p.DstDir == "" {
		writeErr(w, http.StatusBadRequest, "source and destination required")
		return
	}
	a.callDevice(w, r, "move", proto.MethodMove, p.Src+" → "+p.DstDir, p)
}

func (a *App) handleCopy(w http.ResponseWriter, r *http.Request) {
	var p proto.TransferParams
	if err := readJSON(w, r, 8192, &p); err != nil || p.Src == "" || p.DstDir == "" {
		writeErr(w, http.StatusBadRequest, "source and destination required")
		return
	}
	a.callDevice(w, r, "copy", proto.MethodCopy, p.Src+" → "+p.DstDir, p)
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	var p proto.DeleteParams
	if err := readJSON(w, r, 8192, &p); err != nil || p.Path == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	a.callDevice(w, r, "delete", proto.MethodDelete, p.Path, p)
}

// ---- download: agent pushes bytes, we stream them to the admin ----

func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.store.Device(id); !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || len(p) > 4096 {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	if !a.hub.Online(id) {
		writeErr(w, http.StatusServiceUnavailable, errOffline.Error())
		return
	}

	jobID, x := a.transfers.newDownload(id)
	defer a.transfers.remove(jobID)

	entry := AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "download", Device: id, Path: p}
	if err := a.hub.Send(id, proto.Request{ID: randHex(8), Method: proto.MethodFetch,
		Params: mustJSON(proto.FetchParams{JobID: jobID, Path: p})}); err != nil {
		entry.Error = err.Error()
		a.audit.Log(entry)
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	var meta dlMeta
	select {
	case meta = <-x.meta:
	case <-time.After(transferConnectTimeout):
		x.pw.CloseWithError(errTimeout)
		entry.Error = errTimeout.Error()
		a.audit.Log(entry)
		writeErr(w, http.StatusGatewayTimeout, "the PC did not respond in time")
		return
	case <-r.Context().Done():
		x.pw.CloseWithError(errors.New("admin cancelled"))
		return
	}
	if meta.err != nil {
		entry.Error = meta.err.Error()
		a.audit.Log(entry)
		writeErr(w, http.StatusBadGateway, meta.err.Error())
		return
	}

	name := meta.name
	if name == "" {
		name = path.Base(p)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(name))
	if meta.size > 0 {
		w.Header().Set("Content-Length", itoa(meta.size))
	}
	_, copyErr := io.Copy(w, x.pr)
	entry.OK = copyErr == nil
	if copyErr != nil {
		entry.Error = copyErr.Error()
	}
	a.audit.Log(entry)
}

// handleBlobPush receives the file bytes from the agent during a download.
func (a *App) handleBlobPush(w http.ResponseWriter, r *http.Request) {
	dev, ok := a.authDevice(w, r)
	if !ok {
		return
	}
	x, ok := a.transfers.take(r.PathValue("job"), dev)
	if !ok || x.kind != "dl" {
		writeErr(w, http.StatusNotFound, "unknown transfer")
		return
	}
	if msg := r.Header.Get("X-RF-Error"); msg != "" {
		x.meta <- dlMeta{err: errors.New(msg)}
		x.pw.CloseWithError(errors.New(msg))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	mtime, _ := strconv.ParseInt(r.Header.Get("X-RF-Mtime"), 10, 64)
	x.meta <- dlMeta{name: r.Header.Get("X-RF-Name"), size: r.ContentLength, mtime: mtime}
	_, err := io.Copy(x.pw, r.Body)
	x.pw.CloseWithError(err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "transfer failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- upload: admin sends bytes, agent pulls them ----

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.store.Device(id); !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	q := r.URL.Query()
	dir, name := q.Get("dir"), q.Get("name")
	if dir == "" || name == "" || strings.ContainsAny(name, `/\`) {
		writeErr(w, http.StatusBadRequest, "dir and a plain file name are required")
		return
	}
	overwrite := q.Get("overwrite") == "1"
	if !a.hub.Online(id) {
		writeErr(w, http.StatusServiceUnavailable, errOffline.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 2<<30) // 2 GB cap
	jobID, _ := a.transfers.newUpload(id, r.Body)
	defer a.transfers.remove(jobID)

	res, err := a.hub.Call(r.Context(), id, proto.MethodStore, proto.StoreParams{
		JobID: jobID, Dir: dir, Name: name, Overwrite: overwrite,
	})
	entry := AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "upload", Device: id,
		Path: path.Join(dir, name), OK: err == nil}
	if err != nil {
		entry.Error = err.Error()
		a.audit.Log(entry)
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, errOffline):
			status = http.StatusServiceUnavailable
		case errors.Is(err, errTimeout):
			status = http.StatusGatewayTimeout
		}
		writeErr(w, status, err.Error())
		return
	}
	a.audit.Log(entry)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(res)
}

// handleBlobPull streams the admin's uploaded bytes to the agent.
func (a *App) handleBlobPull(w http.ResponseWriter, r *http.Request) {
	dev, ok := a.authDevice(w, r)
	if !ok {
		return
	}
	x, ok := a.transfers.take(r.PathValue("job"), dev)
	if !ok || x.kind != "ul" {
		writeErr(w, http.StatusNotFound, "unknown transfer")
		return
	}
	a.transfers.mu.Lock()
	if x.claimed {
		a.transfers.mu.Unlock()
		writeErr(w, http.StatusConflict, "already in progress")
		return
	}
	x.claimed = true
	a.transfers.mu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, x.src)
}
