package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"emailrelay/internal/httpapi"
	"emailrelay/internal/mailwire"
	"emailrelay/internal/relay"
	"emailrelay/internal/store"
)

// Release builds set version with -ldflags "-X=main.version=<release>".
var version = "dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print the release and platform")
	data := flag.String("data", os.Getenv("RELAY_DATA"), "private data directory (default: emailrelay under XDG_DATA_HOME)")
	listen := flag.String("listen", env("RELAY_LISTEN", "127.0.0.1:8787"), "HTTP listener behind a trusted HTTPS proxy, or local loopback")
	originArg := flag.String("origin", os.Getenv("RELAY_ORIGIN"), "external browser origin, e.g. https://mail.example.org")
	cert := flag.String("tls-cert", os.Getenv("RELAY_TLS_CERT"), "HTTPS certificate chain")
	priv := flag.String("tls-key", os.Getenv("RELAY_TLS_KEY"), "HTTPS private key")
	interval, err := time.ParseDuration(env("RELAY_INTERVAL", "1m"))
	if err != nil {
		return err
	}
	poll := flag.Duration("interval", interval, "polling interval (minimum 5s)")
	max, err := strconv.ParseInt(env("RELAY_MAX_MIB", "32"), 10, 32)
	if err != nil {
		return err
	}
	maxMiB := flag.Int64("max-mib", max, "maximum source message size in MiB")
	flag.Parse()
	if *showVersion {
		fmt.Printf("EmailRelay %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if *data == "" {
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			base = filepath.Join(home, ".local", "share")
		}
		*data = filepath.Join(base, "emailrelay")
	}
	if *poll < 5*time.Second || *maxMiB < 1 || *maxMiB > 256 {
		return errors.New("interval must be at least 5s and max-mib between 1 and 256")
	}
	directTLS := *cert != "" && *priv != ""
	if (*cert == "") != (*priv == "") {
		return errors.New("configure both tls-cert and tls-key")
	}
	origin, err := browserOrigin(*listen, *originArg, directTLS)
	if err != nil {
		return err
	}
	trustProxyIP, err := strconv.ParseBool(env("RELAY_TRUST_PROXY_IP", "false"))
	if err != nil || trustProxyIP && (origin.Scheme != "https" || directTLS) {
		return errors.New("RELAY_TRUST_PROXY_IP must be a boolean; enable it only for a loopback HTTP listener behind an HTTPS proxy")
	}
	syscall.Umask(0077)
	if err = os.MkdirAll(*data, 0700); err != nil {
		return err
	}
	info, err := os.Stat(*data)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("use a dedicated owner-only (0700) directory for relay data")
	}
	lock, err := os.OpenFile(filepath.Join(*data, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("data directory is already in use: %w", err)
	}
	dbPath := filepath.Join(*data, "relay.db")
	_, statErr := os.Stat(dbPath)
	key, err := secretFile(filepath.Join(*data, "master.key"), os.IsNotExist(statErr))
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	db, err := store.Open(dbPath, key)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	has, err := db.HasUsers()
	if err != nil {
		return err
	}
	tokenFile := filepath.Join(*data, "setup-token")
	linkFile := filepath.Join(*data, "setup.html")
	cleanup := func() {
		for _, f := range []string{tokenFile, linkFile} {
			if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
				log.Print("could not remove first-run setup file")
			}
		}
	}
	var token string
	if !has {
		bytes, err := secretFile(tokenFile, true)
		if err != nil {
			return err
		}
		token = base64.RawURLEncoding.EncodeToString(bytes)
		page := "<!doctype html><html lang=\"ko\"><meta charset=\"utf-8\"><meta name=\"referrer\" content=\"no-referrer\"><title>메일 중계 초기 설정</title><h1>메일 중계 초기 설정</h1><a href=\"" + html.EscapeString(origin.String()+"#setup="+token) + "\">웹에서 관리자 계정 만들기</a></html>"
		if err = os.WriteFile(linkFile, []byte(page), 0600); err != nil {
			return err
		}
		log.Printf("first run: open %s or enter the code from %s", linkFile, tokenFile)
	} else {
		cleanup()
	}
	wire, err := mailwire.NewClient(key, os.Getenv("RELAY_ALLOWED_CIDRS"))
	if err != nil {
		return err
	}
	jobs := relay.New(db, wire, *poll, *maxMiB<<20)
	server := &http.Server{Addr: *listen, Handler: httpapi.New(db, jobs, origin, token, cleanup, trustProxyIP), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); jobs.Run(ctx) }()
	serveErr := make(chan error, 1)
	if directTLS {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		go func() { serveErr <- server.ServeTLS(listener, *cert, *priv) }()
	} else {
		go func() { serveErr <- server.Serve(listener) }()
	}
	log.Printf("mail relay listening on %s (browser %s)", listener.Addr(), origin)
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func browserOrigin(listen, raw string, directTLS bool) (*url.URL, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	local := func(h string) bool { return h == "localhost" || net.ParseIP(h).IsLoopback() }
	if !local(host) && !directTLS {
		return nil, errors.New("non-loopback listeners require tls-cert and tls-key; bind loopback for a local reverse proxy")
	}
	if raw == "" {
		if directTLS {
			raw = "https://" + listen
		} else {
			raw = "http://" + listen
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid public origin")
	}
	if u.Scheme != "https" && (directTLS || u.Scheme != "http" || !local(host) || !local(u.Hostname())) {
		return nil, errors.New("use an HTTPS public origin for a remote deployment")
	}
	u.Path = ""
	return u, nil
}

// Keep key material outside SQLite. A missing key for an existing DB is fatal.
func secretFile(path string, mayCreate bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && mayCreate {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_EXCL|os.O_CREATE, 0600)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString(base64.RawURLEncoding.EncodeToString(key) + "\n")
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		return key, closeErr
	}
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("file must have owner-only permissions: %s", path)
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("invalid secret file: %s", path)
	}
	return key, nil
}
