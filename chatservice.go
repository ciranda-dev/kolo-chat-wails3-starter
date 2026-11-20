package main

import (
	"context"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	koloid "github.com/ciranda-dev/kolo-id"
	kololog "github.com/ciranda-dev/kolo-log"
	kolosecure "github.com/ciranda-dev/kolo-secure"
	kolostore "github.com/ciranda-dev/kolo-store"
	koloswarm "github.com/ciranda-dev/kolo-swarm"
	kolosync "github.com/ciranda-dev/kolo-sync"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	maxMessage  = 64 * 1024
	maxWire     = 128 * 1024 // a message as JSON, with its sealed text in base64
	sendTimeout = 10 * time.Second
	codeTooLong = 1
)

// ChatMsg is the payload of the "chat:msg" event. From is the sender's kolo-id (hex public key) and Via says how
// the message arrived: "direct" from the sender, "pin" when its ciphertext came from the pin, "peer" when it was
// pulled from the sender after the direct delivery was missed.
type ChatMsg struct {
	From string `json:"from"`
	Text string `json:"text"`
	Via  string `json:"via"`
}

// PinStatus is the payload of the "chat:pin" event. Error is the sentence to show when the pin cannot be used,
// and Locked counts the pinned messages that wait for their sender's key.
type PinStatus struct {
	Error  string `json:"error"`
	Locked int    `json:"locked"`
}

// wireMsg is what peers send each other on the direct connection, one unidirectional stream each.
type wireMsg struct {
	Kind   string `json:"kind"`             // "hello" or "msg"
	Header []byte `json:"header,omitempty"` // hello: the sender's signed log header
	Proof  []byte `json:"proof,omitempty"`  // hello: proves the sender knows the conversation key
	Reply  bool   `json:"reply,omitempty"`  // hello: answers a hello, so the sender has registered us
	Index  uint64 `json:"index,omitempty"`  // msg: the entry's index in the sender's log
	Sealed []byte `json:"sealed,omitempty"` // msg: the entry's value, the text sealed with the conversation key
}

// member is a peer of the conversation: its signed log header, which names its public key and log ID, and how
// many of its messages the window has shown. Both are kept in the data directory.
type member struct {
	Header []byte `json:"header"`
	Shown  uint64 `json:"shown"`
}

type pinState struct {
	Push, Pull           string // what to tell the user, "" when it works
	Entries, Used, Quota uint64
}

type ChatService struct {
	cfg  Config
	emit func(name string, data any)

	startMu sync.Mutex
	set     settings
	id      *koloid.Identity
	pub     ed25519.PublicKey
	aead    cipher.AEAD
	token   []byte
	store   *kolostore.Store
	log     *kolostore.Log // our own log, the only one we write
	srv     *kolosync.Server
	sw      *koloswarm.Swarm
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	push    chan struct{} // a push to the pin is wanted
	pinDone chan struct{} // closed when the pull from the pin at start is over

	mu       sync.Mutex // guards the fields below
	peers    map[string]*koloswarm.Peer
	members  map[string]*member
	replicas map[string]*kolostore.Replica
	pinned   map[string]uint64 // per author: entries below this index came from the pin
	locked   map[string]int    // per author: entries that wait for a key
	pin      pinState

	showMu sync.Mutex // one message is shown at a time, so they keep their order
	pullMu sync.Mutex // one pull at a time, since a replica takes one
	pushMu sync.Mutex // one push at a time
}

// NewChatService returns a chat that reads cfg when it starts and tells the window what happens through emit.
func NewChatService(cfg Config, emit func(name string, data any)) *ChatService {
	return &ChatService{cfg: cfg, emit: emit}
}

// Start joins the conversation and returns this user's kolo-id (hex public key). It opens the data directory,
// the log and the pin, so it fails with a *ConfigError for a bad setting and with the errors of kolo-pin
// (pin.ErrExpired, pin.ErrUnauthorized) for a bad token. A pin that cannot be reached is not an error: the chat
// works directly and tells the window.
func (c *ChatService) Start() (string, error) {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.id != nil {
		return c.id.PublicKeyString(), nil
	}
	if err := c.start(); err != nil {
		c.shutdown()
		return "", err
	}
	return c.id.PublicKeyString(), nil
}

