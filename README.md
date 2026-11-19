# EmailRelay

English · [한국어](README.ko.md)

Read mail from several accounts in the inbox you already use. EmailRelay runs on your own server. It checks each source mailbox over IMAP or POP3 and delivers new messages to a destination you choose. Each route has its own source, destination and delivery history. Gmail, Yahoo Mail and Naver have server presets. You can also enter the settings for another provider.

| How you want to read the mail | Delivery method |
| --- | --- |
| Open the original message in a folder alongside your other mail | **IMAP append** copies it into an existing destination folder. The sender, MIME body and attachments are preserved. For IMAP sources, EmailRelay also copies the internal date and supported flags. It adds a header to prevent relay loops. |
| Receive forwarded mail at an address, even without IMAP access to that inbox | **SMTP forwarding** sends a new message from an authorized sender and attaches the complete original as an `.eml` file. |

## Start with Docker

Clone this repository or download and extract its source archive. On a Linux host with Docker Engine and Compose v2, run the following in the directory containing `compose.yaml`. The first build downloads the base images and Go dependencies. Docker Desktop users need host networking enabled for Linux containers. [Docker host networking](https://docs.docker.com/engine/network/drivers/host/)

```sh
docker compose up -d --build
docker compose exec relay cat /data/emailrelay/setup-token
```

Open [the local dashboard](http://localhost:8787) and use the one-time code to create the first administrator. On a remote server, run `ssh -L 8787:127.0.0.1:8787 user@server` from your computer, then open the same address. Registration retires the setup code. The administrator can now create accounts for other users. The dashboard is in Korean. The labels below match the screen.

## Set up and verify a route

1. Open **경로 추가** (Add route). Choose the source protocol and enter the mailbox credentials. For IMAP, enter the folder to read, usually `INBOX`. Choose the source and destination presets independently or enter the servers yourself.
2. Choose **IMAP 삽입** for a folder copy or **SMTP 전달** for a forwarded message. Create the destination folder first if using IMAP. For SMTP, use a sender address the account is permitted to send from.
3. Under **처음 가져올 메일**, choose **새로 도착하는 메일** (new arrivals only) or **기존 메일 포함 전체** (include existing mail). Saving starts the route. With new arrivals only, the first successful inventory records the messages already visible in the source. With existing mail included, those messages enter the delivery queue.
4. Use **연결 확인** to check access. For an end-to-end test, wait for **중계 사용 중** (active), then send one message to the source. Check **기록 보기** (history) and open the destination folder or inbox. A test message already present in the initial inventory counts as existing mail.

You can change the import scope until the first inventory completes. Afterward, the source account, source folder and import scope are fixed for that route. You can still update its passwords and destination. IMAP and POP3 access use a username and password over TLS. If the provider requires an app password, issue one under that account’s policy. Accounts that require OAuth2 cannot currently connect. Gmail POP uses recent mode, which exposes mail from the last 30 days. Use IMAP for older mail. [Google app passwords](https://support.google.com/accounts/answer/185833) · [Gmail POP settings](https://support.google.com/mail/answer/7104828)

## Serve the dashboard over HTTPS

**Using an existing NGINX or Caddy proxy.** Copy `.env.example` to `.env`. Set `RELAY_ORIGIN=https://relay.example.org` to your public origin and run `docker compose up -d --build`. On the same host, proxy that domain to `127.0.0.1:8787` and preserve the browser’s original `Host` header. For NGINX, set `proxy_set_header Host $http_host;`. Serve the dashboard at the root of its own hostname. You can change the loopback port with `RELAY_PORT`.

Login attempts normally share the proxy’s IP limit. To count attempts per client, have the proxy overwrite `X-Real-IP` with the connecting client’s IP and set `RELAY_TRUST_PROXY_IP=true`. For NGINX, use `proxy_set_header X-Real-IP $remote_addr;`. Enable this only on a trusted host where the proxy controls access to the loopback backend. The bundled Caddy configuration sets this up for you.

**Using the bundled Caddy configuration.** Point your domain’s DNS records to the server, make TCP ports 80 and 443 reachable, and set `RELAY_DOMAIN` in `.env`. Start both services with the command below. Caddy handles certificate issuance and renewal.

```sh
docker compose -f compose.yaml -f compose.https.yaml up -d --build
```

## Install directly on Linux

The static Linux archives include Go and SQLite. Choose `amd64` for `x86_64` or `arm64` for `aarch64`. The host needs a CA certificate bundle. Download the archive and `SHA256SUMS` together, then run `sha256sum --check --ignore-missing SHA256SUMS` before extracting it. [Releases](https://github.com/HanyeolKo/EmailRelay/releases)

From the extracted directory, run `./bin/emailrelay`. Open the [local dashboard](http://127.0.0.1:8787) and read the one-time code from `setup-token` in the data directory. By default this is `$XDG_DATA_HOME/emailrelay`, falling back to `~/.local/share/emailrelay`.

To build the application from source, use Linux, Go 1.26 or newer, and a C compiler for SQLite.

```sh
mkdir -p bin
CGO_ENABLED=1 go build -trimpath -o bin/emailrelay ./cmd/emailrelay
```

To run it as a systemd user service, stop any manually started copy and install the executable and unit.

```sh
install -Dm755 bin/emailrelay "$HOME/.local/bin/emailrelay"
install -Dm644 deploy/emailrelay.service "$HOME/.config/systemd/user/emailrelay.service"
systemctl --user daemon-reload
systemctl --user enable --now emailrelay
```

The unit reads `~/.config/emailrelay/environment`. Restart with `systemctl --user restart emailrelay` after editing it. Read the service log with `journalctl --user -u emailrelay`. For a user service that starts at boot and stays up after logout, a host administrator can enable lingering for the service account.

| Native setting | Default | Purpose |
| --- | --- | --- |
| `RELAY_LISTEN` | `127.0.0.1:8787` | Dashboard listen address |
| `RELAY_ORIGIN` | Local listener origin | Exact address used in the browser |
| `RELAY_TRUST_PROXY_IP` | `false` | Apply login limits to the single client IP set by the trusted local HTTPS proxy |
| `RELAY_TLS_CERT`, `RELAY_TLS_KEY` | Unset | Certificate chain and private key for direct HTTPS. Required for a non-loopback listener |
| `RELAY_INTERVAL` | `1m` | How often to check eligible routes. Minimum 5 seconds |
| `RELAY_MAX_MIB` | `32` | Maximum original message size, from 1 to 256 MiB |
| `RELAY_DATA` | User data directory | Private directory for the database, key and process lock. Set permissions to 0700 |
| `RELAY_ALLOWED_CIDRS` | Unset | Comma-separated network exceptions for private mail servers, such as `10.1.2.3/32` |

Mail connections use certificate-verified TLS or mandatory STARTTLS. For an internal mail server, allow its address and make its issuing CA available to the relay process.

## Delivery and recovery

One Go process serves the dashboard and checks up to three routes concurrently, handling up to 12 messages per route in a pass. SQLite keeps account settings and delivery checkpoints. Each user, including an administrator, can view only their own routes and history. Web passwords use bcrypt. Mailbox settings are encrypted with AES-256-GCM. The decryption key is held in a separate owner-only file. Someone who obtains both that key and the database can read the stored mailbox credentials.

- **Source mail.** IMAP reads messages without marking them as read. POP3 requires stable UIDLs and supports a list of up to 100,000 messages. The relay leaves originals on the server. Check the provider’s own POP retention settings. Later changes or deletions in the source mailbox are not synchronized.
- **Naver IMAP.** If Naver supplies no usable UIDVALIDITY at setup, EmailRelay identifies messages by their original bytes and server receipt time. It compares the mailbox listing against a fresh read-only connection on each check. A stale listing, reconnection or six-hour cache expiry can trigger a full read and delay delivery by several minutes. Message bodies and attachments are never written to the relay database. This mode supports up to 100,000 visible messages. If identical copies with the same receipt time replace each other between checks and their count stays the same, the server provides no way to distinguish them.
- **Failed or uncertain delivery.** A temporary failure is retried after one minute, with the delay increasing up to one hour. If the destination’s final acknowledgment is lost, the route pauses for review and keeps its destination fixed. Look in that folder or inbox. Choose **건너뛰기** (skip) if the message is there, or **다시 전송** (resend) if it is missing. You can still update the password or route name while a delivery is under review.
- **Size or folder changes.** An oversized message waits for a decision. Raise the limit and retry, or skip it. If a previously valid IMAP UIDVALIDITY changes, create a new route and choose its import scope.

## Back up, upgrade and restore

The Docker volume `relay-data` holds the database and `master.key`. Together they preserve credentials and delivery history. Store their backup with owner-only access. The bundled Caddy uses separate `caddy-data` and `caddy-config` volumes.

```sh
umask 077
backup="emailrelay-backup-$(date +%Y%m%d-%H%M%S).tgz"
docker compose stop relay
docker compose run --rm --no-deps -T --entrypoint tar relay -C /data -czf - emailrelay > "$backup"
tar -tzf "$backup"
docker compose start relay
```

For an upgrade, copy the existing `.env` into the new source directory and run your original Compose command there. Keep the same Compose project name so the new container uses the existing volume. The default project name is `emailrelay`. The `down -v` option deletes named volumes.

For disaster recovery, stop the old relay, build the image on the replacement host, and restore the backup into an empty data volume before starting. Substitute the actual backup filename below. If using bundled Caddy, include both Compose files in the final command.

```sh
docker compose build relay
docker compose run --rm --no-deps -T --entrypoint tar relay -C /data -xzf - < emailrelay-backup.tgz
docker compose up -d
```

For a native installation, stop the user service, back up its entire data directory, install the new executable and start the service. When restoring, use the database and key from the same backup and retain the directory’s 0700 permissions.

## Test and package

Run `go test -race ./...` and `go vet ./...` from the source tree. To create static Linux packages, run `scripts/package.sh 0.1.3` with a musl C compiler, GNU tar, readelf, sha256sum and flock installed. The script writes a source archive, a native archive and checksums to `dist/`. For a two-architecture build, set `ARCHES='amd64 arm64'`, `CC_AMD64` and `CC_ARM64` to use the corresponding musl toolchains.

## License

EmailRelay is distributed under the [MIT License](LICENSE). Third-party components and the bundled font retain their own licenses. Notices are in `deploy/licenses/` in the source, `THIRD_PARTY/` in native archives and `/usr/share/licenses/emailrelay/` in the container.
