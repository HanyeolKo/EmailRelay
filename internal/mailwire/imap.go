package mailwire

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
)

func (c *Client) imap(ctx context.Context, ep Endpoint) (*imapclient.Client, func(), error) {
	return c.imapWithDial(ctx, ep, c.dial)
}

func (c *Client) imapWithDial(ctx context.Context, ep Endpoint, dial func(context.Context, Endpoint) (net.Conn, *tls.Config, func(), error)) (*imapclient.Client, func(), error) {
	conn, cfg, cleanup, err := dial(ctx, ep)
	if err != nil {
		return nil, nil, err
	}
	options := &imapclient.Options{TLSConfig: cfg}
	var client *imapclient.Client
	if ep.Security == "starttls" {
		client, err = imapclient.NewStartTLS(conn, options)
		if err != nil {
			cleanup()
			return nil, nil, ErrTLS
		}
	} else {
		client = imapclient.New(conn, options)
	}
	closeClient := func() { cleanup(); client.Close() }
	if err := client.WaitGreeting(); err != nil {
		closeClient()
		return nil, nil, ErrConnect
	}
	if client.Caps().Has(imap.AuthCap("PLAIN")) {
		err = client.Authenticate(sasl.NewPlainClient("", ep.Username, ep.Password))
	} else {
		err = client.Login(ep.Username, ep.Password).Wait()
	}
	if err != nil {
		closeClient()
		return nil, nil, ErrAuth
	}
	return client, closeClient, nil
}

func (c *Client) Discover(ctx context.Context, ep Endpoint, folder string, after uint32, limit int) (uint32, uint32, []uint32, error) {
	if !fieldOK(folder) || limit < 0 {
		return 0, 0, nil, ErrEndpoint
	}
	client, closeClient, err := c.imap(ctx, ep)
	if err != nil {
		return 0, 0, nil, err
	}
	defer closeClient()
	box, err := client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err := checkIMAPSelection(ep.Host, box, err); err != nil {
		return 0, 0, nil, err
	}
	highest := uint32(box.UIDNext - 1)
	var ids []uint32
	// Small windows bound the work per poll on a large backlog. Grow the
	// window over sparse, expunged UID ranges to reach a later message.
	for start, window := uint64(after)+1, uint64(512); box.NumMessages > 0 && len(ids) < limit && start <= uint64(highest); window *= 2 {
		end := min(start+window-1, uint64(highest))
		set := imap.UIDSet{{Start: imap.UID(start), Stop: imap.UID(end)}}
		data, err := client.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
		if err != nil {
			return 0, 0, nil, ErrSource
		}
		ranges, ok := data.All.(imap.UIDSet)
		if !ok {
			return 0, 0, nil, ErrSource
		}
		for _, r := range ranges {
			if uint64(r.Start) < start || uint64(r.Stop) > end || r.Stop < r.Start {
				return 0, 0, nil, ErrSource
			}
			for n := uint64(r.Start); n <= uint64(r.Stop) && len(ids) < limit; n++ {
				ids = append(ids, uint32(n))
			}
			if len(ids) == limit {
				break
			}
		}
		start = end + 1
	}
	return box.UIDValidity, highest, ids, nil
}

func checkIMAPSelection(host string, box *imap.SelectData, err error) error {
	if err != nil || box == nil {
		return ErrSource
	}
	if box.UIDValidity == 0 && strings.EqualFold(strings.TrimSuffix(host, "."), "imap.naver.com") {
		return ErrNoUIDValidity
	}
	if box.UIDValidity == 0 || box.UIDNext == 0 {
		return ErrSource
	}
	return nil
}

