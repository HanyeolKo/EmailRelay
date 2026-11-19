package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrAuth      = errors.New("invalid credentials or expired session")
	ErrForbidden = errors.New("permission denied")
	ErrConflict  = errors.New("operation conflicts with current state")
	ErrInvalid   = errors.New("invalid input")
	ErrNotFound  = errors.New("record not found")
)

type Store struct {
	db          *sql.DB
	aead        cipher.AEAD
	popIndexKey []byte
}

type User struct {
	ID         int64
	Name, Role string
	Enabled    bool
}

type Session struct {
	User        User
	Token, CSRF string
}

const SessionLifetime = 12 * time.Hour

type Endpoint struct {
	Host, Security, Username, Password string
	Port                               int
}

type RouteSpec struct {
	Name                                                          string
	SourceProtocol                                                string
	Source, Target                                                Endpoint
	SourceFolder, Method, TargetFolder, Sender, Recipient, Policy string
}

type Route struct {
	ID, UserID int64
	RouteSpec
	State, Detail              string
	Validity, Cursor, Inflight uint32
	IMAPIdentityMode           bool
	RecoveryToken              string
	Delivered                  int64
	LastChecked, NextAttempt   time.Time
}

type Log struct {
	At           time.Time
	UID          uint32
	Kind, Detail string
}

const schema = `
CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS users(
 id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, password BLOB NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('admin','user')), enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS sessions(
 token BLOB PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 csrf TEXT NOT NULL, expires INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS session_expiry ON sessions(expires);
CREATE TABLE IF NOT EXISTS routes(
 id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 spec BLOB NOT NULL, state TEXT NOT NULL DEFAULT 'ready', detail TEXT NOT NULL DEFAULT '',
 validity INTEGER NOT NULL DEFAULT 0, cursor INTEGER NOT NULL DEFAULT 0, inflight INTEGER NOT NULL DEFAULT 0,
 delivered INTEGER NOT NULL DEFAULT 0, last_checked INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS route_owner ON routes(user_id);
CREATE TABLE IF NOT EXISTS imap_identity_routes(
 route_id INTEGER PRIMARY KEY REFERENCES routes(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS pop_messages(
 route_id INTEGER NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
 uid INTEGER NOT NULL CHECK(uid BETWEEN 1 AND 4294967295), fingerprint BLOB NOT NULL, uidl BLOB NOT NULL,
 PRIMARY KEY(route_id,uid), UNIQUE(route_id,fingerprint)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS logs(
 id INTEGER PRIMARY KEY, route_id INTEGER NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
 at INTEGER NOT NULL, uid INTEGER NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS log_route ON logs(route_id,id DESC);
`

func Open(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: encryption key must have 32 bytes", ErrInvalid)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_txlock=immediate"}
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	indexKey := hmac.New(sha256.New, key)
	indexKey.Write([]byte("emailrelay:v1:pop3-index"))
	s := &Store{db: db, aead: aead, popIndexKey: indexKey.Sum(nil)}
	if err = s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(schema); err != nil {
		return err
	}
	var verifier []byte
	err = tx.QueryRow("SELECT value FROM metadata WHERE key='key_check'").Scan(&verifier)
	const label = "emailrelay:v1:key_check"
	switch {
	case errors.Is(err, sql.ErrNoRows):
		blob, e := s.seal(nil, label)
		if e != nil {
			return e
		}
		if _, err = tx.Exec("INSERT INTO metadata VALUES('key_check',?)", blob); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err = s.unseal(verifier, label); err != nil {
			return errors.New("database encryption key verification failed")
		}
	}
	// Delivery's remote commit and the local cursor cannot share a transaction.
	// Preserve the pending UID for operator reconciliation after a process exit.
	const interrupted = "중계 도중 서비스가 중단되었습니다. 대상 메일함을 확인한 후 재시도하거나 건너뛰세요."
	if _, err = tx.Exec("INSERT INTO logs(route_id,at,uid,kind,detail) SELECT id,?,inflight,'uncertain',? FROM routes WHERE state='ready' AND inflight>0", time.Now().Unix(), interrupted); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE routes SET state='uncertain',detail=? WHERE state='ready' AND inflight>0", interrupted); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) seal(data []byte, label string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, data, []byte(label)), nil
}
func (s *Store) unseal(blob []byte, label string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(blob) < n {
		return nil, errors.New("invalid encrypted record")
	}
	return s.aead.Open(nil, blob[:n], blob[n:], []byte(label))
}
func (s *Store) Close() error { return s.db.Close() }

func mustChange(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrConflict
	}
	return nil
}

func publicTime(epoch int64) time.Time {
	if epoch == 0 {
		return time.Time{}
	}
	return time.Unix(epoch, 0).UTC()
}
