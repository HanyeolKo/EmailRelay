"use strict";

const main = document.querySelector("#main");
const nav = document.querySelector("#navigation");
const modal = document.querySelector("#editor");
const dialogContent = document.querySelector("#dialog-content");
const app = { session: null, routes: [], users: [], page: "", revision: 0, dialogRevision: 0, pending: false };
const fmtNumber = new Intl.NumberFormat("ko-KR");
const fmtDate = new Intl.DateTimeFormat("ko-KR", { dateStyle: "medium", timeStyle: "short" });
let noticeTimer;
let initialToken = "";
if (location.hash.startsWith("#setup=")) {
  initialToken = new URLSearchParams(location.hash.slice(1)).get("setup") || "";
  history.replaceState(null, "", location.pathname + location.search);
}

function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

function time(value) {
  if (!value || String(value).startsWith("0001-")) return "아직 확인 전";
  const date = new Date(typeof value === "number" && value < 1e11 ? value * 1000 : value);
  return Number.isNaN(date.getTime()) ? "시간 미상" : fmtDate.format(date);
}

async function api(path, method = "GET", payload) {
  const headers = { Accept: "application/json" };
  if (method !== "GET") {
    headers["Content-Type"] = "application/json";
    if (app.session?.csrf) headers["X-CSRF-Token"] = app.session.csrf;
  }
  let response;
  try {
    response = await fetch("/api/" + path, { method, headers, credentials: "same-origin", cache: "no-store", body: payload === undefined ? undefined : JSON.stringify(payload) });
  } catch {
    throw new Error("서버에 연결할 수 없습니다. 연결을 확인한 뒤 다시 시도해 주세요.");
  }
  const data = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (!response.ok) {
    if (response.status === 401 && app.session?.user && !["login", "password"].includes(path)) {
      if (modal.open) modal.close();
      app.session = null;
      void bootstrap();
    }
    throw new Error(data.error || `요청을 처리하지 못했습니다 (${response.status}).`);
  }
  return data;
}

function announce(message) {
  const notice = document.querySelector("#notice");
  notice.textContent = message;
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => { notice.textContent = ""; }, 5000);
}

const mailIcon = '<span class="mail-icon" aria-hidden="true"><i></i></span>';
const errorSlot = '<p class="error" role="alert" tabindex="-1" hidden></p>';
function showError(root, message) {
  const slot = root.querySelector(".error");
  if (!slot) { announce(message); return; }
  slot.textContent = message;
  slot.hidden = false;
  slot.focus();
}

function field(name, label, opts = {}) {
  const { type = "text", value = "", placeholder = "", help = "", complete = "off",
    required = true, disabled = false, min = "", max = "", minlength = "", full = false } = opts;
  return `<label class="field${full ? " full" : ""}"><span>${esc(label)}</span>
    <input name="${esc(name)}" type="${esc(type)}" value="${esc(value)}" placeholder="${esc(placeholder)}" autocomplete="${esc(complete)}"
    ${min ? `min="${esc(min)}"` : ""} ${max ? `max="${esc(max)}"` : ""} ${minlength ? `minlength="${esc(minlength)}"` : ""}
    ${required ? "required" : ""} ${disabled ? "disabled" : ""}>
    ${help ? `<small class="hint">${esc(help)}</small>` : ""}</label>`;
}

function select(name, label, options, value, { disabled = false, full = false, help = "" } = {}) {
  return `<label class="field${full ? " full" : ""}"><span>${esc(label)}</span>
    <select name="${esc(name)}" ${disabled ? "disabled" : ""}>
    ${options.map(([k, v]) => `<option value="${esc(k)}" ${k === value ? "selected" : ""}>${esc(v)}</option>`).join("")}</select>
    ${help ? `<small class="hint">${esc(help)}</small>` : ""}</label>`;
}

function newPassword(name = "password") {
  return field(name, "새 비밀번호", { type: "password", complete: "new-password", help: "UTF-8 기준 12~72바이트로 입력해 주세요." });
}

