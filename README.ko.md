# EmailRelay

[English](README.md) · 한국어

여러 계정의 메일을 평소 쓰는 메일함 한 곳에서 읽을 수 있습니다. 내 서버에 EmailRelay를 설치하고, 웹 관리 화면에서 원본 메일함과 받을 곳을 하나의 **중계 경로**로 등록하세요. 원본은 IMAP이나 POP3로 조회하며, 경로마다 대상과 전달 이력을 따로 관리합니다. Gmail, 야후 메일, 네이버 메일은 서버 설정을 자동으로 채워 줍니다. 다른 메일 서비스도 서버 주소를 직접 입력해 연결할 수 있습니다.

| 받고 싶은 형태 | 중계 방식 |
| --- | --- |
| 평소 쓰는 폴더에서 원래 보낸 사람의 메일을 그대로 열기 | **IMAP 삽입**은 대상 계정에 이미 있는 폴더로 메일을 복사합니다. 원래 보낸 사람, 본문, 첨부파일을 보존합니다. 원본이 IMAP이면 서버 수신 시각과 지원되는 상태 표시도 복사합니다. 순환 중계를 방지하기 위한 헤더 한 줄이 추가됩니다. |
| 이메일 주소로 전달받기 | **SMTP 전달**은 발송 권한이 있는 주소에서 새 메일을 보내고, 원본 전체를 `.eml` 파일로 첨부합니다. 받을 계정의 IMAP 접속정보 없이도 사용할 수 있습니다. |

## Docker로 시작하기

