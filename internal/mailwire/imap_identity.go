package mailwire

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	maxIMAPIndexAge      = 6 * time.Hour
	maxIMAPIndexMessages = 100_000
)

// UID is only a hint for retrieval. Key is the durable identity.
type IMAPIdentity struct {
	UID uint32
	Key string
}

type imapIndexKey struct {
	routeID                          int64
	host, security, username, folder string
	port                             int
}

func indexKey(routeID int64, ep Endpoint, folder string) imapIndexKey {
	return imapIndexKey{routeID, strings.ToLower(ep.Host), ep.Security, ep.Username, folder, ep.Port}
}

// UIDs can be used within one continuous mailbox selection even if the
// server omits UIDVALIDITY. Destroy these hints when its connection ends.
type imapIndex struct {
	client       *imapclient.Client
	closeClient  func()
	cancel       context.CancelFunc
	passwordHash [sha256.Size]byte
	validity     uint32
	created      time.Time
	digests      map[imap.UID]string
	positions    map[imap.UID]uint32
}

func (i *imapIndex) close() {
	i.cancel()
	if i.closeClient != nil {
		i.closeClient()
	}
}

type idleIMAPIndex struct {
	index *imapIndex
	timer *time.Timer
}

func (c *Client) takeIndex(key imapIndexKey) *imapIndex {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	slot := c.idleIndexes[key]
	if slot == nil {
		return nil
	}
	delete(c.idleIndexes, key)
	slot.timer.Stop()
	return slot.index
}

func (c *Client) keepIndex(key imapIndexKey, index *imapIndex) {
	c.indexMu.Lock()
	if c.idleIndexes == nil {
		c.idleIndexes = make(map[imapIndexKey]*idleIMAPIndex)
	}
	prior := c.idleIndexes[key]
	slot := &idleIMAPIndex{index: index}
	c.idleIndexes[key] = slot
	slot.timer = time.AfterFunc(5*time.Minute, func() {
		c.indexMu.Lock()
		expired := c.idleIndexes[key] == slot
		if expired {
			delete(c.idleIndexes, key)
		}
		c.indexMu.Unlock()
		if expired {
			index.close()
		}
	})
	c.indexMu.Unlock()
	if prior != nil {
		prior.timer.Stop()
		prior.index.close()
	}
}

// ListIMAPIdentities freezes the visible UID set at SEARCH, then hashes
// uncached originals in that same read-only selection. No partial snapshot
// can establish a new-only baseline.
func (c *Client) ListIMAPIdentities(ctx context.Context, routeID int64, ep Endpoint, folder string) ([]IMAPIdentity, error) {
	if !fieldOK(folder) {
		return nil, ErrEndpoint
	}
	key := indexKey(routeID, ep, folder)
	index, uids, err := c.currentIMAPIndex(ctx, ep, folder, key)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, index.cancel)
	ids, err := index.scan(uids)
	stopped := stop()
	if err != nil || !stopped || ctx.Err() != nil {
		index.close()
		if err == nil {
			err = ErrSource
		}
		return nil, err
	}
	c.keepIndex(key, index)
	return ids, nil
}

// Naver can freeze the UID window of an open selection: NOOP and SEARCH
// succeed indefinitely, even when a fresh EXAMINE shows different mail.
// Compare the entire ordered UID inventory, including a full rolling window
// whose count never changes. Only reuse hashes within their own selection.
func (c *Client) currentIMAPIndex(ctx context.Context, ep Endpoint, folder string, key imapIndexKey) (*imapIndex, []imap.UID, error) {
	probeCtx, finishProbe := context.WithTimeout(ctx, 45*time.Second)
	defer finishProbe()
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	fresh := &imapIndex{cancel: cancel, passwordHash: sha256.Sum256([]byte(ep.Password)), created: time.Now()}
	stop := context.AfterFunc(probeCtx, cancel)
	var err error
	var uids []imap.UID
	fresh.client, fresh.closeClient, err = c.imapWithDial(lifetime, ep, c.dialUntilCanceled)
	if err == nil {
		box, selectErr := fresh.client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		if selectErr != nil {
			err = ErrSource
		} else {
			fresh.validity = box.UIDValidity
			uids, err = fresh.inventory()
		}
	}
	stopped := stop()
	if err != nil || !stopped || probeCtx.Err() != nil {
		fresh.close()
		if probeCtx.Err() != nil {
			err = probeCtx.Err()
		}
		return nil, nil, err
	}
	if prior := c.takeIndex(key); prior != nil {
		// A recreated folder may reuse every UID while an older selection
		// still serves its frozen contents. Periodically rebuild even if the
		// UID inventories and validity values are indistinguishable.
		if time.Since(prior.created) < maxIMAPIndexAge && prior.passwordHash == fresh.passwordHash && prior.validity == fresh.validity {
			stopPrior := context.AfterFunc(probeCtx, prior.cancel)
			existing, probeErr := prior.inventory()
			if stopPrior() && probeErr == nil && slices.Equal(existing, uids) {
				fresh.close()
				return prior, existing, nil
			}
		}
		prior.close()
	}
	return fresh, uids, nil
}