function showAuth(setup) {
  nav.replaceChildren();
  const heading = setup ? "관리자 계정 만들기" : "로그인";
  main.innerHTML = `<div class="auth-wrap"><aside class="auth-aside">
    <div><h2>여러 메일을<br>한 메일함에서.</h2>
    <p>원본 계정에서 IMAP이나 POP3로 메일을 읽어 원하는 메일함으로 중계합니다.</p></div>
    <div class="diagram" aria-label="중계 경로 예시">
      <div class="diagram-node">${mailIcon}<span>원본 메일함<small>IMAP · POP3</small></span></div>
      <div class="wire" aria-hidden="true"></div>
      <div class="diagram-node">${mailIcon}<span>대상 메일함<small>IMAP 삽입 · SMTP 전달</small></span></div>
    </div></aside><section class="auth-form"><h1>${heading}</h1>
    <p>${setup ? "서버의 초기 설정 토큰으로 첫 계정을 등록하세요." : "메일 중계에 사용할 계정으로 로그인하세요."}</p>
    <form class="form-stack" data-form="${setup ? "setup" : "login"}">
    ${setup ? field("token", "초기 설정 토큰", { type: "password", complete: "off" }) : ""}
    ${field("username", "사용자 이름", { complete: "username" })}
    ${setup ? newPassword() : field("password", "비밀번호", { type: "password", complete: "current-password" })}
    ${errorSlot}<button class="btn primary" type="submit">${setup ? "관리자 계정 등록" : "로그인"}</button>
    </form></section></div>`;
  if (setup && initialToken) main.querySelector('[name="token"]').value = initialToken;
  initialToken = "";
}

function showNav() {
  const items = [["routes", "중계 경로"], ...(app.session.user.role === "admin" ? [["users", "사용자"]] : []), ["account", "내 계정"]];
  nav.innerHTML = items.map(([id, title]) => `<a class="nav-item" href="#${id}" ${app.page === id ? 'aria-current="page"' : ""}>${title}</a>`).join("");
}

function setPageTitle(title) { document.title = `${title} · Mail Relay`; }

function routeState(r) {
  if (r.status === "invalid") return ["원본 폴더 변경", "warn"];
  if (r.status === "blocked") return ["용량 초과", "warn"];
  if (r.status === "uncertain") return ["확인 필요", "warn"];
  if (!r.enabled || r.status === "paused") return ["일시 정지", ""];
  if (r.status === "error") return ["전송 오류", "warn"];
  if (!r.initialized) return ["첫 조회 대기·진행 중", ""];
  if (r.uncertain_uid) return ["메일 전송 중", "good"];
  return ["중계 사용 중", "good"];
}

function attention(r) { return ["uncertain", "error", "blocked", "invalid"].includes(r.status); }

function endpoint(label, name) {
  return `<div class="endpoint">${mailIcon}<div><small>${esc(label)}</small><strong>${esc(name)}</strong></div></div>`;
}

function mailNumber(route, uid) {
  return (route.source_protocol === "pop3" || route.imap_identity_mode ? "중계 번호 " : "원본 UID ") + uid;
}

function routeCard(r) {
  const [state, tone] = routeState(r);
  const blocked = r.status === "blocked";
  const uncertain = r.status === "uncertain";
  const canPause = !r.uncertain_uid && ["ready", "error", "paused"].includes(r.status);
  const method = r.method === "smtp" ? "SMTP 전달" : "IMAP 삽입";
  const button = (action, label) => `<button class="quiet" type="button" data-action="${action}" data-id="${esc(r.id)}">${label}</button>`;
  return `<article class="route"><div class="section-head"><h3>${esc(r.name)}</h3>
    <span class="badge ${tone}">${state}</span></div>
    <div class="path">
      ${endpoint(r.source_protocol === "pop3" ? "원본 · POP3" : `원본 · IMAP · ${r.source_folder || "INBOX"}`, r.source?.username || r.source?.host)}
      <span class="path-arrow" aria-hidden="true">→</span>
      ${endpoint(r.method === "smtp" ? method : `${method} · ${r.target_folder || "INBOX"}`, r.method === "smtp" ? r.recipient : r.target?.username || r.target?.host)}
    </div>
    <div class="route-meta"><span>중계 완료 ${fmtNumber.format(r.delivered || 0)}건</span>
      <span>마지막 확인 · ${esc(time(r.last_checked))}</span></div>
    ${uncertain ? `<p class="info">${esc(mailNumber(r, r.uncertain_uid))} 메일의 수신 여부를 확인할 수 없습니다. 대상 메일함을 확인한 뒤 아래에서 처리 방법을 선택하세요.</p>` : ""}
    ${blocked ? `<p class="info">${esc(mailNumber(r, r.uncertain_uid))} 메일이 중계 용량 제한을 초과했습니다. 서버 설정을 조정한 뒤 다시 시도하거나 이 메일을 건너뛰세요.</p>` : ""}
    ${r.status === "invalid" && !r.detail ? '<p class="info">원본 폴더의 메일 식별자가 바뀌었습니다. 새 경로를 추가해 다시 연결하세요.</p>' : ""}
    ${r.status === "ready" && !r.initialized && r.initial === "new" ? '<p class="hint">‘중계 사용 중’으로 바뀐 뒤 원본 메일함에 새 메일을 보내 전달을 확인하세요.</p>' : ""}
    ${r.status === "error" && r.next_attempt ? `<p class="hint">다음 시도 · ${esc(time(r.next_attempt))}</p>` : ""}
    ${r.detail && !blocked ? `<p class="info">${esc(r.detail)}</p>` : ""}
    <div class="card-actions">
      ${button("test", "연결 확인")} ${button("events", "기록 보기")} ${button("edit", "경로 수정")}
      ${uncertain || blocked ? `${button("retry", blocked ? "다시 시도" : "다시 전송")} ${button("skip", "건너뛰기")}` : ""}
      ${canPause ? button(r.enabled ? "pause" : "resume", r.enabled ? "일시 정지" : "재개") : ""}
      ${!uncertain && r.enabled && r.status === "error" ? button("retry", "지금 재시도") : ""}
      ${button("delete", "삭제")}
    </div></article>`;
}

