// Package integration exercises the public HTTP API and real TLS mail protocols
// using isolated local mailboxes. Its test process has its own trusted CA file.
package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"emailrelay/internal/httpapi"
	"emailrelay/internal/mailwire"
	"emailrelay/internal/relay"
	"emailrelay/internal/store"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const mailPassword = "synthetic-mail-password"
const sourceName = "source@example.test"
const targetName = "archive@example.test"

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	fixture := httptest.NewTLSServer(nil)
	cert := fixture.TLS.Certificates[0]
	fixture.Close()
	path := filepath.Join(t.TempDir(), "loopback.pem")
	check(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600))
	t.Setenv("SSL_CERT_FILE", path)
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

func serveIMAP(t *testing.T, cfg *tls.Config) (int, *imapmemserver.User, *imapmemserver.User) {
	t.Helper()
	mem := imapmemserver.New()
	source := imapmemserver.NewUser(sourceName, mailPassword)
	check(t, source.Create("INBOX", nil))
	mem.AddUser(source)
	target := imapmemserver.NewUser(targetName, mailPassword)
	check(t, target.Create("Archive", nil))
	mem.AddUser(target)
	server := imapserver.New(&imapserver.Options{NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return mem.NewSession(), nil, nil
	}, TLSConfig: cfg, Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	check(t, err)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close(); ln.Close(); <-done })
	return ln.Addr().(*net.TCPAddr).Port, source, target
}

func testMessage(name string) []byte {
	return []byte("From: Sender <sender@example.test>\r\nTo: reader@example.test\r\nSubject: " + name + "\r\nMessage-ID: <" + name + "@example.test>\r\n\r\n" + name + " body\r\n")
}

type api struct {
	address, csrf string
	client        *http.Client
}

func (a *api) call(t *testing.T, method, path string, body any, code int, out any) {
	t.Helper()
	data, err := json.Marshal(body)
	check(t, err)
	req, err := http.NewRequest(method, a.address+path, bytes.NewReader(data))
	check(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", a.address)
	req.Header.Set("X-CSRF-Token", a.csrf)
	res, err := a.client.Do(req)
	check(t, err)
	defer res.Body.Close()
	if res.StatusCode != code {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, msg)
	}
	if out != nil {
		check(t, json.NewDecoder(res.Body).Decode(out))
	}
}

func endpoint(port int, username string) map[string]any {
	return map[string]any{"host": "127.0.0.1", "port": port, "username": username, "password": mailPassword, "security": "tls"}
}

type summary struct {
	Name, Status, Detail string
	Delivered            int
	Initialized          bool
	Checked              string `json:"last_checked"`
}