func (i *imapIndex) inventory() ([]imap.UID, error) {
	// Let the server announce pending expunges and arrivals before SEARCH.
	if err := i.client.Noop().Wait(); err != nil {
		return nil, ErrSource
	}
	if mailbox := i.client.Mailbox(); mailbox == nil || mailbox.NumMessages > maxIMAPIndexMessages {
		return nil, ErrIMAPInventory
	}
	search, err := i.client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, ErrSource
	}
	set, ok := search.All.(imap.UIDSet)
	mailbox := i.client.Mailbox()
	if !ok || mailbox == nil {
		return nil, ErrIMAPInventory
	}
	// Check completeness before expanding any ranges sent by the server.
	var count uint64
	var last imap.UID
	for _, r := range set {
		if r.Start <= last || r.Stop < r.Start {
			return nil, ErrIMAPInventory
		}
		count += uint64(r.Stop) - uint64(r.Start) + 1
		if count > maxIMAPIndexMessages {
			return nil, ErrIMAPInventory
		}
		last = r.Stop
	}
	if count != uint64(mailbox.NumMessages) {
		return nil, ErrIMAPInventory
	}
	uids := make([]imap.UID, 0, int(count))
	for _, r := range set {
		// A uint32 loop over a range ending at the maximum UID wraps.
		for n := uint64(r.Start); n <= uint64(r.Stop); n++ {
			uids = append(uids, imap.UID(n))
		}
	}
	return uids, nil
}

func (i *imapIndex) scan(uids []imap.UID) ([]IMAPIdentity, error) {
	digests := make(map[imap.UID]string, len(uids))
	positions := make(map[imap.UID]uint32, len(uids))
	var missing []imap.UID
	for n, uid := range uids {
		positions[uid] = uint32(n) + 1
		if digest, ok := i.digests[uid]; ok {
			digests[uid] = digest
		} else {
			missing = append(missing, uid)
		}
	}
	for batch := range slices.Chunk(missing, 64) {
		// go-imap cannot dispatch a UID FETCH whose literal precedes UID.
		// Sequence FETCH handles any response item order. Check the returned
		// UID at its expected position to detect expunges between commands.
		var seqs imap.SeqSet
		for _, uid := range batch {
			seqs.AddNum(positions[uid])
		}
		cmd := i.client.Fetch(seqs, identityFetchOptions(false))
		for row := cmd.Next(); row != nil; row = cmd.Next() {
			msg, digest, err := readIdentityRow(row, -1)
			if err != nil {
				// Closing the session unblocks a literal without draining
				// the remaining messages of a failed command.
				i.cancel()
				cmd.Close()
				return nil, err
			}
			uid := imap.UID(msg.UID)
			_, requested := slices.BinarySearch(batch, uid)
			if !requested || positions[uid] != row.SeqNum {
				i.cancel()
				cmd.Close()
				return nil, ErrIdentityChanged
			}
			if digests[uid] != "" {
				i.cancel()
				cmd.Close()
				return nil, ErrIMAPInventory
			}
			digests[uid] = digest
		}
		if err := cmd.Close(); err != nil {
			return nil, ErrIMAPMessageData
		}
		for _, uid := range batch {
			if digests[uid] == "" {
				return nil, ErrIdentityChanged
			}
		}
	}
	occurrences := make(map[string]int)
	ids := make([]IMAPIdentity, 0, len(uids))
	for _, uid := range uids {
		digest := digests[uid]
		occurrences[digest]++
		key := digest + "." + strconv.Itoa(occurrences[digest])
		ids = append(ids, IMAPIdentity{UID: uint32(uid), Key: key})
	}
	i.digests = digests
	i.positions = positions
	return ids, nil
}

func identityFetchOptions(flags bool) *imap.FetchOptions {
	return &imap.FetchOptions{UID: true, InternalDate: true, RFC822Size: true, Flags: flags, BodySection: []*imap.FetchItemBodySection{{Peek: true}}}
}