function dashboard() {
  const routes = app.routes;
  const total = routes.reduce((sum, r) => sum + Number(r.delivered || 0), 0);
  const stats = [["등록 경로", routes.length], ["중계 완료", total], ["확인 필요", routes.filter(attention).length]];
  main.innerHTML = `<div class="section-head page-head"><div><h1>중계 경로</h1>
      <p>원본 메일함과 대상 메일함의 연결 상태를 확인하세요.</p></div>
      <button type="button" class="btn primary" data-action="create"><span class="plus" aria-hidden="true">+</span> 경로 추가</button></div>
    <dl class="stats">${stats.map(([title, value]) => `<div class="stat"><dt>${title}</dt><dd>${fmtNumber.format(value)}</dd></div>`).join("")}</dl>
    <section aria-labelledby="routes-heading"><div class="section-head"><h2 id="routes-heading">내 경로</h2>
      <button type="button" class="quiet" data-action="refresh">새로고침</button></div>
    ${routes.length ? `<div class="route-list">${routes.map(routeCard).join("")}</div>` :
      `<div class="panel empty">${mailIcon}<h3>등록한 중계 경로가 없습니다</h3>
      <p>원본 IMAP 또는 POP3 계정과 메일을 받을 대상 계정을 연결하세요.</p>
      <button type="button" class="btn" data-action="create">첫 경로 추가</button></div>`}</section>`;
}

function usersPage() {
  main.innerHTML = `<div class="section-head page-head"><div><h1>사용자 관리</h1>
    <p>서비스 계정을 등록하고 접근 권한을 관리하세요.</p></div>
    <button type="button" class="btn primary" data-action="add-user">사용자 추가</button></div>
    <section class="panel" aria-label="사용자 목록">${app.users.map(u => `<div class="user-row">
      <div><strong>${esc(u.username)}</strong><small>${u.role === "admin" ? "관리자" : "일반 사용자"} · ${u.enabled ? "사용 중" : "사용 중지"}${String(u.id) === String(app.session.user.id) ? " · 내 계정" : ""}</small></div>
      ${String(u.id) !== String(app.session.user.id) ? `<button class="btn" type="button" data-action="toggle-user" data-id="${esc(u.id)}">${u.enabled ? "사용 중지" : "사용 허용"}</button>` : ""}
    </div>`).join("")}</section>`;
}

function accountPage() {
  const user = app.session.user;
  main.innerHTML = `<div class="page-head"><h1>내 계정</h1></div>
    <section class="account-card"><div class="section-head"><div><h2>${esc(user.username)}</h2><span class="hint">${user.role === "admin" ? "관리자" : "일반 사용자"}</span></div>
    <button type="button" class="btn" data-action="logout">로그아웃</button></div></section>
    <section class="account-card"><h2>비밀번호 변경</h2>
    <form class="form-stack" data-form="password">
    ${field("current_password", "현재 비밀번호", { type: "password", complete: "current-password" })}
    ${newPassword()}${errorSlot}<button class="btn primary" type="submit">비밀번호 저장</button></form></section>`;
}

async function showPage({ quiet = false } = {}) {
  if (!app.session?.user) return;
  const revision = ++app.revision;
  const hash = location.hash.slice(1);
  app.page = hash === "account" || (hash === "users" && app.session.user.role === "admin") ? hash : "routes";
  showNav();
  setPageTitle({ account: "내 계정", users: "사용자 관리", routes: "중계 경로" }[app.page]);
  if (!quiet) main.innerHTML = '<p class="loading" role="status">불러오는 중…</p>';
  try {
    if (app.page === "routes") {
      const { routes } = await api("routes");
      if (revision !== app.revision) return;
      app.routes = routes || [];
      dashboard();
    } else if (app.page === "users") {
      const { users } = await api("users");
      if (revision !== app.revision) return;
      app.users = users || [];
      usersPage();
    } else {
      accountPage();
    }
  } catch (err) {
    if (revision !== app.revision || !app.session?.user) return;
    main.innerHTML = `<div class="panel empty"><p class="error" role="alert">${esc(err.message)}</p>
      <button type="button" class="btn" data-action="refresh">다시 불러오기</button></div>`;
  }
}

