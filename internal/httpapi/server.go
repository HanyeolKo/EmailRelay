package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"emailrelay/internal/store"
	"emailrelay/internal/webui"
)

type Scheduler interface {
	Wake()
	EditRoute(int64, func() error) error
	EditUsers(int64, func() error) error
	Test(context.Context, int64, int64) error
}

type Server struct {
	db           *store.Store
	jobs         Scheduler
	origin       *url.URL
	setupToken   string
	onSetup      func()
	trustProxyIP bool
	authLimiter  authLimiter
}

func New(db *store.Store, jobs Scheduler, origin *url.URL, token string, onSetup func(), trustProxyIP bool) http.Handler {
	s := &Server{db: db, jobs: jobs, origin: origin, setupToken: token, onSetup: onSetup, trustProxyIP: trustProxyIP}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.protected(s.logout))
	mux.HandleFunc("POST /api/password", s.protected(s.password))
	mux.HandleFunc("GET /api/routes", s.protected(s.routes))
	mux.HandleFunc("POST /api/routes", s.protected(s.saveRoute))
	mux.HandleFunc("PUT /api/routes/{id}", s.protected(s.saveRoute))
	mux.HandleFunc("DELETE /api/routes/{id}", s.protected(s.action))
	mux.HandleFunc("POST /api/routes/{id}/action", s.protected(s.action))
	mux.HandleFunc("POST /api/routes/{id}/test", s.protected(s.testRoute))
	mux.HandleFunc("GET /api/routes/{id}/events", s.protected(s.events))
	mux.HandleFunc("GET /api/users", s.protected(s.users))
	mux.HandleFunc("POST /api/users", s.protected(s.createUser))
	mux.HandleFunc("PUT /api/users/{id}", s.protected(s.setUser))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		fail(w, 404, "요청한 API를 찾을 수 없습니다.")
	})
	mux.Handle("/", webui.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("Cache-Control", "no-store")
		if !s.allowedHost(r.Host) {
			fail(w, 403, "허용되지 않은 서버 주소입니다.")
			return
		}
		if mutable(r) {
			if origin := r.Header.Get("Origin"); origin != "" && origin != s.origin.Scheme+"://"+r.Host || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, "같은 웹 주소에서 다시 시도해 주세요.")
				return
			}
			if r.Method != http.MethodDelete {
				media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if media != "application/json" {
					fail(w, 415, "JSON 형식이 필요합니다.")
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) allowedHost(host string) bool {
	if strings.EqualFold(host, s.origin.Host) {
		return true
	}
	// Both loopback names are convenient when opening a local installation.
	if s.origin.Scheme != "http" {
		return false
	}
	u := url.URL{Host: host}
	return isLocal(u.Hostname()) && isLocal(s.origin.Hostname()) && u.Port() == s.origin.Port()
}

func isLocal(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }
func mutable(r *http.Request) bool {
	return r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS"
}
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	reply(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "입력값의 형식이나 길이를 확인해 주세요.")
		return false
	}
	return true
}
func report(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrAuth):
		fail(w, 401, "로그인 정보 또는 비밀번호를 확인해 주세요.")
	case errors.Is(err, store.ErrForbidden):
		fail(w, 403, "이 작업을 수행할 권한이 없습니다.")
	case errors.Is(err, store.ErrNotFound):
		fail(w, 404, "요청한 항목을 찾을 수 없습니다.")
	case errors.Is(err, store.ErrInvalid):
		fail(w, 400, "입력값을 확인해 주세요. 계정 이름, 비밀번호 길이, 메일 서버와 폴더 설정을 확인하세요.")
	case errors.Is(err, store.ErrConflict):
		fail(w, 409, "현재 상태에서는 변경할 수 없습니다. 화면을 새로 고친 뒤 설정이나 작업 상태를 확인해 주세요.")
	default:
		log.Print("database operation failed")
		fail(w, 500, "저장소 작업을 완료하지 못했습니다.")
	}
}

func (s *Server) cookieName() string {
	if s.origin.Scheme == "https" {
		return "__Host-emailrelay"
	}
	return "emailrelay_local"
}
func (s *Server) cookie(w http.ResponseWriter, token string) {
	c := &http.Cookie{Name: s.cookieName(), Value: token, Path: "/", HttpOnly: true, Secure: s.origin.Scheme == "https", SameSite: http.SameSiteStrictMode}
	if token == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}
func (s *Server) identity(r *http.Request) (store.Session, error) {
	c, e := r.Cookie(s.cookieName())
	if e != nil {
		return store.Session{}, store.ErrAuth
	}
	return s.db.Authenticate(c.Value)
}

type endpointHandler func(http.ResponseWriter, *http.Request, store.Session)

