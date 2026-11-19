package mailwire

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/mail"
	"slices"
	"strconv"
	"strings"
)

var ErrUIDL = errors.New("source requires a valid POP3 UIDL listing")
var errPOPRejected = errors.New("POP3 server rejected command")

const maxPOPEntries = 100_000

type popSession struct {
	conn net.Conn
	r    *bufio.Reader
}

// Control lines, including UIDL rows, are bounded by the reader's buffer.
// Message data uses a separate streaming path so long MIME lines are preserved.
func (p *popSession) line() (string, error) {
	b, err := p.r.ReadSlice('\n')
	if err != nil || !bytes.HasSuffix(b, []byte("\r\n")) {
		return "", ErrSource
	}
	return string(b[:len(b)-2]), nil
}

func (p *popSession) response() (string, error) {
	line, err := p.line()
	if err != nil {
		return "", err
	}
	status, message, _ := strings.Cut(line, " ")
	switch status {
	case "+OK":
		return message, nil
	case "-ERR":
		return "", errPOPRejected
	default:
		return "", ErrSource
	}
}

func (p *popSession) command(cmd string) (string, error) {
	if _, err := io.WriteString(p.conn, cmd+"\r\n"); err != nil {
		return "", ErrConnect
	}
	return p.response()
}

func (c *Client) pop(ctx context.Context, ep Endpoint) (*popSession, func(), error) {
	conn, cfg, cleanup, err := c.dial(ctx, ep)
	if err != nil {
		return nil, nil, err
	}
	p := &popSession{conn: conn, r: bufio.NewReader(conn)}
	if _, err := p.response(); err != nil {
		cleanup()
		return nil, nil, ErrConnect
	}
	if ep.Security == "starttls" {
		if _, err := p.command("STLS"); err != nil || p.r.Buffered() != 0 {
			cleanup()
			return nil, nil, ErrTLS
		}
		secure := tls.Client(conn, cfg)
		if err := secure.HandshakeContext(ctx); err != nil {
			cleanup()
			return nil, nil, ErrTLS
		}
		p.conn = secure
		p.r.Reset(secure)
	}
	for _, cmd := range []string{"USER " + popUsername(ep), "PASS " + ep.Password} {
		if _, err := p.command(cmd); err != nil {
			cleanup()
			if errors.Is(err, errPOPRejected) {
				return nil, nil, ErrAuth
			}
			return nil, nil, ErrConnect
		}
	}
	// Disconnect without entering POP3's UPDATE state. Some servers apply
	// a download-and-delete policy to RETR on QUIT (RFC 1939 section 8).
	return p, cleanup, nil
}

func popUsername(ep Endpoint) string {
	// Gmail's recent mode presents a repeatable 30-day maildrop to POP
	// clients. Its normal download-once mode advances only on RETR + QUIT.
	host := strings.TrimSuffix(ep.Host, ".")
	if strings.EqualFold(host, "pop.gmail.com") && !strings.HasPrefix(strings.ToLower(ep.Username), "recent:") {
		return "recent:" + ep.Username
	}
	return ep.Username
}

type popEntry struct {
	number uint32
	id     string
}

func validUIDL(id string) bool {
	if len(id) == 0 || len(id) > 70 {
		return false
	}
	for i := range len(id) {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func parseUIDL(line string) (popEntry, error) {
	number, id, _ := strings.Cut(line, " ")
	n, err := strconv.ParseUint(number, 10, 32)
	if err != nil || n == 0 || !validUIDL(id) {
		return popEntry{}, ErrUIDL
	}
	return popEntry{number: uint32(n), id: id}, nil
}

func (p *popSession) entries() ([]popEntry, error) {
	if _, err := p.command("UIDL"); err != nil {
		if errors.Is(err, errPOPRejected) {
			return nil, ErrUIDL
		}
		return nil, err
	}
	var entries []popEntry
	seen := make(map[string]struct{})
	for {
		row, err := p.line()
		if err != nil {
			return nil, ErrSource
		}
		if row == "." {
			break
		}
		if len(entries) >= maxPOPEntries {
			return nil, ErrUIDL
		}
		entry, err := parseUIDL(row)
		if err != nil {
			return nil, err
		}
		if _, found := seen[entry.id]; found {
			return nil, ErrUIDL
		}
		seen[entry.id] = struct{}{}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b popEntry) int {
		return cmp.Compare(a.number, b.number)
	})
	for i := 1; i < len(entries); i++ {
		if entries[i-1].number == entries[i].number {
			return nil, ErrUIDL
		}
	}
	return entries, nil
}

// POPList returns opaque UIDLs in current maildrop order. Message numbers
// are session-local and must never be persisted as delivery identifiers.
func (c *Client) POPList(ctx context.Context, ep Endpoint) ([]string, error) {
	p, closeSession, err := c.pop(ctx, ep)
	if err != nil {
		return nil, err
	}
	defer closeSession()
	entries, err := p.entries()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.id
	}
	return ids, nil
}

