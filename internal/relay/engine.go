package relay

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"emailrelay/internal/mailwire"
	"emailrelay/internal/store"
)

type Transport interface {
	Discover(context.Context, mailwire.Endpoint, string, uint32, int) (uint32, uint32, []uint32, error)
	Fetch(context.Context, mailwire.Endpoint, string, uint32, uint32, int64) (mailwire.Message, error)
	ListIMAPIdentities(context.Context, int64, mailwire.Endpoint, string) ([]mailwire.IMAPIdentity, error)
	FetchIMAPIdentity(context.Context, int64, mailwire.Endpoint, string, mailwire.IMAPIdentity, int64) (mailwire.Message, error)
	POPList(context.Context, mailwire.Endpoint) ([]string, error)
	POPFetch(context.Context, mailwire.Endpoint, string, uint32, int64) (mailwire.Message, error)
	Deliver(context.Context, mailwire.Endpoint, string, string, string, string, mailwire.Message) error
	TestTarget(context.Context, mailwire.Endpoint, string, string) error
}

type Engine struct {
	db       *store.Store
	wire     Transport
	interval time.Duration
	maxBytes int64
	wake     chan struct{}
	probes   chan struct{}
	users    sync.RWMutex
	routes   [64]sync.Mutex
	scansMu  sync.Mutex
	scans    map[int64]mailboxScan
}

type mailboxScan struct {
	owner  int64
	cancel context.CancelFunc
}

func New(db *store.Store, wire Transport, interval time.Duration, maxBytes int64) *Engine {
	return &Engine{db: db, wire: wire, interval: interval, maxBytes: maxBytes, wake: make(chan struct{}, 1), probes: make(chan struct{}, 2), scans: make(map[int64]mailboxScan)}
}
func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) EditRoute(id int64, fn func() error) error {
	return e.withRoute(id, func() error {
		if err := fn(); err != nil {
			return err
		}
		e.scansMu.Lock()
		defer e.scansMu.Unlock()
		if scan, ok := e.scans[id]; ok {
			scan.cancel()
		}
		return nil
	})
}
func (e *Engine) withRoute(id int64, fn func() error) error {
	e.users.RLock()
	defer e.users.RUnlock()
	m := &e.routes[uint64(id)%uint64(len(e.routes))]
	m.Lock()
	defer m.Unlock()
	return fn()
}
func (e *Engine) EditUsers(id int64, fn func() error) error {
	e.users.Lock()
	defer e.users.Unlock()
	if err := fn(); err != nil {
		return err
	}
	e.scansMu.Lock()
	defer e.scansMu.Unlock()
	for _, scan := range e.scans {
		if scan.owner == id {
			scan.cancel()
		}
	}
	return nil
}
func endpoint(s store.Endpoint) mailwire.Endpoint {
	return mailwire.Endpoint{Host: s.Host, Port: s.Port, Security: s.Security, Username: s.Username, Password: s.Password}
}

func (e *Engine) Test(ctx context.Context, owner, id int64) error {
	select {
	case e.probes <- struct{}{}:
		defer func() { <-e.probes }()
	default:
		return store.ErrConflict
	}
	r, err := e.db.OwnedJob(owner, id)
	if errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err != nil {
		return errors.New("저장된 경로를 읽을 수 없습니다.")
	}
	c, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if r.SourceProtocol == "pop3" {
		_, err = e.wire.POPList(c, endpoint(r.Source))
	} else {
		_, _, _, err = e.wire.Discover(c, endpoint(r.Source), r.SourceFolder, 0, 0)
		// The read-only folder probe succeeded. Building its content index is
		// the worker's job; a connection test must never seed the baseline.
		if errors.Is(err, mailwire.ErrNoUIDValidity) {
			err = nil
		}
	}
	if err != nil {
		return errors.New(reason("소스 메일함 로그인", err))
	}
	if err = e.wire.TestTarget(c, endpoint(r.Target), r.Method, r.TargetFolder); err != nil {
		return errors.New(reason("대상 메일함 로그인", err))
	}
	return nil
}