func (s *Server) protected(next endpointHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, e := s.identity(r)
		if e != nil {
			report(w, e)
			return
		}
		if mutable(r) && !sameSecret(r.Header.Get("X-CSRF-Token"), session.CSRF) {
			fail(w, 403, "화면을 새로 고친 뒤 다시 시도해 주세요.")
			return
		}
		next(w, r, session)
	}
}
func sameSecret(a, b string) bool {
	return b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func userView(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "username": u.Name, "role": u.Role, "enabled": u.Enabled}
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	session, err := s.identity(r)
	if err == nil {
		reply(w, 200, map[string]any{"user": userView(session.User), "csrf": session.CSRF, "needs_setup": false})
		return
	}
	if !errors.Is(err, store.ErrAuth) {
		report(w, err)
		return
	}
	has, e := s.db.HasUsers()
	if e != nil {
		report(w, e)
		return
	}
	reply(w, 200, map[string]any{"user": nil, "needs_setup": !has, "csrf": ""})
}

type authLimiter struct {
	mu      sync.Mutex
	entries map[string]*attempts
}
type attempts struct {
	count   int
	expires time.Time
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if s.trustProxyIP && s.origin.Scheme == "https" && r.TLS == nil && net.ParseIP(host).IsLoopback() {
		// The explicitly trusted local proxy overwrites this header. Ignore
		// lists, duplicates and malformed values instead of choosing a hop.
		if values := r.Header.Values("X-Real-IP"); len(values) == 1 {
			if ip := net.ParseIP(strings.TrimSpace(values[0])); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}

// Reserve a slot before checking credentials, so concurrent attempts also
// count toward the limit. Only failures consume it for the whole window.
func (a *authLimiter) reserve(host string) (success func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.entries == nil {
		a.entries = make(map[string]*attempts)
	}
	now := time.Now()
	v := a.entries[host]
	if v == nil || !v.expires.After(now) {
		for k, e := range a.entries {
			if !e.expires.After(now) {
				delete(a.entries, k)
			}
		}
		if len(a.entries) >= 4096 {
			return nil
		}
		v = &attempts{expires: now.Add(5 * time.Minute)}
		a.entries[host] = v
	}
	if v.count >= 15 {
		return nil
	}
	v.count++
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		v.count--
		if v.count == 0 && a.entries[host] == v {
			delete(a.entries, host)
		}
	}
}
func (s *Server) reserveAuth(w http.ResponseWriter, r *http.Request) func() {
	if success := s.authLimiter.reserve(s.clientIP(r)); success != nil {
		return success
	}
	w.Header().Set("Retry-After", "300")
	fail(w, 429, "시도 횟수가 많습니다. 5분 뒤 다시 시도해 주세요.")
	return nil
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	success := s.reserveAuth(w, r)
	if success == nil {
		return
	}
	var in struct{ Token, Username, Password string }
	if !decode(w, r, &in) {
		return
	}
	if !sameSecret(in.Token, s.setupToken) {
		fail(w, 403, "초기 설정 코드를 확인해 주세요.")
		return
	}
	if e := s.db.Bootstrap(in.Username, in.Password); e != nil {
		report(w, e)
		return
	}
	if s.onSetup != nil {
		s.onSetup()
	}
	s.startSession(w, in.Username, in.Password, success)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	success := s.reserveAuth(w, r)
	if success == nil {
		return
	}
	var in struct{ Username, Password string }
	if !decode(w, r, &in) {
		return
	}
	s.startSession(w, in.Username, in.Password, success)
}
func (s *Server) startSession(w http.ResponseWriter, username, password string, success func()) {
	session, e := s.db.Login(username, password)
	if e != nil {
		report(w, e)
		return
	}
	success()
	s.cookie(w, session.Token)
	reply(w, 200, map[string]any{"user": userView(session.User), "csrf": session.CSRF})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, session store.Session) {
	c, _ := r.Cookie(s.cookieName())
	if e := s.db.Logout(c.Value); e != nil {
		report(w, e)
		return
	}
	s.cookie(w, "")
	w.WriteHeader(204)
}
func (s *Server) password(w http.ResponseWriter, r *http.Request, session store.Session) {
	success := s.reserveAuth(w, r)
	if success == nil {
		return
	}
	var in struct {
		Current  string `json:"current_password"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if e := s.db.ChangePassword(session.User.ID, in.Current, in.Password); e != nil {
		report(w, e)
		return
	}
	s.startSession(w, session.User.Name, in.Password, success)
}
func idOf(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if e != nil || id <= 0 {
		fail(w, 404, "항목 번호를 확인해 주세요.")
		return 0, false
	}
	return id, true
}
