package relay

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"emailrelay/internal/mailwire"
	"emailrelay/internal/store"
)

type uncertainServer struct {
	// This IMAP-only fixture does not implement the POP3 operations.
	Transport
	sent       int
	generation uint32
}

// pollOnce runs one complete scheduling pass synchronously.
func pollOnce(t *testing.T, e *Engine) {
	t.Helper()
	rows, err := e.db.Ready()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := e.runRoute(context.Background(), row.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *uncertainServer) Discover(_ context.Context, _ mailwire.Endpoint, _ string, after uint32, limit int) (uint32, uint32, []uint32, error) {
	var found []uint32
	if after < 1 && limit > 0 {
		found = []uint32{1}
	}
	return s.generation, 1, found, nil
}
func (*uncertainServer) Fetch(context.Context, mailwire.Endpoint, string, uint32, uint32, int64) (mailwire.Message, error) {
	return mailwire.Message{UID: 1, Raw: []byte("test")}, nil
}
func (s *uncertainServer) Deliver(context.Context, mailwire.Endpoint, string, string, string, string, mailwire.Message) error {
	s.sent++
	return mailwire.ErrAmbiguous
}
func (*uncertainServer) TestTarget(context.Context, mailwire.Endpoint, string, string) error {
	return nil
}

func TestAmbiguousReceiptSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay.db")
	key := make([]byte, 32)
	db, err := store.Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Bootstrap("test", "test-local-account"); err != nil {
		t.Fatal(err)
	}
	ep := store.Endpoint{Host: "example.test", Port: 993, Security: "tls", Username: "fixture", Password: "fixture"}
	spec := store.RouteSpec{Name: "fixture", Policy: "all", Source: ep, SourceFolder: "INBOX", Target: ep, TargetFolder: "Archive", Method: "imap"}
	id, err := db.CreateRoute(1, spec)
	if err != nil {
		t.Fatal(err)
	}
	peer := &uncertainServer{generation: 4}
	e := New(db, peer, time.Minute, 1<<20)
	pollOnce(t, e)
	check := func(state string, cursor uint32) {
		t.Helper()
		r, err := db.OwnedJob(1, id)
		if err != nil || r.State != state || r.Cursor != cursor {
			t.Fatalf("state=%s cursor=%d error=%v", r.State, r.Cursor, err)
		}
	}
	check("uncertain", 0)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e = New(db, peer, time.Minute, 1<<20)
	pollOnce(t, e)
	check("uncertain", 0)
	if err = db.Action(1, id, "skip"); err != nil {
		t.Fatal(err)
	}
	pollOnce(t, e)
	check("ready", 1)
	peer.generation = 5
	pollOnce(t, e)
	check("invalid", 1)
	if peer.sent != 1 {
		t.Fatalf("sent %d times", peer.sent)
	}
}