func (e *Engine) Run(ctx context.Context) {
	// A large mailbox can take several minutes to index. Other routes keep
	// their normal polling cadence, with at most one worker per route.
	const capacity = 3
	ready := func() []store.Route {
		routes, err := e.db.Ready()
		if err != nil {
			log.Print("relay: could not read eligible routes")
		}
		return routes
	}
	queue := ready()
	active := make(map[int64]bool)
	done := make(chan int64, capacity)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	for ctx.Err() == nil {
		for len(active) < capacity && len(queue) > 0 {
			id := queue[0].ID
			queue = queue[1:]
			if active[id] {
				continue
			}
			active[id] = true
			workers.Add(1)
			go func() {
				defer workers.Done()
				if err := e.runRoute(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) && ctx.Err() == nil {
					log.Printf("relay: could not persist progress for route %d", id)
				}
				done <- id
			}()
		}
		select {
		case <-ctx.Done():
			return
		case id := <-done:
			delete(active, id)
		case <-ticker.C:
			queue = ready()
		case <-e.wake:
			queue = ready()
		}
	}
}

const batchSize = 12
const operationTimeout = 35 * time.Second

func (e *Engine) runRoute(ctx context.Context, id int64) error {
	var ids []uint32
	var popNumbers map[string]uint32
	var generation uint32
	var scan *store.Route
	var scanCtx context.Context
	var cancelScan context.CancelFunc
	startScan := func(r store.Route) {
		scan = &r
		scanCtx, cancelScan = context.WithTimeout(ctx, 20*time.Minute)
		e.scansMu.Lock()
		e.scans[id] = mailboxScan{owner: r.UserID, cancel: cancelScan}
		e.scansMu.Unlock()
	}
	var identityHints map[string]mailwire.IMAPIdentity
	err := e.withRoute(id, func() error {
		r, err := e.db.Job(id)
		if err != nil {
			return err
		}
		if r.IMAPIdentityMode {
			startScan(r)
			return nil
		}
		c, cancel := context.WithTimeout(ctx, operationTimeout)
		defer cancel()
		if r.SourceProtocol == "pop3" {
			uidls, err := e.wire.POPList(c, endpoint(r.Source))
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return e.db.Fail(id, 0, reason("POP3 메일 목록 확인", err), false)
			}
			generation = 1
			ids, err = e.db.PreparePOP(id, uidls, batchSize)
			if err != nil {
				return err
			}
			// These numbers only speed up retrieval. POPFetch rechecks the UIDL
			// on its own connection before using any number to read a message.
			if len(ids) > 0 {
				popNumbers = make(map[string]uint32, len(uidls))
				for i, uidl := range uidls {
					popNumbers[uidl] = uint32(i + 1)
				}
			}
			return e.db.Checked(id, len(ids) == 0)
		}
		limit := batchSize
		if r.Validity == 0 && r.Policy == "new" {
			limit = 0
		}
		validity, highest, found, err := e.wire.Discover(c, endpoint(r.Source), r.SourceFolder, r.Cursor, limit)
		if errors.Is(err, mailwire.ErrNoUIDValidity) {
			if r.Validity != 0 {
				return e.invalid(id)
			}
			startScan(r)
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return e.db.Fail(id, 0, reason("소스 메일함 확인", err), false)
		}
		if validity == 0 {
			return e.db.Fail(id, 0, "소스 서버에서 유효한 UIDVALIDITY를 받지 못했습니다.", false)
		}
		if r.Validity != 0 && r.Validity != validity {
			return e.invalid(id)
		}
		if r.Validity == 0 {
			var start uint32
			if r.Policy == "new" {
				start = highest
			}
			if err = e.db.Initialize(id, validity, start); err != nil {
				return err
			}
		}
		generation = validity
		ids = found
		return e.db.Checked(id, len(found) == 0)
	})
	if err != nil {
		return err
	}
	if scan != nil {
		// Leave route edits available during a long initial scan. The exact
		// source and initial policy must still match when committing the snapshot.
		defer cancelScan()
		entries, scanErr := e.wire.ListIMAPIdentities(scanCtx, id, endpoint(scan.Source), scan.SourceFolder)
		err = e.withRoute(id, func() error {
			e.scansMu.Lock()
			delete(e.scans, id)
			e.scansMu.Unlock()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(scanCtx.Err(), context.Canceled) {
				return store.ErrConflict
			}
			current, err := e.db.Job(id)
			if err != nil {
				return err
			}
			if current.Source != scan.Source || current.SourceProtocol != "imap" || current.SourceFolder != scan.SourceFolder ||
				current.Policy != scan.Policy || current.Validity != scan.Validity || current.IMAPIdentityMode != scan.IMAPIdentityMode {
				return store.ErrConflict
			}
			if scanCtx.Err() != nil {
				scanErr = scanCtx.Err()
			}
			if scanErr != nil {
				return e.db.Fail(id, 0, reason("네이버 메일 식별 정보 확인", scanErr), false)
			}
			var keys []string
			identityHints = make(map[string]mailwire.IMAPIdentity, len(entries))
			for _, entry := range entries {
				keys = append(keys, entry.Key)
				identityHints[entry.Key] = entry
			}
			ids, err = e.db.PrepareIMAPIdentities(id, keys, batchSize)
			if err != nil {
				return err
			}
			generation = 1 // store-local numbering; mode is tracked separately
			return e.db.Checked(id, len(ids) == 0)
		})
		if err != nil {
			return err
		}
	}
	for _, uid := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var stop bool
		err = e.withRoute(id, func() error {
			r, err := e.db.Job(id)
			if err != nil {
				return err
			}
			if r.Validity != generation || r.IMAPIdentityMode != (scan != nil) || r.Cursor >= uid {
				return nil
			}
			stop, err = e.copyOne(ctx, r, uid, popNumbers, identityHints)
			return err
		})
		if err != nil || stop {
			return err
		}
	}
	return nil
}
func (e *Engine) invalid(id int64) error {
	return e.db.Invalidate(id, "소스 폴더의 UIDVALIDITY가 변경되었습니다. 가져올 범위를 선택해 경로를 다시 만들어 주세요.")
}
func (e *Engine) copyOne(ctx context.Context, r store.Route, uid uint32, popNumbers map[string]uint32, identityHints map[string]mailwire.IMAPIdentity) (bool, error) {
	c, cancel := context.WithTimeout(ctx, operationTimeout)
	var msg mailwire.Message
	var err error
	if r.SourceProtocol == "pop3" {
		uidl, lookupErr := e.db.POPUIDL(r.ID, uid)
		if lookupErr != nil {
			cancel()
			return true, lookupErr
		}
		msg, err = e.wire.POPFetch(c, endpoint(r.Source), uidl, popNumbers[uidl], e.maxBytes)
		msg.UID = uid
	} else if r.IMAPIdentityMode {
		key, lookupErr := e.db.IMAPIdentityKey(r.ID, uid)
		if lookupErr != nil {
			cancel()
			return true, lookupErr
		}
		hint, found := identityHints[key]
		if !found {
			cancel()
			return false, e.db.MissingIMAPIdentity(r.ID, uid)
		}
		msg, err = e.wire.FetchIMAPIdentity(c, r.ID, endpoint(r.Source), r.SourceFolder, hint, e.maxBytes)
		msg.UID = uid
	} else {
		msg, err = e.wire.Fetch(c, endpoint(r.Source), r.SourceFolder, r.Validity, uid, e.maxBytes)
	}
	cancel()
	switch {
	case r.IMAPIdentityMode && (errors.Is(err, mailwire.ErrIdentityChanged) || errors.Is(err, mailwire.ErrVanished)):
		// A changed hint is not a delivery decision. Keep the local number
		// pending until a subsequent complete inventory confirms its identity.
		return true, e.db.Fail(r.ID, 0, "소스 메일 번호가 바뀌어 메일 식별 정보를 다시 조회합니다.", false)
	case errors.Is(err, mailwire.ErrUIDValidity):
		return true, e.invalid(r.ID)
	case errors.Is(err, mailwire.ErrVanished):
		if r.SourceProtocol == "pop3" {
			return false, e.db.MissingPOP(r.ID, uid)
		}
		return false, e.db.Finish(r.ID, uid, "skipped", "소스에서 사라진 메일을 건너뛰었습니다.")
	case errors.Is(err, mailwire.ErrLoop):
		return false, e.db.Finish(r.ID, uid, "skipped", "이미 중계된 메일이어서 순환 전송을 방지했습니다.")
	case errors.Is(err, mailwire.ErrTooLarge):
		return true, e.db.Hold(r.ID, uid, "크기 제한을 초과했습니다. 서버 제한을 조정한 뒤 재시도하거나 이 메일을 건너뛰세요.")
	case err != nil:
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, e.db.Fail(r.ID, 0, reason("소스 메일 읽기", err), false)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err = e.db.Begin(r.ID, r.Validity, uid); err != nil {
		return true, err
	}
	c, cancel = context.WithTimeout(ctx, operationTimeout)
	err = e.wire.Deliver(c, endpoint(r.Target), r.Method, r.TargetFolder, r.Sender, r.Recipient, msg)
	cancel()
	if errors.Is(err, mailwire.ErrLoop) {
		return false, e.db.Finish(r.ID, uid, "skipped", "이미 중계된 메일이어서 순환 전송을 방지했습니다.")
	}
	if errors.Is(err, mailwire.ErrAmbiguous) {
		return true, e.db.Fail(r.ID, uid, "대상 서버의 최종 응답을 받지 못했습니다. 대상 메일함에서 수신 여부를 확인해 주세요.", true)
	}
	if err != nil {
		return true, e.db.Fail(r.ID, uid, reason("대상 메일 전송", err), false)
	}
	return false, e.db.Finish(r.ID, uid, "delivered", "대상 서버가 메일을 받았습니다.")
}

// Never persist raw server replies: some providers include account details.
func reason(stage string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return stage + " 시간이 초과되었습니다. 서버 주소와 네트워크를 확인해 주세요."
	}
	switch {
	case errors.Is(err, mailwire.ErrIMAPInventory):
		return stage + " 실패. 네이버가 응답한 전체 메일 수와 조회 목록을 다시 확인해야 합니다."
	case errors.Is(err, mailwire.ErrIMAPSizeMismatch):
		return stage + " 실패. 서버가 알린 메일 크기와 받은 원문 길이가 서로 다릅니다."
	case errors.Is(err, mailwire.ErrIMAPMessageData):
		return stage + " 실패. 네이버에서 메일 식별에 필요한 원문 또는 수신 시각을 온전히 읽지 못했습니다."
	case errors.Is(err, mailwire.ErrNetworkPolicy):
		return stage + " 실패. 서버 주소가 허용된 외부 메일 서버 네트워크에 속하지 않습니다."
	case errors.Is(err, mailwire.ErrAuth):
		return stage + " 실패. 계정, 앱 비밀번호, 공급자의 외부 메일 앱 접근 설정을 확인해 주세요."
	case errors.Is(err, mailwire.ErrUIDL):
		return stage + " 실패. POP3 서버에서 중복 없이 메일을 구별할 UIDL 목록을 받지 못했습니다. 서버의 UIDL 지원을 확인해 주세요."
	case errors.Is(err, mailwire.ErrTLS):
		return stage + " 실패. TLS 방식과 서버 인증서의 도메인 및 유효성을 확인해 주세요."
	case errors.Is(err, mailwire.ErrConnect):
		return stage + " 실패. 메일 서버 주소와 포트, 방화벽 설정을 확인해 주세요."
	case errors.Is(err, mailwire.ErrDestination):
		return stage + " 실패. 대상 폴더의 쓰기 권한, 발신 주소의 사용 권한, 서버 용량을 확인해 주세요."
	}
	return stage + " 실패. 서버 주소·포트, TLS 방식, 로그인 정보 및 메일함 권한을 확인해 주세요."
}
