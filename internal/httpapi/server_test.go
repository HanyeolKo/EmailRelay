package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"emailrelay/internal/store"
)

type idleJobs struct{}

func (idleJobs) Wake()                                    {}
func (idleJobs) EditRoute(_ int64, fn func() error) error { return fn() }
func (idleJobs) EditUsers(_ int64, fn func() error) error { return fn() }
func (idleJobs) Test(context.Context, int64, int64) error { return nil }

func TestLoginLimitThroughProxy(t *testing.T) {
	public, _ := url.Parse("https://relay.example")
	for _, tt := range []struct {
		name    string
		trusted bool
		peer    string
		header  []string
		want    string
	}{
		{"default", false, "127.0.0.1:52000", []string{"192.0.2.5"}, "127.0.0.1"},
		{"local proxy", true, "[::1]:52000", []string{"192.0.2.5"}, "192.0.2.5"},
		{"remote peer", true, "192.0.2.4:52000", []string{"192.0.2.5"}, "192.0.2.4"},
		{"absent", true, "127.0.0.1:52000", nil, "127.0.0.1"},
		{"list", true, "127.0.0.1:52000", []string{"192.0.2.5, 127.0.0.1"}, "127.0.0.1"},
		{"multiple headers", true, "127.0.0.1:52000", []string{"192.0.2.5", "192.0.2.6"}, "127.0.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/login", nil)
			r.RemoteAddr = tt.peer
			r.Header["X-Real-Ip"] = tt.header
			s := Server{origin: public, trustProxyIP: tt.trusted}
			if got := s.clientIP(r); got != tt.want {
				t.Errorf("client IP = %q, want %q", got, tt.want)
			}
		})
	}
	var limit authLimiter
	const host = "192.0.2.5"
	for range 20 {
		success := limit.reserve(host)
		if success == nil {
			t.Fatal("successful logins exhausted the failure budget")
		}
		success()
	}
	oldSuccess := limit.reserve(host)
	limit.entries[host].expires = time.Now().Add(-time.Minute)
	for range 15 {
		if limit.reserve(host) == nil {
			t.Fatal("new window was prematurely blocked")
		}
	}
	oldSuccess()
	if limit.reserve(host) != nil || limit.reserve("192.0.2.6") == nil {
		t.Fatal("failure budget was not bound to its address and time window")
	}
}

func TestAccessBoundaries(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	origin, _ := url.Parse("https://relay.example")
	handler := New(db, idleJobs{}, origin, "first-run-secret", nil, false)
	call := func(code int, method, path, body, originHeader string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, origin.String()+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", originHeader)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		r.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("%s %s: HTTP %d, expected %d (%s)", method, path, w.Code, code, w.Body.String())
		}
		return w
	}
	call(401, "GET", "/api/routes", "", "", nil, "")
	setup := `{"token":"first-run-secret","username":"owner","password":"owner-long-pass"}`
	call(403, "POST", "/api/setup", setup, "https://elsewhere.example", nil, "")
	call(403, "POST", "/api/setup", strings.Replace(setup, "first-run-secret", "wrong", 1), "", nil, "")
	w := call(200, "POST", "/api/setup", setup, origin.String(), nil, "")
	admin := w.Result().Cookies()[0]
	if !admin.HttpOnly || !admin.Secure || admin.SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie protection missing")
	}
	var sess struct{ CSRF string }
	if err = json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	user := `{"username":"member","password":"member-long-pass","role":"user"}`
	call(403, "POST", "/api/users", user, "", admin, "")
	call(201, "POST", "/api/users", user, "", admin, sess.CSRF)
	spec := `{"name":"Personal","initial":"all","method":"imap","source_folder":"INBOX","target_folder":"Archive","source":{"host":"src.example","port":993,"security":"tls","username":"a","password":"source-private"},"target":{"host":"dst.example","port":993,"security":"tls","username":"b","password":"target-private"}}`
	call(201, "POST", "/api/routes", spec, "", admin, sess.CSRF)
	body := call(200, "GET", "/api/routes", "", "", admin, "").Body.String()
	if !strings.Contains(body, "Personal") || strings.Contains(body, "private") || strings.Contains(body, "password") {
		t.Fatal("route serialization")
	}
	if err = db.Initialize(1, 77, 0); err != nil {
		t.Fatal(err)
	}
	recoveryDecision := func(uid uint32) string {
		t.Helper()
		if err := db.Begin(1, 77, uid); err != nil {
			t.Fatal(err)
		}
		if err := db.Fail(1, uid, "pending confirmation", true); err != nil {
			t.Fatal(err)
		}
		var list struct{ Routes []routeView }
		res := call(200, "GET", "/api/routes", "", "", admin, "")
		if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Routes) != 1 || list.Routes[0].UncertainUID != uid || list.Routes[0].RecoveryToken == "" {
			t.Fatal("missing receipt confirmation")
		}
		return list.Routes[0].RecoveryToken
	}
	act := func(code int, action, decision string) {
		t.Helper()
		body, err := json.Marshal(map[string]string{"action": action, "recovery_token": decision})
		if err != nil {
			t.Fatal(err)
		}
		call(code, "POST", "/api/routes/1/action", string(body), "", admin, sess.CSRF)
	}
	first := recoveryDecision(12)
	changedTarget := strings.Replace(spec, `"target_folder":"Archive"`, `"target_folder":"Other"`, 1)
	call(409, "PUT", "/api/routes/1", changedTarget, "", admin, sess.CSRF)
	refreshed := strings.Replace(spec, "target-private", "renewed-private", 1)
	call(204, "PUT", "/api/routes/1", refreshed, "", admin, sess.CSRF)
	act(409, "retry", "")
	act(204, "retry", first)
	// A second attempt on the very same UID requires its own confirmation.
	second := recoveryDecision(12)
	act(409, "skip", first)
	act(204, "skip", second)
	third := recoveryDecision(13)
	act(409, "retry", second)
	act(204, "skip", third)
	call(204, "PUT", "/api/routes/1", changedTarget, "", admin, sess.CSRF)
	if err = db.Fail(1, 0, "connection failed", false); err != nil {
		t.Fatal(err)
	}
	call(204, "POST", "/api/routes/1/action", `{"action":"retry"}`, "", admin, sess.CSRF)
	w = call(200, "POST", "/api/login", `{"username":"member","password":"member-long-pass"}`, "", nil, "")
	member := w.Result().Cookies()[0]
	var mu struct{ CSRF string }
	_ = json.Unmarshal(w.Body.Bytes(), &mu)
	call(403, "GET", "/api/users", "", "", member, "")
	call(404, "GET", "/api/routes/1/events", "", "", member, "")
	call(404, "DELETE", "/api/routes/1", "", "", member, mu.CSRF)
	call(204, "PUT", "/api/users/2", `{"enabled":false}`, "", admin, sess.CSRF)
	call(401, "GET", "/api/routes", "", "", member, "")
	call(204, "POST", "/api/logout", "{}", "", admin, sess.CSRF)
	call(401, "GET", "/api/routes", "", "", admin, "")
}
