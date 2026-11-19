package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func popSpec(policy string) RouteSpec {
	r := spec()
	r.SourceProtocol = "pop3"
	r.SourceFolder = ""
	r.Source.Port = 995
	r.Policy = policy
	return r
}

func TestPOPLedgerAcrossRenumberingAndRestart(t *testing.T) {
	s, path, key := openTest(t)
	a, m := users(t, s)
	id, err := s.CreateRoute(m.User.ID, popSpec("new"))
	check(t, err)
	expect := func(list []string, want ...uint32) {
		t.Helper()
		ids, err := s.PreparePOP(id, list, 3)
		check(t, err)
		if !slices.Equal(ids, want) {
			t.Fatalf("queued %v, want %v", ids, want)
		}
	}
	expect([]string{"Opaque:A", "opaque:a"})
	r, err := s.Job(id)
	check(t, err)
	if r.Validity != 1 || r.Cursor != 2 || r.SourceProtocol != "pop3" || r.SourceFolder != "" {
		t.Fatal("incorrect POP baseline")
	}
	expect([]string{"opaque:a", "new/three", "Opaque:A", "new/four"}, 3, 4)
	check(t, s.Begin(id, 1, 3))
	check(t, s.Close())
	s, err = Open(path, key)
	check(t, err)
	defer s.Close()
	_, err = s.PreparePOP(id, []string{"new/four"}, 3)
	wantErr(t, err, ErrNotFound)
	check(t, s.Action(m.User.ID, id, "retry"))
	// A manually skipped item must remain suppressed if it reappears.
	expect([]string{"new/four"}, 3, 4)
	got, err := s.POPUIDL(id, 3)
	check(t, err)
	if got != "new/three" {
		t.Fatalf("resumed wrong UIDL: %s", got)
	}
	check(t, s.Hold(id, 3, "held"))
	check(t, s.Action(m.User.ID, id, "skip"))
	check(t, s.Begin(id, 1, 4))
	check(t, s.Finish(id, 4, "delivered", "ok"))
	expect([]string{"new/three", "opaque:a", "Opaque:A", "new/four"})
	expect([]string{"new/five", "Opaque:A"}, 5)
	check(t, s.SetUserEnabled(a.User.ID, m.User.ID, false))
	_, err = s.PreparePOP(id, nil, 3)
	wantErr(t, err, ErrNotFound)
	_, err = s.POPUIDL(id, 5)
	wantErr(t, err, ErrNotFound)
	check(t, s.SetUserEnabled(a.User.ID, m.User.ID, true))
	var raw []byte
	check(t, s.db.QueryRow("SELECT uidl FROM pop_messages WHERE route_id=? AND uid=4", id).Scan(&raw))
	if bytes.Contains(raw, []byte("new/four")) {
		t.Fatal("UIDL stored in plaintext")
	}
	check(t, s.Action(m.User.ID, id, "delete"))
	var count int
	check(t, s.db.QueryRow("SELECT COUNT(*) FROM pop_messages WHERE route_id=?", id).Scan(&count))
	if count != 0 {
		t.Fatal("deleted route retained UIDL history")
	}
}

func TestPOPRequeuesVanishedMailAfterRestart(t *testing.T) {
	s, path, key := openTest(t)
	check(t, s.Bootstrap("admin", password))
	id, err := s.CreateRoute(1, popSpec("all"))
	check(t, err)
	ids, err := s.PreparePOP(id, []string{"copied", "loop", "vanished"}, 12)
	check(t, err)
	if !slices.Equal(ids, []uint32{1, 2, 3}) {
		t.Fatal(ids)
	}
	wantErr(t, s.MissingPOP(id, 3), ErrConflict)
	check(t, s.Begin(id, 1, 1))
	wantErr(t, s.MissingPOP(id, 1), ErrConflict)
	check(t, s.Finish(id, 1, "delivered", "copied"))
	check(t, s.Finish(id, 2, "skipped", "loop"))
	check(t, s.MissingPOP(id, 3))
	wantErr(t, s.MissingPOP(id, 1), ErrConflict)
	events, err := s.Logs(1, id)
	check(t, err)
	if events[0].UID != 3 || events[0].Kind != "skipped" {
		t.Fatal("missing POP event")
	}
	check(t, s.Close())
	s, err = Open(path, key)
	check(t, err)
	defer s.Close()
	ids, err = s.PreparePOP(id, []string{"vanished", "loop", "copied"}, 12)
	check(t, err)
	if !slices.Equal(ids, []uint32{4}) {
		t.Fatalf("requeued %v", ids)
	}
	value, err := s.POPUIDL(id, 4)
	check(t, err)
	if value != "vanished" {
		t.Fatal("wrong UIDL requeued")
	}
	check(t, s.Begin(id, 1, 4))
	check(t, s.Finish(id, 4, "delivered", "ok"))
	ids, err = s.PreparePOP(id, []string{"vanished", "copied", "loop"}, 12)
	check(t, err)
	if len(ids) != 0 {
		t.Fatalf("copied messages requeued: %v", ids)
	}
}

func TestPOPBatchAndLegacyProtocol(t *testing.T) {
	s, _, _ := openTest(t)
	check(t, s.Bootstrap("admin", password))
	id, err := s.CreateRoute(1, spec())
	check(t, err)

	// Reproduce the encrypted route format predating SourceProtocol.
	raw, err := json.Marshal(spec())
	check(t, err)
	var old map[string]json.RawMessage
	check(t, json.Unmarshal(raw, &old))
	delete(old, "SourceProtocol")
	raw, err = json.Marshal(old)
	check(t, err)
	blob, err := s.seal(raw, binding(1, id))
	check(t, err)
	_, err = s.db.Exec("UPDATE routes SET spec=? WHERE id=?", blob, id)
	check(t, err)
	r, err := s.Job(id)
	check(t, err)
	if r.SourceProtocol != "imap" {
		t.Fatal("legacy route lost IMAP protocol")
	}
	_, err = s.PreparePOP(id, nil, 3)
	wantErr(t, err, ErrConflict)
	p := r.RouteSpec
	p.SourceProtocol = "pop3"
	p.Source.Password = ""
	wantErr(t, s.UpdateRoute(1, id, p), ErrInvalid)
	p.Source.Password = "new-pop-password"
	check(t, s.UpdateRoute(1, id, p))
	full := make([]string, 300)
	for i := range full {
		full[i] = fmt.Sprintf("item:%d", i)
	}
	// A late duplicate aborts every insert and the initial baseline.
	bad := append(slices.Clone(full), full[0])
	_, err = s.PreparePOP(id, bad, 12)
	wantErr(t, err, ErrInvalid)
	r, err = s.Job(id)
	check(t, err)
	if r.Validity != 0 {
		t.Fatal("invalid snapshot initialized route")
	}
	for _, badUIDL := range []string{"", "has space", strings.Repeat("a", 71)} {
		_, err = s.PreparePOP(id, []string{badUIDL}, 12)
		wantErr(t, err, ErrInvalid)
	}
	ids, err := s.PreparePOP(id, full, 3)
	check(t, err)
	if !slices.Equal(ids, []uint32{1, 2, 3}) {
		t.Fatalf("all policy queued %v", ids)
	}
	first, err := s.POPUIDL(id, 1)
	check(t, err)
	if first != full[0] {
		t.Fatalf("first UIDL %q", first)
	}
	p.SourceProtocol = "imap"
	wantErr(t, s.UpdateRoute(1, id, p), ErrConflict)
}