func (c *ChatService) start() (err error) {
	if c.set, err = c.cfg.resolve(); err != nil {
		return err
	}
	if err = os.MkdirAll(c.set.DataDir, 0o700); err != nil {
		return err
	}
	if c.id, err = loadIdentity(c.set.DataDir); err != nil {
		return err
	}
	c.pub = c.id.PublicKey()
	if c.aead, err = chacha20poly1305.NewX(c.set.conversationKey[:]); err != nil {
		return err
	}
	backend, err := kolostore.OpenPebble(filepath.Join(c.set.DataDir, "store"))
	if err != nil {
		return err
	}
	c.store = kolostore.New(backend)
	if c.log, err = openOwnLog(c.store, c.id); err != nil {
		return err
	}
	if c.token, err = capability(c.set, c.id, c.log.ID()); err != nil {
		return err
	}
	c.peers = map[string]*koloswarm.Peer{}
	c.replicas = map[string]*kolostore.Replica{}
	c.pinned = map[string]uint64{}
	c.locked = map[string]int{}
	if c.members, err = loadMembers(c.set.DataDir); err != nil {
		return err
	}
	c.srv = kolosync.NewServer(c.authorize)
	if err = c.srv.Offer(c.log); err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.push = make(chan struct{}, 1)
	c.pinDone = make(chan struct{})
	if c.sw, err = koloswarm.New(c.ctx, c.set.Listen, c.id, &koloswarm.Config{LookupInterval: c.set.LookupInterval}); err != nil {
		return err
	}
	if err = c.sw.Bootstrap(c.ctx, []netip.AddrPort{c.set.bootstrap}); err != nil {
		return err
	}
	log.Print("kolo-id ", c.id.PublicKeyString())

	c.wg.Go(c.swarmLoop)
	c.wg.Go(c.pushLoop)
	c.wg.Go(func() { // on open: the pin first, then the peers
		defer close(c.pinDone)
		if err := c.pullFromPin(c.ctx); err != nil {
			log.Print("pin pull: ", err)
		}
	})
	c.pushSoon()
	c.wg.Go(func() { // join the conversation's topic
		err := c.sw.Join(c.ctx, c.topic(), &koloswarm.JoinOptions{Announce: true, Lookup: true})
		if err != nil && c.ctx.Err() == nil {
			log.Print("join: ", err)
		}
	})
	return nil
}

// ServiceShutdown stops the chat and closes the store. Wails calls it when the app quits.
func (c *ChatService) ServiceShutdown() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return c.shutdown()
}

func (c *ChatService) shutdown() error {
	if c.cancel != nil {
		c.cancel()
	}
	var errs []error
	if c.sw != nil {
		errs = append(errs, c.sw.Close())
	}
	c.wg.Wait()
	if c.store != nil {
		errs = append(errs, c.store.Close()) // also closes the log and the replicas
	}
	c.id, c.store, c.sw, c.cancel = nil, nil, nil, nil
	return errors.Join(errs...)
}

// topic is where the conversation's peers find each other: a hash of the conversation key, so only those who
// know the key look at it.
func (c *ChatService) topic() koloswarm.Topic {
	return sha256.Sum256(append([]byte("kolochat/topic/v1"), c.set.conversationKey[:]...))
}

// swarmLoop connects to every peer the swarm finds.
func (c *ChatService) swarmLoop() {
	for {
		ev, err := c.sw.Next(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil {
				log.Print("swarm: ", err)
			}
			return
		}
		if ev.Kind == koloswarm.Connected && ev.Peer != nil {
			c.addPeer(ev.Peer)
		}
	}
}

