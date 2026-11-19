package mailwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http/httptest"
	"net/mail"
	"net/netip"
	"net/textproto"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

var original = []byte("From: sender@example.org\r\nTo: old@example.org\r\nSubject: hello\r\nMessage-ID: <sample@example.org>\r\n\r\n.\r\nOriginal body.\r\n")

type ackLossSession struct {
	imapserver.Session
	conn      *imapserver.Conn
	nextFetch *atomic.Int32
}

func (s *ackLossSession) Append(folder string, msg imap.LiteralReader, opts *imap.AppendOptions) (*imap.AppendData, error) {
	data, err := s.Session.Append(folder, msg, opts)
	if folder == "LostAck" && err == nil {
		s.conn.NetConn().Close()
	}
	return data, err
}

func (s *ackLossSession) Fetch(w *imapserver.FetchWriter, set imap.NumSet, opts *imap.FetchOptions) error {
	switch mode := s.nextFetch.Swap(0); mode {
	case 1, 2:
		row := w.CreateMessage(2)
		body := row.WriteBodySection(opts.BodySection[0], int64(len(original)))
		if _, err := body.Write(original); err != nil {
			return err
		}
		if err := body.Close(); err != nil {
			return err
		}
		uid := imap.UID(2)
		if mode == 2 {
			uid++ // An expunge shifted the sequence after SEARCH.
		}
		row.WriteUID(uid)
		row.WriteRFC822Size(int64(len(original)))
		return row.Close()
	case 3:
		return nil // The UID was found in SEARCH but FETCH returned no row.
	}
	return s.Session.Fetch(w, set, opts)
}

