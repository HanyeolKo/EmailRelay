package httpapi

import (
	"errors"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"emailrelay/internal/store"
)

type endpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}
type routeForm struct {
	Name           string   `json:"name"`
	Source         endpoint `json:"source"`
	SourceProtocol string   `json:"source_protocol"`
	SourceFolder   string   `json:"source_folder"`
	Target         endpoint `json:"target"`
	Method         string   `json:"method"`
	TargetFolder   string   `json:"target_folder"`
	Sender         string   `json:"sender"`
	Recipient      string   `json:"recipient"`
	Initial        string   `json:"initial"`
}
type routeView struct {
	routeForm
	ID               int64  `json:"id"`
	Enabled          bool   `json:"enabled"`
	Status           string `json:"status"`
	Detail           string `json:"detail"`
	Delivered        int64  `json:"delivered"`
	Initialized      bool   `json:"initialized"`
	IMAPIdentityMode bool   `json:"imap_identity_mode,omitempty"`
	LastChecked      string `json:"last_checked"`
	NextAttempt      string `json:"next_attempt"`
	UncertainUID     uint32 `json:"uncertain_uid"`
	RecoveryToken    string `json:"recovery_token,omitempty"`
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
func publicEndpoint(e store.Endpoint) endpoint {
	return endpoint{Host: e.Host, Port: e.Port, Security: e.Security, Username: e.Username}
}
func publicRoute(r store.Route) routeView {
	var pending uint32
	if r.State == "uncertain" || r.State == "blocked" {
		pending = r.Inflight
	}
	return routeView{routeForm: routeForm{Name: r.Name, Source: publicEndpoint(r.Source), SourceProtocol: r.SourceProtocol, SourceFolder: r.SourceFolder, Target: publicEndpoint(r.Target), Method: r.Method, TargetFolder: r.TargetFolder, Sender: r.Sender, Recipient: r.Recipient, Initial: r.Policy}, ID: r.ID, Enabled: r.State != "paused", Status: r.State, Detail: r.Detail, Delivered: r.Delivered, Initialized: r.Validity != 0, IMAPIdentityMode: r.IMAPIdentityMode, LastChecked: ts(r.LastChecked), NextAttempt: ts(r.NextAttempt), UncertainUID: pending, RecoveryToken: r.RecoveryToken}
}
func (s *Server) routes(w http.ResponseWriter, r *http.Request, a store.Session) {
	rows, e := s.db.Routes(a.User.ID)
	if e != nil {
		report(w, e)
		return
	}
	out := make([]routeView, 0, len(rows))
	for _, row := range rows {
		out = append(out, publicRoute(row))
	}
	reply(w, 200, map[string]any{"routes": out})
}
func field(value string, max int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= max && strings.IndexFunc(value, unicode.IsControl) < 0
}
func validHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	if len(h) > 253 {
		return false
	}
	for _, part := range strings.Split(h, ".") {
		if len(part) == 0 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
func (e endpoint) spec(edit bool) (store.Endpoint, bool) {
	e.Host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(e.Host)), ".")
	e.Username = strings.TrimSpace(e.Username)
	return store.Endpoint{Host: e.Host, Port: e.Port, Security: e.Security, Username: e.Username, Password: e.Password}, validHost(e.Host) && e.Port >= 1 && e.Port <= 65535 && (e.Security == "tls" || e.Security == "starttls") && field(e.Username, 320) && (edit && e.Password == "" || field(e.Password, 2048))
}
func address(v string) bool {
	a, err := mail.ParseAddress(v)
	return field(v, 254) && err == nil && a.Address == v && a.Name == "" && strings.Count(v, "@") == 1 && !strings.ContainsAny(v, " <>\t")
}
func (f routeForm) spec(edit bool) (store.RouteSpec, string) {
	var out store.RouteSpec
	var ok bool
	if out.Source, ok = f.Source.spec(edit); !ok {
		return out, "소스 메일 서버 주소, 포트, 계정 및 비밀번호를 확인해 주세요."
	}
	if out.Target, ok = f.Target.spec(edit); !ok {
		return out, "대상 서버 주소, 포트, 계정 및 비밀번호를 확인해 주세요."
	}
	out.Name = strings.TrimSpace(f.Name)
	switch f.SourceProtocol {
	case "", "imap":
		out.SourceProtocol = "imap"
		out.SourceFolder = strings.TrimSpace(f.SourceFolder)
		if !field(out.SourceFolder, 512) {
			return out, "소스 IMAP 폴더 이름을 확인해 주세요."
		}
	case "pop3":
		out.SourceProtocol = "pop3"
	default:
		return out, "소스 수신 방식은 IMAP 또는 POP3를 선택해 주세요."
	}
	out.Method = f.Method
	out.Policy = f.Initial
	if !field(out.Name, 120) || (out.Policy != "new" && out.Policy != "all") {
		return out, "경로 이름과 가져올 범위를 확인해 주세요."
	}
	switch out.Method {
	case "imap":
		out.TargetFolder = strings.TrimSpace(f.TargetFolder)
		if !field(out.TargetFolder, 512) {
			return out, "대상 메일함의 기존 폴더 이름을 입력해 주세요."
		}
		if out.Source.Host == out.Target.Host && out.Source.Username == out.Target.Username && strings.EqualFold(out.SourceFolder, out.TargetFolder) {
			return out, "같은 메일함의 같은 폴더는 대상으로 지정할 수 없습니다."
		}
	case "smtp":
		out.Sender = strings.TrimSpace(f.Sender)
		out.Recipient = strings.TrimSpace(f.Recipient)
		if !address(out.Sender) || !address(out.Recipient) {
			return out, "보내는 주소와 받는 주소에 각각 이메일 주소 하나를 입력해 주세요."
		}
	default:
		return out, "전달 방식을 선택해 주세요."
	}
	return out, ""
}
func (s *Server) saveRoute(w http.ResponseWriter, r *http.Request, a store.Session) {
	var f routeForm
	if !decode(w, r, &f) {
		return
	}
	edit := r.Method == http.MethodPut
	spec, msg := f.spec(edit)
	if msg != "" {
		fail(w, 400, msg)
		return
	}
	if edit {
		id, ok := idOf(w, r)
		if !ok {
			return
		}
		e := s.jobs.EditRoute(id, func() error { return s.db.UpdateRoute(a.User.ID, id, spec) })
		if e != nil {
			report(w, e)
			return
		}
		s.jobs.Wake()
		w.WriteHeader(204)
		return
	}
	id, e := s.db.CreateRoute(a.User.ID, spec)
	if e != nil {
		report(w, e)
		return
	}
	s.jobs.Wake()
	reply(w, 201, map[string]any{"id": id})
}
func (s *Server) action(w http.ResponseWriter, r *http.Request, a store.Session) {
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	action := "delete"
	var in struct {
		Action        string `json:"action"`
		RecoveryToken string `json:"recovery_token"`
	}
	if r.Method != http.MethodDelete {
		if !decode(w, r, &in) {
			return
		}
		switch in.Action {
		case "pause", "resume", "retry", "skip":
			action = in.Action
		default:
			report(w, store.ErrInvalid)
			return
		}
	}
	e := s.jobs.EditRoute(id, func() error {
		if action == "retry" || action == "skip" {
			current, err := s.db.OwnedJob(a.User.ID, id)
			if err != nil {
				return err
			}
			if in.RecoveryToken != current.RecoveryToken {
				return store.ErrConflict
			}
		}
		return s.db.Action(a.User.ID, id, action)
	})
	if e != nil {
		report(w, e)
		return
	}
	s.jobs.Wake()
	w.WriteHeader(204)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request, a store.Session) {
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	rows, e := s.db.Logs(a.User.ID, id)
	if e != nil {
		report(w, e)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{"at": ts(row.At), "status": row.Kind, "message": row.Detail, "uid": row.UID})
	}
	reply(w, 200, map[string]any{"events": out})
}
func (s *Server) users(w http.ResponseWriter, r *http.Request, a store.Session) {
	rows, e := s.db.Users(a.User.ID)
	if e != nil {
		report(w, e)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, userView(u))
	}
	reply(w, 200, map[string]any{"users": out})
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request, a store.Session) {
	if a.User.Role != "admin" {
		report(w, store.ErrForbidden)
		return
	}
	var in struct{ Username, Password, Role string }
	if !decode(w, r, &in) {
		return
	}
	e := s.db.CreateUser(a.User.ID, in.Username, in.Password, in.Role)
	if e != nil {
		report(w, e)
		return
	}
	w.WriteHeader(201)
}
func (s *Server) testRoute(w http.ResponseWriter, r *http.Request, a store.Session) {
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	e := s.jobs.Test(r.Context(), a.User.ID, id)
	if e != nil {
		if errors.Is(e, store.ErrNotFound) || errors.Is(e, store.ErrConflict) {
			report(w, e)
			return
		}
		fail(w, 422, e.Error())
		return
	}
	reply(w, 200, map[string]string{"message": "소스와 대상 서버에 로그인했습니다. 대상 IMAP은 폴더 열기까지, SMTP는 로그인까지 확인합니다. 실제 수신 여부는 첫 메일의 전달 기록과 대상 메일함에서 확인하세요."})
}
func (s *Server) setUser(w http.ResponseWriter, r *http.Request, a store.Session) {
	if a.User.Role != "admin" {
		report(w, store.ErrForbidden)
		return
	}
	id, ok := idOf(w, r)
	if !ok {
		return
	}
	var in struct{ Enabled *bool }
	if !decode(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		report(w, store.ErrInvalid)
		return
	}
	e := s.jobs.EditUsers(id, func() error { return s.db.SetUserEnabled(a.User.ID, id, *in.Enabled) })
	if e != nil {
		report(w, e)
		return
	}
	s.jobs.Wake()
	w.WriteHeader(204)
}