async function bootstrap() {
  const revision = ++app.revision;
  try {
    const session = await api("session");
    if (revision !== app.revision) return;
    app.session = session;
    if (session.user) await showPage();
    else {
      app.routes = []; app.users = []; app.page = "";
      setPageTitle(session.needs_setup ? "관리자 계정 만들기" : "로그인");
      showAuth(session.needs_setup);
    }
  } catch (err) {
    if (revision !== app.revision) return;
    nav.replaceChildren();
    main.innerHTML = `<div class="panel empty"><p class="error" role="alert">${esc(err.message)}</p>
      <button type="button" class="btn" data-action="reload">다시 연결</button></div>`;
  }
}

function openDialog(title, body) {
  ++app.dialogRevision;
  dialogContent.innerHTML = `<div class="dialog-head"><h2 id="dialog-title" tabindex="-1">${esc(title)}</h2>
    <button type="button" class="quiet" aria-label="닫기" data-action="close">×</button></div>${body}`;
  if (!modal.open) modal.showModal();
  modal.scrollTop = 0;
  dialogContent.querySelector("#dialog-title").focus();
}

function dialogFooter(submit, tone = "primary") {
  return `<div class="dialog-foot"><button type="button" class="btn" data-action="close">취소</button>
    <button type="submit" class="btn ${tone}">${esc(submit)}</button></div>`;
}

const providers = {
  gmail: {
    label: "Google (Gmail)", domain: "gmail.com", smtpSecurity: "tls",
    hint: "전체 이메일 주소와 2단계 인증 후 발급한 앱 비밀번호를 입력하세요. 계정 정책에서 앱 비밀번호를 허용해야 합니다.",
    help: "https://support.google.com/accounts/answer/185833?hl=ko",
    popHelp: "https://support.google.com/mail/answer/7104828?hl=ko",
  },
  yahoo: {
    label: "Yahoo Mail", domain: "mail.yahoo.com", smtpSecurity: "tls",
    hint: "전체 이메일 주소와 Yahoo에서 발급한 앱 비밀번호를 입력하세요.",
    help: "https://help.yahoo.com/kb/imap-settings-sln4075.html",
    popHelp: "https://help.yahoo.com/kb/SLN4724.html",
  },
  naver: {
    label: "네이버 메일", domain: "naver.com", smtpSecurity: "starttls",
    hint: "네이버 메일에서 해당 프로토콜을 사용 설정하고 네이버 아이디와 2단계 인증용 애플리케이션 비밀번호를 입력하세요.",
    help: "https://help.naver.com/service/30029/contents/21344",
    popHelp: "https://help.naver.com/service/30029/contents/21342",
  },
};
const defaultPorts = {
  imap: { tls: 993, starttls: 143 }, pop3: { tls: 995, starttls: 110 }, smtp: { tls: 465, starttls: 587 },
};

function preset(id, protocol) {
  const provider = providers[id];
  const security = protocol === "smtp" ? provider.smtpSecurity : "tls";
  const prefix = protocol === "pop3" ? "pop" : protocol;
  return { host: prefix + "." + provider.domain, security, port: defaultPorts[protocol][security] };
}

function endpointFields(side, item, editing, locked, protocol) {
  const value = item || {};
  const provider = Object.keys(providers).find(id => {
    const settings = preset(id, protocol);
    return settings.host === value.host && settings.security === value.security && settings.port === value.port;
  }) || "";
  return `${select(`${side}_provider`, "서버 자동 입력", [["", "직접 입력"], ...Object.entries(providers).map(([id, p]) => [id, p.label])], provider, { disabled: locked })}
    <p class="hint full" id="${side}-provider-help" aria-live="polite" hidden></p>
    ${field(`${side}_host`, "서버 주소", { value: value.host, disabled: locked, full: true })}
    ${select(`${side}_security`, "연결 보안", [["tls", "TLS"], ["starttls", "STARTTLS"]], value.security || "tls", { disabled: locked })}
    ${field(`${side}_port`, "포트", { type: "number", value: value.port || defaultPorts[protocol].tls, min: 1, max: 65535, disabled: locked })}
    ${field(`${side}_username`, "메일 계정 아이디", { value: value.username, placeholder: "name@example.com", disabled: locked })}
    ${field(`${side}_password`, "메일 계정 비밀번호", { type: "password", required: !editing, complete: "new-password", placeholder: editing ? "기존 값 유지" : "", help: editing ? "변경할 때만 입력하세요." : "제공업체에서 요구하면 앱 비밀번호를 입력하세요." })}`;
}

