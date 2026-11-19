package store

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const password = "Example-Long-Password"

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
func openTest(t *testing.T) (*Store, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.sqlite")
	key := bytes.Repeat([]byte{0xa5}, 32)
	s, err := Open(path, key)
	check(t, err)
	t.Cleanup(func() { s.Close() })
	return s, path, key
}
func spec() RouteSpec {
	return RouteSpec{Name: "work", Source: Endpoint{Host: "mail.example.test", Security: "tls", Port: 993, Username: "in@example.test", Password: "secret-source-phrase"}, Target: Endpoint{Host: "smtp.example.test", Security: "starttls", Port: 587, Username: "out@example.test", Password: "secret-dest-phrase"}, Method: "smtp", SourceFolder: "INBOX", Sender: "user@example.test", Recipient: "owner@example.test", Policy: "all"}
}
func users(t *testing.T, s *Store) (Session, Session) {
	t.Helper()
	check(t, s.Bootstrap("Admin", password))
	a, err := s.Login("ADMIN", password)
	check(t, err)
	check(t, s.CreateUser(a.User.ID, "member", password, "user"))
	m, err := s.Login("member", password)
	check(t, err)
	return a, m
}

func TestBootstrapAndAuthentication(t *testing.T) {
	s, _, _ := openTest(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"owner1", "owner2"} {
		wg.Add(1)
		go func(name string) { defer wg.Done(); results <- s.Bootstrap(name, password) }(name)
	}
	wg.Wait()
	close(results)
	var count int
	for err := range results {
		if err == nil {
			count++
		} else {
			wantErr(t, err, ErrConflict)
		}
	}
	if count != 1 {
		t.Fatalf("bootstrap successes %d", count)
	}
	var name string
	check(t, s.db.QueryRow("SELECT name FROM users").Scan(&name))
	a, err := s.Login(name, password)
	check(t, err)
	var storedToken, storedPassword []byte
	check(t, s.db.QueryRow("SELECT token FROM sessions").Scan(&storedToken))
	check(t, s.db.QueryRow("SELECT password FROM users").Scan(&storedPassword))
	if bytes.Equal(storedToken, []byte(a.Token)) || bytes.Equal(storedPassword, []byte(password)) || a.Token == a.CSRF || a.CSRF == "" {
		t.Fatal("plaintext credentials or invalid CSRF")
	}
	got, err := s.Authenticate(a.Token)
	check(t, err)
	if got != a {
		t.Fatal("session mismatch")
	}
	check(t, s.ChangePassword(a.User.ID, password, password+"-new"))
	_, err = s.Authenticate(a.Token)
	wantErr(t, err, ErrAuth)
	_, err = s.Login(name, password)
	wantErr(t, err, ErrAuth)
	a, err = s.Login(name, password+"-new")
	check(t, err)
	check(t, s.Logout(a.Token))
	_, err = s.Authenticate(a.Token)
	wantErr(t, err, ErrAuth)
	a, err = s.Login(name, password+"-new")
	check(t, err)
	_, err = s.db.Exec("UPDATE sessions SET expires=1")
	check(t, err)
	_, err = s.Authenticate(a.Token)
	wantErr(t, err, ErrAuth)
}

func TestOwnershipEncryptionAndRevocation(t *testing.T) {
	s, path, _ := openTest(t)
	a, m := users(t, s)
	wantErr(t, s.CreateUser(m.User.ID, "another", password, "admin"), ErrForbidden)
	_, err := s.Users(m.User.ID)
	wantErr(t, err, ErrForbidden)
	wantErr(t, s.SetUserEnabled(m.User.ID, a.User.ID, false), ErrForbidden)
	wantErr(t, s.SetUserEnabled(a.User.ID, a.User.ID, false), ErrConflict)
	route := spec()
	id, err := s.CreateRoute(m.User.ID, route)
	check(t, err)
	var blob []byte
	check(t, s.db.QueryRow("SELECT spec FROM routes WHERE id=?", id).Scan(&blob))
	if bytes.Contains(blob, []byte(route.Target.Password)) || bytes.Contains(blob, []byte(route.Source.Username)) {
		t.Fatal("route stored as plaintext")
	}
	list, err := s.Routes(m.User.ID)
	check(t, err)
	if len(list) != 1 || list[0].Source.Password != "" || list[0].Target.Password != "" {
		t.Fatal("unsafe list")
	}
	foreign, err := s.Routes(a.User.ID)
	check(t, err)
	if len(foreign) > 0 {
		t.Fatal("foreign route listed")
	}
	_, err = s.OwnedJob(a.User.ID, id)
	wantErr(t, err, ErrNotFound)
	wantErr(t, s.UpdateRoute(a.User.ID, id, route), ErrNotFound)
	wantErr(t, s.Action(a.User.ID, id, "delete"), ErrNotFound)
	_, err = s.Logs(a.User.ID, id)
	wantErr(t, err, ErrNotFound)
	route.Target.Password = ""
	route.Source.Password = ""
	route.Name = "updated"
	check(t, s.UpdateRoute(m.User.ID, id, route))
	updated, err := s.Job(id)
	check(t, err)
	if updated.Name != "updated" || updated.Target.Password != spec().Target.Password {
		t.Fatal("lost password on update")
	}
	route.Target.Host = "another.example.test"
	wantErr(t, s.UpdateRoute(m.User.ID, id, route), ErrInvalid)
	check(t, s.Initialize(id, 102, 0))
	route = spec()
	route.SourceFolder = "Archive"
	wantErr(t, s.UpdateRoute(m.User.ID, id, route), ErrConflict)
	check(t, s.SetUserEnabled(a.User.ID, m.User.ID, false))
	_, err = s.Authenticate(m.Token)
	wantErr(t, err, ErrAuth)
	_, err = s.Job(id)
	wantErr(t, err, ErrNotFound)
	ready, err := s.Ready()
	check(t, err)
	if len(ready) != 0 {
		t.Fatal("disabled user scheduled")
	}
	_, err = s.OwnedJob(m.User.ID, id)
	wantErr(t, err, ErrNotFound)
	if other, err := Open(path, bytes.Repeat([]byte{9}, 32)); err == nil {
		other.Close()
		t.Fatal("wrong key accepted")
	}
}

