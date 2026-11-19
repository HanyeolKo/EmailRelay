# EmailRelay

[English](README.md) · 한국어

EmailRelay는 여러 계정으로 들어오는 메일을 한 메일함에 모아 줍니다. 예를 들어 네이버로 온 메일을 평소 쓰는 Gmail의 업무 폴더에서 읽을 수 있습니다. 내 서버에 EmailRelay를 설치한 뒤 웹에서 메일을 가져올 곳과 받을 곳을 지정하세요. 이 둘을 묶은 **중계 경로**마다 전달 이력이 따로 남습니다.

원본 메일은 IMAP이나 POP3로 가져옵니다. Gmail, 야후 메일, 네이버 메일은 서비스 이름을 고르면 서버 설정이 자동으로 채워집니다. 다른 서비스도 서버 주소를 직접 입력해 연결할 수 있습니다.

| 메일을 받는 방법 | 설정과 동작 |
| --- | --- |
| 메일함의 특정 폴더에 모으기 | **IMAP 삽입**은 대상 계정의 기존 폴더에 메일을 복사합니다. 보낸 사람 정보와 본문, 첨부파일이 그대로 남습니다. IMAP으로 가져오면 원래 서버의 수신 시각과 읽음·별표 같은 표시도 유지합니다. 한 번 중계한 메일이 되돌아오는 것을 막기 위해 헤더 한 줄을 추가합니다. |
| 이메일 주소로 전달받기 | **SMTP 전달**을 선택하면 발송 권한이 있는 주소에서 새 메일을 보내고 원본 전체를 `.eml` 파일로 첨부합니다. 받을 계정의 IMAP 접속 정보 없이도 사용할 수 있습니다. |

## Docker로 시작하기

