package integration

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

type mailbox struct {
	mu       sync.Mutex
	messages [][]byte
}

func (m *mailbox) snapshot() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.messages...)
}

func serveSMTP(t *testing.T, cfg *tls.Config) (int, *mailbox) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	check(t, err)
	var inbox mailbox
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); inbox.receive(conn) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	return ln.Addr().(*net.TCPAddr).Port, &inbox
}

func (m *mailbox) receive(raw net.Conn) {
	_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
	conn := textproto.NewConn(raw)
	defer conn.Close()
	if conn.PrintfLine("220 relay-test ESMTP") != nil {
		return
	}
	expected := base64.StdEncoding.EncodeToString([]byte("\x00" + targetName + "\x00" + mailPassword))
	authenticated, sender, recipient := false, false, false
	for {
		line, err := conn.ReadLine()
		if err != nil {
			return
		}
		switch {
		case strings.HasPrefix(line, "EHLO "):
			conn.PrintfLine("250-relay-test")
			conn.PrintfLine("250 AUTH PLAIN")
		case line == "AUTH PLAIN "+expected:
			authenticated = true
			conn.PrintfLine("235 OK")
		case authenticated && line == "MAIL FROM:<"+targetName+">":
			sender = true
			conn.PrintfLine("250 sender accepted")
		case sender && line == "RCPT TO:<reader@example.test>":
			recipient = true
			conn.PrintfLine("250 recipient accepted")
		case recipient && line == "DATA":
			conn.PrintfLine("354 end with dot")
			message, err := conn.ReadDotBytes()
			if err != nil {
				return
			}
			m.mu.Lock()
			m.messages = append(m.messages, message)
			m.mu.Unlock()
			conn.PrintfLine("250 accepted")
			sender, recipient = false, false
		case line == "QUIT":
			conn.PrintfLine("221 bye")
			return
		default:
			conn.PrintfLine("550 unexpected command")
		}
	}
}

func originalAttachment(t *testing.T, data []byte) []byte {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(data))
	check(t, err)
	kind, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	check(t, err)
	if kind != "multipart/mixed" || m.Header.Get("Subject") != "Fwd: new" {
		t.Fatalf("unexpected forwarded headers %v", m.Header)
	}
	parts := multipart.NewReader(m.Body, params["boundary"])
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			t.Fatal("missing original.eml attachment")
		}
		check(t, err)
		if part.FileName() == "original.eml" {
			body, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
			check(t, err)
			return body
		}
	}
}
