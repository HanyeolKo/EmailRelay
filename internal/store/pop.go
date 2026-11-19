package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// PreparePOP assigns stable route-local numbers to a POP3 UIDL snapshot.
// Mailbox message numbers can change on every connection. A durable ledger
// also remembers UIDLs no longer visible so reappearance cannot replay mail.
func (s *Store) PreparePOP(id int64, uidls []string, limit int) ([]uint32, error) {
	return s.prepareIdentities(id, "pop3", uidls, limit)
}

// PrepareIMAPIdentities records a complete ordered snapshot of opaque message
// keys when the source cannot provide stable UIDs. Initial policy, mode and
// keys commit together so a failed snapshot never becomes the baseline.
func (s *Store) PrepareIMAPIdentities(id int64, keys []string, limit int) ([]uint32, error) {
	return s.prepareIdentities(id, "imap", keys, limit)
}

// pop_messages is the original on-disk name of the route-local identity ledger.
// POP3 and IMAP routes have separate, immutable identities after initialization.
func (s *Store) prepareIdentities(id int64, protocol string, keys []string, limit int) ([]uint32, error) {
	if limit < 0 {
		return nil, ErrInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := s.scanRoute(tx.QueryRow("SELECT "+routeCols+fromRoutes+eligible+" AND r.id=?", time.Now().Unix(), id))
	if err != nil {
		return nil, err
	}
	if r.SourceProtocol != protocol || r.Validity > 1 {
		return nil, ErrConflict
	}
	maxKeyLength := 70 // RFC 1939 UIDL limit
	if protocol == "imap" {
		maxKeyLength = 130
		if !r.IMAPIdentityMode {
			// A real IMAP server may use UIDVALIDITY=1. Its cursor has
			// no mapping to local identity numbers, even when it is zero.
			if r.Validity != 0 || r.Cursor != 0 || r.Delivered != 0 {
				return nil, ErrConflict
			}
			if _, err := tx.Exec("INSERT INTO imap_identity_routes(route_id) VALUES(?)", id); err != nil {
				return nil, err
			}
		}
	}
	var highest uint32
	if err = tx.QueryRow("SELECT COALESCE(MAX(uid),0) FROM pop_messages WHERE route_id=?", id).Scan(&highest); err != nil {
		return nil, err
	}
	// A pending record which vanished before retrieval is removed and its UID
	// becomes the cursor. Never reuse its number even when that row was last.
	highest = max(highest, r.Cursor)

	// Scope lookup fingerprints to a single mailbox route. Stored UIDLs may
	// contain provider metadata, so the reversible value is separately sealed.
	routeKey := hmac.New(sha256.New, s.popIndexKey)
	routeKey.Write(binary.BigEndian.AppendUint64(nil, uint64(id)))
	index := hmac.New(sha256.New, routeKey.Sum(nil))
	insert, err := tx.Prepare("INSERT INTO pop_messages(route_id,uid,fingerprint,uidl) VALUES(?,?,?,?)")
	if err != nil {
		return nil, err
	}
	defer insert.Close()

	// Never scan historical rows to reconcile the current snapshot. Query the
	// unique index a bounded chunk at a time and write only newly discovered IDs.
	seen := make(map[string]struct{}, len(keys))
	for group := range slices.Chunk(keys, 256) {
		fingerprints := make([][]byte, len(group))
		for i, key := range group {
			if !validIdentityKey(key, maxKeyLength) {
				return nil, ErrInvalid
			}
			if _, duplicate := seen[key]; duplicate {
				return nil, ErrInvalid
			}
			seen[key] = struct{}{}
			index.Reset()
			index.Write([]byte(key))
			fingerprints[i] = index.Sum(nil)
		}
		known, err := knownFingerprints(tx, id, fingerprints)
		if err != nil {
			return nil, err
		}
		for i, key := range group {
			if known[string(fingerprints[i])] {
				continue
			}
			if highest == math.MaxUint32 {
				return nil, errors.New("route exceeded the local message number limit")
			}
			highest++
			blob, err := s.seal([]byte(key), identityBinding(protocol, id, highest))
			if err != nil {
				return nil, err
			}
			if _, err = insert.Exec(id, highest, fingerprints[i], blob); err != nil {
				return nil, err
			}
		}
	}
	if r.Validity == 0 {
		if r.Policy == "new" {
			r.Cursor = highest
		}
		if _, err = tx.Exec("UPDATE routes SET validity=1,cursor=? WHERE id=?", r.Cursor, id); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query("SELECT uid FROM pop_messages WHERE route_id=? AND uid>? ORDER BY uid LIMIT ?", id, r.Cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uint32
	for rows.Next() {
		var uid uint32
		if err = rows.Scan(&uid); err != nil {
			return nil, err
		}
		ids = append(ids, uid)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

func validIdentityKey(key string, maxLength int) bool {
	if len(key) < 1 || len(key) > maxLength {
		return false
	}
	for i := range len(key) {
		if key[i] < '!' || key[i] > '~' {
			return false
		}
	}
	return true
}

func knownFingerprints(tx *sql.Tx, id int64, keys [][]byte) (map[string]bool, error) {
	args := make([]any, 1, len(keys)+1)
	args[0] = id
	for _, key := range keys {
		args = append(args, key)
	}
	params := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	rows, err := tx.Query("SELECT fingerprint FROM pop_messages WHERE route_id=? AND fingerprint IN ("+params+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := make(map[string]bool, len(keys))
	for rows.Next() {
		var key []byte
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		known[string(key)] = true
	}
	return known, rows.Err()
}

func identityBinding(protocol string, id int64, uid uint32) string {
	return fmt.Sprintf("emailrelay:v1:%s:%d:%d", protocol, id, uid)
}

// MissingPOP releases a UIDL which was queued but disappeared before retrieval.
// Its next appearance receives a fresh local number, since no copy was made.
func (s *Store) MissingPOP(id int64, uid uint32) error {
	return s.missingIdentity(id, uid, "pop3")
}

// MissingIMAPIdentity releases a pending identity after a complete source
// snapshot confirms absence. An interrupted submission cannot be released.
func (s *Store) MissingIMAPIdentity(id int64, uid uint32) error {
	return s.missingIdentity(id, uid, "imap")
}

func (s *Store) missingIdentity(id int64, uid uint32, protocol string) error {
	if uid == 0 {
		return ErrInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := s.scanRoute(tx.QueryRow("SELECT "+routeCols+fromRoutes+"AND r.id=?", id))
	if err != nil {
		return err
	}
	if r.SourceProtocol != protocol || r.IMAPIdentityMode != (protocol == "imap") || r.Validity != 1 || r.State != "ready" || r.Inflight != 0 || uid <= r.Cursor {
		return ErrConflict
	}
	// Consume only the next pending number, preserving every older decision.
	err = mustChange(tx.Exec(`DELETE FROM pop_messages WHERE route_id=? AND uid=?
		AND uid=(SELECT MIN(uid) FROM pop_messages WHERE route_id=? AND uid>?)`, id, uid, id, r.Cursor))
	if err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE routes SET cursor=?,detail='',last_checked=?,failures=0,next_attempt=0 WHERE id=?", uid, time.Now().Unix(), id); err != nil {
		return err
	}
	if err = record(tx, id, uid, "skipped", "소스에서 사라진 메일입니다. 같은 메일이 다시 나타나면 중계합니다."); err != nil {
		return err
	}
	return tx.Commit()
}

// POPUIDL resolves the queued local number immediately before source retrieval.
func (s *Store) POPUIDL(id int64, uid uint32) (string, error) {
	return s.identityKey(id, uid, "pop3")
}

// IMAPIdentityKey resolves a local number to the key used in a fresh snapshot.
func (s *Store) IMAPIdentityKey(id int64, uid uint32) (string, error) {
	return s.identityKey(id, uid, "imap")
}

func (s *Store) identityKey(id int64, uid uint32, protocol string) (string, error) {
	var blob []byte
	err := s.db.QueryRow("SELECT p.uidl FROM pop_messages p JOIN routes r ON r.id=p.route_id JOIN users u ON u.id=r.user_id WHERE u.enabled=1 AND p.route_id=? AND p.uid=? AND "+identityMode+"=?", id, uid, protocol == "imap").Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	raw, err := s.unseal(blob, identityBinding(protocol, id, uid))
	return string(raw), err
}
