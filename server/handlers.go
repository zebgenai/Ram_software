package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"remotefiles/internal/proto"
)

//go:embed web
var webFS embed.FS

// dummyHash is checked when an unknown username logs in, so the response time
// does not reveal whether the username exists.
var dummyHash, _ = hashPassword("this-is-never-a-real-password-000")

type App struct {
	cfg       Config
	store     *Store
	audit     *Audit
	hub       *Hub
	sessions  *Sessions
	limiter   *Limiter
	transfers *Transfers
	excel     *ExcelSessions
	pending   *PendingSaves
}

type ctxKey string

const (
	userKey ctxKey = "user"
	roleKey ctxKey = "role"
)

// agent binaries served from AGENTS_DIR, keyed by the platform in the URL.
var agentFiles = map[string]struct{ file, ext string }{
	"windows":   {"Excel-windows-amd64.exe", ".exe"},
	"mac-arm64": {"Excel-darwin-arm64", ""},
	"mac-intel": {"Excel-darwin-amd64", ""},
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "index.html")
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "favicon.ico")
	})

	// admin API
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("GET /api/me", a.viewer(a.handleMe))
	mux.HandleFunc("GET /api/devices", a.viewer(a.handleDevices))
	mux.HandleFunc("PATCH /api/devices/{id}", a.admin(a.handleRenameDevice))
	mux.HandleFunc("DELETE /api/devices/{id}", a.admin(a.handleRevokeDevice))
	// browse + download: any signed-in role
	mux.HandleFunc("GET /api/devices/{id}/drives", a.viewer(a.handleDrives))
	mux.HandleFunc("GET /api/devices/{id}/list", a.viewer(a.handleList))
	mux.HandleFunc("GET /api/devices/{id}/download", a.viewer(a.handleDownload))
	// change files: editor and up
	mux.HandleFunc("POST /api/devices/{id}/mkdir", a.editor(a.handleMkdir))
	mux.HandleFunc("POST /api/devices/{id}/rename", a.editor(a.handleRename))
	mux.HandleFunc("POST /api/devices/{id}/move", a.editor(a.handleMove))
	mux.HandleFunc("POST /api/devices/{id}/copy", a.editor(a.handleCopy))
	mux.HandleFunc("POST /api/devices/{id}/upload", a.editor(a.handleUpload))
	// delete: admin only
	mux.HandleFunc("POST /api/devices/{id}/delete", a.admin(a.handleDelete))
	// Excel editing (OnlyOffice): open config (editor+); the editor page; and the
	// two Document-Server endpoints (no session — secured by unguessable sid + JWT).
	mux.HandleFunc("POST /api/devices/{id}/excel/open", a.editor(a.handleExcelOpen))
	mux.HandleFunc("GET /excel/editor", a.editor(a.handleEditorPage))
	mux.HandleFunc("GET /excel/file/{sid}", a.handleExcelFile)
	mux.HandleFunc("POST /excel/callback/{sid}", a.handleExcelCallback)
	// invites, audit, users: admin only
	mux.HandleFunc("GET /api/invites", a.admin(a.handleInvites))
	mux.HandleFunc("POST /api/invites", a.admin(a.handleCreateInvite))
	mux.HandleFunc("DELETE /api/invites/{hash}", a.admin(a.handleDeleteInvite))
	mux.HandleFunc("GET /api/audit", a.admin(a.handleAudit))
	mux.HandleFunc("GET /api/users", a.admin(a.handleUsers))
	mux.HandleFunc("POST /api/users", a.admin(a.handleCreateUser))
	mux.HandleFunc("PATCH /api/users/{name}", a.admin(a.handleUpdateUser))
	mux.HandleFunc("DELETE /api/users/{name}", a.admin(a.handleDeleteUser))

	// public invite page + agent download
	mux.HandleFunc("GET /join/{token}", a.handleJoinPage)
	mux.HandleFunc("GET /join/{token}/agent/{platform}", a.handleAgentDownload)

	// agent API
	mux.HandleFunc("POST /agent/enroll", a.handleEnroll)
	mux.HandleFunc("POST /agent/poll", a.handlePoll)
	mux.HandleFunc("POST /agent/result", a.handleResult)
	mux.HandleFunc("POST /agent/blob/{job}", a.handleBlobPush) // download: agent → server
	mux.HandleFunc("GET /agent/blob/{job}", a.handleBlobPull)  // upload: server → agent

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/join/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func clientIP(r *http.Request) string {
	// The app is only reachable through nginx, which sets X-Real-IP.
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// need wraps a handler with session auth and a minimum role. State-changing
// requests must also carry X-Requested-With (CSRF defence on top of SameSite=Strict).
func (a *App) need(min string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		user, role, ok := a.sessions.Get(c.Value)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "session expired")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "rf" {
			writeErr(w, http.StatusForbidden, "missing CSRF header")
			return
		}
		if !atLeast(role, min) {
			writeErr(w, http.StatusForbidden, "your role does not allow this action")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, roleKey, role)
		h(w, r.WithContext(ctx))
	}
}