func (p *popSession) readMessage(maxBytes int64) ([]byte, error) {
	var out bytes.Buffer
	startOfLine := true
	for {
		part, err := p.r.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull {
			return nil, ErrSource
		}
		if startOfLine && part[0] == '.' {
			if bytes.Equal(part, []byte(".\r\n")) {
				return out.Bytes(), nil
			}
			part = part[1:]
		}
		if int64(len(part)) > maxBytes-int64(out.Len()) {
			return nil, ErrTooLarge
		}
		out.Write(part)
		startOfLine = err == nil
		if startOfLine && !bytes.HasSuffix(out.Bytes(), []byte("\r\n")) {
			return nil, ErrSource
		}
	}
}

func (p *popSession) verify(entry popEntry) (bool, error) {
	line, err := p.command("UIDL " + strconv.FormatUint(uint64(entry.number), 10))
	if errors.Is(err, errPOPRejected) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	actual, err := parseUIDL(line)
	return actual == entry, err
}

func (p *popSession) number(uidl string, hint uint32) (uint32, error) {
	if hint != 0 {
		matched, err := p.verify(popEntry{number: hint, id: uidl})
		if err != nil || matched {
			return hint, err
		}
	}
	entries, err := p.entries()
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(entries, func(e popEntry) bool { return e.id == uidl })
	if i < 0 {
		return 0, ErrVanished
	}
	matched, err := p.verify(entries[i])
	if err != nil {
		return 0, err
	}
	if !matched {
		return 0, ErrUIDL
	}
	return entries[i].number, nil
}

// POPFetch uses a message number only after checking the requested UIDL in
// this authenticated transaction. A stale hint triggers a fresh UIDL listing.
func (c *Client) POPFetch(ctx context.Context, ep Endpoint, uidl string, hint uint32, maxBytes int64) (Message, error) {
	if !validUIDL(uidl) || maxBytes <= 0 || maxBytes >= 1<<32 {
		return Message{}, ErrEndpoint
	}
	p, closeSession, err := c.pop(ctx, ep)
	if err != nil {
		return Message{}, err
	}
	defer closeSession()
	n, err := p.number(uidl, hint)
	if err != nil {
		return Message{}, err
	}
	number := strconv.FormatUint(uint64(n), 10)
	listing, err := p.command("LIST " + number)
	if err != nil {
		return Message{}, ErrSource
	}
	fields := strings.Fields(listing)
	if len(fields) < 2 || fields[0] != number {
		return Message{}, ErrSource
	}
	size, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return Message{}, ErrSource
	}
	if size > uint64(maxBytes) {
		return Message{}, ErrTooLarge
	}
	if _, err = p.command("RETR " + number); err != nil {
		return Message{}, ErrSource
	}
	raw, err := p.readMessage(maxBytes)
	if err != nil {
		return Message{}, err
	}
	// LIST counts the original octets, excluding extra dot-stuffing and
	// the terminator. A short but terminated RETR is still incomplete.
	if uint64(len(raw)) != size {
		return Message{}, ErrSource
	}
	if c.hasMarker(raw) {
		return Message{}, ErrLoop
	}
	msg := Message{Raw: raw}
	// POP3 has neither mailbox flags nor IMAP INTERNALDATE.
	if m, err := mail.ReadMessage(io.LimitReader(bytes.NewReader(raw), 256<<10)); err == nil {
		msg.Date, _ = m.Header.Date()
	}
	return msg, nil
}
