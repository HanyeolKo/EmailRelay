package store

import (
	"time"
)

const eligible = "AND r.inflight=0 AND (r.state='ready' OR (r.state='error' AND r.next_attempt<=?))"

// Ready takes a lightweight snapshot. Job reloads settings under the worker's
// per-route lock immediately before making any remote request.
func (s *Store) Ready() ([]Route, error) {
	rows, err := s.db.Query("SELECT r.id"+fromRoutes+eligible+" ORDER BY r.last_checked,r.id", time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Route, 0)
	for rows.Next() {
		var r Route
		if err = rows.Scan(&r.ID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) Job(id int64) (Route, error) {
	return s.scanRoute(s.db.QueryRow("SELECT "+routeCols+fromRoutes+eligible+" AND r.id=?", time.Now().Unix(), id))
}

func (s *Store) OwnedJob(userID, id int64) (Route, error) {
	return s.scanRoute(s.db.QueryRow("SELECT "+routeCols+fromRoutes+"AND r.user_id=? AND r.id=?", userID, id))
}

func (s *Store) Initialize(id int64, validity, cursor uint32) error {
	if validity == 0 {
		return ErrInvalid
	}
	return mustChange(s.db.Exec("UPDATE routes SET validity=?,cursor=? WHERE id=? AND validity=0 AND inflight=0 AND state IN ('ready','error')", validity, cursor, id))
}

func (s *Store) Checked(id int64, empty bool) error {
	return mustChange(s.db.Exec("UPDATE routes SET state='ready',detail='',last_checked=?,next_attempt=0,failures=CASE WHEN ? THEN 0 ELSE failures END WHERE id=? AND state IN ('ready','error') AND inflight=0", time.Now().Unix(), empty, id))
}

func (s *Store) Begin(id int64, validity, uid uint32) error {
	if validity == 0 || uid == 0 {
		return ErrInvalid
	}
	return mustChange(s.db.Exec("UPDATE routes SET inflight=?,state='ready' WHERE id=? AND state='ready' AND validity=? AND cursor<? AND inflight=0", uid, id, validity, uid))
}

func (s *Store) Finish(id int64, uid uint32, kind, detail string) error {
	if uid == 0 || (kind != "delivered" && kind != "skipped") {
		return ErrInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = mustChange(tx.Exec(`UPDATE routes SET cursor=?,inflight=0,state='ready',detail='',failures=0,next_attempt=0,last_checked=?,delivered=delivered+?
	WHERE id=? AND validity>0 AND state='ready' AND cursor<? AND (inflight=? OR (inflight=0 AND ?='skipped'))`, uid, time.Now().Unix(), kind == "delivered", id, uid, uid, kind))
	if err != nil {
		return err
	}
	if err = record(tx, id, uid, kind, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Fail(id int64, uid uint32, detail string, ambiguous bool) error {
	if ambiguous && uid == 0 {
		return ErrInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	state := "error"
	if ambiguous {
		state = "uncertain"
		err = mustChange(tx.Exec("UPDATE routes SET state=?,detail=?,last_checked=?,next_attempt=0 WHERE id=? AND state='ready' AND inflight=?", state, detail, now, id, uid))
	} else {
		err = mustChange(tx.Exec(`UPDATE routes SET state=?,detail=?,last_checked=?,inflight=0,next_attempt=?+MIN(3600,60*(1<<MIN(failures,6))),failures=MIN(failures+1,7)
		WHERE id=? AND state IN ('ready','error') AND inflight=?`, state, detail, now, now, id, uid))
	}
	if err != nil {
		return err
	}
	if err = record(tx, id, uid, state, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Hold(id int64, uid uint32, detail string) error {
	if uid == 0 {
		return ErrInvalid
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = mustChange(tx.Exec("UPDATE routes SET state='blocked',detail=?,inflight=?,last_checked=?,next_attempt=0 WHERE id=? AND state='ready' AND validity>0 AND cursor<? AND inflight=0", detail, uid, time.Now().Unix(), id, uid))
	if err != nil {
		return err
	}
	if err = record(tx, id, uid, "blocked", detail); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Invalidate(id int64, detail string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = mustChange(tx.Exec("UPDATE routes SET state='invalid',detail=?,last_checked=?,next_attempt=0 WHERE id=? AND state IN ('ready','error') AND inflight=0", detail, time.Now().Unix(), id))
	if err != nil {
		return err
	}
	if err = record(tx, id, 0, "invalid", detail); err != nil {
		return err
	}
	return tx.Commit()
}