// maxBytes < 0 streams all bytes through SHA-256 without retaining the body.
// An oversized message is also hashed so a recycled UID cannot mark another
// message blocked. All three lengths must agree before accepting the digest.
func readIdentityRow(row *imapclient.FetchMessageData, maxBytes int64) (Message, string, error) {
	var msg Message
	var digest [sha256.Size]byte
	size, length := int64(-1), int64(-1)
	for item := row.Next(); item != nil; item = row.Next() {
		switch v := item.(type) {
		case imapclient.FetchItemDataUID:
			msg.UID = uint32(v.UID)
		case imapclient.FetchItemDataInternalDate:
			msg.Date = v.Time
		case imapclient.FetchItemDataRFC822Size:
			size = v.Size
		case imapclient.FetchItemDataFlags:
			for _, flag := range v.Flags {
				msg.Flags = append(msg.Flags, string(flag))
			}
		case imapclient.FetchItemDataBodySection:
			if v.Section == nil || !v.MatchCommand(&imap.FetchItemBodySection{Peek: true}) || v.Literal == nil || length != -1 {
				return Message{}, "", ErrIMAPMessageData
			}
			var err error
			literalSize := v.Literal.Size()
			if literalSize < 0 {
				return Message{}, "", ErrIMAPMessageData
			}
			if literalSize <= maxBytes {
				msg.Raw, err = io.ReadAll(io.LimitReader(v.Literal, maxBytes+1))
				length = int64(len(msg.Raw))
				digest = sha256.Sum256(msg.Raw)
			} else {
				h := sha256.New()
				length, err = io.Copy(h, v.Literal)
				h.Sum(digest[:0])
			}
			if err != nil || length != literalSize {
				return Message{}, "", ErrIMAPMessageData
			}
		}
	}
	if msg.UID == 0 || msg.Date.IsZero() || length < 0 || size < 0 {
		return Message{}, "", ErrIMAPMessageData
	}
	if size != length {
		return Message{}, "", ErrIMAPSizeMismatch
	}
	// INTERNALDATE is immutable for a message and provides an additional
	// distinction for identical source copies made at different times.
	combined := [sha256.Size + 8]byte{}
	copy(combined[:], digest[:])
	binary.BigEndian.PutUint64(combined[sha256.Size:], uint64(msg.Date.Unix()))
	sum := sha256.Sum256(combined[:])
	return msg, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func (c *Client) FetchIMAPIdentity(ctx context.Context, routeID int64, ep Endpoint, folder string, identity IMAPIdentity, maxBytes int64) (Message, error) {
	digest, occurrence, _ := strings.Cut(identity.Key, ".")
	n, err := strconv.ParseUint(occurrence, 10, 32)
	if !fieldOK(folder) || identity.UID == 0 || len(digest) != base64.RawURLEncoding.EncodedLen(sha256.Size) || err != nil || n == 0 || maxBytes <= 0 || maxBytes >= 1<<32 {
		return Message{}, ErrEndpoint
	}
	key := indexKey(routeID, ep, folder)
	index := c.takeIndex(key)
	if index == nil {
		return Message{}, ErrIdentityChanged
	}
	if index.passwordHash != sha256.Sum256([]byte(ep.Password)) || index.digests[imap.UID(identity.UID)] != digest {
		index.close()
		return Message{}, ErrIdentityChanged
	}
	stop := context.AfterFunc(ctx, index.cancel)
	verified := false
	defer func() {
		if stop() && ctx.Err() == nil && verified {
			c.keepIndex(key, index)
		} else {
			index.close()
		}
	}()
	cmd := index.client.Fetch(imap.SeqSetNum(index.positions[imap.UID(identity.UID)]), identityFetchOptions(true))
	var msg Message
	var actual string
	for row := cmd.Next(); row != nil; row = cmd.Next() {
		if actual != "" {
			index.cancel()
			cmd.Close()
			return Message{}, ErrSource
		}
		msg, actual, err = readIdentityRow(row, maxBytes)
		if err != nil {
			index.cancel()
			cmd.Close()
			return Message{}, err
		}
	}
	if err := cmd.Close(); err != nil {
		return Message{}, ErrIdentityChanged
	}
	if msg.UID != identity.UID || actual != digest {
		return Message{}, ErrIdentityChanged
	}
	verified = true
	if msg.Raw == nil {
		return Message{}, ErrTooLarge
	}
	if c.hasMarker(msg.Raw) {
		return Message{}, ErrLoop
	}
	return msg, nil
}