func (a *api) waitRoutes(t *testing.T, expected map[string]int, checkedAfter time.Time) time.Time {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var state []summary
	for time.Now().Before(deadline) {
		var reply struct{ Routes []summary }
		a.call(t, "GET", "/api/routes", nil, 200, &reply)
		state = reply.Routes
		ok := len(reply.Routes) == len(expected)
		var latest time.Time
		for _, r := range reply.Routes {
			if r.Status != "ready" {
				t.Fatalf("route %s entered %s: %s", r.Name, r.Status, r.Detail)
			}
			count, exists := expected[r.Name]
			if !exists || !r.Initialized || r.Checked == "" || count != r.Delivered {
				ok = false
				continue
			}
			checked, err := time.Parse(time.RFC3339, r.Checked)
			check(t, err)
			if !checked.After(checkedAfter) {
				ok = false
			}
			if checked.After(latest) {
				latest = checked
			}
		}
		if ok {
			return latest
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("routes did not reach %v; last state %#v", expected, state)
	return time.Time{}
}

func TestWebToIMAPAndSMTP(t *testing.T) {
	for _, protocol := range []string{"imap", "pop3"} {
		t.Run(protocol, func(t *testing.T) { testWebRelay(t, protocol) })
	}
}

func testWebRelay(t *testing.T, protocol string) {
	t.Helper()
	cfg := testTLS(t)
	imapPort, source, target := serveIMAP(t, cfg)
	smtpPort, inbox := serveSMTP(t, cfg)
	sourcePort := imapPort
	add := func(name string, raw []byte) {
		_, err := source.Append("INBOX", bytes.NewReader(raw), &imap.AppendOptions{Time: time.Now().UTC()})
		check(t, err)
	}
	if protocol == "pop3" {
		sourcePort, add = servePOP3(t, cfg)
	}
	add("old", testMessage("old"))
	key := bytes.Repeat([]byte{0xf2}, 32)
	db, err := store.Open(filepath.Join(t.TempDir(), "relay.sqlite"), key)
	check(t, err)
	t.Cleanup(func() { db.Close() })
	wire, err := mailwire.NewClient(key, "127.0.0.1/32")
	check(t, err)
	engine := relay.New(db, wire, 100*time.Millisecond, 1<<20)
	web := httptest.NewUnstartedServer(nil)
	origin, err := url.Parse("http://" + web.Listener.Addr().String())
	check(t, err)
	web.Config.Handler = httpapi.New(db, engine, origin, "test-setup-token", nil, false)
	web.Start()
	t.Cleanup(web.Close)
	jar, err := cookiejar.New(nil)
	check(t, err)
	// Account setup and login run bcrypt. Race builds on shared runners
	// need more time for these local HTTP requests.
	api := api{address: web.URL, client: &http.Client{Jar: jar, Timeout: 15 * time.Second}}
	var session struct{ CSRF string }
	api.call(t, "POST", "/api/setup", map[string]string{"token": "test-setup-token", "username": "owner", "password": "fixture-application-password"}, 200, &session)
	api.csrf = session.CSRF
	for _, kind := range []string{"imap", "smtp"} {
		route := map[string]any{"name": kind, "source": endpoint(sourcePort, sourceName), "method": kind}
		if protocol == "imap" {
			route["source_folder"] = "INBOX" // Older clients omit source_protocol.
		} else {
			route["source_protocol"] = protocol
		}
		if kind == "imap" {
			route["initial"] = "all"
			route["target"] = endpoint(imapPort, targetName)
			route["target_folder"] = "Archive"
		} else {
			route["initial"] = "new"
			route["target"] = endpoint(smtpPort, targetName)
			route["sender"] = targetName
			route["recipient"] = "reader@example.test"
		}
		var created struct{ ID int64 }
		api.call(t, "POST", "/api/routes", route, 201, &created)
		if created.ID == 0 {
			t.Fatalf("no ID for %s route", kind)
		}
		api.call(t, "POST", fmt.Sprintf("/api/routes/%d/test", created.ID), struct{}{}, 200, nil)
	}
	before, err := target.Status("Archive", &imap.StatusOptions{NumMessages: true})
	check(t, err)
	if before.NumMessages == nil || *before.NumMessages != 0 || len(inbox.snapshot()) != 0 {
		t.Fatal("connection check delivered mail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); engine.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	api.waitRoutes(t, map[string]int{"imap": 1, "smtp": 0}, time.Time{})
	raw := testMessage("new")
	add("new", raw)
	engine.Wake()
	totals := map[string]int{"imap": 2, "smtp": 1}
	checked := api.waitRoutes(t, totals, time.Time{})
	api.waitRoutes(t, totals, checked) // Observe a later completed poll on both routes.
	cancel()
	<-done
	status, err := target.Status("Archive", &imap.StatusOptions{NumMessages: true})
	check(t, err)
	if status.NumMessages == nil || *status.NumMessages != 2 {
		t.Fatalf("unexpected destination status %+v", status)
	}
	messages := inbox.snapshot()
	if len(messages) != 1 {
		t.Fatalf("SMTP accepted %d messages, expected just new arrival", len(messages))
	}
	if !bytes.Equal(originalAttachment(t, messages[0]), raw) {
		t.Fatal("SMTP did not preserve the new source message as an attachment")
	}
	if protocol == "imap" {
		src, err := source.Status("INBOX", &imap.StatusOptions{NumMessages: true, NumUnseen: true})
		check(t, err)
		if src.NumMessages == nil || *src.NumMessages != 2 || src.NumUnseen == nil || *src.NumUnseen != 2 {
			t.Fatalf("source messages were changed: %+v", src)
		}
	}
}