function routeDialog(r) {
  const editing = Boolean(r);
  const item = r || {};
  const locked = Boolean(item.initialized);
  const pendingReceipt = item.status === "uncertain";
  const method = item.method || "imap";
  const sourceProtocol = item.source_protocol || "imap";
  openDialog(editing ? "중계 경로 수정" : "중계 경로 추가", `<form data-form="route" data-id="${esc(item.id || "")}">
    <div class="dialog-body form-stack">
    ${field("name", "경로 이름", { value: item.name, placeholder: "예: 업무 메일 모아보기" })}
    <fieldset class="group"><legend>원본 메일함</legend><div class="grid">
      ${select("source_protocol", "받는 방식", [["imap", "IMAP"], ["pop3", "POP3"]], sourceProtocol, { disabled: locked })}
      ${endpointFields("source", item.source, editing, locked, sourceProtocol)}
      ${field("source_folder", "읽을 폴더", { value: item.source_folder || "INBOX", disabled: locked })}
      ${select("initial", "처음 가져올 메일", [["new", "새로 도착하는 메일"], ["all", "기존 메일 포함 전체"]], item.initial || "new", { disabled: locked })}
      <p class="hint full" id="pop-help" hidden>POP3는 서버 목록의 고유 식별자(UIDL)로 메일을 구분합니다. 중계 서버는 원본을 삭제하지 않습니다. 메일 제공업체의 원본 보관 설정도 확인하세요.</p>
      <p class="info full" id="gmail-pop-help" hidden>Gmail POP3는 최근 모드로 최근 30일간의 메일을 조회합니다. 더 오래된 메일은 IMAP으로 가져오세요. Gmail 설정에서 POP를 켜고 ‘받은편지함에 Gmail 사본 보관’을 선택하세요.</p>
      <p class="hint full">${locked ? "다른 원본 계정이나 시작 범위를 사용하려면 새 경로를 추가하세요." : "새 메일은 첫 연결에 성공한 후 추가된 메일부터, 전체는 처음 연결했을 때 서버에서 조회되는 기존 메일까지 가져옵니다."}</p>
    </div></fieldset>
    <fieldset class="group"><legend>대상 메일함</legend><div class="grid">
      ${select("method", "중계 방식", [["imap", "IMAP 삽입"], ["smtp", "SMTP 전달"]], method, { disabled: pendingReceipt })}
      ${endpointFields("target", item.target, editing, pendingReceipt, method)}
      ${pendingReceipt ? '<p class="hint full">대상 메일함에서 수신 여부를 확인하고 대기 중인 메일을 처리하면 대상 주소와 폴더를 변경할 수 있습니다.</p>' : ""}
      <p class="hint full" id="method-help"></p>
      <div class="grid full" data-method="imap">
        ${field("target_folder", "넣을 폴더", { value: item.target_folder || "INBOX", full: true })}
      </div>
      <div class="grid full" data-method="smtp">
        ${field("sender", "보내는 주소", { type: "email", value: item.sender, placeholder: "relay@example.com", help: "SMTP 서버가 발송을 허용한 주소" })}
        ${field("recipient", "받는 주소", { type: "email", value: item.recipient, placeholder: "inbox@example.com" })}
      </div>
    </div></fieldset>${errorSlot}</div>${dialogFooter(editing ? "변경 저장" : "경로 추가")}
    </form>`);
  const form = dialogContent.querySelector("form");
  for (const side of ["source", "target"]) {
    const provider = form.elements[side + "_provider"];
    provider.dataset.active = provider.value;
    renderProvider(form, side);
    updateEndpointPassword(form, side);
  }
  updateProtocols(form);
}

function protocolFor(form, side) {
  return side === "source" ? form.elements.source_protocol.value : form.elements.method.value;
}

function updateEndpointPassword(form, side) {
  const item = app.routes.find(r => String(r.id) === form.dataset.id);
  const protocol = protocolFor(form, side);
  const keys = ["host", "port", "security", "username"];
  const current = keys.map(key => {
    const value = form.elements[side + "_" + key].value.trim();
    if (key === "host") return value.toLowerCase().replace(/\.$/, "");
    if (key === "port") return Number(value);
    return value;
  });
  const password = form.elements[side + "_password"];
  const identity = JSON.stringify([protocol, ...current]);
  if (password.value && password.dataset.identity !== identity && password.dataset.identity !== undefined) {
    password.value = "";
    password.dataset.cleared = "1";
  }
  password.dataset.identity = identity;
  if (password.value) delete password.dataset.cleared;
  const oldProtocol = side === "source" ? item?.source_protocol || "imap" : item?.method;
  const changed = item && (protocol !== oldProtocol || keys.some((key, i) => current[i] !== item[side][key]));
  password.required = !item || changed;
  password.placeholder = item ? (changed ? "새 연결의 비밀번호" : "기존 값 유지") : "";
  const hint = password.closest("label").querySelector(".hint");
  if (password.dataset.cleared) hint.textContent = "연결 정보가 바뀌어 입력한 비밀번호를 비웠습니다. 해당 계정의 비밀번호를 다시 입력하세요.";
  else if (item) hint.textContent = changed ? "연결 정보가 바뀌어 비밀번호를 다시 입력해야 합니다." : "변경할 때만 입력하세요.";
  else hint.textContent = "제공업체에서 요구하면 앱 비밀번호를 입력하세요.";
}