func fixture(t *testing.T) (*Client, *tls.Config, net.Listener, Endpoint) {
	t.Helper()
	certServer := httptest.NewTLSServer(nil)
	cfg := &tls.Config{Certificates: certServer.TLS.Certificates, MinVersion: tls.VersionTLS12}
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	certServer.Close()
	c, err := NewClient([]byte("test-key"), "127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	c.roots = roots
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return c, cfg, ln, Endpoint{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Security: "starttls",
		Username: "user", Password: "local-test",
	}
}

func TestIMAPCopy(t *testing.T) {
	c, cfg, ln, ep := fixture(t)
	store := imapmemserver.New()
	u := imapmemserver.NewUser(ep.Username, ep.Password)
	store.AddUser(u)
	for _, folder := range []string{"INBOX", "Target", "LostAck"} {
		if err := u.Create(folder, nil); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Unix(1738352400, 0).UTC()
	for range 3 {
		_, err := u.Append("INBOX", bytes.NewReader(original), &imap.AppendOptions{Time: stamp, Flags: []imap.Flag{imap.FlagFlagged}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var nextFetch atomic.Int32
	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &ackLossSession{store.NewSession(), conn, &nextFetch}, nil, nil
		},
		TLSConfig: cfg, Logger: log.New(io.Discard, "", 0),
	})
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	v, high, ids, err := c.Discover(ctx, ep, "INBOX", 0, 0)
	if err != nil || high != 3 || len(ids) != 0 || v == 0 {
		t.Fatalf("baseline = %d %d %v %v", v, high, ids, err)
	}
	_, _, ids, err = c.Discover(ctx, ep, "INBOX", 1, 1)
	if err != nil || !slices.Equal(ids, []uint32{2}) {
		t.Fatalf("page = %v %v", ids, err)
	}
	for _, test := range []struct {
		validity, uid uint32
		max           int64
		want          error
	}{{v, 2, 1, ErrTooLarge}, {v + 1, 1, 4096, ErrUIDValidity}, {v, 10, 4096, ErrVanished}} {
		if _, err := c.Fetch(ctx, ep, "INBOX", test.validity, test.uid, test.max); !errors.Is(err, test.want) {
			t.Errorf("Fetch(%d, %d, %d): %v, want %v", test.validity, test.uid, test.max, err, test.want)
		}
	}
	nextFetch.Store(1)
	late, err := c.Fetch(ctx, ep, "INBOX", v, 2, 4096)
	if err != nil || !bytes.Equal(late.Raw, original) {
		t.Fatalf("body before UID: %v", err)
	}
	for _, mode := range []int32{2, 3} {
		nextFetch.Store(mode)
		if _, err := c.Fetch(ctx, ep, "INBOX", v, 2, 1); !errors.Is(err, ErrSource) {
			t.Fatalf("FETCH changed since SEARCH (mode %d): %v", mode, err)
		}
	}
	msg, err := c.Fetch(ctx, ep, "INBOX", v, 2, 4096)
	if err != nil || !bytes.Equal(msg.Raw, original) || !msg.Date.Equal(stamp) || !slices.Contains(msg.Flags, string(imap.FlagFlagged)) || slices.Contains(msg.Flags, string(imap.FlagSeen)) {
		t.Fatalf("source = %+v, %v", msg, err)
	}
	if err := c.TestTarget(ctx, ep, "imap", "Target"); err != nil {
		t.Fatal(err)
	}
	if err := c.Deliver(ctx, ep, "imap", "Missing", "", "", msg); !errors.Is(err, ErrDestination) {
		t.Fatalf("missing destination = %v", err)
	}
	if err := c.Deliver(ctx, ep, "imap", "LostAck", "", "", msg); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("persisted without acknowledgement = %v", err)
	}
	if _, high, _, err := c.Discover(ctx, ep, "LostAck", 0, 0); err != nil || high != 1 {
		t.Fatalf("target stored uncertain mail: highest %d, error %v", high, err)
	}
	if err := c.Deliver(ctx, ep, "imap", "Target", "", "", msg); err != nil {
		t.Fatal(err)
	}
	tv, high, ids, err := c.Discover(ctx, ep, "Target", 0, 10)
	if err != nil || high != 1 || !slices.Equal(ids, []uint32{1}) {
		t.Fatalf("target = %v %v %v", high, ids, err)
	}
	if _, err := c.Fetch(ctx, ep, "Target", tv, 1, 4096); !errors.Is(err, ErrLoop) {
		t.Fatalf("loop = %v", err)
	}
	other, _ := NewClient([]byte("another-instance"), "127.0.0.1/32")
	other.roots = c.roots
	copy, err := other.Fetch(ctx, ep, "Target", tv, 1, 4096)
	_, body, _ := bytes.Cut(copy.Raw, []byte("\r\n"))
	if err != nil || !copy.Date.Equal(stamp) || !slices.Equal(copy.Flags, msg.Flags) || !bytes.Equal(body, original) {
		t.Fatalf("copied flags, date or bytes mismatch: %v", err)
	}
}

func TestNetworkPolicy(t *testing.T) {
	c, err := NewClient(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"::1", "169.254.169.254", "::ffff:127.0.0.1", "10.0.0.1", "100.100.100.200", "64:ff9b::7f00:1", "fc00::1", "192.168.0.1", "168.63.129.16", "fe80::1", "2002:7f00:1::1"} {
		if c.allowed(netip.MustParseAddr(ip)) {
			t.Errorf("allowed %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !c.allowed(netip.MustParseAddr(ip)) {
			t.Errorf("blocked %s", ip)
		}
	}
	if _, err := NewClient(nil, "invalid-cidr"); err == nil {
		t.Fatal("accepted invalid CIDR")
	}
	ep := Endpoint{Host: "127.0.0.1", Port: 995, Security: "tls", Username: "u", Password: "p"}
	if err := c.TestTarget(t.Context(), ep, "smtp", ""); !errors.Is(err, ErrNetworkPolicy) {
		t.Fatalf("dialed private address: %v", err)
	}
}

func TestSMTPStartTLSProbe(t *testing.T) {
	for _, mode := range []string{"success", "no-upgrade", "untrusted-certificate"} {
		t.Run(mode, func(t *testing.T) {
			c, cfg, ln, ep := fixture(t)
			if mode == "untrusted-certificate" {
				c.roots = x509.NewCertPool()
			}
			finish := make(chan error, 1)
			go func() {
				finish <- func() error {
					conn, err := ln.Accept()
					if err != nil {
						return err
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(5 * time.Second))
					peer := textproto.NewConn(conn)
					peer.PrintfLine("220 mock SMTP")
					if hello, err := peer.ReadLine(); err != nil || !strings.HasPrefix(hello, "EHLO ") {
						return errors.New("expected EHLO")
					}
					if mode == "no-upgrade" {
						peer.PrintfLine("250-localhost\r\n250 AUTH LOGIN")
						if _, err := peer.ReadLine(); err == nil {
							return errors.New("client sent a command after TLS downgrade")
						}
						return nil
					}
					peer.PrintfLine("250-localhost\r\n250 STARTTLS")
					if upgrade, _ := peer.ReadLine(); upgrade != "STARTTLS" {
						return errors.New("expected TLS before credentials")
					}
					peer.PrintfLine("220 start TLS now")
					secure := tls.Server(conn, cfg)
					err = secure.Handshake()
					if mode == "untrusted-certificate" {
						if err == nil {
							return errors.New("client trusted unexpected certificate")
						}
						return nil
					}
					if err != nil {
						return err
					}
					peer = textproto.NewConn(secure)
					for _, step := range []struct{ prefix, reply string }{
						{"EHLO ", "250-localhost\r\n250 AUTH LOGIN"},
						{"AUTH LOGIN", "334 VXNlcm5hbWU6"},
						{base64.StdEncoding.EncodeToString([]byte(ep.Username)), "334 UGFzc3dvcmQ6"},
						{base64.StdEncoding.EncodeToString([]byte(ep.Password)), "235 success"},
					} {
						cmd, err := peer.ReadLine()
						if err != nil || !strings.HasPrefix(cmd, step.prefix) {
							return errors.New("unexpected command in AUTH LOGIN exchange")
						}
						peer.PrintfLine("%s", step.reply)
					}
					if _, err := peer.ReadLine(); err == nil {
						return errors.New("probe continued after login")
					}
					return nil
				}()
			}()
			err := c.TestTarget(t.Context(), ep, "smtp", "")
			if serverErr := <-finish; serverErr != nil {
				t.Fatal(serverErr)
			}
			if mode == "success" && err != nil || mode != "success" && !errors.Is(err, ErrTLS) {
				t.Fatalf("unexpected probe result: %v", err)
			}
		})
	}
}

// A submission server can accept DATA and disappear before the final 250.
// Exercise both that path and a definite rejection over a real TLS connection.
func TestSMTPAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		reply string
		want  error
	}{{"250 queued", nil}, {"550 reject: sensitive provider information", ErrDestination}, {"", ErrAmbiguous}} {
		t.Run(test.reply, func(t *testing.T) {
			c, cfg, ln, ep := fixture(t)
			ep.Security = "tls"
			data := make(chan []byte, 1)
			finish := make(chan error, 1)
			go func() {
				conn, err := tls.NewListener(ln, cfg).Accept()
				if err != nil {
					finish <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				peer := textproto.NewConn(conn)
				peer.PrintfLine("220 mock ESMTP")
				for _, step := range []struct{ prefix, reply string }{
					{"EHLO ", "250-localhost\r\n250 AUTH PLAIN"},
					{"AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00user\x00local-test")), "235 OK"},
					{"MAIL FROM:<relay@example.org>", "250 ok"},
					{"RCPT TO:<target@example.org>", "250 ok"},
					{"DATA", "354 go"},
				} {
					s, err := peer.ReadLine()
					if err != nil || !strings.HasPrefix(s, step.prefix) {
						finish <- fmt.Errorf("unexpected SMTP command for %s: %v", strings.Fields(step.prefix)[0], err)
						return
					}
					peer.PrintfLine("%s", step.reply)
				}
				payload, err := peer.ReadDotBytes()
				if err == nil {
					data <- payload
					if test.reply != "" {
						err = peer.PrintfLine("%s", test.reply)
					}
				}
				finish <- err
			}()
			err := c.Deliver(t.Context(), ep, "smtp", "", "relay@example.org", "target@example.org", Message{Raw: original})
			if serverErr := <-finish; serverErr != nil {
				t.Fatal(serverErr)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("delivery = %v, want %v", err, test.want)
			}
			outer, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(<-data)))
			if err != nil || outer.Header.Get(markerHeader) != c.marker() || outer.Header.Get("Subject") != "Fwd: hello" {
				t.Fatalf("forward headers: %v", err)
			}
			_, params, err := mime.ParseMediaType(outer.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			parts := multipart.NewReader(outer.Body, params["boundary"])
			if _, err := parts.NextPart(); err != nil {
				t.Fatal(err)
			}
			attachment, err := parts.NextPart()
			if err != nil || attachment.FileName() != "original.eml" {
				t.Fatalf("attachment: %v", err)
			}
			restored, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, attachment))
			if err != nil || !bytes.Equal(restored, original) {
				t.Fatal("forward lost original bytes", err)
			}
		})
	}
}
