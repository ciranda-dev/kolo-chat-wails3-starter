package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	kolodht "github.com/ciranda-dev/kolo-dht"
	koloid "github.com/ciranda-dev/kolo-id"
	"github.com/ciranda-dev/kolo-pin"
	kolosecure "github.com/ciranda-dev/kolo-secure"
)

const testKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// events records what a ChatService tells the window. all keeps every event as JSON, for the checks that
// something never shows up in one.
type events struct {
	mu       sync.Mutex
	msgs     []ChatMsg
	pins     []PinStatus
	requests []InviteRequest
	joins    []JoinStatus
	invites  []InviteInfo
	profiles [][]Member
	rooms    [][]string
	all      []string
}

func (e *events) emit(name string, data any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch v := data.(type) {
	case ChatMsg:
		e.msgs = append(e.msgs, v)
	case PinStatus:
		e.pins = append(e.pins, v)
	case InviteRequest:
		e.requests = append(e.requests, v)
	case JoinStatus:
		e.joins = append(e.joins, v)
	case InviteInfo:
		e.invites = append(e.invites, v)
	case []Member:
		e.profiles = append(e.profiles, v)
	case []string:
		e.rooms = append(e.rooms, v)
	}
	if b, err := json.Marshal(data); err == nil {
		e.all = append(e.all, name+" "+string(b))
	}
}

func (e *events) messages() []ChatMsg {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ChatMsg(nil), e.msgs...)
}

func (e *events) pin() PinStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.pins) == 0 {
		return PinStatus{}
	}
	return e.pins[len(e.pins)-1]
}

// dataDir returns a private directory for one chat's data, as the app creates it. t.TempDir is open to others,
// which the identity file refuses.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// network is what the chats run against: a bootstrap node and a pin, both on loopback.
type network struct {
	bootstrap string
	pin       pin.Peer
	pinDir    string
}

func startBootstrap(t *testing.T) string {
	t.Helper()
	id, err := koloid.Generate()
	if err != nil {
		t.Fatal(err)
	}
	node, err := kolodht.New(context.Background(), "127.0.0.1:0", id, &kolodht.Config{Server: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Close() })
	return node.Addr().String()
}

// startPin runs a pinning peer, the same one `kolo-pin run` runs, accepting only owners if any are given.
func startPin(t *testing.T, owners ...ed25519.PublicKey) (pin.Peer, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pin")
	id, err := pin.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := kolosecure.Listen(ctx, "127.0.0.1:0", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- pin.Serve(ctx, ln, pin.Options{Dir: dir, MaxBytes: 50 << 20, Owners: owners}) }()
	t.Cleanup(func() {
		cancel()
		<-done
		ln.Close()
	})
	return pin.Peer{ID: id.PublicKey(), Addr: ln.Addr().String()}, dir
}

func newNetwork(t *testing.T) network {
	peer, dir := startPin(t)
	return network{bootstrap: startBootstrap(t), pin: peer, pinDir: dir}
}

func (n network) config(dir string) Config {
	return Config{
		DataDir:         dir,
		Bootstrap:       n.bootstrap,
		ConversationKey: testKey,
		PinAddr:         n.pin.Addr,
		PinKey:          hex.EncodeToString(n.pin.ID),
		LookupInterval:  time.Second,
	}
}

// start runs a chat with the given config and stops it when the test ends.
func start(t *testing.T, cfg Config) (*ChatService, *events) {
	t.Helper()
	ev := &events{}
	c := NewChatService(cfg, ev.emit)
	if _, err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.ServiceShutdown() })
	return c, ev
}

func knows(c, other *ChatService) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.members[hex.EncodeToString(other.pub)]
	return ok
}

// introduced waits until the two chats have connected and exchanged their hellos.
func introduced(t *testing.T, a, b *ChatService) {
	t.Helper()
	eventually(t, "the peers to meet", func() bool { return knows(a, b) && knows(b, a) })
}

func pinned(t *testing.T, n network, c *ChatService) pin.Info {
	t.Helper()
	info, err := pin.Status(context.Background(), c.id, n.pin, c.log.ID())
	if err != nil {
		return pin.Info{}
	}
	return info
}

func texts(msgs []ChatMsg) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Text)
	}
	return out
}