저장소를 복제하거나 소스 압축 파일을 받아 풀어 주세요. Linux 서버에 Docker Engine과 Compose v2를 설치한 뒤, `compose.yaml`이 있는 디렉터리에서 다음 명령을 실행하세요. 처음 빌드할 때 기본 이미지와 Go 의존성을 내려받습니다. Docker Desktop에서는 Linux 컨테이너의 호스트 네트워킹을 켜야 합니다. [Docker 호스트 네트워킹 안내](https://docs.docker.com/engine/network/drivers/host/)

```sh
docker compose up -d --build
docker compose exec relay cat /data/emailrelay/setup-token
```

[로컬 관리 화면](http://localhost:8787)을 열고 두 번째 명령이 출력한 일회용 코드로 첫 관리자 계정을 만드세요. 원격 서버에 설치했다면 내 컴퓨터에서 `ssh -L 8787:127.0.0.1:8787 user@server`를 실행하고 SSH 연결을 유지한 채 같은 주소를 여세요. `user@server`에는 접속할 SSH 계정과 서버 주소를 넣습니다. 관리자 등록을 마치면 코드는 만료됩니다. 이후에는 관리 화면에서 다른 사용자의 계정을 만들 수 있습니다.

## 경로를 만들고 첫 메일 확인하기

1. **경로 추가**에서 원본 계정에 접속할 방식(IMAP 또는 POP3)을 고르고 로그인 정보를 입력하세요. IMAP은 읽을 폴더도 지정해야 합니다. 받은편지함은 보통 `INBOX`입니다. 원본과 대상에는 서로 다른 메일 서비스를 쓸 수 있습니다.
2. **IMAP 삽입**을 쓰려면 대상 계정에 메일을 담을 폴더가 있는지 먼저 확인하세요. **SMTP 전달**을 쓸 때는 해당 SMTP 계정에서 발송을 허용한 주소를 ‘보내는 주소’에 입력하세요.
3. **처음 가져올 메일**에서 전달 범위를 고른 뒤 저장하면 중계가 시작됩니다. **새로 도착하는 메일**을 고르면 첫 조회 목록에 있는 메일은 건너뛰고 이후에 들어오는 메일부터 전달합니다. 이전 메일까지 받으려면 **기존 메일 포함 전체**를 고르세요.
4. **연결 확인**을 누르면 계정에 접속할 수 있는지 알 수 있습니다. 실제 전달을 확인하려면 경로에 **중계 사용 중**이 뜬 뒤 원본 계정으로 테스트 메일을 한 통 보내세요. **기록 보기**에서 전달 결과를 확인하고 받을 곳으로 지정한 폴더나 받은편지함을 열어 보세요.

첫 조회가 끝나기 전에는 가져올 범위를 바꿀 수 있습니다. 그 뒤에는 원본 계정·폴더와 처음 가져올 범위가 고정됩니다. 비밀번호나 대상 메일함은 이후에도 수정할 수 있습니다. 단, 경로에 **확인 필요**가 뜨면 먼저 대상 메일함에서 그 메일의 도착 여부를 확인해야 합니다.

IMAP과 POP3는 TLS 연결에서 사용자 이름과 비밀번호로 인증합니다. 공급자가 앱 비밀번호를 요구하면 해당 계정에서 발급받아 입력하세요. OAuth2 인증을 의무화한 계정은 현재 연결할 수 없습니다. Gmail POP는 최근 모드로 지난 30일간의 메일을 조회합니다. 더 오래된 메일까지 가져오려면 IMAP을 선택하세요. [Google 앱 비밀번호 안내](https://support.google.com/accounts/answer/185833) · [Gmail POP 설정](https://support.google.com/mail/answer/7104828)

## HTTPS로 관리 화면 공개하기

**이미 NGINX나 Caddy를 쓰고 있다면** `.env.example`을 `.env`로 복사하세요. `RELAY_ORIGIN=https://relay.example.org`의 예시 주소를 실제 관리 화면 주소로 바꾼 뒤 `docker compose up -d --build`를 실행합니다.

관리 화면은 전용 도메인의 루트 경로(`/`)에서 열립니다. 같은 서버에 있는 프록시가 이 도메인의 요청을 `127.0.0.1:8787`로 보내도록 설정하세요. 이때 브라우저가 보낸 `Host` 헤더를 그대로 전달해야 합니다. NGINX라면 `proxy_set_header Host $http_host;`를 사용하세요. 로컬 포트는 `RELAY_PORT`로 바꿀 수 있습니다.

기본 설정에서는 같은 프록시를 거치는 사용자들의 로그인 시도 횟수를 합산합니다. 접속자별로 집계하려면 프록시가 `X-Real-IP` 헤더를 실제 접속자의 IP 하나로 덮어쓰도록 설정하세요. NGINX에서는 `proxy_set_header X-Real-IP $remote_addr;`를 씁니다. 로컬에서 신뢰하는 프록시만 백엔드에 접근할 수 있을 때 `.env`에 `RELAY_TRUST_PROXY_IP=true`를 설정하세요.

**함께 제공하는 Caddy를 쓰는 경우**에는 도메인의 DNS 레코드를 서버로 연결하고 외부에서 TCP 80·443 포트에 접근할 수 있도록 설정하세요. `.env`에 `RELAY_DOMAIN`을 지정한 뒤 다음 명령으로 두 서비스를 시작합니다. Caddy가 인증서를 발급하고 갱신합니다. 이 구성에서는 실제 접속자 IP 전달도 자동으로 설정됩니다.

```sh
docker compose -f compose.yaml -f compose.https.yaml up -d --build
```

## Linux에 직접 설치하기

Linux 배포본에는 SQLite까지 포함한 정적 실행 파일이 들어 있습니다. `x86_64` 서버에는 `amd64`, `aarch64` 서버에는 `arm64` 압축 파일을 받으세요. TLS 인증서를 검증하려면 서버에 CA 인증서 묶음이 있어야 합니다. 압축 파일과 `SHA256SUMS`를 같은 디렉터리에 받은 뒤 `sha256sum --check --ignore-missing SHA256SUMS`로 확인하고 압축을 푸세요. [배포본 받기](https://github.com/HanyeolKo/EmailRelay/tree/downloads)

압축을 푼 디렉터리에서 `./bin/emailrelay`를 실행하세요. 데이터 디렉터리의 `setup-token` 파일에 있는 일회용 코드를 [로컬 관리 화면](http://127.0.0.1:8787)에 입력합니다. 기본 데이터 디렉터리는 `$XDG_DATA_HOME/emailrelay`이고 `XDG_DATA_HOME`이 설정되지 않았으면 `~/.local/share/emailrelay`입니다.

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

서비스는 `~/.config/emailrelay/environment`에서 환경 설정을 읽습니다. 수정한 뒤에는 `systemctl --user restart emailrelay`로 적용하세요. 로그는 `journalctl --user -u emailrelay`로 조회합니다. 서버 관리자는 서비스 계정에 systemd의 lingering을 활성화할 수 있습니다. 그러면 서비스가 부팅할 때 시작되고 로그아웃 후에도 계속 실행됩니다.

| 직접 설치 시 환경 변수 | 기본값 | 용도 |
| --- | --- | --- |
| `RELAY_LISTEN` | `127.0.0.1:8787` | 관리 화면이 요청을 받을 주소 |
| `RELAY_ORIGIN` | 로컬 수신 주소 | 브라우저에서 열 때 사용하는 정확한 주소 |
| `RELAY_TRUST_PROXY_IP` | `false` | 루프백 프록시가 덮어쓴 단일 `X-Real-IP`를 로그인 시도 제한에 사용 |
| `RELAY_TLS_CERT`, `RELAY_TLS_KEY` | 미설정 | 직접 HTTPS를 제공할 때 쓰는 인증서 체인과 개인키. 루프백 이외의 주소에서 요청을 받으려면 필수 |
| `RELAY_INTERVAL` | `1m` | 중계할 경로를 확인하는 주기. 최소 5초 |
| `RELAY_MAX_MIB` | `32` | 원본 메일 한 통의 최대 크기. 1~256 MiB |
| `RELAY_DATA` | 사용자 데이터 디렉터리 | 데이터베이스, 키, 중복 실행 방지용 잠금 파일을 보관하는 경로. 권한은 0700 |
| `RELAY_ALLOWED_CIDRS` | 미설정 | 접속을 허용할 내부 메일 서버의 네트워크 대역. 예를 들어 `10.1.2.3/32`이며 여러 대역은 쉼표로 구분 |

메일 서버에는 인증서를 검증하는 TLS 또는 필수 STARTTLS로 연결합니다. 사내 메일 서버를 연결할 때는 위 설정으로 해당 서버 주소를 허용하고 중계 프로세스가 사내 인증서를 발급한 CA를 신뢰할 수 있도록 등록하세요.

## 중계 동작과 오류 처리

하나의 Go 프로세스가 웹 관리 화면과 중계를 함께 실행합니다. 최대 세 경로를 동시에 확인하고 경로당 한 번에 최대 12통을 처리합니다. SQLite에는 계정 설정과 메일별 중계 진행 상태를 저장합니다. 관리자를 포함한 각 사용자는 자신이 등록한 경로와 기록만 볼 수 있습니다. 웹 로그인 비밀번호는 bcrypt 해시로 저장합니다. 메일 계정 설정은 AES-256-GCM으로 암호화하고 복호화 키는 소유자만 읽을 수 있는 별도 파일에 보관합니다. 데이터베이스와 키를 함께 가진 사람은 저장된 메일 계정 접속 정보를 읽을 수 있습니다.

- **원본 메일 보존** — 중계 서버는 원본 메일을 삭제하지 않습니다. IMAP으로 메일을 가져올 때도 원본의 읽음 상태를 바꾸지 않습니다. POP3는 서버가 메일마다 고정된 고유 식별자(UIDL)를 제공해야 하며 최대 10만 통까지 조회합니다. 메일 서비스에서 따로 정한 POP 보관 정책도 확인하세요. 중계가 끝난 뒤 원본을 수정하거나 삭제해도 이미 전달한 메일에는 반영되지 않습니다.
- **네이버 IMAP** — IMAP은 `UIDVALIDITY` 값으로 같은 폴더의 메일 번호를 계속 사용해도 되는지 판단합니다. 네이버에서 처음부터 유효한 값을 받지 못하면 EmailRelay는 메일 원문과 서버 수신 시각으로 각 메일을 구분합니다. 조회할 때마다 새 읽기 전용 연결을 열어 기존 연결의 목록이 최신인지 대조합니다. 목록이 다르거나 연결이 바뀌면 원문을 다시 읽습니다. 목록이 같아도 6시간이 지나면 다시 확인합니다. 큰 메일함은 이때 중계까지 몇 분이 걸릴 수 있습니다. 최대 10만 통까지 조회하며 본문과 첨부파일은 중계 데이터베이스에 저장하지 않습니다. 두 조회 사이에 원문과 수신 시각이 같은 복사본 하나가 사라지고 같은 복사본 하나가 추가되면 개수도 같아서 둘을 구분할 수 없습니다.
- **메일이 도착했는지 알 수 없을 때** — 일시적 오류가 나면 1분 뒤에 다시 시도하며 오류가 반복되면 간격을 최대 1시간까지 늘립니다. 메일을 보낸 뒤 대상 서버의 최종 응답을 받지 못하면 화면에 **확인 필요**가 뜨고 해당 경로의 중계가 멈춥니다. 먼저 대상 메일함을 열어 보세요. 이미 도착했다면 **건너뛰기**, 도착하지 않았다면 **다시 전송**을 선택하세요. 확인하는 동안 대상은 고정되지만 비밀번호와 경로 이름은 수정할 수 있습니다.
- **용량 초과 또는 폴더 교체** — 메일이 크기 제한을 넘으면 해당 경로의 중계가 멈춥니다. 크기 제한을 높인 뒤 다시 시도하거나 해당 메일을 건너뛸 수 있습니다. 기존에 유효했던 IMAP 폴더의 `UIDVALIDITY`가 바뀌어도 중계가 멈춥니다. 이때는 새 경로를 만들고 처음 가져올 메일의 범위를 선택하세요.

## 백업, 업데이트, 복구

Docker의 `relay-data` 볼륨에는 데이터베이스와 `master.key`가 있습니다. 접속 정보와 전달 이력을 복구하려면 두 파일이 모두 필요합니다. 백업은 소유자만 읽을 수 있는 곳에 보관하세요. 함께 제공하는 Caddy는 `caddy-data`와 `caddy-config` 볼륨을 따로 씁니다.

```sh
umask 077
backup="emailrelay-backup-$(date +%Y%m%d-%H%M%S).tgz"
docker compose stop relay
docker compose run --rm --no-deps -T --entrypoint tar relay -C /data -czf - emailrelay > "$backup"
tar -tzf "$backup"
docker compose start relay
```

업데이트할 때는 기존 `.env`를 새 소스 디렉터리로 복사하고 그 디렉터리에서 원래 쓰던 Compose 명령을 실행하세요. 예전 볼륨을 그대로 연결하려면 Compose 프로젝트 이름도 같아야 합니다. 기본 이름은 `emailrelay`입니다. `down -v`를 실행하면 이름이 지정된 볼륨과 그 안의 데이터가 삭제됩니다.

다른 서버로 복구하려면 기존 중계를 먼저 중지하세요. 새 서버에서 이미지를 빌드한 뒤 빈 데이터 볼륨에 백업을 풀고 서비스를 시작합니다. 아래의 `emailrelay-backup.tgz`를 실제 파일명으로 바꾸세요. 함께 제공하는 Caddy를 쓰면 마지막 명령에 두 Compose 파일을 모두 지정하세요.

```sh
docker compose build relay
docker compose run --rm --no-deps -T --entrypoint tar relay -C /data -xzf - < emailrelay-backup.tgz
docker compose up -d
```

Linux에 직접 설치한 경우 사용자 서비스를 중지하고 데이터 디렉터리 전체를 백업한 다음, 새 실행 파일을 설치하고 서비스를 시작하세요. 복구할 때는 같은 백업에서 나온 데이터베이스와 키를 함께 복원하고 디렉터리 권한을 0700으로 유지하세요.

## 테스트와 배포본 만들기

소스 디렉터리에서 `go test -race ./...`와 `go vet ./...`를 실행하세요. 정적 Linux 배포본은 musl용 C 컴파일러, GNU tar, readelf, sha256sum, flock을 준비한 뒤 `scripts/package.sh 0.1.3`으로 만듭니다. `dist/`에 소스 압축 파일과 현재 CPU용 실행 파일이 담긴 압축 파일, 체크섬 파일이 생깁니다. 두 CPU용 배포본을 함께 만들려면 `ARCHES='amd64 arm64'`를 지정하고 `CC_AMD64`와 `CC_ARM64`에 각각의 musl 컴파일러를 설정하세요.

## 라이선스

EmailRelay는 MIT 라이선스로 배포합니다. 외부 구성 요소와 포함된 글꼴에는 각각의 라이선스가 적용됩니다. 고지문은 소스의 `deploy/licenses/`, Linux 배포본의 `THIRD_PARTY/`, 컨테이너의 `/usr/share/licenses/emailrelay/`에서 확인할 수 있습니다. [MIT 라이선스 전문](LICENSE)
