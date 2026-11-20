package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	invite "github.com/ciranda-dev/kolo-invite"
)

// logCapture keeps what the chat logs, to check that no secret is in it.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLog sends the log to a buffer until the test ends.
func captureLog(t *testing.T) *logCapture {
	t.Helper()
	l := &logCapture{}
	log.SetOutput(io.MultiWriter(l, os.Stderr))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

// joiner is the configuration of someone who has no conversation key and no pin: both come from an invite.
func (n network) joiner(dir string) Config {
	return Config{DataDir: dir, Bootstrap: n.bootstrap, LookupInterval: time.Second}
}

func (e *events) request(i int) (InviteRequest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) <= i {
		return InviteRequest{}, false
	}
	return e.requests[i], true
}

func (e *events) joinStatuses() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, j := range e.joins {
		out = append(out, j.Status)
	}
	return out
}

func (e *events) dump() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.all, "\n")
}

// joinAsync runs Join in the background, since an invite that needs approval waits for a person.
func joinAsync(c *ChatService, code string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.Join(code) }()
	return done
}

func waitJoin(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		t.Fatal("Join did not return")
		return nil
	}
}

func mustCreate(t *testing.T, c *ChatService, opts InviteOptions) InviteInfo {
	t.Helper()
	info, err := c.CreateInvite(opts)
	if err != nil {
		t.Fatal(err)
	}
	if info.Code == "" || info.ID == "" {
		t.Fatalf("an invite needs a code and an ID: %+v", info)
	}
	return info
}

// refused checks that a join failed with the kolo-invite error want, and with a sentence for the window that
// holds every word of says.
func refused(t *testing.T, err error, want error, says ...string) {
	t.Helper()
	var ie *InviteError
	if !errors.Is(err, want) || !errors.As(err, &ie) {
		t.Fatalf("want %v as an *InviteError, got %v", want, err)
	}
	for _, w := range says {
		if !strings.Contains(strings.ToLower(ie.Message), w) {
			t.Fatalf("the sentence %q should say %q", ie.Message, w)
		}
	}
}

// linked waits until two chats are connected on the conversation's topic.
func linked(t *testing.T, a, b *ChatService) {
	t.Helper()
	has := func(c, other *ChatService) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.peers[hex.EncodeToString(other.pub)]
		return ok
	}
	eventually(t, "the chats to connect", func() bool { return has(a, b) && has(b, a) })
}