// (a) Both online: the message arrives directly, and is also in the sender's log and on the pin.
func TestDirectDeliveryAlsoWritesLogAndPin(t *testing.T) {
	n := newNetwork(t)
	a, _ := start(t, n.config(dataDir(t)))
	b, bev := start(t, n.config(dataDir(t)))
	introduced(t, a, b)

	if err := a.Send(defaultRoom, "hello"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the direct message", func() bool { return len(bev.messages()) == 1 })
	if m := bev.messages()[0]; m.Text != "hello" || m.Via != "direct" || m.From != a.id.PublicKeyString() {
		t.Fatalf("got %+v", m)
	}

	if a.log.Len() != 1 {
		t.Fatalf("log has %d entries", a.log.Len())
	}
	stored, err := a.log.Get(0)
	if err != nil || strings.Contains(string(stored), "hello") {
		t.Fatalf("the entry must hold sealed text, got %q, %v", stored, err)
	}
	if room, text, err := a.open(a.pub, stored); err != nil || text != "hello" || room != defaultRoom {
		t.Fatalf("open: %q, %q, %v", room, text, err)
	}
	eventually(t, "the pin to hold the entry", func() bool { return pinned(t, n, a).Entries == 1 })

	time.Sleep(2 * time.Second)
	if got := bev.messages(); len(got) != 1 {
		t.Fatalf("a duplicate arrived: %v", texts(got))
	}
}

// (b) B is closed while A sends. When B opens again it shows the missed messages, in order, once, and not the
// one it got directly before. The ciphertext comes from the pin; the keys come from A, who is online.
func TestMissedMessagesComeFromThePin(t *testing.T) {
	n := newNetwork(t)
	a, _ := start(t, n.config(dataDir(t)))
	bdir := dataDir(t)
	b, bev := start(t, n.config(bdir))
	introduced(t, a, b)

	a.Send(defaultRoom, "one")
	eventually(t, "the direct message", func() bool { return len(bev.messages()) == 1 })
	if err := b.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}

	a.Send(defaultRoom, "two")
	a.Send(defaultRoom, "three")
	eventually(t, "the pin to hold three entries", func() bool { return pinned(t, n, a).Entries == 3 })

	b2, b2ev := start(t, n.config(bdir)) // same data directory, so the same identity and the same members
	eventually(t, "the missed messages", func() bool { return len(b2ev.messages()) >= 2 })
	got := b2ev.messages()
	if got[0].Text != "two" || got[1].Text != "three" || got[0].Via != "pin" || got[1].Via != "pin" {
		t.Fatalf("got %+v", got)
	}
	time.Sleep(2 * time.Second)
	if got := b2ev.messages(); len(got) != 2 {
		t.Fatalf("expected two messages, got %v", texts(got))
	}
	if next, _ := b2.shown(hex.EncodeToString(a.pub)); next != 3 {
		t.Fatalf("shown %d", next)
	}

	// Live again: the next message arrives directly, once.
	introduced(t, a, b2)
	a.Send(defaultRoom, "four")
	eventually(t, "the direct message", func() bool { return len(b2ev.messages()) == 3 })
	if m := b2ev.messages()[2]; m.Text != "four" || m.Via != "direct" {
		t.Fatalf("got %+v", m)
	}
}

// The pin holds ciphertext, and the key of an entry comes only from its sender. With the sender offline, B
// opens, sees that messages wait, and shows them once the sender is back.
func TestPinnedMessagesWaitForTheSendersKey(t *testing.T) {
	n := newNetwork(t)
	adir, bdir := dataDir(t), dataDir(t)
	a, _ := start(t, n.config(adir))
	b, _ := start(t, n.config(bdir))
	introduced(t, a, b)
	b.ServiceShutdown()

	a.Send(defaultRoom, "while you were away")
	eventually(t, "the pin to hold the entry", func() bool { return pinned(t, n, a).Entries == 1 })
	a.ServiceShutdown()

	b2, b2ev := start(t, n.config(bdir))
	eventually(t, "B to see a waiting message", func() bool { return b2ev.pin().Locked == 1 })
	if got := b2ev.messages(); len(got) != 0 {
		t.Fatalf("nothing can be read without the key, got %v", texts(got))
	}

	a2, _ := start(t, n.config(adir)) // A comes back with the same identity and log
	introduced(t, a2, b2)
	eventually(t, "the message", func() bool { return len(b2ev.messages()) == 1 })
	if m := b2ev.messages()[0]; m.Text != "while you were away" || m.Via != "pin" {
		t.Fatalf("got %+v", m)
	}
	eventually(t, "the waiting count to clear", func() bool { return b2ev.pin().Locked == 0 })
}

