package mailwire

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

type popExchange struct{ command, reply string }

// Script one transaction at a time to exercise changing maildrops. An
// unexpected DELE, QUIT, or credential before STLS fails the test.
func popServer(t *testing.T, ln net.Listener, cfg *tls.Config, security string, script ...popExchange) func() {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			conn, err := ln.Accept()
			if err != nil {
				return err
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			if security == "tls" {
				conn = tls.Server(conn, cfg)
			}
			r := bufio.NewReader(conn)
			if _, err := io.WriteString(conn, "+OK ready\r\n"); err != nil {
				return err
			}
			if security == "starttls" {
				line, err := r.ReadString('\n')
				if err != nil || line != "STLS\r\n" {
					return errors.New("expected STLS before credentials")
				}
				if _, err := io.WriteString(conn, "+OK upgrade\r\n"); err != nil {
					return err
				}
				conn = tls.Server(conn, cfg)
				r.Reset(conn)
			}
			for _, step := range script {
				line, err := r.ReadString('\n')
				if err != nil || line != step.command+"\r\n" {
					verb, _, _ := strings.Cut(step.command, " ")
					return fmt.Errorf("unexpected client request at %s", verb)
				}
				if _, err = io.WriteString(conn, step.reply); err != nil {
					return err
				}
			}
			if _, err := r.ReadByte(); err == nil {
				return errors.New("unexpected command after transaction")
			}
			return nil
		}()
	}()
	return func() {
		t.Helper()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func popAuth(ep Endpoint, steps ...popExchange) []popExchange {
	return append([]popExchange{{"USER " + ep.Username, "+OK\r\n"}, {"PASS " + ep.Password, "+OK\r\n"}}, steps...)
}

func TestPOP3RenumberedMaildrop(t *testing.T) {
	for _, security := range []string{"tls", "starttls"} {
		t.Run(security, func(t *testing.T) {
			c, cfg, ln, ep := fixture(t)
			ep.Security = security
			done := popServer(t, ln, cfg, security, popAuth(ep,
				popExchange{"UIDL", "+OK\r\n2 Aa-:17!\r\n1 aa-:17!\r\n.\r\n"})...)
			ids, err := c.POPList(t.Context(), ep)
			done()
			if err != nil || !reflect.DeepEqual(ids, []string{"aa-:17!", "Aa-:17!"}) {
				t.Fatalf("UIDLs=%v err=%v", ids, err)
			}
			const headers = "Date: Sat, 01 Feb 2025 00:00:00 +0000\r\nSubject: test\r\n\r\n"
			// Include dot lines and MIME data across the reader's buffer boundary.
			long := strings.Repeat("X", 9000)
			raw := headers + ".\r\n..leading\r\n" + long + "\r\n"
			wire := headers + "..\r\n...leading\r\n" + long + "\r\n.\r\n"
			// The previous #1 was expunged and #2 became #1.
			done = popServer(t, ln, cfg, security, popAuth(ep,
				popExchange{"UIDL 2", "-ERR no such message\r\n"},
				popExchange{"UIDL", "+OK\r\n1 Aa-:17!\r\n.\r\n"},
				popExchange{"UIDL 1", "+OK 1 Aa-:17!\r\n"},
				popExchange{"LIST 1", fmt.Sprintf("+OK 1 %d\r\n", len(raw))},
				popExchange{"RETR 1", "+OK follows\r\n" + wire})...)
			msg, err := c.POPFetch(t.Context(), ep, ids[1], 2, int64(len(raw)))
			done()
			if err != nil || !bytes.Equal(msg.Raw, []byte(raw)) || msg.Date.Year() != 2025 || len(msg.Flags) != 0 {
				t.Fatalf("raw preserved=%t date=%v flags=%v err=%v", string(msg.Raw) == raw, msg.Date, msg.Flags, err)
			}
		})
	}
}

func TestPOP3UIDLValidation(t *testing.T) {
	for _, test := range []struct{ name, listing string }{
		{"unsupported", "-ERR command unavailable\r\n"},
		{"duplicate-id", "+OK\r\n1 Abc\r\n2 Abc\r\n.\r\n"},
		{"duplicate-number", "+OK\r\n2 foo\r\n2 bar\r\n.\r\n"},
		{"invalid-octet", "+OK\r\n1 foo bar\r\n.\r\n"},
		{"zero-number", "+OK\r\n0 a\r\n.\r\n"},
		{"oversized-id", "+OK\r\n1 " + strings.Repeat("x", 71) + "\r\n.\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, cfg, ln, ep := fixture(t)
			ep.Security = "tls"
			done := popServer(t, ln, cfg, ep.Security, popAuth(ep, popExchange{"UIDL", test.listing})...)
			ids, err := c.POPList(t.Context(), ep)
			done()
			if !errors.Is(err, ErrUIDL) || len(ids) != 0 {
				t.Fatalf("partial or invalid listing accepted: %v %v", ids, err)
			}
		})
	}
}

