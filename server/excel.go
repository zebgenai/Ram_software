package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"remotefiles/internal/proto"
)

// ---- minimal HS256 JWT (OnlyOffice uses this) ----

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwtSign(secret string, payload any) (string, error) {
	hdr := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := hdr + "." + b64url(pb)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + b64url(mac.Sum(nil)), nil
}

// jwtVerify checks the signature and returns the raw payload JSON.
func jwtVerify(secret, token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := b64url(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil, errors.New("bad signature")
	}
	return base64.RawURLEncoding.DecodeString(parts[1])
}

// ---- edit sessions ----

type excelSession struct {
	device    string
	dir       string
	name      string
	fullTxt   string // full path on the agent
	baseMtime int64  // file mtime (Unix s) when first fetched — basis for conflict detection
	created   time.Time
}

type ExcelSessions struct {
	mu sync.Mutex
	m  map[string]*excelSession
}

func NewExcelSessions() *ExcelSessions {
	e := &ExcelSessions{m: map[string]*excelSession{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			e.mu.Lock()
			for k, s := range e.m {
				if time.Since(s.created) > 12*time.Hour {
					delete(e.m, k)
				}
			}
			e.mu.Unlock()
		}
	}()
	return e
}

func (e *ExcelSessions) add(s *excelSession) string {
	id := randToken(24)
	s.created = time.Now()
	e.mu.Lock()
	e.m[id] = s
	e.mu.Unlock()
	return id
}

func (e *ExcelSessions) get(id string) (*excelSession, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.m[id]
	return s, ok
}

// ---- supported document types (OnlyOffice documentType by extension) ----

var docTypes = map[string]string{
	".xlsx": "cell", ".xlsm": "cell", ".xls": "cell", ".csv": "cell", ".ods": "cell",
	".docx": "word", ".doc": "word", ".odt": "word", ".rtf": "word", ".txt": "word",
	".pptx": "slide", ".ppt": "slide", ".odp": "slide",
	".pdf": "pdf",
}

// docTypeFor returns the OnlyOffice documentType for a file, or "" if unsupported.
func docTypeFor(name string) string {
	return docTypes[strings.ToLower(path.Ext(name))]
}

func isEditableDoc(name string) bool { return docTypeFor(name) != "" }

// splitAgentPath splits an agent file path (Windows or Unix) into a directory
// and a separator-free file name, keeping the trailing separator for a drive
// root ("D:\") or filesystem root ("/") so filepath.Join rebuilds it correctly
// on the agent.
func splitAgentPath(p string) (dir, name string) {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "", p
	}
	name = p[i+1:]
	dir = p[:i]
	if dir == "" || strings.HasSuffix(dir, ":") {
		dir = p[:i+1] // "/file" -> "/", "D:\file" -> "D:\"
	}
	return dir, name
}

// ---- handlers ----

// handleExcelOpen (editor+) returns the OnlyOffice editor config for a file.
func (a *App) handleExcelOpen(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.ExcelEnabled() {
		writeErr(w, http.StatusNotImplemented, "WPS editing is not configured on this server")
		return
	}
	id := r.PathValue("id")
	if _, ok := a.store.Device(id); !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || !isEditableDoc(p) {
		writeErr(w, http.StatusBadRequest, "an editable document path is required")
		return
	}
	if !a.hub.Online(id) {
		writeErr(w, http.StatusServiceUnavailable, errOffline.Error())
		return
	}

	// The agent's path is OS-native (Windows uses backslashes), so split on
	// either separator — path.Base/Dir only understand '/'.
	dir, name := splitAgentPath(p)
	sid := a.excel.add(&excelSession{device: id, dir: dir, name: name, fullTxt: p})
	key := randHex(16) // document version key

	document := map[string]any{
		"fileType": strings.TrimPrefix(strings.ToLower(path.Ext(name)), "."),
		"key":      key,
		"title":    name,
		"url":      a.cfg.InternalBaseURL + "/excel/file/" + sid,
	}
	editorConfig := map[string]any{
		"mode":        "edit",
		"callbackUrl": a.cfg.InternalBaseURL + "/excel/callback/" + sid,
		"lang":        "en",
		"user":        map[string]string{"id": actor(r), "name": actor(r)},
		"customization": map[string]any{
			"autosave":      true,
			"forcesave":     true,
			"compactHeader": true,
		},
	}
	config := map[string]any{
		"document":     document,
		"documentType": docTypeFor(name),
		"editorConfig": editorConfig,
		"type":         "desktop",
	}
	if a.cfg.OfficeJWTSecret != "" {
		tok, err := jwtSign(a.cfg.OfficeJWTSecret, config)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not sign editor config")
			return
		}
		config["token"] = tok
	}

	a.audit.Log(AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "excel-open", Device: id, Path: p, OK: true})
	writeJSON(w, http.StatusOK, map[string]any{
		"apiJs":  a.cfg.OfficePublicURL + "/web-apps/apps/api/documents/api.js",
		"config": config,
	})
}

