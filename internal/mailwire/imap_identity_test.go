package mailwire

import (
	"bytes"
	"errors"
	"io"
	"log"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

func TestNoUIDValidityOnlyAfterNaverSelected(t *testing.T) {
	for _, test := range []struct {
		host      string
		box       *imap.SelectData
		err, want error
	}{
		{"imap.naver.com", &imap.SelectData{}, nil, ErrNoUIDValidity},
		{"IMAP.NAVER.COM.", &imap.SelectData{UIDNext: 1}, nil, ErrNoUIDValidity},
		{"imap.naver.com", &imap.SelectData{}, io.EOF, ErrSource},
		{"imap.naver.com", nil, nil, ErrSource},
		{"other.naver.com", &imap.SelectData{}, nil, ErrSource},
		{"imap.naver.com.invalid", &imap.SelectData{}, nil, ErrSource},
		{"imap.naver.com", &imap.SelectData{UIDValidity: 1}, nil, ErrSource},
		{"imap.naver.com", &imap.SelectData{UIDValidity: 1, UIDNext: 1}, nil, nil},
	} {
		if got := checkIMAPSelection(test.host, test.box, test.err); !errors.Is(got, test.want) {
			t.Errorf("host %s, box %+v: %v, want %v", test.host, test.box, got, test.want)
		}
	}
}

type identityFixture struct {
	recreated      atomic.Bool
	rolled         atomic.Bool
	omitNextFetch  atomic.Bool
	fetches        atomic.Int32
	selections     atomic.Int32
	breakNextFetch atomic.Bool
	replyVariant   atomic.Int32
	oversized      atomic.Bool
}

type identityTestSession struct {
	imapserver.Session
	state *identityFixture
	conn  *imapserver.Conn
}

func (s *identityTestSession) Select(folder string, o *imap.SelectOptions) (*imap.SelectData, error) {
	if !o.ReadOnly {
		return nil, errors.New("expected EXAMINE")
	}
	s.state.selections.Add(1)
	if s.state.recreated.Load() {
		return s.Session.Select("Recreated", o)
	}
	if s.state.rolled.Load() {
		folder = "Rolled"
	}
	box, err := s.Session.Select(folder, o)
	if err == nil {
		box.UIDValidity = 0
		if s.state.oversized.Load() {
			box.NumMessages = maxIMAPIndexMessages + 1
		}
	}
	return box, err
}

func (s *identityTestSession) Fetch(w *imapserver.FetchWriter, set imap.NumSet, o *imap.FetchOptions) error {
	if len(o.BodySection) != 1 || !o.BodySection[0].Peek {
		return errors.New("expected read-only body request")
	}
	if s.state.breakNextFetch.Swap(false) {
		return s.conn.NetConn().Close()
	}
	if s.state.omitNextFetch.Swap(false) {
		return nil
	}
	seqs := set.(imap.SeqSet)
	values, _ := seqs.Nums()
	s.state.fetches.Add(int32(len(values)))
	if variant := s.state.replyVariant.Swap(0); variant != 0 {
		row := w.CreateMessage(values[0])
		body := []byte("Subject: test\r\n\r\nno id")
		if variant == 2 {
			body = bytes.ReplaceAll(body, []byte("no id"), []byte("OTHER"))
		}
		length := int64(len(body))
		lit := row.WriteBodySection(o.BodySection[0], length)
		if _, err := lit.Write(body); err != nil {
			return err
		}
		if err := lit.Close(); err != nil {
			return err
		}
		row.WriteUID(imap.UID(values[0]))
		row.WriteInternalDate(time.Unix(1720450000, 0))
		if variant == 3 {
			length++
		}
		row.WriteRFC822Size(length)
		return row.Close()
	}
	return s.Session.Fetch(w, set, o)
}

func TestIMAPContentIdentity(t *testing.T) {
	c, cfg, ln, ep := fixture(t)
	state := &identityFixture{}
	db := imapmemserver.New()
	user := imapmemserver.NewUser(ep.Username, ep.Password)
	db.AddUser(user)
	for _, folder := range []string{"INBOX", "Recreated"} {
		if err := user.Create(folder, nil); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Unix(1720450000, 0)
	appendSource := func(folder string, raw []byte, date time.Time) {
		t.Helper()
		if _, err := user.Append(folder, bytes.NewReader(raw), &imap.AppendOptions{Time: date}); err != nil {
			t.Fatal(err)
		}
	}
	sameID := bytes.ReplaceAll(original, []byte("Original body."), []byte("Modified body."))
	noID := []byte("Subject: test\r\n\r\nno id")
	loop := append([]byte(markerHeader+": "+c.marker()+"\r\n"), original...)
	messages := [][]byte{original, sameID, noID, original, loop}
	for _, raw := range messages {
		appendSource("INBOX", raw, stamp)
	}
	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &identityTestSession{db.NewSession(), state, conn}, nil, nil
		},
		TLSConfig: cfg, Logger: log.New(io.Discard, "", 0),
	})
	go server.Serve(ln)
	t.Cleanup(func() {
		if i := c.takeIndex(indexKey(1, ep, "INBOX")); i != nil {
			i.close()
		}
		server.Close()
	})
	ctx := t.Context()
	list := func() []IMAPIdentity {
		t.Helper()
		ids, err := c.ListIMAPIdentities(ctx, 1, ep, "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	ids := list()
	if len(ids) != len(messages) {
		t.Fatalf("snapshot count = %d", len(ids))
	}
	keys := map[string]bool{}
	for _, id := range ids {
		if keys[id.Key] || len(id.Key) > 70 {
			t.Fatalf("invalid identity: %+v", id)
		}
		keys[id.Key] = true
	}
	if !slices.Equal(ids, list()) || state.selections.Load() != 2 || state.fetches.Load() != 5 {
		t.Fatalf("poll did not reuse read-only session, selects %d fetches %d", state.selections.Load(), state.fetches.Load())
	}
	t.Run("independent routes reading one mailbox", func(t *testing.T) {
		second, err := c.ListIMAPIdentities(ctx, 2, ep, "INBOX")
		if err != nil || !slices.Equal(ids, second) {
			t.Fatalf("second route inventory: %v", err)
		}
		// A different worker is in a command, so its selection is checked out.
		busy := c.takeIndex(indexKey(1, ep, "INBOX"))
		if busy == nil {
			t.Fatal("missing first route selection")
		}
		defer c.keepIndex(indexKey(1, ep, "INBOX"), busy)
		defer func() {
			if i := c.takeIndex(indexKey(2, ep, "INBOX")); i != nil {
				i.close()
			}
		}()
		msg, err := c.FetchIMAPIdentity(ctx, 2, ep, "INBOX", second[1], 4096)
		if err != nil || !bytes.Equal(msg.Raw, sameID) {
			t.Fatalf("other worker disrupted the second route: %v", err)
		}
	})
	msg, err := c.FetchIMAPIdentity(ctx, 1, ep, "INBOX", ids[1], 4096)
	if err != nil || !bytes.Equal(msg.Raw, sameID) || slices.Contains(msg.Flags, string(imap.FlagSeen)) {
		t.Fatalf("verified fetch: %v", err)
	}
	for _, tc := range []struct {
		id   IMAPIdentity
		max  int64
		want error
	}{
		{ids[0], 10, ErrTooLarge}, {ids[4], 4096, ErrLoop},
		{IMAPIdentity{UID: ids[4].UID, Key: ids[1].Key}, 4096, ErrIdentityChanged},
	} {
		_, err := c.FetchIMAPIdentity(ctx, 1, ep, "INBOX", tc.id, tc.max)
		if !errors.Is(err, tc.want) {
			t.Fatalf("fetch %+v: %v, want %v", tc.id, err, tc.want)
		}
	}
	// A new connection can assign different UIDs and return positive
	// UIDVALIDITY after an earlier selection had none.
	for _, n := range []int{2, 1, 4, 0, 3} {
		appendSource("Recreated", messages[n], stamp)
	}
	state.recreated.Store(true)
	afterReset := list()
	for _, id := range afterReset {
		if !keys[id.Key] {
			t.Fatalf("identity changed on reconnect: %+v", id)
		}
	}
	if afterReset[0].Key != ids[2].Key {
		t.Fatal("UID reuse hid different source mail")
	}
	before := state.fetches.Load()
	appendSource("Recreated", original, stamp.Add(time.Second))
	afterAppend := list()
	if len(afterAppend) != 6 || keys[afterAppend[5].Key] || state.fetches.Load() != before+1 {
		t.Fatal("incremental inventory failed")
	}
	appendSource("Recreated", noID, stamp.Add(time.Second))
	state.omitNextFetch.Store(true)
	if partial, err := c.ListIMAPIdentities(ctx, 1, ep, "INBOX"); len(partial) != 0 || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("incomplete inventory accepted: %v %v", partial, err)
	}
	fresh := list()
	state.breakNextFetch.Store(true)
	if _, err := c.FetchIMAPIdentity(ctx, 1, ep, "INBOX", fresh[0], 4096); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("lost session: %v", err)
	}
	if restored := list(); len(restored) != 7 || !slices.Equal(restored, fresh) {
		t.Fatal("reconnect after partial snapshot changed keys")
	}
	for _, tc := range []struct {
		variant int32
		want    error
	}{{1, nil}, {2, ErrIdentityChanged}, {3, ErrIMAPSizeMismatch}} {
		state.replyVariant.Store(tc.variant)
		_, err := c.FetchIMAPIdentity(ctx, 1, ep, "INBOX", fresh[0], 4096)
		if !errors.Is(err, tc.want) {
			t.Fatalf("late UID, variant %d: %v", tc.variant, err)
		}
		list()
	}
	t.Run("recreated folder reuses all UIDs", func(t *testing.T) {
		state.recreated.Store(false)
		old := list()
		if err := user.Rename("INBOX", "Old", nil); err != nil {
			t.Fatal(err)
		}
		if err := user.Create("INBOX", nil); err != nil {
			t.Fatal(err)
		}
		replacement := []byte("Subject: replacement\r\n\r\nnew contents")
		appendSource("INBOX", replacement, stamp)
		for _, raw := range messages[1:] {
			appendSource("INBOX", raw, stamp)
		}
		key := indexKey(1, ep, "INBOX")
		index := c.takeIndex(key)
		index.created = time.Now().Add(-maxIMAPIndexAge)
		c.keepIndex(key, index)
		before := state.fetches.Load()
		current := list()
		if len(current) != len(old) || current[0].UID != old[0].UID || current[0].Key == old[0].Key || state.fetches.Load() != before+5 {
			t.Fatal("expired selection hid replacement")
		}
		state.oversized.Store(true)
		partial, err := c.ListIMAPIdentities(ctx, 1, ep, "INBOX")
		state.oversized.Store(false)
		if len(partial) != 0 || !errors.Is(err, ErrIMAPInventory) || state.fetches.Load() != before+5 {
			t.Fatalf("oversized inventory accepted: %d %v", len(partial), err)
		}
	})
	t.Run("cached selection freezes while visible window rolls", func(t *testing.T) {
		state.recreated.Store(false)
		start := list()
		if len(start) != 5 || start[4].UID != 5 {
			t.Fatal("unexpected original window")
		}
		if err := user.Create("Rolled", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := user.Append("Rolled", bytes.NewReader(original), &imap.AppendOptions{Time: stamp, Flags: []imap.Flag{imap.FlagDeleted}}); err != nil {
			t.Fatal(err)
		}
		for _, raw := range messages[1:] {
			appendSource("Rolled", raw, stamp)
		}
		newMail := []byte("Subject: arrived\r\n\r\nnext")
		appendSource("Rolled", newMail, stamp.Add(time.Minute))
		setup := imapmemserver.NewUserSession(user)
		if _, err := setup.Select("Rolled", &imap.SelectOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := setup.Expunge(nil, nil); err != nil {
			t.Fatal(err)
		}
		setup.Close()
		// The existing selection still serves 1..5 after NOOP and SEARCH.
		// Only subsequent selections see 2..6, with UIDVALIDITY still zero.
		state.rolled.Store(true)
		before := state.fetches.Load()
		updated := list()
		if len(updated) != len(start) || updated[0].UID != 2 || updated[4].UID != 6 || state.fetches.Load() != before+5 {
			t.Fatalf("stale inventory: %+v", updated)
		}
		msg, err := c.FetchIMAPIdentity(ctx, 1, ep, "INBOX", updated[4], 4096)
		if err != nil || !bytes.Equal(msg.Raw, newMail) {
			t.Fatalf("fresh delivery fetch: %v", err)
		}
	})
}
