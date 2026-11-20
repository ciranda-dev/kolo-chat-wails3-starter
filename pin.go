package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	koloid "github.com/ciranda-dev/kolo-id"
	kololog "github.com/ciranda-dev/kolo-log"
	"github.com/ciranda-dev/kolo-pin"
	kolosecure "github.com/ciranda-dev/kolo-secure"
	kolostore "github.com/ciranda-dev/kolo-store"
	kolosync "github.com/ciranda-dev/kolo-sync"
)

// pinTimeout bounds one conversation with the pin: connecting, pushing or pulling.
const pinTimeout = 30 * time.Second

// capability returns the signed token that lets the pin keep our logs: the messages and the directory. A token
// from the configuration is checked and used as it is. Otherwise the app issues one with its own identity, for
// the pin and these two logs, keeps it in the data directory and reuses it until half of its lifetime has
// passed, since the pin refuses a second capability issued in the same second as the first. A token from step 4
// covers only the messages, so it is replaced by one for both.
func capability(set settings, id *koloid.Identity, logs ...kololog.LogID) ([]byte, error) {
	if set.token != nil {
		return set.token, checkToken(set.token, id, set.pinKey, logs...)
	}
	path := filepath.Join(set.DataDir, "pin-token")
	if token, err := readFile(path, maxStateFile); err == nil {
		c, err := pin.ParseCapability(token)
		if err == nil && c.Quota == set.PinQuota && time.Until(c.Expires) > set.PinTTL/2 &&
			checkToken(token, id, set.pinKey, logs...) == nil {
			return token, nil
		}
	}
	token, err := pin.Issue(id, pin.Capability{
		Peer:    set.pinKey,
		Logs:    logs,
		Expires: time.Now().Add(set.PinTTL),
		Quota:   set.PinQuota,
	})
	if err != nil {
		return nil, &ConfigError{Setting: "pin-ttl", Err: err}
	}
	if err := os.WriteFile(path, token, 0o600); err != nil {
		return nil, err
	}
	return token, nil
}

// checkToken says why a token cannot work, with the errors of kolo-pin: ErrInvalidCapability for bytes that are
// not a capability signed by its owner, ErrUnauthorized for one that is not from this identity, for this pin and
// for these logs, and ErrExpired for one that has expired. The pin checks all of it again when it is used.
func checkToken(token []byte, id *koloid.Identity, pinKey ed25519.PublicKey, logs ...kololog.LogID) error {
	c, err := pin.ParseCapability(token)
	switch {
	case err != nil:
		err = fmt.Errorf("%w: %w", pin.ErrInvalidCapability, err)
	case !c.Owner.Equal(id.PublicKey()):
		err = fmt.Errorf("%w: issued by another identity", pin.ErrUnauthorized)
	case !c.Peer.Equal(pinKey):
		err = fmt.Errorf("%w: issued for another pin", pin.ErrUnauthorized)
	case slices.ContainsFunc(logs, func(id kololog.LogID) bool { return !c.Covers(id) }):
		err = fmt.Errorf("%w: does not cover this conversation's logs", pin.ErrUnauthorized)
	case !time.Now().Before(c.Expires):
		err = fmt.Errorf("%w: at %s", pin.ErrExpired, c.Expires.Format(time.RFC3339))
	}
	if err != nil {
		return &ConfigError{Setting: "pin-token", Err: err}
	}
	return nil
}