// Convenience wrappers by minimum role.
func (a *App) viewer(h http.HandlerFunc) http.HandlerFunc { return a.need(RoleViewer, h) }
func (a *App) editor(h http.HandlerFunc) http.HandlerFunc { return a.need(RoleEditor, h) }
func (a *App) admin(h http.HandlerFunc) http.HandlerFunc  { return a.need(RoleAdmin, h) }

func actor(r *http.Request) string {
	if u, ok := r.Context().Value(userKey).(string); ok {
		return u
	}
	return ""
}

// ---- admin handlers ----

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if r.Header.Get("X-Requested-With") != "rf" {
		writeErr(w, http.StatusForbidden, "missing CSRF header")
		return
	}
	if a.limiter.Blocked(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts, try again in 15 minutes")
		return
	}
	var body struct{ Username, Password string }
	if err := readJSON(w, r, 4096, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	name := normUser(body.Username)
	user, role, ok := a.authenticate(name, body.Password)
	if !ok {
		a.limiter.Fail(ip)
		a.audit.Log(AuditEntry{Actor: body.Username, IP: ip, Action: "login", OK: false, Error: "bad credentials"})
		writeErr(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	a.limiter.Reset(ip)
	tok := a.sessions.Create(user, role)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: a.cfg.SecureCookie, SameSite: http.SameSiteStrictMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	a.audit.Log(AuditEntry{Actor: user, IP: ip, Action: "login", OK: true, Path: role})
	writeJSON(w, http.StatusOK, map[string]string{"user": user, "role": role})
}

// authenticate checks the built-in root admin (from env) first, then store
// users. It always runs one password hash to blunt username-enumeration timing.
func (a *App) authenticate(name, pw string) (user, role string, ok bool) {
	rootName := normUser(a.cfg.AdminUser)
	if subtle.ConstantTimeCompare([]byte(name), []byte(rootName)) == 1 {
		if checkPassword(a.cfg.AdminHash, pw) {
			return rootName, RoleAdmin, true
		}
		return "", "", false
	}
	u, found := a.store.UserAuth(name)
	if !found {
		checkPassword(dummyHash, pw) // constant-ish work
		return "", "", false
	}
	if u.Disabled || !checkPassword(u.PassHash, pw) {
		return "", "", false
	}
	return u.Username, u.Role, true
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.cfg.SecureCookie, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func role(r *http.Request) string {
	if v, ok := r.Context().Value(roleKey).(string); ok {
		return v
	}
	return ""
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user": actor(r), "role": role(r), "company": a.cfg.CompanyName,
		"excel": a.cfg.ExcelEnabled(),
	})
}

// handleEditorPage serves the OnlyOffice host page with a CSP loosened just
// enough to load the Document Server's api.js and its editor iframe.
func (a *App) handleEditorPage(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.ExcelEnabled() {
		http.Error(w, "Excel editing is not configured", http.StatusNotImplemented)
		return
	}
	office := a.cfg.OfficePublicURL
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; "+
			"script-src 'self' 'unsafe-inline' "+office+"; "+
			"style-src 'self' 'unsafe-inline' "+office+"; "+
			"frame-src "+office+"; "+
			"connect-src 'self' "+office+"; "+
			"img-src 'self' data: "+office+"; "+
			"font-src 'self' data: "+office+"; "+
			"base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	static, _ := fs.Sub(webFS, "web")
	http.ServeFileFS(w, r, static, "editor.html")
}

type deviceView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Username     string    `json:"username"`
	AgentVersion string    `json:"agentVersion"`
	EnrolledAt   time.Time `json:"enrolledAt"`
	LastSeen     time.Time `json:"lastSeen"`
	LastIP       string    `json:"lastIp"`
	Online       bool      `json:"online"`
}