func (c *ChatService) addPeer(p *koloswarm.Peer) {
	key := hex.EncodeToString(p.PublicKey())
	c.mu.Lock()
	c.peers[key] = p
	c.mu.Unlock()
	log.Print("peer connected: ", key[:8])
	c.emitPeers()

	if conn, ok := p.Conn().(*kolosecure.Conn); ok { // not for a relayed connection
		c.wg.Go(func() { c.srv.Serve(c.ctx, conn) })
	}
	c.wg.Go(func() {
		defer func() {
			c.mu.Lock()
			if c.peers[key] == p {
				delete(c.peers, key)
			}
			c.mu.Unlock()
			log.Print("peer gone: ", key[:8])
			c.emitPeers()
		}()
		if err := c.sendTo(p, c.hello(p, false)); err != nil {
			log.Print("hello: ", err)
		}
		c.receive(p)
	})
}

// receive handles what one peer sends, one stream at a time.
func (c *ChatService) receive(p *koloswarm.Peer) {
	for {
		st, err := p.Conn().AcceptUniStream(c.ctx)
		if err != nil {
			return
		}
		st.SetReadDeadline(time.Now().Add(sendTimeout))
		data, err := io.ReadAll(io.LimitReader(st, maxWire+1))
		if err != nil || len(data) > maxWire {
			st.CancelRead(codeTooLong)
			continue
		}
		var m wireMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Kind {
		case "hello":
			c.onHello(p, m)
		case "msg":
			c.onMessage(p, m)
		}
	}
}

func (c *ChatService) sendTo(p *koloswarm.Peer, m wireMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, sendTimeout)
	defer cancel()
	st, err := p.Conn().OpenUniStream(ctx)
	if err != nil {
		return err
	}
	st.SetWriteDeadline(time.Now().Add(sendTimeout))
	if _, err := st.Write(data); err != nil {
		st.CancelWrite(codeTooLong)
		return err
	}
	return st.Close()
}

// Send seals the text with the conversation key and appends it to our log. Then it hands it to every peer
// that is connected, and to the pin for those who are not. A pin that fails never fails the Send.
func (c *ChatService) Send(text string) error {
	if len(text) > maxMessage {
		return errors.New("message too long")
	}
	if c.log == nil {
		return errors.New("not started")
	}
	sealed, err := c.seal(text)
	if err != nil {
		return err
	}
	index, err := c.log.Append(sealed)
	if err != nil {
		return err
	}
	c.mu.Lock()
	peers := make([]*koloswarm.Peer, 0, len(c.peers))
	for _, p := range c.peers {
		peers = append(peers, p)
	}
	c.mu.Unlock()
	for _, p := range peers {
		if err := c.sendTo(p, wireMsg{Kind: "msg", Index: index, Sealed: sealed}); err != nil {
			log.Print("direct send: ", err) // the pin and the next sync still carry it
		}
	}
	c.pushSoon()
	return nil
}

// seal and open protect the text with the conversation key (XChaCha20-Poly1305), bound to the author's key.
// This is the layer the pin and anyone with entry keys cannot remove.
func (c *ChatService) seal(text string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(text), c.pub), nil
}

func (c *ChatService) open(author ed25519.PublicKey, sealed []byte) (string, error) {
	n := c.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed text too short")
	}
	text, err := c.aead.Open(nil, sealed[:n], sealed[n:], author)
	return string(text), err
}

// hello introduces us: our signed log header, which names our key and log ID, and a proof that we know the
// conversation key, bound to both keys so it cannot be replayed to another peer.
func (c *ChatService) hello(p *koloswarm.Peer, reply bool) wireMsg {
	return wireMsg{Kind: "hello", Header: c.log.Header().Encode(), Proof: c.proof(c.pub, p.PublicKey()), Reply: reply}
}

func (c *ChatService) proof(from, to ed25519.PublicKey) []byte {
	mac := hmac.New(sha256.New, c.set.conversationKey[:])
	mac.Write([]byte("kolochat/hello/v1"))
	mac.Write(from)
	mac.Write(to)
	return mac.Sum(nil)
}