저장소를 복제하거나 소스 압축 파일을 받아 풀어 주세요. Linux 서버에 Docker Engine과 Compose v2를 설치한 뒤, `compose.yaml`이 있는 디렉터리에서 다음 명령을 실행하세요. 처음 빌드할 때 기본 이미지와 Go 의존성을 내려받습니다. Docker Desktop에서는 Linux 컨테이너의 호스트 네트워킹을 켜야 합니다. [Docker 호스트 네트워킹 안내](https://docs.docker.com/engine/network/drivers/host/)

```sh
docker compose up -d --build
docker compose exec relay cat /data/emailrelay/setup-token
```

[로컬 관리 화면](http://localhost:8787)을 열고 두 번째 명령에 표시된 일회용 코드로 첫 관리자 계정을 만드세요. 원격 서버에 설치했다면 내 컴퓨터에서 `ssh -L 8787:127.0.0.1:8787 user@server`를 실행한 상태로 같은 주소를 여세요. `user@server`에는 접속할 SSH 계정과 서버 주소를 넣습니다. 관리자 등록을 마치면 코드는 만료됩니다. 이후에는 관리 화면에서 다른 사용자의 계정을 만들 수 있습니다.

## 경로를 만들고 첫 메일 확인하기

1. **경로 추가**를 열고 원본 계정의 받는 방식과 로그인 정보를 입력하세요. IMAP을 선택했다면 읽을 폴더도 지정합니다. 일반적인 받은편지함은 `INBOX`입니다. 원본과 대상의 메일 서비스는 각각 선택할 수 있습니다.
2. 폴더로 복사하려면 **IMAP 삽입**을 선택하고, 사용할 폴더를 대상 계정에 먼저 만드세요. 주소로 받으려면 **SMTP 전달**을 선택하고, 해당 SMTP 계정으로 발송할 수 있는 보낸 사람 주소를 입력하세요.
3. **처음 가져올 메일**을 선택한 뒤 저장하면 중계를 시작합니다. **새로 도착하는 메일**은 처음으로 정상 조회한 목록을 기존 메일로 기록하고 이후에 들어오는 메일부터 전달합니다. **기존 메일 포함 전체**는 그 첫 목록에 있던 메일까지 전달 대기열에 넣습니다.
4. **연결 확인**으로 접속을 점검하세요. 실제 도착 여부는 경로 상태가 **중계 사용 중**으로 바뀐 뒤 원본 계정에 메일을 한 통 보내 확인합니다. **기록 보기**에서 전달 결과를 확인하고, 대상 계정의 지정 폴더나 받은편지함을 열어 보세요. 첫 목록에 들어 있던 시험 메일도 기존 메일로 분류됩니다.

첫 목록을 확정하기 전에는 가져올 범위를 바꿀 수 있습니다. 확정 후에는 그 경로의 원본 계정·폴더와 가져올 범위가 고정되며, 비밀번호와 대상 설정은 계속 수정할 수 있습니다.

IMAP과 POP3는 TLS 연결에서 사용자 이름과 비밀번호로 인증합니다. 공급자가 앱 비밀번호를 요구하면 해당 계정에서 발급받아 입력하세요. OAuth2 인증을 의무화한 계정은 현재 연결할 수 없습니다. Gmail POP는 최근 모드로 지난 30일간의 메일을 조회합니다. 더 오래된 메일까지 가져오려면 IMAP을 선택하세요. [Google 앱 비밀번호 안내](https://support.google.com/accounts/answer/185833) · [Gmail POP 설정](https://support.google.com/mail/answer/7104828)

## HTTPS로 관리 화면 공개하기

**이미 NGINX나 Caddy를 쓰는 경우**에는 `.env.example`을 `.env`로 복사하고, `RELAY_ORIGIN=https://relay.example.org`의 주소를 실제 공개 주소로 바꾼 뒤 `docker compose up -d --build`를 실행하세요. 같은 서버의 프록시에서 해당 도메인의 요청을 `127.0.0.1:8787`로 전달하고 브라우저가 보낸 원래 `Host` 헤더를 보존하세요. NGINX에서는 `proxy_set_header Host $http_host;`로 설정합니다. 관리 화면에는 전용 호스트 이름을 할당하고 해당 호스트의 루트 경로에서 서비스하세요. 로컬 포트는 `RELAY_PORT`로 변경할 수 있습니다.

기본 설정에서는 같은 프록시를 통해 접속한 사용자가 로그인 시도 한도를 공유합니다. 접속자별로 제한하려면 프록시가 요청의 `X-Real-IP`를 실제 접속자 IP 하나로 덮어쓰도록 설정한 뒤 `.env`에 `RELAY_TRUST_PROXY_IP=true`를 추가하세요. NGINX 지시문은 `proxy_set_header X-Real-IP $remote_addr;`입니다. 중계 서버가 신뢰할 수 있는 루프백 프록시에서만 요청을 받는 구성에서 켜세요.

**함께 제공하는 Caddy를 쓰는 경우**에는 도메인의 DNS 레코드를 서버로 연결하고 외부에서 TCP 80·443 포트에 접근할 수 있도록 설정하세요. `.env`에 `RELAY_DOMAIN`을 지정한 뒤 다음 명령으로 두 서비스를 시작합니다. Caddy가 인증서 발급과 갱신을 처리하며, 이 구성에서는 실제 접속자 IP 전달도 자동으로 설정됩니다.

```sh
docker compose -f compose.yaml -f compose.https.yaml up -d --build
```

## Linux에 직접 설치하기

Linux 배포본에는 Go로 빌드한 실행 파일과 SQLite가 정적으로 포함됩니다. `x86_64` 서버는 `amd64`, `aarch64` 서버는 `arm64` 압축 파일을 받으세요. 서버에는 TLS 인증서 검증에 쓰는 CA 인증서 묶음이 필요합니다. 배포본과 `SHA256SUMS`를 같은 디렉터리에 내려받고, 압축을 풀기 전에 `sha256sum --check --ignore-missing SHA256SUMS`로 파일을 검증하세요. [배포본 받기](https://github.com/HanyeolKo/EmailRelay/releases)

압축을 푼 디렉터리에서 `./bin/emailrelay`를 실행하세요. 데이터 디렉터리의 `setup-token` 파일에 있는 일회용 코드를 [로컬 관리 화면](http://127.0.0.1:8787)에 입력합니다. 기본 데이터 디렉터리는 `$XDG_DATA_HOME/emailrelay`이고, `XDG_DATA_HOME`이 설정되지 않았으면 `~/.local/share/emailrelay`입니다.

소스에서 직접 빌드하려면 Linux, Go 1.26 이상, SQLite 빌드에 필요한 C 컴파일러를 준비하세요.

```sh
mkdir -p bin
CGO_ENABLED=1 go build -trimpath -o bin/emailrelay ./cmd/emailrelay
```

systemd 사용자 서비스로 실행하려면 직접 실행해 둔 프로세스를 종료한 뒤 실행 파일과 서비스 설정을 설치하세요.

```sh
install -Dm755 bin/emailrelay "$HOME/.local/bin/emailrelay"
install -Dm644 deploy/emailrelay.service "$HOME/.config/systemd/user/emailrelay.service"
systemctl --user daemon-reload
systemctl --user enable --now emailrelay
```

서비스는 `~/.config/emailrelay/environment`에서 환경 설정을 읽습니다. 수정한 뒤에는 `systemctl --user restart emailrelay`로 적용하세요. 로그는 `journalctl --user -u emailrelay`로 조회합니다. 부팅할 때 시작해 로그아웃 후에도 계속 실행하려면 서버 관리자가 서비스 계정에 systemd의 lingering을 활성화할 수 있습니다.

| 직접 설치 시 환경 변수 | 기본값 | 용도 |
| --- | --- | --- |
| `RELAY_LISTEN` | `127.0.0.1:8787` | 관리 화면이 요청을 받을 주소 |
| `RELAY_ORIGIN` | 로컬 수신 주소 | 브라우저에서 열 때 사용하는 정확한 주소 |
| `RELAY_TRUST_PROXY_IP` | `false` | 루프백 프록시가 덮어쓴 단일 `X-Real-IP`를 로그인 시도 제한에 사용 |
| `RELAY_TLS_CERT`, `RELAY_TLS_KEY` | 미설정 | 직접 HTTPS를 제공할 때 쓰는 인증서 체인과 개인키. 루프백 이외의 주소에서 요청을 받으려면 필수 |
| `RELAY_INTERVAL` | `1m` | 중계할 경로를 확인하는 주기. 최소 5초 |
| `RELAY_MAX_MIB` | `32` | 원본 메일 한 통의 최대 크기. 1~256 MiB |
| `RELAY_DATA` | 사용자 데이터 디렉터리 | 데이터베이스, 키, 중복 실행 방지용 잠금 파일을 보관하는 경로. 권한은 0700 |
| `RELAY_ALLOWED_CIDRS` | 미설정 | 내부 메일 서버로의 접속을 허용할 네트워크 대역. 예를 들어 `10.1.2.3/32`이며 여러 대역은 쉼표로 구분 |

메일 서버에는 인증서를 검증하는 TLS 또는 필수 STARTTLS로 연결합니다. 사내 메일 서버를 연결할 때는 위 설정으로 해당 서버 주소를 허용하고, 중계 프로세스가 사내 인증서를 발급한 CA를 신뢰할 수 있도록 등록하세요.

## 중계 동작과 오류 처리

하나의 Go 프로세스가 웹 관리 화면과 중계를 함께 실행합니다. 최대 세 경로를 동시에 확인하고 경로당 한 번에 최대 12통을 처리합니다. SQLite에는 계정 설정과 메일별 중계 진행 상태를 저장합니다. 관리자를 포함한 각 사용자는 자신이 등록한 경로와 기록만 볼 수 있습니다. 웹 로그인 비밀번호는 bcrypt 해시로 저장합니다. 메일 계정 설정은 AES-256-GCM으로 암호화하고, 복호화 키는 소유자만 읽을 수 있는 별도 파일에 보관합니다. 데이터베이스와 키를 함께 가진 사람은 저장된 메일 계정 접속정보를 읽을 수 있습니다.

- **원본 메일 보존** — IMAP으로 가져올 때 원본의 읽음 상태를 유지합니다. POP3는 서버가 안정적인 고유 식별자(UIDL)를 제공해야 하며 목록은 최대 10만 통까지 지원합니다. 중계 서버는 원본을 보존합니다. 메일 공급자 자체의 POP 보관 설정도 확인하세요. 중계 후 원본에서 수정하거나 삭제한 내용은 대상에 동기화되지 않습니다.
- **네이버 IMAP** — 처음부터 유효한 폴더 식별값(`UIDVALIDITY`)을 받지 못하면 원문 전체와 서버 수신 시각을 기준으로 메일을 식별합니다. 매번 새 읽기 전용 연결의 목록을 기존 연결과 대조합니다. 오래된 목록이 감지되거나 연결이 바뀌거나 6시간의 캐시 보관 기간이 지나면 원문을 다시 읽습니다. 큰 메일함은 이때 중계까지 몇 분이 걸릴 수 있습니다. 이 방식은 조회 가능한 메일 최대 10만 통을 지원합니다. 원문과 첨부파일은 중계 데이터베이스에 저장하지 않습니다. 원문과 수신 시각까지 같은 복사본이 두 조회 사이에 다른 복사본으로 교체되어 개수까지 같다면 서버 정보만으로 둘을 구분할 수 없습니다.
- **실패 또는 수신 여부 불확실** — 일시적 오류는 1분 뒤 재시도하며, 반복되면 최대 1시간까지 간격을 늘립니다. 대상 서버의 최종 응답을 받지 못하면 해당 경로가 확인 대기 상태가 되고 대상이 고정됩니다. 그 메일함을 열어 이미 도착했다면 **건너뛰기**, 도착하지 않았다면 **다시 전송**을 선택하세요. 확인 대기 중에도 비밀번호와 경로 이름은 수정할 수 있습니다.
- **용량 초과 또는 폴더 교체** — 크기 제한을 넘긴 메일은 사용자의 결정을 기다립니다. 제한을 올리고 다시 시도하거나 해당 메일을 건너뛸 수 있습니다. 기존에 유효하던 IMAP 폴더의 `UIDVALIDITY`가 바뀌었다면 새 경로를 만들고 처음 가져올 범위를 선택하세요.

## 백업, 업데이트, 복구

Docker의 `relay-data` 볼륨에는 데이터베이스와 `master.key`가 있습니다. 접속정보와 전달 이력을 복구하려면 두 파일이 모두 필요합니다. 백업은 소유자만 읽을 수 있는 곳에 보관하세요. 함께 제공하는 Caddy는 별도의 `caddy-data`, `caddy-config` 볼륨을 씁니다.

```sh
umask 077
backup="emailrelay-backup-$(date +%Y%m%d-%H%M%S).tgz"
docker compose stop relay
docker compose run --rm --no-deps -T --entrypoint tar relay -C /data -czf - emailrelay > "$backup"
tar -tzf "$backup"
docker compose start relay
```

업데이트할 때는 기존 `.env`를 새 소스 디렉터리로 복사하고, 그 디렉터리에서 원래 쓰던 Compose 명령을 실행하세요. 예전 볼륨을 그대로 연결하려면 Compose 프로젝트 이름도 같아야 합니다. 기본 이름은 `emailrelay`입니다. `down -v`를 실행하면 이름이 지정된 볼륨과 그 안의 데이터가 삭제됩니다.

다른 서버로 복구하려면 기존 중계를 먼저 중지하세요. 새 서버에서 이미지를 빌드한 뒤 빈 데이터 볼륨에 백업을 풀고 서비스를 시작합니다. 아래의 `emailrelay-backup.tgz`를 실제 파일명으로 바꾸세요. 함께 제공하는 Caddy를 쓰면 마지막 명령에 두 Compose 파일을 모두 지정하세요.

```sh
docker compose build relay
docker compose run --rm --no-deps -T --entrypoint tar relay -C /data -xzf - < emailrelay-backup.tgz
docker compose up -d
```

Linux에 직접 설치한 경우 사용자 서비스를 중지하고 데이터 디렉터리 전체를 백업한 다음, 새 실행 파일을 설치하고 서비스를 시작하세요. 복구할 때는 같은 백업에서 나온 데이터베이스와 키를 함께 복원하고 디렉터리 권한을 0700으로 유지하세요.

## 테스트와 배포본 만들기

소스 디렉터리에서 `go test -race ./...`와 `go vet ./...`를 실행하세요. 정적 Linux 배포본은 musl용 C 컴파일러, GNU tar, readelf, sha256sum, flock을 준비한 뒤 `scripts/package.sh 0.1.3`으로 만듭니다. `dist/`에 소스 압축 파일, 현재 CPU용 실행 파일이 담긴 압축 파일, 체크섬 파일이 생성됩니다. 두 CPU용 배포본을 함께 만들려면 `ARCHES='amd64 arm64'`를 지정하고 `CC_AMD64`와 `CC_ARM64`에 각각의 musl 컴파일러를 설정하세요.

## 라이선스

EmailRelay는 MIT 라이선스로 배포합니다. 외부 구성 요소와 포함된 글꼴에는 각각의 라이선스가 적용됩니다. 고지문은 소스의 `deploy/licenses/`, Linux 배포본의 `THIRD_PARTY/`, 컨테이너의 `/usr/share/licenses/emailrelay/`에서 확인할 수 있습니다. [MIT 라이선스 전문](LICENSE)