// The whole story with three instances: an owner, B who joins with approval, and C who is refused again and
// again (used up, expired, revoked, denied) until an invite that works. Messages flow between the owner and B,
// including the ones B misses while it is closed, which come from the pin.
func TestInviteJoinAndChat(t *testing.T) {
	logs := captureLog(t)
	n := newNetwork(t)
	a, aev := start(t, n.config(dataDir(t)))
	bdir, cdir := dataDir(t), dataDir(t)
	b, bev := start(t, n.joiner(bdir))
	c, cev := start(t, n.joiner(cdir))

	// Before an invite there is no conversation to talk in or to invite to.
	if b.Joined() || !a.Joined() {
		t.Fatal("only the owner starts in a conversation")
	}
	if err := b.Send("anyone?"); !errors.Is(err, ErrNotInConversation) {
		t.Fatalf("got %v", err)
	}
	if _, err := b.CreateInvite(InviteOptions{}); !errors.Is(err, ErrNotInConversation) {
		t.Fatalf("got %v", err)
	}
	var codes []string

	// B joins with an invite that needs approval, and gets in when the owner approves.
	first := mustCreate(t, a, InviteOptions{TTLSeconds: 3600, Uses: 1, RequireApproval: true})
	codes = append(codes, first.Code)
	if !first.RequireApproval || first.State != "active" || first.Uses != 1 || first.Expires == "" {
		t.Fatalf("got %+v", first)
	}
	joining := joinAsync(b, first.Code)
	eventually(t, "the request", func() bool { _, ok := aev.request(0); return ok })
	req, _ := aev.request(0)
	if req.Joiner != b.id.PublicKeyString() || req.Invite != first.ID || req.Withdrawn {
		t.Fatalf("got %+v", req)
	}
	select {
	case err := <-joining:
		t.Fatalf("Join returned before the owner answered: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := a.Approve(req.Request); err != nil {
		t.Fatal(err)
	}
	if err := waitJoin(t, joining); err != nil {
		t.Fatal(err)
	}
	if !b.Joined() {
		t.Fatal("B should be in the conversation")
	}
	if got := strings.Join(bev.joinStatuses(), ","); got != "joining,waiting,joined" {
		t.Fatalf("the window was told %q", got)
	}
	infos, err := a.Invites()
	if err != nil || len(infos) != 1 || infos[0].Used != 1 || infos[0].State != "used up" {
		t.Fatalf("got %+v, %v", infos, err)
	}

	// What an invite gave B lives in its data directory, readable by B alone.
	if fi, err := os.Stat(filepath.Join(bdir, "conversation.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("conversation.json: %v, %v", fi, err)
	}
	saved, found, err := loadConversation(bdir)
	if err != nil || !found || hex.EncodeToString(saved.Key) != testKey || saved.PinAddr != n.pin.Addr || saved.check() != nil {
		t.Fatalf("saved %+v, %v, %v", saved, found, err)
	}

	// C is refused with a sentence that says why. The first invite has no use left.
	err = c.Join(first.Code)
	refused(t, err, invite.ErrUsedUp, "no uses left")
	if c.Joined() {
		t.Fatal("a refused join joins nothing")
	}
	if got := cev.joinStatuses(); got[len(got)-1] != "failed" {
		t.Fatalf("the window was told %v", got)
	}

	// An expired invite.
	short := mustCreate(t, a, InviteOptions{TTLSeconds: 1, Uses: 1})
	codes = append(codes, short.Code)
	time.Sleep(2 * time.Second)
	refused(t, c.Join(short.Code), invite.ErrExpired, "expired")

	// A revoked invite stops working, though it still has uses and time left.
	many := mustCreate(t, a, InviteOptions{TTLSeconds: 3600, Uses: 5})
	codes = append(codes, many.Code)
	if err := a.Revoke(many.ID); err != nil {
		t.Fatal(err)
	}
	refused(t, c.Join(many.Code), invite.ErrRevoked, "revoked")
	refused(t, c.Join(many.Code), invite.ErrRevoked, "revoked") // a reused code fails again
	if err := a.Revoke("not hex"); err == nil {
		t.Fatal("a bad invite ID must fail")
	}

	// A code that is no code, and a code of nobody.
	refused(t, c.Join("not an invite"), invite.ErrInvalidCode, "not an invite code")

	// A denied request fails clearly, and the owner is told the request is gone when C leaves.
	manual := mustCreate(t, a, InviteOptions{TTLSeconds: 3600, Uses: 2, RequireApproval: true})
	codes = append(codes, manual.Code)
	denied := joinAsync(c, manual.Code)
	// Answering B's request told the window it is gone, so Approve and Deny leave the screen.
	if w, ok := aev.request(1); !ok || !w.Withdrawn {
		t.Fatalf("an answered request must be withdrawn from the window, got %+v", w)
	}
	eventually(t, "the second request", func() bool { _, ok := aev.request(2); return ok })
	req, _ = aev.request(2)
	if req.Joiner != c.id.PublicKeyString() {
		t.Fatalf("got %+v", req)
	}
	if err := a.Deny(req.Request); err != nil {
		t.Fatal(err)
	}
	refused(t, waitJoin(t, denied), invite.ErrDenied, "denied")
	if err := a.Approve(req.Request); !errors.Is(err, invite.ErrNotFound) {
		t.Fatalf("an answered request is gone, got %v", err)
	}

	// B and the owner talk, directly.
	linked(t, a, b)
	if err := a.Send("hello b"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the message to B", func() bool { return len(bev.messages()) == 1 })
	if m := bev.messages()[0]; m.Text != "hello b" || m.Via != "direct" || m.From != a.id.PublicKeyString() {
		t.Fatalf("got %+v", m)
	}
	if err := b.Send("hello a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the message to the owner", func() bool { return len(aev.messages()) == 1 })
	if m := aev.messages()[0]; m.Text != "hello a" || m.Via != "direct" || m.From != b.id.PublicKeyString() {
		t.Fatalf("got %+v", m)
	}

	// B is closed while the owner writes. Every message is on the pin, which has the owner's log; B comes back
	// with what its data directory kept, a conversation key it never had in a setting.
	if err := b.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	a.Send("two")
	a.Send("three")
	eventually(t, "the pin to hold three entries", func() bool { return pinned(t, n, a).Entries == 3 })
	b2, b2ev := start(t, n.joiner(bdir))
	if !b2.Joined() {
		t.Fatal("B should come back in its conversation")
	}
	eventually(t, "the missed messages", func() bool { return len(b2ev.messages()) >= 2 })
	got := b2ev.messages()
	if got[0].Text != "two" || got[1].Text != "three" || got[0].Via != "pin" || got[1].Via != "pin" {
		t.Fatalf("got %+v", got)
	}
	time.Sleep(2 * time.Second)
	if got := b2ev.messages(); len(got) != 2 {
		t.Fatalf("each message shows once, in order, got %v", texts(got))
	}
	linked(t, a, b2)
	a.Send("four")
	eventually(t, "the next message", func() bool { return len(b2ev.messages()) == 3 })
	if m := b2ev.messages()[2]; m.Text != "four" || m.Via != "direct" {
		t.Fatalf("got %+v", m)
	}

	// C finally joins with an invite that needs no approval, and the owner is told who came.
	open := mustCreate(t, a, InviteOptions{TTLSeconds: 3600, Uses: 1})
	codes = append(codes, open.Code)
	if err := c.Join(open.Code); err != nil {
		t.Fatal(err)
	}
	linked(t, a, c)
	if err := c.Send("hi from c"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the message from C", func() bool { return len(aev.messages()) == 2 })
	if m := aev.messages()[1]; m.Text != "hi from c" || m.From != c.id.PublicKeyString() {
		t.Fatalf("got %+v", m)
	}

	// The conversation key is nowhere in the clear: not in the logs, not in what the windows were told, not in
	// the errors, and neither are the invite codes.
	key, _ := hex.DecodeString(testKey)
	secrets := []string{testKey, strings.ToUpper(testKey), base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key)}
	secrets = append(secrets, codes...)
	where := map[string]string{"log": logs.String(), "owner": aev.dump(), "B": bev.dump() + b2ev.dump(), "C": cev.dump()}
	for place, text := range where {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("a secret is in the %s output", place)
			}
		}
	}
}

// A payload that is not what an inviter sends is refused, whatever the invite says.
func TestReadGrantRefusesBadPayloads(t *testing.T) {
	n := newNetwork(t)
	owner, _ := start(t, n.config(dataDir(t)))
	other, _ := start(t, n.config(dataDir(t)))
	good, err := owner.grantPayload()
	if err != nil {
		t.Fatal(err)
	}
	if g, err := readGrant(good, owner.pub); err != nil || hex.EncodeToString(g.Key) != testKey {
		t.Fatalf("got %+v, %v", g, err)
	}
	var g grant
	json.Unmarshal(good, &g)
	with := func(change func(*grant)) []byte {
		v := g
		change(&v)
		payload, _ := json.Marshal(v)
		return payload
	}
	for name, payload := range map[string][]byte{
		"not JSON":      []byte("nope"),
		"empty":         nil,
		"other version": with(func(v *grant) { v.Version = 2 }),
		"short key":     with(func(v *grant) { v.Key = v.Key[:16] }),
		"no pin key":    with(func(v *grant) { v.PinKey = nil }),
		"bad pin":       with(func(v *grant) { v.PinAddr = "pin.example" }),
		"no header":     with(func(v *grant) { v.Header = nil }),
	} {
		if _, err := readGrant(payload, owner.pub); !errors.Is(err, invite.ErrProtocol) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := readGrant(good, other.pub); !errors.Is(err, invite.ErrProtocol) {
		t.Errorf("a header of another owner: got %v", err)
	}
}

func TestInviteOptionsAreChecked(t *testing.T) {
	n := newNetwork(t)
	a, _ := start(t, n.config(dataDir(t)))
	for name, opts := range map[string]InviteOptions{
		"negative expiry": {TTLSeconds: -5},
		"too long":        {TTLSeconds: int64(maxInviteTTL/time.Second) + 1},
		"too many uses":   {Uses: maxInviteUses + 1},
	} {
		if _, err := a.CreateInvite(opts); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
	info := mustCreate(t, a, InviteOptions{}) // the defaults: one day, one use, no approval
	if info.Uses != 1 || info.RequireApproval {
		t.Fatalf("got %+v", info)
	}
	if exp, err := time.Parse(time.RFC3339, info.Expires); err != nil || time.Until(exp) < 23*time.Hour {
		t.Fatalf("got %q, %v", info.Expires, err)
	}
	for i := 1; i < maxActiveInvites; i++ {
		mustCreate(t, a, InviteOptions{})
	}
	if _, err := a.CreateInvite(InviteOptions{}); err == nil || !strings.Contains(err.Error(), "active invites") {
		t.Fatalf("got %v", err)
	}
	if err := a.Revoke(info.ID); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, a, InviteOptions{}) // a revoked invite makes room
}

// A joiner starts without a key or a pin, and the pin settings alone are not a joiner.
func TestJoinerConfig(t *testing.T) {
	set, err := (Config{}).resolve()
	if err != nil || set.inConversation {
		t.Fatalf("got %+v, %v", set, err)
	}
	for name, cfg := range map[string]Config{
		"a pin key": {PinKey: testKey},
		"a token":   {PinToken: "00"},
	} {
		var ce *ConfigError
		if _, err := cfg.resolve(); !errors.Is(err, ErrMissing) || !errors.As(err, &ce) || ce.Setting != "conversation-key" {
			t.Errorf("%s without a conversation key: got %v", name, err)
		}
	}
}

// An owner cannot start on a data directory that joined another conversation.
func TestConversationOfTheDataDirectoryWins(t *testing.T) {
	n := newNetwork(t)
	dir := dataDir(t)
	other := conversation{Key: bytes.Repeat([]byte{7}, 32), PinAddr: n.pin.Addr, PinKey: n.pin.ID}
	if err := saveConversation(dir, other); err != nil {
		t.Fatal(err)
	}
	_, err := NewChatService(n.config(dir), (&events{}).emit).Start()
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Setting != "conversation-key" {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("the error shows the key")
	}
	// A joiner that has it goes on with it.
	b, _ := start(t, n.joiner(dir))
	if !b.Joined() {
		t.Fatal("the saved conversation should be the joiner's")
	}
}