// handleExcelFile streams the current file from the agent to the Document Server.
// The random session id in the URL is the only secret, so it must be unguessable.
func (a *App) handleExcelFile(w http.ResponseWriter, r *http.Request) {
	s, ok := a.excel.get(r.PathValue("sid"))
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if !a.hub.Online(s.device) {
		http.Error(w, "device offline", http.StatusServiceUnavailable)
		return
	}
	jobID, x := a.transfers.newDownload(s.device)
	defer a.transfers.remove(jobID)
	if err := a.hub.Send(s.device, proto.Request{ID: randHex(8), Method: proto.MethodFetch,
		Params: mustJSON(proto.FetchParams{JobID: jobID, Path: s.fullTxt})}); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var meta dlMeta
	select {
	case meta = <-x.meta:
	case <-time.After(transferConnectTimeout):
		x.pw.CloseWithError(errTimeout)
		http.Error(w, "the PC did not respond", http.StatusGatewayTimeout)
		return
	case <-r.Context().Done():
		x.pw.CloseWithError(errors.New("cancelled"))
		return
	}
	if meta.err != nil {
		http.Error(w, meta.err.Error(), http.StatusBadGateway)
		return
	}
	// Record the file's mtime the first time the editor fetches it; this is the
	// version the edit is based on, used later to detect concurrent PC edits.
	if meta.mtime > 0 && s.baseMtime == 0 {
		s.baseMtime = meta.mtime
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if meta.size > 0 {
		w.Header().Set("Content-Length", itoa(meta.size))
	}
	_, _ = io.Copy(w, x.pr)
}

// handleExcelCallback receives save notifications from the Document Server.
func (a *App) handleExcelCallback(w http.ResponseWriter, r *http.Request) {
	s, ok := a.excel.get(r.PathValue("sid"))
	if !ok {
		writeJSON(w, http.StatusOK, map[string]int{"error": 1})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]int{"error": 1})
		return
	}

	var cb struct {
		Status int    `json:"status"`
		URL    string `json:"url"`
		Token  string `json:"token"`
	}
	_ = json.Unmarshal(body, &cb)

	// Verify JWT when a secret is configured. The signed payload may be the
	// callback body itself, carried in the Authorization header or body token.
	if a.cfg.OfficeJWTSecret != "" {
		tok := cb.Token
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimPrefix(h, "Bearer ")
		}
		payload, err := jwtVerify(a.cfg.OfficeJWTSecret, tok)
		if err != nil {
			a.audit.Log(AuditEntry{Actor: "onlyoffice", IP: clientIP(r), Action: "excel-save",
				Device: s.device, Path: s.fullTxt, OK: false, Error: "bad JWT: " + err.Error()})
			writeJSON(w, http.StatusOK, map[string]int{"error": 1})
			return
		}
		// The verified payload holds the authoritative status/url.
		var inner struct {
			Status int    `json:"status"`
			URL    string `json:"url"`
		}
		if json.Unmarshal(payload, &inner) == nil && inner.Status != 0 {
			cb.Status, cb.URL = inner.Status, inner.URL
		}
	}

	// Status 2 = all editors closed, save now; 6 = force-save while editing.
	if cb.Status == 2 || cb.Status == 6 {
		err := a.saveEditedFile(r, s, cb.URL)
		switch {
		case err == nil:
			a.audit.Log(AuditEntry{Actor: "onlyoffice", IP: clientIP(r), Action: "excel-save",
				Device: s.device, Path: s.fullTxt, OK: true})
		case errors.Is(err, errQueuedWhileOpen):
			// File is open on the PC: the edit is held on the server and written
			// back automatically once the file is closed. Report success to the
			// Document Server so the editor shows the work as saved.
			a.audit.Log(AuditEntry{Actor: "onlyoffice", IP: clientIP(r), Action: "excel-save",
				Device: s.device, Path: s.fullTxt, OK: true, Error: "file open on PC — will save when closed"})
		case errors.Is(err, errConflictStashed):
			// The PC's own changes changed the file; we keep the PC version and
			// stash this edit on the server. No message is shown to the user.
			a.audit.Log(AuditEntry{Actor: "onlyoffice", IP: clientIP(r), Action: "excel-save",
				Device: s.device, Path: s.fullTxt, OK: false,
				Error: "conflict — file changed on PC; edit kept on server for review (not overwritten)"})
		default:
			a.audit.Log(AuditEntry{Actor: "onlyoffice", IP: clientIP(r), Action: "excel-save",
				Device: s.device, Path: s.fullTxt, OK: false, Error: err.Error()})
			writeJSON(w, http.StatusOK, map[string]int{"error": 1})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"error": 0})
}

