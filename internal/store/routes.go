package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func endpointValid(e Endpoint) bool {
	return e.Host != "" && e.Port > 0 && e.Port <= 65535 && (e.Security == "tls" || e.Security == "starttls") && e.Username != "" && e.Password != ""
}

func validSpec(spec RouteSpec) bool {
	if spec.Name == "" || !endpointValid(spec.Source) || !endpointValid(spec.Target) || (spec.Policy != "new" && spec.Policy != "all") {
		return false
	}
	if spec.SourceProtocol != "pop3" && (spec.SourceProtocol != "imap" || spec.SourceFolder == "") {
		return false
	}
	switch spec.Method {
	case "imap":
		return spec.TargetFolder != ""
	case "smtp":
		return spec.Sender != "" && spec.Recipient != ""
	default:
		return false
	}
}

func (spec *RouteSpec) normalizeSource() {
	if spec.SourceProtocol == "" {
		spec.SourceProtocol = "imap"
	}
	if spec.SourceProtocol == "pop3" {
		spec.SourceFolder = ""
	}
}

func binding(userID, id int64) string { return fmt.Sprintf("emailrelay:v1:route:%d:%d", userID, id) }
func (s *Store) writeSpec(tx *sql.Tx, userID, id int64, spec RouteSpec) error {
	if !validSpec(spec) {
		return ErrInvalid
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	blob, err := s.seal(raw, binding(userID, id))
	if err != nil {
		return err
	}
	return mustChange(tx.Exec("UPDATE routes SET spec=? WHERE id=? AND user_id=?", blob, id, userID))
}

func (s *Store) CreateRoute(userID int64, spec RouteSpec) (int64, error) {
	spec.normalizeSource()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("INSERT INTO routes(user_id,spec) SELECT id,X'' FROM users WHERE id=? AND enabled=1", userID)
	if err = mustChange(res, err); err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err = s.writeSpec(tx, userID, id, spec); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func sameEndpoint(a, b Endpoint) bool {
	return strings.EqualFold(a.Host, b.Host) && a.Port == b.Port && a.Username == b.Username && a.Security == b.Security
}

const identityMode = "EXISTS(SELECT 1 FROM imap_identity_routes i WHERE i.route_id=r.id)"
const latestRecoveryEvent = "CASE WHEN r.state IN ('uncertain','blocked') THEN COALESCE((SELECT l.id FROM logs l WHERE l.route_id=r.id ORDER BY l.id DESC LIMIT 1),0) ELSE 0 END"
const routeCols = "r.id,r.user_id,r.spec,r.state,r.detail,r.validity,r.cursor,r.inflight,r.delivered,r.last_checked,r.next_attempt," + identityMode + "," + latestRecoveryEvent
const fromRoutes = " FROM routes r JOIN users u ON u.id=r.user_id WHERE u.enabled=1 "

type scanner interface{ Scan(...any) error }

func (s *Store) scanRoute(row scanner) (Route, error) {
	var r Route
	var blob []byte
	var checked, next, recoveryEvent int64
	err := row.Scan(&r.ID, &r.UserID, &blob, &r.State, &r.Detail, &r.Validity, &r.Cursor, &r.Inflight, &r.Delivered, &checked, &next, &r.IMAPIdentityMode, &recoveryEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return Route{}, ErrNotFound
	}
	if err != nil {
		return Route{}, err
	}
	raw, err := s.unseal(blob, binding(r.UserID, r.ID))
	if err != nil {
		return Route{}, err
	}
	if err = json.Unmarshal(raw, &r.RouteSpec); err != nil {
		return Route{}, err
	}
	r.normalizeSource()
	r.LastChecked = publicTime(checked)
	r.NextAttempt = publicTime(next)
	if r.State == "uncertain" || r.State == "blocked" {
		// A retry of the same message can produce a second ambiguous receipt.
		// Bind a confirmation to that event without exposing global log IDs.
		mac := hmac.New(sha256.New, s.popIndexKey)
		fmt.Fprintf(mac, "emailrelay:v1:recovery:%d:%d:%d:%s:%d", r.UserID, r.ID, recoveryEvent, r.State, r.Inflight)
		r.RecoveryToken = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	return r, nil
}
func (s *Store) UpdateRoute(userID, id int64, spec RouteSpec) error {
	spec.normalizeSource()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := s.scanRoute(tx.QueryRow("SELECT "+routeCols+fromRoutes+"AND r.user_id=? AND r.id=?", userID, id))
	if err != nil {
		return err
	}
	stableSource := old.SourceProtocol == spec.SourceProtocol && sameEndpoint(old.Source, spec.Source)
	stableTarget := old.Method == spec.Method && sameEndpoint(old.Target, spec.Target)
	if old.Validity != 0 && (!stableSource || old.SourceFolder != spec.SourceFolder || old.Policy != spec.Policy) {
		return ErrConflict
	}
	// Until the operator resolves an ambiguous receipt, the configured
	// destination must still identify the endpoint used for that submission.
	if old.State == "uncertain" && (!stableTarget ||
		old.Method == "imap" && old.TargetFolder != spec.TargetFolder ||
		old.Method == "smtp" && (old.Sender != spec.Sender || old.Recipient != spec.Recipient)) {
		return ErrConflict
	}
	if spec.Source.Password == "" {
		if !stableSource {
			return ErrInvalid
		}
		spec.Source.Password = old.Source.Password
	}
	if spec.Target.Password == "" {
		if !stableTarget {
			return ErrInvalid
		}
		spec.Target.Password = old.Target.Password
	}
	if err = s.writeSpec(tx, userID, id, spec); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE routes SET state='ready',detail='',next_attempt=0,failures=0 WHERE id=? AND state='error' AND inflight=0", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Routes(userID int64) ([]Route, error) {
	rows, err := s.db.Query("SELECT "+routeCols+fromRoutes+"AND r.user_id=? ORDER BY r.id DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Route, 0)
	for rows.Next() {
		r, e := s.scanRoute(rows)
		if e != nil {
			return nil, e
		}
		r.Source.Password = ""
		r.Target.Password = ""
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Logs(userID, id int64) ([]Log, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owned bool
	err = tx.QueryRow("SELECT EXISTS(SELECT 1"+fromRoutes+"AND r.id=? AND r.user_id=?)", id, userID).Scan(&owned)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, ErrNotFound
	}
	rows, err := tx.Query("SELECT at,uid,kind,detail FROM logs WHERE route_id=? ORDER BY id DESC LIMIT 100", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Log, 0)
	for rows.Next() {
		var l Log
		var at int64
		if err = rows.Scan(&at, &l.UID, &l.Kind, &l.Detail); err != nil {
			return nil, err
		}
		l.At = publicTime(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Keep a bounded history per route. Message contents never enter these rows.
func record(tx *sql.Tx, id int64, uid uint32, kind, detail string) error {
	if _, err := tx.Exec("INSERT INTO logs(route_id,at,uid,kind,detail) VALUES(?,?,?,?,?)", id, time.Now().Unix(), uid, kind, detail); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM logs WHERE route_id=? AND id <= (SELECT id FROM logs WHERE route_id=? ORDER BY id DESC LIMIT 1 OFFSET 200)", id, id)
	return err
}

func (s *Store) Action(userID, id int64, action string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	var pending uint32
	err = tx.QueryRow("SELECT r.state,r.inflight"+fromRoutes+"AND r.id=? AND r.user_id=?", id, userID).Scan(&state, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if action == "delete" {
		if _, err = tx.Exec("DELETE FROM routes WHERE id=? AND user_id=?", id, userID); err != nil {
			return err
		}
		return tx.Commit()
	}
	var detail string
	switch action {
	case "pause":
		if (state != "ready" && state != "error") || pending != 0 {
			return ErrConflict
		}
		state = "paused"
		detail = "사용자가 경로를 일시 중지했습니다."
	case "resume":
		if state != "paused" {
			return ErrConflict
		}
		state = "ready"
		detail = "사용자가 경로를 재개했습니다."
	case "retry":
		if state != "uncertain" && state != "blocked" && state != "error" {
			return ErrConflict
		}
		state = "ready"
		detail = "사용자가 재시도를 선택했습니다."
	case "skip":
		if (state != "uncertain" && state != "blocked") || pending == 0 {
			return ErrConflict
		}
		if err = mustChange(tx.Exec("UPDATE routes SET cursor=? WHERE id=? AND cursor<?", pending, id, pending)); err != nil {
			return err
		}
		state = "ready"
		detail = "사용자가 이 메일을 건너뛰었습니다."
	default:
		return ErrInvalid
	}
	if _, err = tx.Exec("UPDATE routes SET state=?,detail='',inflight=0,failures=0,next_attempt=0 WHERE id=?", state, id); err != nil {
		return err
	}
	if err = record(tx, id, pending, action, detail); err != nil {
		return err
	}
	return tx.Commit()
}