func TestCursorAndCrashReconciliation(t *testing.T) {
	s, path, key := openTest(t)
	a, _ := users(t, s)
	id, err := s.CreateRoute(a.User.ID, spec())
	check(t, err)
	check(t, s.Initialize(id, 111, 3))
	wantErr(t, s.Initialize(id, 222, 0), ErrConflict)
	wantErr(t, s.Begin(id, 222, 7), ErrConflict)
	check(t, s.Begin(id, 111, 7))
	wantErr(t, s.Finish(id, 8, "delivered", ""), ErrConflict)
	check(t, s.Close())
	s, err = Open(path, key)
	check(t, err)
	defer s.Close()
	_, err = s.Job(id)
	wantErr(t, err, ErrNotFound)
	r, err := s.OwnedJob(a.User.ID, id)
	check(t, err)
	if r.State != "uncertain" || r.Inflight != 7 || r.Cursor != 3 {
		t.Fatalf("recovery state = %s inflight %d cursor %d", r.State, r.Inflight, r.Cursor)
	}
	check(t, s.Action(a.User.ID, id, "skip"))
	check(t, s.Begin(id, 111, 9))
	check(t, s.Fail(id, 9, "connection lost", true))
	check(t, s.Action(a.User.ID, id, "retry"))
	r, err = s.Job(id)
	check(t, err)
	if r.Cursor != 7 || r.Inflight != 0 {
		t.Fatal("wrong reconciliation cursor")
	}
	check(t, s.Begin(id, 111, 9))
	check(t, s.Finish(id, 9, "delivered", "ok"))
	check(t, s.Finish(id, 11, "skipped", "expunged"))
	check(t, s.Hold(id, 12, "oversized"))
	check(t, s.Action(a.User.ID, id, "skip"))
	r, err = s.Job(id)
	check(t, err)
	if r.Delivered != 1 || r.Cursor != 12 {
		t.Fatalf("count %d cursor %d", r.Delivered, r.Cursor)
	}
	check(t, s.Invalidate(id, "mailbox replaced"))
	wantErr(t, s.Action(a.User.ID, id, "retry"), ErrConflict)
	events, err := s.Logs(a.User.ID, id)
	check(t, err)
	if len(events) < 6 || events[0].Kind != "invalid" {
		t.Fatal("missing events")
	}
	check(t, s.Action(a.User.ID, id, "delete"))
	_, err = s.Job(id)
	wantErr(t, err, ErrNotFound)
	var dummy int
	wantErr(t, s.db.QueryRow("SELECT 1 FROM logs LIMIT 1").Scan(&dummy), sql.ErrNoRows)
}

func TestBackoffAndPause(t *testing.T) {
	s, _, _ := openTest(t)
	a, _ := users(t, s)
	id, err := s.CreateRoute(a.User.ID, spec())
	check(t, err)
	check(t, s.Initialize(id, 42, 0))
	for attempts := range 2 {
		check(t, s.Checked(id, false))
		check(t, s.Begin(id, 42, 1))
		check(t, s.Fail(id, 1, "temporarily unavailable", false))
		r, e := s.OwnedJob(a.User.ID, id)
		check(t, e)
		expect := 60 * time.Second * time.Duration(attempts+1)
		wait := time.Until(r.NextAttempt)
		if wait < expect-2*time.Second || wait > expect+time.Second {
			t.Fatalf("backoff %v expected %v", wait, expect)
		}
		_, e = s.Job(id)
		wantErr(t, e, ErrNotFound)
		_, e = s.db.Exec("UPDATE routes SET next_attempt=0 WHERE id=?", id)
		check(t, e)
	}
	check(t, s.Action(a.User.ID, id, "pause"))
	_, err = s.Job(id)
	wantErr(t, err, ErrNotFound)
	check(t, s.Action(a.User.ID, id, "resume"))
	_, err = s.Job(id)
	check(t, err)
}
