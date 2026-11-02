package main

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Roles, from least to most privileged.
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleAdmin  = "admin"
)

func roleRank(r string) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleEditor:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

func validRole(r string) bool { return roleRank(r) > 0 }

// atLeast reports whether role `have` meets the minimum `need`.
func atLeast(have, need string) bool { return roleRank(have) >= roleRank(need) }

type User struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	PassHash  string    `json:"passHash"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"createdAt"`
}

var (
	errUserExists = errors.New("a user with that name already exists")
	errUserGone   = errors.New("user not found")
)

// normUser lowercases and trims a username (names are case-insensitive).
func normUser(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// validUsername allows letters, digits, dot, dash, underscore; 3–32 chars.
func validUsername(name string) error {
	if len(name) < 3 || len(name) > 32 {
		return errors.New("username must be 3–32 characters")
	}
	for _, c := range name {
		if !(unicode.IsLetter(c) || unicode.IsDigit(c) || c == '.' || c == '-' || c == '_') {
			return errors.New("username may use letters, digits, . - _ only")
		}
	}
	return nil
}

func validPassword(pw string) error {
	if len(pw) < 10 {
		return errors.New("password must be at least 10 characters")
	}
	if len(pw) > 200 {
		return errors.New("password is too long")
	}
	return nil
}

// ---- store methods ----

func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, 0, len(s.data.Users))
	for _, u := range s.data.Users {
		c := *u
		c.PassHash = "" // never leak hashes
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// UserAuth returns the stored user (with hash) for login checks.
func (s *Store) UserAuth(name string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.data.Users[normUser(name)]
	if !ok {
		return User{}, false
	}
	return *u, true
}

func (s *Store) AddUser(u User) error {
	key := normUser(u.Username)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Users[key]; ok {
		return errUserExists
	}
	u.Username = key
	u.CreatedAt = time.Now().UTC()
	s.data.Users[key] = &u
	return s.saveLocked()
}

func (s *Store) UpdateUser(name string, fn func(*User) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.data.Users[normUser(name)]
	if !ok {
		return errUserGone
	}
	if err := fn(u); err != nil {
		return err
	}
	return s.saveLocked()
}

func (s *Store) DeleteUser(name string) (bool, error) {
	key := normUser(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Users[key]; !ok {
		return false, nil
	}
	delete(s.data.Users, key)
	return true, s.saveLocked()
}

// ---- HTTP handlers (admin only; wired in routes) ----

func (a *App) handleUsers(w http.ResponseWriter, r *http.Request) {
	out := a.store.Users()
	// Present the built-in root admin as a non-editable row so the UI can show it.
	root := userView{Username: normUser(a.cfg.AdminUser), Role: RoleAdmin, Builtin: true}
	views := []userView{root}
	for _, u := range out {
		views = append(views, userView{Username: u.Username, Role: u.Role, Disabled: u.Disabled, CreatedAt: u.CreatedAt})
	}
	writeJSON(w, http.StatusOK, views)
}

type userView struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Disabled  bool      `json:"disabled"`
	Builtin   bool      `json:"builtin,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Role, Password string }
	if err := readJSON(w, r, 4096, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	name := normUser(body.Username)
	if err := validUsername(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if name == normUser(a.cfg.AdminUser) {
		writeErr(w, http.StatusConflict, "that name is reserved for the built-in admin")
		return
	}
	if !validRole(body.Role) {
		writeErr(w, http.StatusBadRequest, "role must be admin, editor or viewer")
		return
	}
	if err := validPassword(body.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := hashPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not hash password")
		return
	}
	if err := a.store.AddUser(User{Username: name, Role: body.Role, PassHash: hash}); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	a.audit.Log(AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "user-add", Path: name + " (" + body.Role + ")", OK: true})
	w.WriteHeader(http.StatusCreated)
}

func (a *App) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	name := normUser(r.PathValue("name"))
	if name == normUser(a.cfg.AdminUser) {
		writeErr(w, http.StatusForbidden, "the built-in admin cannot be changed here")
		return
	}
	var body struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if err := readJSON(w, r, 4096, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	// Don't let an admin lock themselves out of their own account.
	if actor(r) == name && ((body.Role != nil && *body.Role != RoleAdmin) || (body.Disabled != nil && *body.Disabled)) {
		writeErr(w, http.StatusBadRequest, "you cannot change your own role or disable yourself")
		return
	}
	var changes []string
	err := a.store.UpdateUser(name, func(u *User) error {
		if body.Role != nil {
			if !validRole(*body.Role) {
				return errors.New("invalid role")
			}
			u.Role = *body.Role
			changes = append(changes, "role="+*body.Role)
		}
		if body.Disabled != nil {
			u.Disabled = *body.Disabled
			changes = append(changes, map[bool]string{true: "disabled", false: "enabled"}[*body.Disabled])
		}
		if body.Password != nil {
			if err := validPassword(*body.Password); err != nil {
				return err
			}
			h, err := hashPassword(*body.Password)
			if err != nil {
				return err
			}
			u.PassHash = h
			changes = append(changes, "password reset")
		}
		return nil
	})
	if err != nil {
		if err == errUserGone {
			writeErr(w, http.StatusNotFound, "user not found")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	// A role change, disable, or password reset should end existing sessions.
	a.sessions.RevokeUser(name)
	a.audit.Log(AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "user-update", Path: name + ": " + strings.Join(changes, ", "), OK: true})
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := normUser(r.PathValue("name"))
	if name == normUser(a.cfg.AdminUser) {
		writeErr(w, http.StatusForbidden, "the built-in admin cannot be deleted")
		return
	}
	if actor(r) == name {
		writeErr(w, http.StatusBadRequest, "you cannot delete your own account")
		return
	}
	ok, err := a.store.DeleteUser(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	a.sessions.RevokeUser(name)
	a.audit.Log(AuditEntry{Actor: actor(r), IP: clientIP(r), Action: "user-delete", Path: name, OK: true})
	w.WriteHeader(http.StatusNoContent)
}