// onHello registers a peer that proves it knows the conversation key and answers with our own hello. The
// answer tells the peer that we registered it, so it may pull our log; a hello that is an answer tells us
// the same, so we may pull its log.
func (c *ChatService) onHello(p *koloswarm.Peer, m wireMsg) {
	pub := p.PublicKey()
	h, err := kololog.DecodeLogHeader(m.Header)
	if err != nil || kololog.VerifyLogHeader(pub, h) != nil || !hmac.Equal(m.Proof, c.proof(pub, c.pub)) {
		log.Print("hello refused from ", hex.EncodeToString(pub)[:8])
		return
	}
	if err := c.addMember(pub, m.Header); err != nil {
		log.Print("member: ", err)
		return
	}
	if !m.Reply {
		if err := c.sendTo(p, c.hello(p, true)); err != nil {
			log.Print("hello: ", err)
		}
		return
	}
	c.wg.Go(func() { c.syncFromPeer(p) })
}

// authorize lets the members of the conversation read our log with its entry keys, and nobody else.
func (c *ChatService) authorize(peer, owner ed25519.PublicKey, logID kololog.LogID) kolosync.Access {
	c.mu.Lock()
	_, ok := c.members[hex.EncodeToString(peer)]
	c.mu.Unlock()
	if ok && owner.Equal(c.pub) && logID == c.log.ID() {
		return kolosync.Keys
	}
	return kolosync.NoAccess
}

// onMessage shows a message that arrives directly if it is the next one of its sender, and drops it if it was
// shown already. A gap means we missed some: a sync from the sender shows them all, in order.
func (c *ChatService) onMessage(p *koloswarm.Peer, m wireMsg) {
	author := hex.EncodeToString(p.PublicKey())
	c.showMu.Lock()
	defer c.showMu.Unlock()
	next, known := c.shown(author)
	switch {
	case !known: // its hello is not here yet, and the sync that follows it brings this message too
	case m.Index > next:
		c.wg.Go(func() { c.syncFromPeer(p) })
	case m.Index == next:
		text, err := c.open(p.PublicKey(), m.Sealed)
		if err != nil {
			log.Print("direct message refused: ", err)
			return
		}
		c.setShown(author, next+1)
		c.emit("chat:msg", ChatMsg{From: author, Text: text, Via: "direct"})
	}
}

// syncFromPeer pulls the sender's log from the sender, with its entry keys, and shows what is new. It waits
// for the pull from the pin at start, so what the pin has is taken from the pin.
func (c *ChatService) syncFromPeer(p *koloswarm.Peer) {
	select {
	case <-c.pinDone:
	case <-c.ctx.Done():
		return
	}
	conn, ok := p.Conn().(*kolosecure.Conn)
	if !ok {
		return
	}
	author := hex.EncodeToString(p.PublicKey())
	rep, err := c.replica(author)
	if err != nil {
		log.Print("replica: ", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, pinTimeout)
	defer cancel()
	c.pullMu.Lock()
	err = kolosync.Pull(ctx, conn, rep)
	c.pullMu.Unlock()
	if err != nil && c.ctx.Err() == nil {
		log.Print("sync from peer: ", err)
		return
	}
	c.show(author)
}

// show gives the window the messages of an author that it has not seen, in the order of the author's log. It
// stops at the first entry whose key is missing: the pin sent the ciphertext, and the key comes only from the
// author, so the entry waits and nothing after it jumps ahead.
func (c *ChatService) show(author string) {
	c.showMu.Lock()
	defer c.showMu.Unlock()
	rep, err := c.replica(author)
	if err != nil {
		return
	}
	pub := rep.Header().LogKey
	for {
		index, _ := c.shown(author)
		if index >= rep.Len() {
			break
		}
		sealed, err := rep.Get(index)
		if errors.Is(err, kololog.ErrKeyUnavailable) {
			break
		}
		if err == nil {
			var text string
			if text, err = c.open(pub, sealed); err == nil {
				c.mu.Lock()
				via := "peer"
				if index < c.pinned[author] {
					via = "pin"
				}
				c.mu.Unlock()
				c.emit("chat:msg", ChatMsg{From: author, Text: text, Via: via})
			}
		}
		if err != nil && !errors.Is(err, kololog.ErrDeleted) {
			log.Printf("entry %d of %s skipped: %v", index, author[:8], err)
		}
		c.setShown(author, index+1)
	}
	locked := 0
	if next, _ := c.shown(author); rep.Len() > next {
		locked = int(rep.Len() - next)
	}
	c.setPin(func(*pinState) { c.locked[author] = locked })
}

// pushLoop pushes to the pin whenever one is wanted; wishes that come while it works are merged into one.
func (c *ChatService) pushLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.push:
		}
		if err := c.pushToPin(c.ctx); err != nil && c.ctx.Err() == nil {
			log.Print("pin push: ", err)
		}
	}
}

