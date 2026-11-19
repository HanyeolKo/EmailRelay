package relay

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"emailrelay/internal/mailwire"
	"emailrelay/internal/store"
)

type naverPeer struct {
	Transport
	messages  []mailwire.IMAPIdentity
	onList    func(context.Context) error
	discovers int
	sent      []string
}

func (p *naverPeer) Discover(context.Context, mailwire.Endpoint, string, uint32, int) (uint32, uint32, []uint32, error) {
	p.discovers++
	return 0, 0, nil, mailwire.ErrNoUIDValidity
}

func (p *naverPeer) ListIMAPIdentities(ctx context.Context, _ int64, _ mailwire.Endpoint, _ string) ([]mailwire.IMAPIdentity, error) {
	list := slices.Clone(p.messages)
	if p.onList != nil {
		if err := p.onList(ctx); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (p *naverPeer) FetchIMAPIdentity(_ context.Context, _ int64, _ mailwire.Endpoint, _ string, hint mailwire.IMAPIdentity, _ int64) (mailwire.Message, error) {
	if !slices.Contains(p.messages, hint) {
		return mailwire.Message{}, mailwire.ErrIdentityChanged
	}
	return mailwire.Message{UID: hint.UID, Raw: []byte(hint.Key)}, nil
}

func (p *naverPeer) Deliver(_ context.Context, _ mailwire.Endpoint, _, _, _, _ string, msg mailwire.Message) error {
	p.sent = append(p.sent, string(msg.Raw))
	return nil
}

func (*naverPeer) TestTarget(context.Context, mailwire.Endpoint, string, string) error {
	return nil
}

type identityFixture struct {
	db   *store.Store
	path string
	key  []byte
	id   int64
	spec store.RouteSpec
}

func setupIdentity(t *testing.T, policy string) *identityFixture {
	t.Helper()
	f := &identityFixture{path: filepath.Join(t.TempDir(), "relay.db"), key: make([]byte, 32)}
	db, err := store.Open(f.path, f.key)
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { f.db.Close() })
	if err := db.Bootstrap("owner", "test-local-account"); err != nil {
		t.Fatal(err)
	}
	ep := store.Endpoint{Host: "imap.naver.com", Port: 993, Username: "fixture", Password: "fixture", Security: "tls"}
	target := ep
	target.Host = "target.example.test"
	f.spec = store.RouteSpec{Name: "fixture", SourceProtocol: "imap", Source: ep, SourceFolder: "INBOX", Target: target, TargetFolder: "Archive", Method: "imap", Policy: policy}
	f.id, err = db.CreateRoute(1, f.spec)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestNaverIdentityStartsAndResumes(t *testing.T) {
	for _, policy := range []string{"new", "all"} {
		t.Run(policy, func(t *testing.T) {
			f := setupIdentity(t, policy)
			peer := &naverPeer{messages: []mailwire.IMAPIdentity{{UID: 9, Key: "old1"}, {UID: 12, Key: "old2"}}}
			e := New(f.db, peer, time.Minute, 4096)
			if err := e.Test(context.Background(), 1, f.id); err != nil {
				t.Fatal(err)
			}
			r, err := f.db.Job(f.id)
			if err != nil || r.IMAPIdentityMode || r.Validity != 0 {
				t.Fatal("connection test committed a baseline", err)
			}
			pollOnce(t, e)
			var want []string
			if policy == "all" {
				want = []string{"old1", "old2"}
			}
			if !slices.Equal(peer.sent, want) {
				t.Fatalf("initial delivery %v, want %v", peer.sent, want)
			}
			if err := f.db.Close(); err != nil {
				t.Fatal(err)
			}
			f.db, err = store.Open(f.path, f.key)
			if err != nil {
				t.Fatal(err)
			}
			e = New(f.db, peer, time.Minute, 4096)
			// Recycle an old UID for a different email and renumber the rest.
			peer.messages = []mailwire.IMAPIdentity{{UID: 4, Key: "old2"}, {UID: 9, Key: "arrived"}, {UID: 11, Key: "old1"}}
			pollOnce(t, e)
			pollOnce(t, e)
			want = append(want, "arrived")
			if !slices.Equal(peer.sent, want) || peer.discovers != 2 {
				t.Fatalf("after restart, delivered %v, want %v (discovery count %d)", peer.sent, want, peer.discovers)
			}
		})
	}
}

func TestNaverStaleUIDLeavesOriginalPending(t *testing.T) {
	f := setupIdentity(t, "all")
	peer := &naverPeer{messages: []mailwire.IMAPIdentity{{UID: 8, Key: "original"}}}
	peer.onList = func(context.Context) error {
		peer.messages = []mailwire.IMAPIdentity{{UID: 8, Key: "replacement"}, {UID: 9, Key: "original"}}
		return nil
	}
	e := New(f.db, peer, time.Minute, 4096)
	pollOnce(t, e)
	r, err := f.db.OwnedJob(1, f.id)
	if err != nil || r.State != "error" || r.Cursor != 0 || len(peer.sent) != 0 {
		t.Fatalf("stale UID: state %s cursor %d sent %v err %v", r.State, r.Cursor, peer.sent, err)
	}
	peer.onList = nil
	if err := f.db.Action(1, f.id, "retry"); err != nil {
		t.Fatal(err)
	}
	pollOnce(t, e)
	if !slices.Equal(peer.sent, []string{"original", "replacement"}) {
		t.Fatalf("fresh snapshot delivered %v", peer.sent)
	}
}

func TestNaverInitialScanDiscardsChangedSettings(t *testing.T) {
	f := setupIdentity(t, "new")
	started := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer := &naverPeer{messages: []mailwire.IMAPIdentity{{UID: 30, Key: "old"}}}
	peer.onList = func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	e := New(f.db, peer, time.Minute, 4096)
	result := make(chan error, 1)
	go func() { result <- e.runRoute(ctx, f.id) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("scan never started")
	}
	f.spec.Policy = "all"
	err := e.EditRoute(f.id, func() error { return f.db.UpdateRoute(1, f.id, f.spec) })
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale snapshot: %v", err)
	}
	r, err := f.db.Job(f.id)
	if err != nil || r.IMAPIdentityMode || r.Validity != 0 || len(peer.sent) != 0 {
		t.Fatal("committed baseline for outdated settings", err)
	}
	peer.onList = nil
	pollOnce(t, e)
	if !slices.Equal(peer.sent, []string{"old"}) {
		t.Fatalf("updated all-mail policy delivered %v", peer.sent)
	}
}

func TestEditUsersOnlyCancelsAffectedScans(t *testing.T) {
	e := New(nil, nil, time.Minute, 4096)
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	e.scans[3] = mailboxScan{owner: 10, cancel: cancelFirst}
	e.scans[4] = mailboxScan{owner: 20, cancel: cancelSecond}
	err := e.EditUsers(10, func() error { return store.ErrForbidden })
	if !errors.Is(err, store.ErrForbidden) || first.Err() != nil || second.Err() != nil {
		t.Fatal("rejected account edit interrupted a scan")
	}
	err = e.EditUsers(10, func() error { return nil })
	if err != nil || !errors.Is(first.Err(), context.Canceled) || second.Err() != nil {
		t.Fatal("successful account edit did not cancel only that account's scan")
	}
}