function renderProvider(form, side) {
  const provider = providers[form.elements[side + "_provider"].value];
  const protocol = protocolFor(form, side);
  const help = form.querySelector("#" + side + "-provider-help");
  help.hidden = !provider;
  if (provider) {
    const pop = protocol === "pop3";
    help.innerHTML = `${esc(provider.hint)} <a href="${esc(pop ? provider.popHelp : provider.help)}" target="_blank" rel="noreferrer">공식 설정 안내</a>`;
  }
  if (side === "source") {
    const gmailHost = form.elements.source_host.value.trim().toLowerCase().replace(/\.$/, "") === "pop.gmail.com";
    form.querySelector("#gmail-pop-help").hidden = protocol !== "pop3" || !gmailHost;
  }
}

function updateProtocols(form) {
  const isPOP = form.elements.source_protocol.value === "pop3";
  const folder = form.elements.source_folder;
  folder.closest("label").hidden = isPOP;
  folder.disabled = isPOP || form.elements.source_protocol.disabled;
  form.querySelector("#pop-help").hidden = !isPOP;
  form.elements.source_host.placeholder = isPOP ? "pop.example.com" : "imap.example.com";
  form.elements.target_host.placeholder = form.elements.method.value === "smtp" ? "smtp.example.com" : "imap.example.com";
  form.querySelector("#method-help").textContent = form.elements.method.value === "smtp"
    ? "지정한 보내는 주소로 새 메일을 발송합니다. 원본 메일 전체가 .eml 파일로 첨부됩니다."
    : (isPOP ? "대상 폴더에 발신자와 본문, 첨부가 담긴 원본 메일을 넣습니다." : "대상 폴더에 원본 메일을 넣고 원래의 발신자와 수신 시각을 유지합니다.");
  form.elements.method.setAttribute("aria-describedby", "method-help");
  for (const group of form.querySelectorAll("[data-method]")) {
    group.hidden = group.dataset.method !== form.elements.method.value;
    for (const input of group.querySelectorAll("input")) input.disabled = group.hidden || form.elements.method.disabled;
  }
}

function addUserDialog() {
  openDialog("사용자 추가", `<form data-form="user"><div class="dialog-body form-stack">
    ${field("username", "사용자 이름", { complete: "off" })}${newPassword()}
    ${select("role", "권한", [["user", "일반 사용자"], ["admin", "관리자"]], "user")}
    ${errorSlot}</div>${dialogFooter("사용자 추가")}</form>`);
}

function confirmDialog(action, id, title, message, submit, tone = "primary") {
  const decision = ["skip", "retry"].includes(action) ? app.routes.find(r => String(r.id) === id)?.recovery_token : "";
  openDialog(title, `<form data-form="confirm" data-operation="${esc(action)}" data-id="${esc(id)}" data-recovery-token="${esc(decision)}">
    <div class="dialog-body form-stack"><p>${esc(message)}</p>${errorSlot}</div>${dialogFooter(submit, tone)}</form>`);
}

async function eventsDialog(r) {
  openDialog("중계 기록", `<div class="dialog-body"><h3>${esc(r.name)}</h3><div id="events"><p class="loading">불러오는 중…</p></div></div>`);
  const revision = app.dialogRevision;
  try {
    const { events } = await api(`routes/${encodeURIComponent(r.id)}/events`);
    if (!modal.open || app.dialogRevision !== revision) return;
    document.querySelector("#events").innerHTML = events?.length ? events.map(e => `<div class="event-row">
      <div><p>${esc(e.message || e.status)}</p><small class="time"><time>${esc(time(e.at))}</time>${e.uid ? esc(mailNumber(r, e.uid)) : ""}</small></div></div>`).join("") :
      '<p class="loading">아직 중계 기록이 없습니다.</p>';
  } catch (err) {
    if (!modal.open || app.dialogRevision !== revision) return;
    document.querySelector("#events").innerHTML = `<p class="error" role="alert">${esc(err.message)}</p>`;
  }
}

async function testConnection(route) {
  openDialog("연결 확인", `<div class="dialog-body form-stack"><h3>${esc(route.name)}</h3>
    <p class="hint">원본과 대상 서버에 로그인하고 ${route.source_protocol === "pop3" ? "원본 POP3의 고유 식별자 목록을" : "원본 IMAP 폴더를"} 확인합니다.</p>
    <p id="test-result" role="status">서버 연결을 확인하는 중…</p></div>`);
  const revision = app.dialogRevision;
  const path = `routes/${encodeURIComponent(route.id)}/test`;
  try {
    const result = await api(path, "POST", {});
    if (modal.open && app.dialogRevision === revision) document.querySelector("#test-result").textContent = result.message || "연결을 확인했습니다.";
  } catch (err) {
    if (modal.open && app.dialogRevision === revision) {
      const slot = document.querySelector("#test-result");
      slot.className = "error";
      slot.textContent = err.message;
    }
  }
}

