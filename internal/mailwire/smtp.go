package mailwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
	"unicode"
)

// Some submission servers advertise LOGIN only. This exchange is reachable
// only after TLS has succeeded and the server certificate has been verified.
type loginAuth struct {
	username, password string
	step               int
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, ErrTLS
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	default:
		return nil, ErrAuth
	}
}

func (c *Client) smtp(ctx context.Context, ep Endpoint) (*smtp.Client, func(), error) {
	conn, cfg, cleanup, err := c.dial(ctx, ep)
	if err != nil {
		return nil, nil, err
	}
	client, err := smtp.NewClient(conn, ep.Host)
	if err != nil {
		cleanup()
		return nil, nil, ErrConnect
	}
	if ep.Security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok || client.StartTLS(cfg) != nil {
			cleanup()
			return nil, nil, ErrTLS
		}
	}
	_, mechanisms := client.Extension("AUTH")
	var auth smtp.Auth
	for _, mechanism := range strings.Fields(strings.ToUpper(mechanisms)) {
		switch mechanism {
		case "PLAIN":
			auth = smtp.PlainAuth("", ep.Username, ep.Password, ep.Host)
		case "LOGIN":
			if auth == nil {
				auth = &loginAuth{username: ep.Username, password: ep.Password}
			}
		}
	}
	if auth == nil || client.Auth(auth) != nil {
		cleanup()
		return nil, nil, ErrAuth
	}
	return client, cleanup, nil
}

func validAddress(s string) bool {
	if !fieldOK(s) || strings.ContainsAny(s, "<>") || len(s) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s && addr.Name == ""
}

func (c *Client) submit(ctx context.Context, ep Endpoint, sender, recipient string, msg Message) error {
	if !validAddress(sender) || !validAddress(recipient) {
		return ErrEndpoint
	}
	client, done, err := c.smtp(ctx, ep)
	if err != nil {
		return err
	}
	defer done()
	if client.Mail(sender) != nil || client.Rcpt(recipient) != nil {
		return ErrDestination
	}
	data, err := client.Data()
	if err != nil {
		return ErrDestination
	}
	out := bufio.NewWriter(data)
	if err := c.forward(out, sender, recipient, msg.Raw); err != nil {
		return ErrDestination
	}
	if err := out.Flush(); err != nil {
		return ErrDestination
	}
	// Only Close transmits the SMTP end-of-data dot. Once it has been sent,
	// a transport error cannot distinguish a lost reply from a lost message.
	if err := data.Close(); err != nil {
		var reply *textproto.Error
		if errors.As(err, &reply) && reply.Code >= 400 && reply.Code <= 599 {
			return ErrDestination
		}
		return ErrAmbiguous
	}
	return nil
}

func forwardedSubject(raw []byte) string {
	subject := "Forwarded message"
	if m, err := mail.ReadMessage(io.LimitReader(bytes.NewReader(raw), 256<<10)); err == nil {
		if value := m.Header.Get("Subject"); value != "" {
			decoded, err := new(mime.WordDecoder).DecodeHeader(value)
			if err == nil {
				value = decoded
			}
			safe := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, value)
			runes := []rune(safe)
			subject = "Fwd: " + string(runes[:min(160, len(runes))])
		}
	}
	return mime.BEncoding.Encode("utf-8", subject)
}

func (c *Client) forward(w io.Writer, sender, recipient string, raw []byte) error {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	parts := multipart.NewWriter(w)
	_, err := fmt.Fprintf(w, "From: <%s>\r\nTo: <%s>\r\nDate: %s\r\nMessage-ID: <%x@emailrelay.invalid>\r\nSubject: %s\r\n%s: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n",
		sender, recipient, time.Now().UTC().Format(time.RFC1123Z), id, forwardedSubject(raw), markerHeader, c.marker(), parts.Boundary())
	if err != nil {
		return err
	}
	textPart, err := parts.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain; charset=us-ascii"}})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(textPart, "The original message is attached as original.eml.\r\n"); err != nil {
		return err
	}
	attachment, err := parts.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"application/octet-stream; name=\"original.eml\""},
		"Content-Disposition":       {"attachment; filename=\"original.eml\""},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return err
	}
	var line [78]byte
	for len(raw) > 0 {
		n := min(57, len(raw))
		base64.StdEncoding.Encode(line[:], raw[:n])
		length := base64.StdEncoding.EncodedLen(n)
		line[length], line[length+1] = '\r', '\n'
		if _, err := attachment.Write(line[:length+2]); err != nil {
			return err
		}
		raw = raw[n:]
	}
	return parts.Close()
}