func TestPOP3FetchFailures(t *testing.T) {
	const uidl = "opaque"
	for _, test := range []struct {
		name  string
		steps []popExchange
		want  error
	}{
		{"vanished", []popExchange{{"UIDL", "+OK\r\n.\r\n"}}, ErrVanished},
		{"changed", []popExchange{{"UIDL", "+OK\r\n3 opaque\r\n.\r\n"}, {"UIDL 3", "+OK 3 changed\r\n"}}, ErrUIDL},
		{"oversized", []popExchange{{"UIDL", "+OK\r\n3 opaque\r\n.\r\n"}, {"UIDL 3", "+OK 3 opaque\r\n"}, {"LIST 3", "+OK 3 50000\r\n"}}, ErrTooLarge},
		{"understated-size", []popExchange{{"UIDL", "+OK\r\n3 opaque\r\n.\r\n"}, {"UIDL 3", "+OK 3 opaque\r\n"}, {"LIST 3", "+OK 3 1\r\n"}, {"RETR 3", "+OK\r\n" + strings.Repeat("x", 1025) + "\r\n.\r\n"}}, ErrTooLarge},
		{"short-terminated-message", []popExchange{{"UIDL", "+OK\r\n3 opaque\r\n.\r\n"}, {"UIDL 3", "+OK 3 opaque\r\n"}, {"LIST 3", "+OK 3 80\r\n"}, {"RETR 3", "+OK\r\nSubject: only the first line\r\n.\r\n"}}, ErrSource},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, cfg, ln, ep := fixture(t)
			ep.Security = "tls"
			done := popServer(t, ln, cfg, ep.Security, popAuth(ep, test.steps...)...)
			msg, err := c.POPFetch(t.Context(), ep, uidl, 0, 1024)
			done()
			if !errors.Is(err, test.want) || len(msg.Raw) != 0 {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestPOP3VerifyCurrentAndStaleHint(t *testing.T) {
	for _, test := range []struct {
		name   string
		number string
		prefix []popExchange
	}{
		{"current", "2", nil},
		{"reused-number", "1", []popExchange{
			{"UIDL 2", "+OK 2 different\r\n"},
			{"UIDL", "+OK\r\n1 requested\r\n2 different\r\n.\r\n"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, cfg, ln, ep := fixture(t)
			ep.Security = "tls"
			raw := "X-EmailRelay-Instance: " + c.marker() + "\r\n\r\nhello\r\n"
			steps := append(test.prefix, popExchange{"UIDL " + test.number, "+OK " + test.number + " requested\r\n"},
				popExchange{"LIST " + test.number, fmt.Sprintf("+OK %s %d\r\n", test.number, len(raw))},
				popExchange{"RETR " + test.number, "+OK\r\n" + raw + ".\r\n"})
			done := popServer(t, ln, cfg, ep.Security, popAuth(ep, steps...)...)
			msg, err := c.POPFetch(t.Context(), ep, "requested", 2, 1024)
			done()
			if err != ErrLoop || len(msg.Raw) != 0 {
				t.Fatalf("expected loop marker to stop relay, got %v", err)
			}
		})
	}
}

func TestPOP3TruncatedDataFailsClosed(t *testing.T) {
	// Never deliver a prefix as a complete message when the link drops.
	for _, content := range []string{"partial", "Subject: hello\r\n", "bad line\n.\r\n"} {
		p := popSession{r: bufio.NewReader(strings.NewReader(content))}
		msg, err := p.readMessage(1024)
		if err != ErrSource || len(msg) != 0 {
			t.Fatalf("incomplete POP3 message accepted: %v", err)
		}
	}
}

func TestPOP3RequiresTLSAndAuth(t *testing.T) {
	c, cfg, ln, ep := fixture(t)
	done := popServer(t, ln, cfg, "plain", popExchange{"STLS", "-ERR no TLS\r\n"})
	_, err := c.POPList(t.Context(), ep)
	done()
	if !errors.Is(err, ErrTLS) {
		t.Fatal(err)
	}

	ep.Security = "tls"
	done = popServer(t, ln, cfg, ep.Security, popExchange{"USER " + ep.Username, "+OK\r\n"},
		popExchange{"PASS " + ep.Password, "-ERR account-private-detail\r\n"})
	_, err = c.POPList(t.Context(), ep)
	done()
	if err != ErrAuth {
		t.Fatalf("expected sanitized authentication failure, got %v", err)
	}

	c.roots = x509.NewCertPool()
	protocol := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			err = tls.Server(conn, cfg).Handshake()
		}
		protocol <- err
	}()
	_, err = c.POPList(t.Context(), ep)
	if err != ErrTLS || <-protocol == nil {
		t.Fatal("untrusted POP3 certificate accepted")
	}
}

func TestGmailPOPRecentMode(t *testing.T) {
	for _, test := range []struct{ host, user, want string }{
		{"POP.GMAIL.COM.", "Name@example.net", "recent:Name@example.net"},
		{"pop.gmail.com", "recent:Name@example.net", "recent:Name@example.net"},
		{"pop.mail.yahoo.com", "Name@yahoo.com", "Name@yahoo.com"},
		{"pop.gmail.com.example.net", "Name@gmail.com", "Name@gmail.com"},
	} {
		if got := popUsername(Endpoint{Host: test.host, Username: test.user}); got != test.want {
			t.Errorf("popUsername(%q, %q)=%q", test.host, test.user, got)
		}
	}
}