// pushToPin gives the pin our two logs, the messages and the directory, which it pulls and verifies from what
// it does not have yet. It is how every message and every change of the directory reaches the pin, and it can
// be repeated at any time. The errors are those of kolo-pin: pin.ErrExpired, pin.ErrUnauthorized,
// pin.ErrRevoked, pin.ErrQuotaExceeded, pin.ErrDiskFull, pin.ErrUnreachable.
func (c *ChatService) pushToPin(ctx context.Context) error {
	c.pushMu.Lock() // the pin answers busy to a second Add of the same log
	defer c.pushMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, pinTimeout)
	defer cancel()
	peer := pin.Peer{ID: c.set.pinKey, Addr: c.set.PinAddr}
	var info pin.Info
	var errs []error
	for _, l := range []*kolostore.Log{c.log, c.dirLog} {
		got, err := pin.Add(ctx, c.id, peer, l, c.token)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		info.Used, info.Quota = got.Used, got.Quota // the owner's, over both logs
		if l == c.log {
			info.Entries = got.Entries
		}
	}
	err := errors.Join(errs...)
	c.setPin(func(s *pinState) {
		s.Push = describe(err, c.set)
		s.Entries, s.Used, s.Quota = info.Entries, info.Used, info.Quota
	})
	return err
}

// pullFromPin brings the replica of every known peer's log, and of its directory, up to date from the pin. The
// pin sends ciphertext only, so an entry is stored but stays unreadable until its key arrives from the sender
// (see syncFromPeer). A directory then shows what its entries with keys say, which is the older profile.
func (c *ChatService) pullFromPin(ctx context.Context) error {
	authors := slices.DeleteFunc(c.authors(), func(a string) bool { return !c.isAccepted(a) }) // the roster only
	if len(authors) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pinTimeout)
	defer cancel()
	conn, err := kolosecure.Dial(ctx, c.set.PinAddr, c.id, c.set.pinKey, nil)
	if err != nil {
		err = fmt.Errorf("%w: %w", pin.ErrUnreachable, err)
		c.setPin(func(s *pinState) { s.Pull = describe(err, c.set) })
		return err
	}
	defer conn.Close()
	var errs []error
	for _, author := range authors {
		if err := c.pullPinnedMessages(ctx, conn, author); err != nil {
			errs = append(errs, fmt.Errorf("pin pull: %w", err))
		}
		err := c.pullDirectory(ctx, conn, author, true)
		if err != nil && !errors.Is(err, kolosync.ErrNotFound) && !errors.Is(err, errNoDirectory) {
			errs = append(errs, fmt.Errorf("pin pull of directory: %w", err))
		}
	}
	err = errors.Join(errs...)
	c.setPin(func(s *pinState) { s.Pull = describe(err, c.set) })
	return err
}

// pullPinnedMessages pulls the ciphertext of one author's messages from the pin and shows what can be read.
func (c *ChatService) pullPinnedMessages(ctx context.Context, conn *kolosecure.Conn, author string) error {
	rep, err := c.replica(author)
	if err == nil {
		c.pullMu.Lock()
		err = kolosync.PullCiphertext(ctx, conn, rep)
		c.pullMu.Unlock()
	}
	if errors.Is(err, kolosync.ErrNotFound) {
		return nil // the author has not pinned anything yet
	}
	if err != nil {
		return err
	}
	c.markPinned(author, rep.Len())
	c.show(author)
	return nil
}

// describe turns an error of the pin into the sentence the window shows, and "" for no error.
func describe(err error, set settings) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, pin.ErrExpired):
		return "The pin token has expired, so new messages are not pinned. Restart the app to issue a new one, or set a new -pin-token. Messages still go directly to peers who are online."
	case errors.Is(err, pin.ErrQuotaExceeded):
		return "The pin quota is full, so new messages are not pinned. Raise -pin-quota and restart. Messages still go directly to peers who are online."
	case errors.Is(err, pin.ErrDiskFull):
		return "The pin has no room left, so new messages are not pinned. Ask its operator for more space."
	case errors.Is(err, pin.ErrUnauthorized):
		return "The pin refused this identity or token. Check -pin-key and -pin-token, and ask the operator whether your key is allowed."
	case errors.Is(err, pin.ErrRevoked):
		return "The pin token was revoked or replaced by a newer one."
	case errors.Is(err, pin.ErrUnreachable):
		return "The pin at " + set.PinAddr + " is unreachable. Messages still go directly to peers who are online."
	}
	return "Pin error: " + err.Error()
}