func (c *Client) Fetch(ctx context.Context, ep Endpoint, folder string, validity, uid uint32, maxBytes int64) (Message, error) {
	if !fieldOK(folder) || uid == 0 || maxBytes <= 0 || maxBytes >= 1<<32 {
		return Message{}, ErrEndpoint
	}
	client, closeClient, err := c.imap(ctx, ep)
	if err != nil {
		return Message{}, err
	}
	defer closeClient()
	box, err := client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil || box.UIDValidity == 0 {
		return Message{}, ErrSource
	}
	if box.UIDValidity != validity {
		return Message{}, ErrUIDValidity
	}
	// Resolve the current sequence number in this selection. The client
	// cannot route a UID FETCH if the server sends its body before its UID.
	// Only an empty UID search establishes that the message vanished.
	search, err := client.Search(&imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(imap.UID(uid))}}, nil).Wait()
	if err != nil {
		return Message{}, ErrSource
	}
	seq, ok := search.All.(imap.SeqSet)
	if !ok {
		return Message{}, ErrSource
	}
	if len(seq) == 0 {
		return Message{}, ErrVanished
	}
	if len(seq) != 1 || seq[0].Start == 0 || seq[0].Start != seq[0].Stop {
		return Message{}, ErrSource
	}
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Size: maxBytes + 1}}
	options := &imap.FetchOptions{UID: true, RFC822Size: true, InternalDate: true, Flags: true, BodySection: []*imap.FetchItemBodySection{section}}
	cmd := client.Fetch(seq, options)
	defer cmd.Close()
	var result Message
	for reply := cmd.Next(); reply != nil; reply = cmd.Next() {
		var m Message
		var size int64 = -1
		var tooLarge bool
		for item := reply.Next(); item != nil; item = reply.Next() {
			switch v := item.(type) {
			case imapclient.FetchItemDataUID:
				m.UID = uint32(v.UID)
			case imapclient.FetchItemDataRFC822Size:
				size = v.Size
			case imapclient.FetchItemDataFlags:
				for _, flag := range v.Flags {
					m.Flags = append(m.Flags, string(flag))
				}
			case imapclient.FetchItemDataInternalDate:
				m.Date = v.Time
			case imapclient.FetchItemDataBodySection:
				if v.Literal == nil || v.Section.Specifier != imap.PartSpecifierNone || len(v.Section.Part) != 0 {
					continue
				}
				if v.Literal.Size() > maxBytes {
					tooLarge = true
					continue
				}
				m.Raw, err = io.ReadAll(io.LimitReader(v.Literal, maxBytes+1))
				if err != nil {
					return Message{}, ErrSource
				}
			}
		}
		if m.UID == uid {
			if tooLarge || size > maxBytes || int64(len(m.Raw)) > maxBytes {
				return Message{}, ErrTooLarge
			}
			if m.Raw == nil || int64(len(m.Raw)) != size {
				return Message{}, ErrSource
			}
			result = m
		}
	}
	if err := cmd.Close(); err != nil {
		return Message{}, ErrSource
	}
	if result.UID == 0 {
		// An expunge can shift sequence numbers after SEARCH. Retry the UID
		// without advancing its cursor or using another message's contents.
		return Message{}, ErrSource
	}
	if c.hasMarker(result.Raw) {
		return Message{}, ErrLoop
	}
	return result, nil
}

func appendFlags(source []string) []imap.Flag {
	var flags []imap.Flag
	for _, raw := range source {
		for _, f := range []imap.Flag{imap.FlagSeen, imap.FlagFlagged, imap.FlagAnswered, imap.FlagDraft} {
			if strings.EqualFold(raw, string(f)) {
				flags = append(flags, f)
				break
			}
		}
	}
	return flags
}

func (c *Client) append(ctx context.Context, ep Endpoint, folder string, msg Message) error {
	if !fieldOK(folder) {
		return ErrEndpoint
	}
	client, closeClient, err := c.imap(ctx, ep)
	if err != nil {
		return err
	}
	defer closeClient()
	header := markerHeader + ": " + c.marker() + "\r\n"
	options := &imap.AppendOptions{Flags: appendFlags(msg.Flags), Time: msg.Date}
	cmd := client.Append(folder, int64(len(header))+int64(len(msg.Raw)), options)
	_, writeErr := io.Copy(cmd, io.MultiReader(strings.NewReader(header), bytes.NewReader(msg.Raw)))
	closeErr := cmd.Close()
	_, ackErr := cmd.Wait()
	var reply *imap.Error
	if errors.As(ackErr, &reply) && (reply.Type == imap.StatusResponseTypeNo || reply.Type == imap.StatusResponseTypeBad) {
		return ErrDestination
	}
	if writeErr != nil || closeErr != nil || ackErr != nil {
		return ErrAmbiguous
	}
	return nil
}

const markerHeader = "X-EmailRelay-Instance"

func (c *Client) marker() string {
	if c.instanceMarker == "" {
		return "emailrelay-v1"
	}
	return c.instanceMarker
}

// Inspect only top-level header lines. Even a malformed source message can
// acquire a trace header and must be recognized if it comes back to us.
func (c *Client) hasMarker(raw []byte) bool {
	for len(raw) > 0 {
		line, rest, _ := bytes.Cut(raw, []byte("\n"))
		raw = rest
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			break
		}
		key, value, found := bytes.Cut(line, []byte(":"))
		if found && bytes.EqualFold(key, []byte(markerHeader)) && string(bytes.TrimSpace(value)) == c.marker() {
			return true
		}
	}
	return false
}

func (c *Client) Deliver(ctx context.Context, target Endpoint, method, folder, sender, recipient string, msg Message) error {
	if c.hasMarker(msg.Raw) {
		return ErrLoop
	}
	switch method {
	case "imap":
		return c.append(ctx, target, folder, msg)
	case "smtp":
		return c.submit(ctx, target, sender, recipient, msg)
	default:
		return ErrEndpoint
	}
}

func (c *Client) TestTarget(ctx context.Context, target Endpoint, method, folder string) error {
	switch method {
	case "imap":
		if !fieldOK(folder) {
			return ErrEndpoint
		}
		client, done, err := c.imap(ctx, target)
		if err != nil {
			return err
		}
		defer done()
		if _, err = client.Status(folder, &imap.StatusOptions{UIDValidity: true}).Wait(); err != nil {
			return ErrDestination
		}
		return nil
	case "smtp":
		_, done, err := c.smtp(ctx, target)
		if err == nil {
			done()
		}
		return err
	default:
		return ErrEndpoint
	}
}