func (c *ChatService) pushSoon() {
	select {
	case c.push <- struct{}{}:
	default:
	}
}

func (c *ChatService) emitPeers() {
	c.mu.Lock()
	n := len(c.peers)
	c.mu.Unlock()
	c.emit("chat:peers", n)
}

// setPin changes the pin's state under the lock and tells the window.
func (c *ChatService) setPin(change func(*pinState)) {
	c.mu.Lock()
	change(&c.pin)
	st := PinStatus{Error: c.pin.Push}
	if st.Error == "" {
		st.Error = c.pin.Pull
	}
	for _, n := range c.locked {
		st.Locked += n
	}
	c.mu.Unlock()
	c.emit("chat:pin", st)
}

func loadIdentity(dir string) (*koloid.Identity, error) {
	path := filepath.Join(dir, "identity.key")
	id, err := koloid.Load(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	if id, err = koloid.Generate(); err != nil {
		return nil, err
	}
	if err = id.SaveNew(path); err != nil && !errors.Is(err, koloid.ErrSavedNotSynced) {
		return nil, err
	}
	return id, nil
}

// openOwnLog opens our log in the store, or creates it the first time.
func openOwnLog(st *kolostore.Store, id *koloid.Identity) (*kolostore.Log, error) {
	ids, err := st.List()
	if err != nil {
		return nil, err
	}
	for _, logID := range ids {
		pub, replica, err := st.Owner(logID)
		if err != nil {
			return nil, err
		}
		if !replica && pub.Equal(id.PublicKey()) {
			return st.Open(id, logID)
		}
	}
	return st.Create(id)
}

// replica opens, once, the read-only copy of a member's log that we keep in our store.
func (c *ChatService) replica(author string) (*kolostore.Replica, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rep, ok := c.replicas[author]; ok {
		return rep, nil
	}
	m, ok := c.members[author]
	if !ok {
		return nil, fmt.Errorf("unknown member %s", author[:8])
	}
	h, err := kololog.DecodeLogHeader(m.Header)
	if err != nil {
		return nil, err
	}
	rep, err := c.store.OpenReplica(h.LogKey, h.LogID, m.Header)
	if err != nil {
		return nil, err
	}
	c.replicas[author] = rep
	return rep, nil
}

func (c *ChatService) markPinned(author string, n uint64) {
	c.mu.Lock()
	c.pinned[author] = n
	c.mu.Unlock()
}

// authors lists the members, as hex public keys.
func (c *ChatService) authors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.members))
	for k := range c.members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (c *ChatService) shown(author string) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.members[author]
	if !ok {
		return 0, false
	}
	return m.Shown, true
}

func (c *ChatService) setShown(author string, n uint64) {
	c.mu.Lock()
	c.members[author].Shown = n
	err := saveMembers(c.set.DataDir, c.members)
	c.mu.Unlock()
	if err != nil {
		log.Print("members: ", err)
	}
}

func (c *ChatService) addMember(pub ed25519.PublicKey, header []byte) error {
	author := hex.EncodeToString(pub)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.members[author]; ok {
		return nil
	}
	c.members[author] = &member{Header: header}
	return saveMembers(c.set.DataDir, c.members)
}

func loadMembers(dir string) (map[string]*member, error) {
	members := map[string]*member{}
	data, err := os.ReadFile(filepath.Join(dir, "members.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return members, nil
	}
	if err != nil {
		return nil, err
	}
	return members, json.Unmarshal(data, &members)
}

// saveMembers replaces members.json in one step, so a crash leaves the old file or the new one.
func saveMembers(dir string, members map[string]*member) error {
	data, err := json.Marshal(members)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "members.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "members.json"))
}