func (a *App) handleDevices(w http.ResponseWriter, r *http.Request) {
	ds := a.store.Devices()
	out := make([]deviceView, 0, len(ds))
	for _, d := range ds {
		out = append(out, deviceView{
			ID: d.ID, Name: d.Name, Hostname: d.Hostname, OS: d.OS, Arch: d.Arch,
			Username: d.Username, AgentVersion: d.AgentVersion, EnrolledAt: d.EnrolledAt,
			LastSeen: d.LastSeen, LastIP: d.LastIP, Online: a.hub.Online(d.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	var body struct{ Name string }
	if err := readJSON(w, r, 4096, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	name := strings.TrimSpace(body.Name)
	if utf8.RuneCountInString(name) > 64 {
		writeErr(w, http.StatusBadRequest, "name too long")
		return
	}
	id := r.PathValue("id")
	if err := a.store.RenameDevice(id, name); err != nil {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	a.audit.Log(AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "rename", Device: id, Path: name, OK: true})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := a.store.DeleteDevice(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	a.hub.Forget(id)
	a.audit.Log(AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "revoke", Device: id, OK: true})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) callDevice(w http.ResponseWriter, r *http.Request, action, method, path string, params any) {
	id := r.PathValue("id")
	if _, ok := a.store.Device(id); !ok {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	res, err := a.hub.Call(r.Context(), id, method, params)
	entry := AuditEntry{Actor: actor(r), IP: clientIP(r), Action: action, Device: id, Path: path, OK: err == nil}
	if err != nil {
		entry.Error = err.Error()
		a.audit.Log(entry)
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, errOffline):
			status = http.StatusServiceUnavailable
		case errors.Is(err, errTimeout):
			status = http.StatusGatewayTimeout
		case errors.Is(err, errBusy):
			status = http.StatusTooManyRequests
		}
		writeErr(w, status, err.Error())
		return
	}
	a.audit.Log(entry)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(res)
}

func (a *App) handleDrives(w http.ResponseWriter, r *http.Request) {
	a.callDevice(w, r, "drives", proto.MethodDrives, "", nil)
}

func (a *App) handleList(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" || len(p) > 4096 {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	a.callDevice(w, r, "list", proto.MethodList, p, proto.ListParams{Path: p})
}

type inviteView struct {
	Hash      string     `json:"hash"`
	Note      string     `json:"note"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"`
	DeviceID  string     `json:"deviceId,omitempty"`
}

func (a *App) handleInvites(w http.ResponseWriter, r *http.Request) {
	inv := a.store.Invites()
	out := make([]inviteView, 0, len(inv))
	for _, v := range inv {
		out = append(out, inviteView{v.TokenHash, v.Note, v.CreatedAt, v.ExpiresAt, v.UsedAt, v.DeviceID})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note  string `json:"note"`
		Hours int    `json:"hours"`
	}
	if err := readJSON(w, r, 4096, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.Hours <= 0 || body.Hours > 72 {
		body.Hours = 24
	}
	note := strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(note) > 64 {
		writeErr(w, http.StatusBadRequest, "note too long")
		return
	}
	tok := randToken(24) // 32 URL-safe chars
	now := time.Now().UTC()
	inv := Invite{TokenHash: sha256Hex(tok), Note: note, CreatedAt: now, ExpiresAt: now.Add(time.Duration(body.Hours) * time.Hour)}
	if err := a.store.AddInvite(inv); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save")
		return
	}
	a.audit.Log(AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "invite", Path: note, OK: true})
	writeJSON(w, http.StatusCreated, map[string]any{
		"url":       a.cfg.PublicURL + "/join/" + tok,
		"expiresAt": inv.ExpiresAt,
	})
}

func (a *App) handleDeleteInvite(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteInvite(r.PathValue("hash")); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleAudit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.audit.Recent(300))
}

// ---- invite page + download ----

var joinTmpl = template.Must(template.ParseFS(webFS, "web/join.html"))

func validTokenShape(t string) bool {
	if len(t) != 32 {
		return false
	}
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (a *App) handleJoinPage(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	_, ok := a.store.ValidInvite(sha256Hex(tok))
	if !validTokenShape(tok) {
		ok = false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
	}
	_ = joinTmpl.Execute(w, map[string]any{"Valid": ok, "Token": tok, "Company": a.cfg.CompanyName})
}

func (a *App) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	if !validTokenShape(tok) {
		http.NotFound(w, r)
		return
	}
	if _, ok := a.store.ValidInvite(sha256Hex(tok)); !ok {
		http.Error(w, "This invite link is invalid, expired or already used.", http.StatusNotFound)
		return
	}
	af, ok := agentFiles[r.PathValue("platform")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(a.cfg.AgentsDir, af.file))
	if err != nil {
		log.Printf("agent binary missing: %v", err)
		http.Error(w, "Agent not available on the server", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	// The invite token travels in the file name; the agent reads it on first run.
	name := "Excel-" + tok + af.ext
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---- agent API ----

func (a *App) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limiter.Blocked("enroll:" + ip) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var req proto.EnrollRequest
	if err := readJSON(w, r, 8192, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if !req.Consent {
		writeErr(w, http.StatusBadRequest, "user consent is required")
		return
	}
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if utf8.RuneCountInString(s) > 128 {
			return string([]rune(s)[:128])
		}
		return s
	}
	secret := randToken(32)
	d := Device{
		ID: randHex(8), Hostname: clip(req.Hostname), OS: clip(req.OS), Arch: clip(req.Arch),
		Username: clip(req.Username), AgentVersion: clip(req.AgentVersion),
		SecretHash: sha256Hex(secret), EnrolledAt: time.Now().UTC(), LastSeen: time.Now().UTC(), LastIP: ip,
	}
	if err := a.store.Enroll(sha256Hex(req.Token), d); err != nil {
		a.limiter.Fail("enroll:" + ip)
		a.audit.Log(AuditEntry{Actor: "agent", IP: ip, Action: "enroll", OK: false, Error: err.Error()})
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	a.audit.Log(AuditEntry{Actor: "agent", IP: ip, Action: "enroll", Device: d.ID, Path: d.Hostname, OK: true})
	writeJSON(w, http.StatusOK, proto.EnrollResponse{DeviceID: d.ID, Secret: secret})
}

func (a *App) authDevice(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, secret, err := parseDeviceAuth(r)
	if err == nil {
		if d, ok := a.store.Device(id); ok &&
			subtle.ConstantTimeCompare([]byte(sha256Hex(secret)), []byte(d.SecretHash)) == 1 {
			a.store.TouchDevice(id, clientIP(r))
			return id, true
		}
	}
	writeErr(w, http.StatusUnauthorized, "device not enrolled or revoked")
	return "", false
}

func (a *App) handlePoll(w http.ResponseWriter, r *http.Request) {
	id, ok := a.authDevice(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.hub.Poll(r.Context(), id))
}

func (a *App) handleResult(w http.ResponseWriter, r *http.Request) {
	id, ok := a.authDevice(w, r)
	if !ok {
		return
	}
	var resp proto.Response
	if err := readJSON(w, r, 16<<20, &resp); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	a.hub.Deliver(id, resp)
	w.WriteHeader(http.StatusNoContent)
}
