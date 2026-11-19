package store

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestIMAPIdentityModeMigrationAndBaseline(t *testing.T) {
	s, path, key := openTest(t)
	check(t, s.Bootstrap("admin", password))
	native, err := s.CreateRoute(1, spec())
	check(t, err)
	check(t, s.Initialize(native, 1, 0))
	newMail := spec()
	newMail.Policy = "new"
	id, err := s.CreateRoute(1, newMail)
	check(t, err)
	// Simulate the pre-upgrade schema, including a real UIDVALIDITY of 1.
	_, err = s.db.Exec("DROP TABLE imap_identity_routes")
	check(t, err)
	check(t, s.Close())
	s, err = Open(path, key)
	check(t, err)
	defer s.Close()
	_, err = s.PrepareIMAPIdentities(native, []string{"old"}, 9)
	wantErr(t, err, ErrConflict)
	r, err := s.Job(native)
	check(t, err)
	if r.IMAPIdentityMode || r.Validity != 1 || r.Cursor != 0 {
		t.Fatal("native checkpoint changed during upgrade")
	}
	maxKey := strings.Repeat("a", 130)
	for _, keys := range [][]string{{maxKey, maxKey}, {"a b"}, {maxKey + "1"}} {
		_, err = s.PrepareIMAPIdentities(id, keys, 5)
		wantErr(t, err, ErrInvalid)
	}
	r, err = s.Job(id)
	check(t, err)
	if r.IMAPIdentityMode || r.Validity != 0 {
		t.Fatal("incomplete snapshot became baseline")
	}
	ids, err := s.PrepareIMAPIdentities(id, []string{maxKey, "existing"}, 5)
	check(t, err)
	if len(ids) != 0 {
		t.Fatal("new-only route queued existing messages")
	}
	r, err = s.Job(id)
	check(t, err)
	if !r.IMAPIdentityMode || r.Validity != 1 || r.Cursor != 2 {
		t.Fatal("incomplete IMAP baseline")
	}
	wantErr(t, s.Initialize(id, 1, 0), ErrConflict)
	_, err = s.PreparePOP(id, nil, 5)
	wantErr(t, err, ErrConflict)
	_, err = s.POPUIDL(id, 1)
	wantErr(t, err, ErrNotFound)
	var blob []byte
	check(t, s.db.QueryRow("SELECT uidl FROM pop_messages WHERE route_id=? AND uid=1", id).Scan(&blob))
	if bytes.Contains(blob, []byte(maxKey)) {
		t.Fatal("stored a plaintext key")
	}
	ids, err = s.PrepareIMAPIdentities(id, []string{"existing", "arrived", maxKey}, 5)
	check(t, err)
	if !slices.Equal(ids, []uint32{3}) {
		t.Fatalf("queued after baseline: %v", ids)
	}
}

func TestIMAPIdentitiesRecoverWithoutReplay(t *testing.T) {
	s, path, key := openTest(t)
	check(t, s.Bootstrap("admin", password))
	id, err := s.CreateRoute(1, spec())
	check(t, err)
	snapshot := []string{"same-digest/1", "same-digest/2", "disappears", "fourth"}
	ids, err := s.PrepareIMAPIdentities(id, snapshot, 3)
	check(t, err)
	if !slices.Equal(ids, []uint32{1, 2, 3}) {
		t.Fatal(ids)
	}
	check(t, s.Begin(id, 1, 1))
	check(t, s.Finish(id, 1, "delivered", "ok"))
	check(t, s.Begin(id, 1, 2))
	check(t, s.Close())
	s, err = Open(path, key)
	check(t, err)
	defer s.Close()
	r, err := s.OwnedJob(1, id)
	check(t, err)
	if !r.IMAPIdentityMode || r.State != "uncertain" || r.Inflight != 2 || r.Cursor != 1 {
		t.Fatalf("wrong restart state: mode %v state %s inflight %d cursor %d", r.IMAPIdentityMode, r.State, r.Inflight, r.Cursor)
	}
	_, err = s.PrepareIMAPIdentities(id, snapshot, 5)
	wantErr(t, err, ErrNotFound)
	wantErr(t, s.MissingIMAPIdentity(id, 2), ErrConflict)
	check(t, s.Action(1, id, "skip"))
	ids, err = s.PrepareIMAPIdentities(id, []string{"fourth", "same-digest/2", "same-digest/1"}, 5)
	check(t, err)
	if !slices.Equal(ids, []uint32{3, 4}) {
		t.Fatal(ids)
	}
	wantErr(t, s.MissingIMAPIdentity(id, 4), ErrConflict)
	check(t, s.MissingIMAPIdentity(id, 3))
	check(t, s.Begin(id, 1, 4))
	check(t, s.Fail(id, 4, "uncertain", true))
	check(t, s.Action(1, id, "retry"))
	ids, err = s.PrepareIMAPIdentities(id, snapshot, 5)
	check(t, err)
	if !slices.Equal(ids, []uint32{4, 5}) {
		t.Fatalf("retry and reappearing mail: %v", ids)
	}
	key5, err := s.IMAPIdentityKey(id, 5)
	check(t, err)
	if key5 != "disappears" {
		t.Fatal("lost pending message after it reappeared")
	}
	for _, uid := range ids {
		check(t, s.Begin(id, 1, uid))
		check(t, s.Finish(id, uid, "delivered", "ok"))
	}
	slices.Reverse(snapshot)
	ids, err = s.PrepareIMAPIdentities(id, snapshot, 5)
	check(t, err)
	if len(ids) != 0 {
		t.Fatalf("replayed after reordering: %v", ids)
	}
}
