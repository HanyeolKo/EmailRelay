package integration

import (
	"crypto/tls"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type popMessage struct {
	id  string
	raw []byte
}

func servePOP3(t *testing.T, cfg *tls.Config) (int, func(string, []byte)) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	check(t, err)
	var mu sync.Mutex
	var messages []popMessage
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			snapshot := append([]popMessage(nil), messages...)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				servePOPSession(t, raw, snapshot)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	return ln.Addr().(*net.TCPAddr).Port, func(id string, raw []byte) {
		mu.Lock()
		defer mu.Unlock()
		// Deliberately renumber the existing messages between sessions.
		messages = append([]popMessage{{id, raw}}, messages...)
	}
}

func servePOPSession(t *testing.T, raw net.Conn, snapshot []popMessage) {
	_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
	conn := textproto.NewConn(raw)
	defer conn.Close()
	conn.PrintfLine("+OK")
	for _, expected := range []string{"USER " + sourceName, "PASS " + mailPassword} {
		line, err := conn.ReadLine()
		if err != nil || line != expected {
			conn.PrintfLine("-ERR")
			return
		}
		conn.PrintfLine("+OK")
	}
	for {
		line, err := conn.ReadLine()
		if err != nil {
			return
		}
		if line == "UIDL" {
			conn.PrintfLine("+OK")
			for i, msg := range snapshot {
				conn.PrintfLine("%d %s", i+1, msg.id)
			}
			conn.PrintfLine(".")
			continue
		}
		command, arg, _ := strings.Cut(line, " ")
		if command == "DELE" {
			t.Error("relay attempted to delete source mail")
			return
		}
		index, err := strconv.Atoi(arg)
		if err != nil || index < 1 || index > len(snapshot) {
			conn.PrintfLine("-ERR")
			continue
		}
		msg := snapshot[index-1]
		switch command {
		case "UIDL":
			conn.PrintfLine("+OK %d %s", index, msg.id)
		case "LIST":
			conn.PrintfLine("+OK %d %d", index, len(msg.raw))
		case "RETR":
			conn.PrintfLine("+OK")
			data := conn.DotWriter()
			_, _ = data.Write(msg.raw)
			_ = data.Close()
		default:
			conn.PrintfLine("-ERR")
		}
	}
}