function readRoute(form) {
  const get = key => form.elements.namedItem(key).value;
  const endpoint = side => ({ host: get(`${side}_host`).trim(), port: Number(get(`${side}_port`)), security: get(`${side}_security`), username: get(`${side}_username`).trim(), password: get(`${side}_password`) });
  const smtp = get("method") === "smtp";
  return { name: get("name").trim(), source: endpoint("source"), target: endpoint("target"), source_protocol: get("source_protocol"),
    source_folder: get("source_protocol") === "pop3" ? "" : get("source_folder").trim(), target_folder: smtp ? "" : get("target_folder").trim(), method: get("method"),
    sender: smtp ? get("sender").trim() : "", recipient: smtp ? get("recipient").trim() : "", initial: get("initial") };
}

async function mutate(action, id, recoveryToken) {
  const path = `routes/${encodeURIComponent(id)}`;
  if (action === "delete") await api(path, "DELETE");
  else if (action === "disable-user" || action === "enable-user") await api(`users/${encodeURIComponent(id)}`, "PUT", { enabled: action === "enable-user" });
  else await api(`${path}/action`, "POST", { action, recovery_token: recoveryToken || undefined });
}

const operationNotes = { pause: "경로를 일시 정지했습니다.", resume: "중계를 재개했습니다.", retry: "재시도를 요청했습니다.", skip: "해당 메일을 건너뛰었습니다.", delete: "경로를 삭제했습니다.", "enable-user": "계정 사용을 허용했습니다.", "disable-user": "계정 사용을 중지했습니다." };

async function submitForm(form) {
  const kind = form.dataset.form;
  const values = Object.fromEntries(new FormData(form));
  const id = form.dataset.id;
  if (["setup", "user", "password"].includes(kind)) {
    const bytes = new TextEncoder().encode(values.password).length;
    if (bytes < 12 || bytes > 72) throw new Error("비밀번호를 UTF-8 기준 12~72바이트로 입력해 주세요.");
  }
  if (kind === "login" || kind === "setup") {
    await api(kind, "POST", values);
    form.reset();
    await bootstrap();
    return;
  }
  if (kind === "route") {
    await api(id ? `routes/${encodeURIComponent(id)}` : "routes", id ? "PUT" : "POST", readRoute(form));
    announce(id ? "중계 경로를 수정했습니다." : "중계 경로를 추가했습니다.");
  } else if (kind === "confirm") {
    await mutate(form.dataset.operation, id, form.dataset.recoveryToken);
    announce(operationNotes[form.dataset.operation]);
  } else if (kind === "user") {
    await api("users", "POST", values);
    announce("사용자를 추가했습니다.");
  } else if (kind === "password") {
    await api("password", "POST", values);
    form.reset();
    await bootstrap();
    announce("비밀번호를 변경했습니다.");
    return;
  }
  if (modal.open) modal.close();
  await showPage({ quiet: true });
}

document.addEventListener("submit", async event => {
  if (!event.target.matches("form[data-form]")) return;
  event.preventDefault();
  if (app.pending) return;
  const form = event.target;
  const error = form.querySelector(".error");
  if (error) error.hidden = true;
  const submit = form.querySelector('[type="submit"]');
  submit.disabled = app.pending = true;
  try {
    await submitForm(form);
  } catch (err) {
    if (form.isConnected) showError(form, err.message);
  } finally {
    submit.disabled = app.pending = false;
  }
});

function changeEndpoint(event) {
  const input = event.target;
  const form = event.target.closest('form[data-form="route"]');
  if (!form || !/^(method|source_protocol|(source|target)_(provider|host|port|security|username|password))$/.test(input.name)) return;
  const side = input.name === "method" || input.name.startsWith("target_") ? "target" : "source";
  const protocol = protocolFor(form, side);
  const provider = form.elements[side + "_provider"];
  const host = form.elements[side + "_host"];
  const port = form.elements[side + "_port"];
  const security = form.elements[side + "_security"];
  const choosing = input === provider;
  const switching = input.name === "method" || input.name === "source_protocol";
  if (choosing && !provider.dataset.active && provider.value) {
    provider.dataset.custom = JSON.stringify({ host: host.value, port: port.value, security: security.value, protocol });
  }
  if (choosing || switching) {
    if (provider.value) {
      const data = preset(provider.value, protocol);
      host.value = data.host;
      security.value = data.security;
      port.value = data.port;
    } else if (choosing && provider.dataset.custom) {
      const custom = JSON.parse(provider.dataset.custom);
      if (custom.protocol === protocol) {
        host.value = custom.host;
        port.value = custom.port;
        security.value = custom.security;
      }
    }
  } else if ([host, port, security].includes(input)) {
    provider.value = "";
  }
  if ((switching || input === security) && !provider.value && ["993", "143", "995", "110", "465", "587"].includes(port.value)) {
    port.value = defaultPorts[protocol][security.value];
  }
  provider.dataset.active = provider.value;
  if (switching) updateProtocols(form);
  if (choosing || switching || [host, port, security].includes(input)) renderProvider(form, side);
  updateEndpointPassword(form, side);
}

