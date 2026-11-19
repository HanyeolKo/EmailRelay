// Package mailwire copies messages between TLS-authenticated mail servers.
package mailwire

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Public errors contain only local descriptions. In particular, server reply
// text can contain echoed commands or credentials and must not be propagated.
var (
	ErrEndpoint         = errors.New("invalid mail server settings")
	ErrNetworkPolicy    = errors.New("mail server address blocked by network policy")
	ErrConnect          = errors.New("could not connect to the mail server")
	ErrTLS              = errors.New("mail server TLS negotiation or certificate verification failed")
	ErrAuth             = errors.New("mail server authentication failed")
	ErrSource           = errors.New("unable to read the source mailbox")
	ErrDestination      = errors.New("destination mailbox or submission server rejected the request")
	ErrAmbiguous        = errors.New("delivery acknowledgement lost; check the destination before retrying")
	ErrVanished         = errors.New("source message no longer exists")
	ErrTooLarge         = errors.New("source message exceeds the configured size limit")
	ErrUIDValidity      = errors.New("source mailbox UID validity changed")
	ErrNoUIDValidity    = errors.New("source mailbox did not provide UID validity")
	ErrIdentityChanged  = errors.New("source message identity changed during synchronization")
	ErrIMAPInventory    = errors.New("IMAP search did not return a complete mailbox inventory")
	ErrIMAPMessageData  = errors.New("IMAP fetch returned incomplete source message data")
	ErrIMAPSizeMismatch = errors.New("IMAP source message size disagrees with the complete body literal")
	ErrLoop             = errors.New("message was already relayed by this instance")
)

type Endpoint struct {
	Host               string
	Port               int
	Security           string
	Username, Password string
}

type Message struct {
	UID   uint32
	Raw   []byte
	Date  time.Time
	Flags []string
}

type Client struct {
	privateNetworks []netip.Prefix
	instanceMarker  string
	roots           *x509.CertPool // nil uses the host trust store
	indexMu         sync.Mutex
	idleIndexes     map[imapIndexKey]*idleIMAPIndex
}

func NewClient(instanceKey []byte, allowedCIDRs string) (*Client, error) {
	c := &Client{}
	if len(instanceKey) > 0 {
		h := hmac.New(sha256.New, instanceKey)
		h.Write([]byte("EmailRelay/mail-loop-marker/v1"))
		c.instanceMarker = hex.EncodeToString(h.Sum(nil)[:16])
	}
	if strings.TrimSpace(allowedCIDRs) != "" {
		for _, cidr := range strings.Split(allowedCIDRs, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
			if err != nil {
				return nil, errors.New("invalid mail server network CIDR")
			}
			c.privateNetworks = append(c.privateNetworks, p.Masked())
		}
	}
	return c, nil
}

var specialNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

var globalIPv6 = netip.MustParsePrefix("2000::/3")

func (c *Client) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	for _, p := range c.privateNetworks {
		if p.Contains(ip) {
			return true
		}
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !globalIPv6.Contains(ip) {
		return false
	}
	for _, p := range specialNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func fieldOK(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\r\n\x00")
}

// dial checks every resolved address, then dials a checked numeric IP. The
// original hostname is retained for SNI and certificate verification.
func (c *Client) dial(ctx context.Context, ep Endpoint) (net.Conn, *tls.Config, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	conn, cfg, closeConn, err := c.dialUntilCanceled(ctx, ep)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return conn, cfg, func() { closeConn(); cancel() }, nil
}

func (c *Client) dialUntilCanceled(ctx context.Context, ep Endpoint) (net.Conn, *tls.Config, func(), error) {
	if !fieldOK(ep.Host) || strings.ContainsAny(ep.Host, "%@/[] \\") ||
		ep.Port < 1 || ep.Port > 65535 || (ep.Security != "tls" && ep.Security != "starttls") ||
		!fieldOK(ep.Username) || !fieldOK(ep.Password) {
		return nil, nil, nil, ErrEndpoint
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", ep.Host)
	if err != nil || len(ips) == 0 {
		return nil, nil, nil, ErrConnect
	}
	for _, ip := range ips {
		if !c.allowed(ip) {
			return nil, nil, nil, ErrNetworkPolicy
		}
	}
	var raw net.Conn
	dialer := net.Dialer{Timeout: 8 * time.Second}
	for _, ip := range ips {
		raw, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(ep.Port)))
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, nil, nil, ErrConnect
	}
	// IMAP manages its own read deadlines. Closing the socket on cancellation
	// enforces the caller deadline even while a library waits for a tagged reply.
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	cleanup := func() { stop(); raw.Close() }
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: ep.Host, RootCAs: c.roots}
	if ep.Security == "tls" {
		conn := tls.Client(raw, cfg)
		if err := conn.HandshakeContext(ctx); err != nil {
			cleanup()
			return nil, nil, nil, ErrTLS
		}
		return conn, cfg, cleanup, nil
	}
	return raw, cfg, cleanup, nil
}
