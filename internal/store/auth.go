package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

func normalizedName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || len(name) > 128 || !utf8.ValidString(name) || strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", fmt.Errorf("%w: username must contain 1–128 bytes without whitespace", ErrInvalid)
	}
	return name, nil
}

func hashPassword(password string) ([]byte, error) {
	if len(password) < 12 || len(password) > 72 {
		return nil, fmt.Errorf("%w: password must contain 12–72 bytes", ErrInvalid)
	}
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

func (s *Store) HasUsers() (bool, error) {
	var exists bool
	err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM users)").Scan(&exists)
	return exists, err
}

func (s *Store) Bootstrap(name, password string) error {
	name, err := normalizedName(name)
	if err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return mustChange(s.db.Exec("INSERT INTO users(name,password,role) SELECT ?,?,'admin' WHERE NOT EXISTS (SELECT 1 FROM users)", name, hash))
}

func admin(tx *sql.Tx, id int64) error {
	var allowed bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND role='admin' AND enabled=1)", id).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func (s *Store) CreateUser(adminID int64, name, password, role string) error {
	name, err := normalizedName(name)
	if err != nil {
		return err
	}
	if role != "admin" && role != "user" {
		return ErrInvalid
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = admin(tx, adminID); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO users(name,password,role) VALUES(?,?,?)", name, hash, role); err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique {
			return ErrConflict
		}
		return err
	}
	return tx.Commit()
}

func (s *Store) Users(adminID int64) ([]User, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = admin(tx, adminID); err != nil {
		return nil, err
	}
	rows, err := tx.Query("SELECT id,name,role,enabled FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var u User
		if err = rows.Scan(&u.ID, &u.Name, &u.Role, &u.Enabled); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) SetUserEnabled(adminID, id int64, enabled bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = admin(tx, adminID); err != nil {
		return err
	}
	if adminID == id && !enabled {
		return ErrConflict
	}
	res, err := tx.Exec("UPDATE users SET enabled=? WHERE id=?", enabled, id)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	if !enabled {
		if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ChangePassword(id int64, current, password string) error {
	if len(current) > 72 {
		return ErrAuth
	}
	newHash, err := hashPassword(password)
	if err != nil {
		return err
	}
	var prev []byte
	err = s.db.QueryRow("SELECT password FROM users WHERE id=? AND enabled=1", id).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAuth
	}
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword(prev, []byte(current)) != nil {
		return ErrAuth
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = mustChange(tx.Exec("UPDATE users SET password=? WHERE id=? AND password=? AND enabled=1", newHash, id, prev)); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

// Equalize bcrypt work for unknown names and incorrect passwords.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-probe-only"), bcrypt.DefaultCost)

func (s *Store) Login(name, password string) (Session, error) {
	if len(password) > 72 {
		return Session{}, ErrAuth
	}
	var sess Session
	name = strings.ToLower(strings.TrimSpace(name))
	var hash []byte
	err := s.db.QueryRow("SELECT id,name,role,enabled,password FROM users WHERE name=?", name).Scan(&sess.User.ID, &sess.User.Name, &sess.User.Role, &sess.User.Enabled, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		hash = dummyHash
	} else if err != nil {
		return Session{}, err
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || !sess.User.Enabled {
		return Session{}, ErrAuth
	}
	sess.Token, err = randomToken()
	if err != nil {
		return Session{}, err
	}
	sess.CSRF, err = randomToken()
	if err != nil {
		return Session{}, err
	}
	digest := sha256.Sum256([]byte(sess.Token))
	now := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM sessions WHERE expires<=?", now.Unix()); err != nil {
		return Session{}, err
	}
	err = mustChange(tx.Exec("INSERT INTO sessions(token,user_id,csrf,expires) SELECT ?,id,?,? FROM users WHERE id=? AND enabled=1 AND password=?", digest[:], sess.CSRF, now.Add(SessionLifetime).Unix(), sess.User.ID, hash))
	if errors.Is(err, ErrConflict) {
		return Session{}, ErrAuth
	}
	if err != nil {
		return Session{}, err
	}
	return sess, tx.Commit()
}

func (s *Store) Authenticate(token string) (Session, error) {
	if len(token) != 43 {
		return Session{}, ErrAuth
	}
	digest := sha256.Sum256([]byte(token))
	sess := Session{Token: token}
	err := s.db.QueryRow("SELECT u.id,u.name,u.role,u.enabled,s.csrf FROM sessions s JOIN users u ON s.user_id=u.id WHERE s.token=? AND s.expires>? AND u.enabled=1", digest[:], time.Now().Unix()).Scan(&sess.User.ID, &sess.User.Name, &sess.User.Role, &sess.User.Enabled, &sess.CSRF)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrAuth
	}
	return sess, err
}

func (s *Store) Logout(token string) error {
	digest := sha256.Sum256([]byte(token))
	_, err := s.db.Exec("DELETE FROM sessions WHERE token=?", digest[:])
	return err
}