// errQueuedWhileOpen means the edit couldn't be written because the file is open
// on the PC, so it was queued on the server to be saved once the file is closed.
var errQueuedWhileOpen = errors.New("queued: file open on the PC")

// errConflictStashed means the file changed on the PC since editing started, so
// the edit wasn't written (PC changes preserved); the edit is kept on the server
// for manual review. No message is shown to the user.
var errConflictStashed = errors.New("conflict: file changed on the PC; edit stashed on server")

// isLockErr reports whether an agent store error is the "file open on the PC"
// marker returned by the agent (errFileLocked).
func isLockErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "RF_FILE_LOCKED")
}

// isConflictErr reports whether the agent refused to overwrite because the file
// changed on the PC since editing started (errConflict).
func isConflictErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "RF_CONFLICT")
}

// storeBytes pushes file bytes to the agent, writing them to dir/name. baseMtime,
// when non-zero, makes the agent refuse to overwrite a file that changed on the
// PC since editing started (returns an RF_CONFLICT error).
func (a *App) storeBytes(ctx context.Context, device, dir, name string, data []byte, baseMtime int64) error {
	jobID, _ := a.transfers.newUpload(device, bytes.NewReader(data))
	defer a.transfers.remove(jobID)
	_, err := a.hub.Call(ctx, device, proto.MethodStore, proto.StoreParams{
		JobID: jobID, Dir: dir, Name: name, Overwrite: true, BaseMtime: baseMtime,
	})
	return err
}

// saveEditedFile downloads the edited document from the Document Server and
// writes it back to the agent (overwriting, with a backup). If the file is open
// on the PC, the bytes are queued on the server and errQueuedWhileOpen is
// returned; a background loop writes them once the file is released.
func (a *App) saveEditedFile(r *http.Request, s *excelSession, dsURL string) error {
	if dsURL == "" {
		return errors.New("no document URL in callback")
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(dsURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("could not download edited file from document server")
	}
	data, err := io.ReadAll(http.MaxBytesReader(nil, resp.Body, 256<<20))
	if err != nil {
		return err
	}

	err = a.storeBytes(r.Context(), s.device, s.dir, s.name, data, s.baseMtime)
	if err == nil {
		a.pending.remove(s.device, s.fullTxt) // supersede any earlier queued copy
		return nil
	}
	if isLockErr(err) {
		if perr := a.pending.put(pendingMeta{
			Device: s.device, Dir: s.dir, Name: s.name, Full: s.fullTxt,
			BaseMtime: s.baseMtime, Created: time.Now(),
		}, data); perr != nil {
			return perr // couldn't even queue — surface as a real error
		}
		return errQueuedWhileOpen
	}
	if isConflictErr(err) {
		// The PC's own changes win; keep this edit on the server for review and
		// stop retrying so the PC version is never overwritten.
		a.pending.remove(s.device, s.fullTxt)
		_ = a.pending.putConflict(pendingMeta{
			Device: s.device, Dir: s.dir, Name: s.name, Full: s.fullTxt,
			BaseMtime: s.baseMtime, Created: time.Now(),
		}, data)
		return errConflictStashed
	}
	return err
}

// runPendingRetry writes queued edits (files that were open on the PC) back to
// their originals as soon as the PC releases them. Bytes live on the server only.
func (a *App) runPendingRetry() {
	for range time.Tick(20 * time.Second) {
		for _, it := range a.pending.all() {
			if time.Since(it.meta.Created) > 24*time.Hour {
				a.pending.remove(it.meta.Device, it.meta.Full)
				a.audit.Log(AuditEntry{Actor: "system", Action: "excel-save", Device: it.meta.Device,
					Path: it.meta.Full, OK: false, Error: "gave up after 24h — file stayed open on the PC"})
				continue
			}
			if !a.hub.Online(it.meta.Device) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			err := a.storeBytes(ctx, it.meta.Device, it.meta.Dir, it.meta.Name, it.data, it.meta.BaseMtime)
			cancel()
			switch {
			case err == nil:
				a.pending.remove(it.meta.Device, it.meta.Full)
				a.audit.Log(AuditEntry{Actor: "system", Action: "excel-save", Device: it.meta.Device,
					Path: it.meta.Full, OK: true, Error: "saved after the file was closed on the PC"})
			case isLockErr(err):
				// still open — keep waiting
			case isConflictErr(err):
				// The file was edited on the PC while we waited. Keep the PC
				// version; stash this edit for review and stop retrying.
				a.pending.remove(it.meta.Device, it.meta.Full)
				_ = a.pending.putConflict(it.meta, it.data)
				a.audit.Log(AuditEntry{Actor: "system", Action: "excel-save", Device: it.meta.Device,
					Path: it.meta.Full, OK: false,
					Error: "conflict — file changed on PC; edit kept on server for review (not overwritten)"})
			default:
				log.Printf("pending save retry for %s: %v", it.meta.Full, err)
			}
		}
	}
}
