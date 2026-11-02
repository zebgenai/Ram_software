package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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
	device  string
	dir     string
	name    string
	fullTxt string // full path on the agent
	created time.Time
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

// ---- supported spreadsheet types ----

var excelExts = map[string]bool{
	".xlsx": true, ".xlsm": true, ".xls": true, ".csv": true, ".ods": true,
}

func isSpreadsheet(name string) bool {
	return excelExts[strings.ToLower(path.Ext(name))]
}

// ---- handlers ----

// handleExcelOpen (editor+) returns the OnlyOffice editor config for a file.
func (a *App) handleExcelOpen(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.ExcelEnabled() {
		writeErr(w, http.StatusNotImplemented, "Excel editing is not configured on this server")
		return
	}
	id := r.PathValue("id")
	if _, ok := a.store.Device(id); !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || !isSpreadsheet(p) {
		writeErr(w, http.StatusBadRequest, "a spreadsheet file path is required")
		return
	}
	if !a.hub.Online(id) {
		writeErr(w, http.StatusServiceUnavailable, errOffline.Error())
		return
	}

	name := path.Base(p)
	sid := a.excel.add(&excelSession{device: id, dir: path.Dir(p), name: name, fullTxt: p})
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
		"documentType": "cell",
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
		if err := a.saveEditedFile(r, s, cb.URL); err != nil {
			a.audit.Log(AuditEntry{Actor: "onlyoffice", IP: clientIP(r), Action: "excel-save",
				Device: s.device, Path: s.fullTxt, OK: false, Error: err.Error()})
			writeJSON(w, http.StatusOK, map[string]int{"error": 1})
			return
		}
		a.audit.Log(AuditEntry{Actor: "onlyoffice", IP: clientIP(r), Action: "excel-save",
			Device: s.device, Path: s.fullTxt, OK: true})
	}
	writeJSON(w, http.StatusOK, map[string]int{"error": 0})
}

// saveEditedFile downloads the edited document from the Document Server and
// writes it back to the agent (overwriting, with a backup).
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
	body := http.MaxBytesReader(nil, resp.Body, 2<<30)

	jobID, _ := a.transfers.newUpload(s.device, body)
	defer a.transfers.remove(jobID)
	_, err = a.hub.Call(r.Context(), s.device, proto.MethodStore, proto.StoreParams{
		JobID: jobID, Dir: s.dir, Name: s.name, Overwrite: true,
	})
	return err
}