// (c) A token that does not fit fails Start with a typed error naming the setting.
func TestBadTokenFailsStartWithTypedError(t *testing.T) {
	n := newNetwork(t)
	other, _ := koloid.Generate()
	otherPin, _ := koloid.Generate()

	issue := func(dir string, owner *koloid.Identity, c pin.Capability) string {
		if owner == nil {
			owner, _ = loadIdentity(dir)
		}
		token, err := pin.Issue(owner, c)
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(token)
	}
	for _, tc := range []struct {
		name  string
		token func(dir string) string
		want  error
	}{
		{"not hex", func(string) string { return "zz" }, ErrMalformed},
		{"not a capability", func(string) string { return "00ff00ff" }, pin.ErrInvalidCapability},
		{"another pin", func(dir string) string {
			return issue(dir, nil, pin.Capability{Peer: otherPin.PublicKey(), Expires: time.Now().Add(time.Hour)})
		}, pin.ErrUnauthorized},
		{"another owner", func(dir string) string {
			return issue(dir, other, pin.Capability{Peer: n.pin.ID, Expires: time.Now().Add(time.Hour)})
		}, pin.ErrUnauthorized},
		{"expired", func(dir string) string {
			return issue(dir, nil, pin.Capability{Peer: n.pin.ID, Issued: time.Now().Add(-2 * time.Hour), Expires: time.Now().Add(-time.Hour)})
		}, pin.ErrExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := dataDir(t)
			cfg := n.config(dir)
			cfg.PinToken = tc.token(dir)
			_, err := NewChatService(cfg, (&events{}).emit).Start()
			var ce *ConfigError
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrConfig) || !errors.As(err, &ce) || ce.Setting != "pin-token" {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// (c) The pin refuses a token it does not accept, and the chat goes on working directly.
func TestPinRefusesUnknownOwner(t *testing.T) {
	stranger, _ := koloid.Generate()
	peer, _ := startPin(t, stranger.PublicKey()) // accepts only the stranger
	n := network{bootstrap: startBootstrap(t), pin: peer}
	a, _ := start(t, n.config(dataDir(t)))
	b, bev := start(t, n.config(dataDir(t)))
	introduced(t, a, b)

	a.Send(defaultRoom, "still direct")
	if err := a.pushToPin(context.Background()); !errors.Is(err, pin.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	eventually(t, "the direct message", func() bool { return len(bev.messages()) == 1 })
}

// (c) An expired token is refused by the pin with pin.ErrExpired, and the window is told.
func TestExpiredTokenIsReported(t *testing.T) {
	n := newNetwork(t)
	dir := dataDir(t)
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := pin.Issue(id, pin.Capability{Peer: n.pin.ID, Expires: time.Now().Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := n.config(dir)
	cfg.PinToken = hex.EncodeToString(token)
	a, aev := start(t, cfg)
	if err := a.pushToPin(context.Background()); err != nil {
		t.Fatalf("a fresh token works: %v", err)
	}

	time.Sleep(3 * time.Second)
	a.Send(defaultRoom, "too late for the pin")
	if err := a.pushToPin(context.Background()); !errors.Is(err, pin.ErrExpired) {
		t.Fatalf("got %v", err)
	}
	if msg := aev.pin().Error; !strings.Contains(msg, "expired") {
		t.Fatalf("the window was told %q", msg)
	}
}

// (c) With the quota full the pin refuses with pin.ErrQuotaExceeded, the window is told, and B still gets
// every message directly.
func TestQuotaFullIsReported(t *testing.T) {
	n := newNetwork(t)
	cfg := n.config(dataDir(t))
	cfg.PinQuota = 2048
	a, aev := start(t, cfg)
	b, bev := start(t, n.config(dataDir(t)))
	introduced(t, a, b)

	const total = 12
	for i := 0; i < total; i++ {
		if err := a.Send(defaultRoom, "a message that takes some room"); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every message directly", func() bool { return len(bev.messages()) == total })
	if err := a.pushToPin(context.Background()); !errors.Is(err, pin.ErrQuotaExceeded) {
		t.Fatalf("got %v", err)
	}
	if msg := aev.pin().Error; !strings.Contains(msg, "quota is full") {
		t.Fatalf("the window was told %q", msg)
	}
}

func TestConfig(t *testing.T) {
	env := map[string]string{"KOLO_CHAT_PIN": "10.0.0.5:7450", "KOLO_CHAT_PIN_QUOTA": "1000", "KOLO_CHAT_PIN_KEY": testKey}
	cfg, err := LoadConfig([]string{"-pin", "10.0.0.9:7450", "-pin-ttl", "48h", "-conversation-key", testKey}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PinAddr != "10.0.0.9:7450" || cfg.PinQuota != 1000 || cfg.PinTTL != 48*time.Hour {
		t.Fatalf("a flag beats the environment: %+v", cfg)
	}
	set, err := cfg.resolve()
	if err != nil || set.Bootstrap != defaultBootstrap || set.Listen != defaultListen || set.LookupInterval != defaultLookupInterval {
		t.Fatalf("defaults: %+v, %v", set, err)
	}

	for _, tc := range []struct {
		name   string
		change func(*Config)
		want   error
		field  string
	}{
		{"no conversation key", func(c *Config) { c.ConversationKey = "" }, ErrMissing, "conversation-key"},
		{"short conversation key", func(c *Config) { c.ConversationKey = "abcd" }, ErrMalformed, "conversation-key"},
		{"no pin key", func(c *Config) { c.PinKey = "" }, ErrMissing, "pin-key"},
		{"bad pin address", func(c *Config) { c.PinAddr = "pin.example" }, ErrMalformed, "pin"},
		{"bad bootstrap", func(c *Config) { c.Bootstrap = "x" }, ErrMalformed, "bootstrap"},
	} {
		c := cfg
		tc.change(&c)
		_, err := c.resolve()
		var ce *ConfigError
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrConfig) || !errors.As(err, &ce) || ce.Setting != tc.field {
			t.Errorf("%s: got %v", tc.name, err)
		}
	}
	if _, err := LoadConfig([]string{"-pin-ttl", "soon"}, func(string) string { return "" }, io.Discard); err == nil {
		t.Error("a bad duration must fail")
	}
}