// Selects emit both input and change. Apply a preset once per user selection.
document.addEventListener("change", e => { if (e.target.tagName === "SELECT") changeEndpoint(e); });
document.addEventListener("input", e => { if (e.target.tagName === "INPUT") changeEndpoint(e); });

document.addEventListener("click", async event => {
  const btn = event.target.closest("[data-action]");
  if (!btn || app.pending) return;
  const action = btn.dataset.action;
  const id = btn.dataset.id;
  const route = app.routes.find(r => String(r.id) === id);
  try {
    if (action === "close") modal.close();
    else if (action === "reload") await bootstrap();
    else if (action === "create") routeDialog(null);
    else if (action === "edit" && route) routeDialog(route);
    else if (action === "events" && route) await eventsDialog(route);
    else if (action === "test" && route) {
      btn.disabled = true;
      try { await testConnection(route); }
      finally { btn.disabled = false; }
    }
    else if (action === "refresh") await showPage({ quiet: true });
    else if (action === "add-user") addUserDialog();
    else if (action === "toggle-user") {
      const user = app.users.find(u => String(u.id) === id);
      if (!user) return;
      const label = user.enabled ? "사용 중지" : "사용 허용";
      confirmDialog(user.enabled ? "disable-user" : "enable-user", id, `계정 ${label}`, `${user.username} 계정의 서비스 이용을 ${user.enabled ? "중지" : "허용"}합니다.`, label);
    } else if (action === "delete" && route) {
      confirmDialog(action, id, "경로 삭제", `‘${route.name}’ 경로의 설정과 기록을 삭제합니다. 계속할까요?`, "경로 삭제", "danger");
    } else if (action === "skip" && route) {
      confirmDialog(action, id, "메일 건너뛰기", `${mailNumber(route, route.uncertain_uid)} 메일을 건너뛰면 해당 메일은 이 경로로 다시 보내지 않습니다. 계속할까요?`, "건너뛰기");
    } else if (action === "retry" && route?.status === "blocked") {
      confirmDialog(action, id, "메일 중계 재시도", "서버의 용량 제한 설정을 확인하셨나요? 이 메일을 다시 처리합니다.", "다시 시도");
    } else if (action === "retry" && route && route.uncertain_uid) {
      confirmDialog(action, id, "다시 전송", "이미 대상 메일함에 도착했다면 같은 메일이 한 번 더 도착할 수 있습니다. 다시 전송할까요?", "다시 전송");
    } else if (["pause", "resume", "retry"].includes(action) && route) {
      app.pending = btn.disabled = true;
      await mutate(action, id);
      announce(operationNotes[action]);
      await showPage({ quiet: true });
    } else if (action === "logout") {
      app.pending = btn.disabled = true;
      await api("logout", "POST", {});
      await bootstrap();
    }
  } catch (err) { announce(err.message); }
  finally {
    if (["pause", "resume", "retry", "logout"].includes(action)) app.pending = btn.disabled = false;
  }
});
modal.addEventListener("cancel", e => { if (app.pending) e.preventDefault(); });
modal.addEventListener("close", () => { ++app.dialogRevision; dialogContent.replaceChildren(); });
window.addEventListener("hashchange", () => { if (modal.open) modal.close(); void showPage(); });
let checkingRoutes = false;
function canUpdateRoutes() {
  return app.session?.user && app.page === "routes" && !modal.open && !document.hidden &&
    !app.pending && !main.contains(document.activeElement);
}
async function updateRoutes() {
  if (checkingRoutes || !canUpdateRoutes()) return;
  checkingRoutes = true;
  const revision = app.revision;
  try {
    const response = await api("routes");
    const routes = response.routes || [];
    if (revision === app.revision && canUpdateRoutes() && JSON.stringify(routes) !== JSON.stringify(app.routes)) {
      app.routes = routes;
      dashboard();
    }
  } catch {
    // Keep the last view available. A manual refresh reports connection errors.
  } finally {
    checkingRoutes = false;
  }
}
window.setInterval(updateRoutes, 20000);
document.addEventListener("visibilitychange", updateRoutes);
void bootstrap();

