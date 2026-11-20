package main

import (
	"bytes"
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

	kolobraid "github.com/ciranda-dev/kolo-braid"
	koloid "github.com/ciranda-dev/kolo-id"
	invite "github.com/ciranda-dev/kolo-invite"
	kolokv "github.com/ciranda-dev/kolo-kv"
	kololog "github.com/ciranda-dev/kolo-log"
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

// ChatMsg is the payload of the "chat:msg" event. From is the sender's kolo-id (hex public key), Room is the room it was sent to and Via says how
// the message arrived: "direct" from the sender, "pin" when its ciphertext came from the pin, "peer" when it was
// pulled from the sender after the direct delivery was missed.
type ChatMsg struct {
	From string `json:"from"`
	Room string `json:"room"`
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
	Kind      string `json:"kind"`                // "hello", "msg" or "dir"
	Header    []byte `json:"header,omitempty"`    // hello: the sender's signed log header
	DirHeader []byte `json:"dirHeader,omitempty"` // hello: the signed header of the sender's directory log
	Proof     []byte `json:"proof,omitempty"`     // hello: proves the sender knows the conversation key
	Reply     bool   `json:"reply,omitempty"`     // hello: answers a hello, so the sender has registered us
	Group     []byte `json:"group,omitempty"`     // hello: the ID of the sender's braid, which must be ours
	Index     uint64 `json:"index,omitempty"`     // msg: the entry's index in the sender's log
	Sealed    []byte `json:"sealed,omitempty"`    // msg: the entry's value, the text sealed with the conversation key
	// "dir" has no fields: the sender's directory changed, pull it.
}

// member is a peer of the conversation: its signed log header, which names its public key and log ID, the signed
// header of its directory log (see profile.go), and how many of its messages the window has shown. All are kept
// in the data directory.
type member struct {
	Header    []byte `json:"header"`
	DirHeader []byte `json:"dirHeader,omitempty"`
	Shown     uint64 `json:"shown"`
}

type pinState struct {
	Push, Pull           string // what to tell the user, "" when it works
	Entries, Used, Quota uint64
}

// ChatService is the engine of one conversation: its own kolo-id, swarm, logs, pin and invites in its own folder.
// Conversations runs one per conversation and routes the window's calls to it.
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
	log     *kolostore.Log // our own log of messages, one of the two we write
	dirLog  *kolostore.Log // our directory log, the other one (see profile.go)
	dir     *kolokv.DB     // the key-value store on dirLog: the only writer of it
	srv     *kolosync.Server
	sw      *koloswarm.Swarm
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	push    chan struct{}     // a push to the pin is wanted
	pinDone chan struct{}     // closed when the pull from the pin at start is over
	invDB   kolostore.Backend // the invites' own store, used for nothing else
	inv     *invite.Inviter
	dirMu   sync.Mutex // one write of our directory at a time
	pubMu   sync.Mutex // one publication of the directory to the window at a time

	mu          sync.Mutex       // guards the fields below
	links       map[string]*link // the connections of the conversation's topic, by peer key
	members     map[string]*member
	replicas    map[string]*kolostore.Replica
	dirs        map[string]*directory // per member: the read-only copy of its directory
	lastView    directoryView         // what the window was told last
	pinned      map[string]uint64     // per author: entries below this index came from the pin
	locked      map[string]int        // per author: entries that wait for a key
	pin         pinState
	active      bool                                // in a conversation: it has its key and its pin
	joining     *joinAttempt                        // the Join that waits for its inviter
	cancelJoin  context.CancelFunc                  // stops that Join
	requests    map[invite.RequestID]pendingRequest // the requests that wait for Approve or Deny
	accepted    map[string]kolobraid.Member         // the roster of the braid's view, by hex key: who the chat talks to
	group       kolobraid.GroupID
	pauses      map[string]*pause // members whose braid is paused after a fault
	unconfirmed bool              // a braid write ended before our strand was confirmed
	lastState   []byte            // the last "chat:state", as JSON

	showMu sync.Mutex // one message is shown at a time, so they keep their order
	pullMu sync.Mutex // one pull at a time, since a replica takes one
	pushMu sync.Mutex // one push at a time

	braidDB kolostore.Backend // the braid's own store, so a join attempt never shares one
	braid   *kolobraid.Braid
}

// link is one connection to a peer of the conversation, split into parts by its mux. Its hello is proven once it
// proves the conversation key and names our braid; the peer is registered once it is also on the roster.
type link struct {
	p      *koloswarm.Peer
	mx     *mux
	key    string
	once   sync.Once
	proven chan struct{}

	// guarded by ChatService.mu
	hello      *wireMsg  // its proven hello, kept to register it once it is accepted
	since      time.Time // when its hello was proven
	answered   bool      // it sent a reply hello: it accepted us
	otherGroup bool      // its hello names another braid
	registered bool
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
	backend, err := kolostore.OpenPebble(filepath.Join(c.set.DataDir, "store"))
	if err != nil {
		return err
	}
	c.store = kolostore.New(backend)
	if err = c.openOwnLogs(); err != nil {
		return err
	}
	if c.braidDB, err = openBraidStore(c.set.DataDir); err != nil {
		return err
	}
	c.links = map[string]*link{}
	c.replicas = map[string]*kolostore.Replica{}
	c.dirs = map[string]*directory{}
	c.pinned = map[string]uint64{}
	c.locked = map[string]int{}
	if c.members, err = loadMembers(c.set.DataDir); err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.push = make(chan struct{}, 1)
	swarmCfg := &koloswarm.Config{LookupInterval: c.set.LookupInterval, MaxTopics: swarmTopics, DHT: dhtConfig()}
	if c.sw, err = koloswarm.New(c.ctx, c.set.Listen, c.id, swarmCfg); err != nil {
		return err
	}
	if err = c.sw.Bootstrap(c.ctx, []netip.AddrPort{c.set.bootstrap}); err != nil {
		return err
	}
	log.Print("kolo-id ", c.id.PublicKeyString())

	c.wg.Go(c.swarmLoop)
	if err = c.openInvites(); err != nil {
		return err
	}
	_, err = os.Lstat(filepath.Join(c.set.DataDir, "conversation.json"))
	founder := errors.Is(err, fs.ErrNotExist) // a key from the configuration and no conversation yet: we start it
	conv, found, err := c.savedConversation() // from the configuration, or from the invite that made us a member
	if err != nil {
		return err
	}
	if !found { // a joiner waits for Join
		return c.noBraidYet()
	}
	return c.enter(conv, founder)
}

// enter makes this chat a member of the conversation: it has the key and the pin, offers its logs and the braid,
// pulls from the pin first and then from the peers, and joins the conversation's topic. Start calls it for the
// founder or a member that joined earlier, and Join for a new member. The conversation is saved once the braid
// opens: the founder creates it.
func (c *ChatService) enter(conv conversation, founder bool) (err error) {
	c.set.useConversation(conv)
	if c.aead, err = chacha20poly1305.NewX(c.set.conversationKey[:]); err != nil {
		return err
	}
	if err = c.openBraid(founder); err != nil {
		return err
	}
	if err = saveConversation(c.set.DataDir, conv); err != nil {
		return err
	}
	if c.token, err = capability(c.set, c.id, c.log.ID(), c.dirLog.ID()); err != nil {
		return err
	}
	c.srv = kolosync.NewServer(c.authorize)
	for _, l := range []*kolostore.Log{c.log, c.dirLog} {
		if err = c.srv.Offer(l); err != nil {
			return err
		}
	}
	c.pinDone = make(chan struct{})
	c.mu.Lock()
	c.active = true // what the goroutines below read was written before this
	c.mu.Unlock()
	c.startBraid() // the accepted set, before anything is pulled
	c.openDirectories()

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

// ServiceShutdown stops the chat and closes the store. Conversations calls it when the app quits.
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
	if c.inv != nil {
		errs = append(errs, c.inv.Close())
	}
	c.wg.Wait()
	if c.braid != nil {
		errs = append(errs, c.braid.Close())
	}
	if c.braidDB != nil {
		errs = append(errs, c.braidDB.Close())
	}
	c.closeDirectories()
	if c.store != nil {
		errs = append(errs, c.store.Close()) // also closes the log and the replicas
	}
	if c.invDB != nil {
		errs = append(errs, c.invDB.Close())
	}
	c.mu.Lock()
	c.active, c.joining, c.cancelJoin = false, nil, nil
	c.mu.Unlock()
	c.id, c.store, c.sw, c.cancel, c.inv, c.invDB, c.braid, c.braidDB = nil, nil, nil, nil, nil, nil, nil, nil
	c.log, c.dirLog, c.dir = nil, nil, nil
	return errors.Join(errs...)
}

// topic is where the conversation's peers find each other: a hash of the conversation key, so only those who
// know the key look at it.
func (c *ChatService) topic() koloswarm.Topic {
	return sha256.Sum256(append([]byte("kolochat/topic/v1"), c.set.conversationKey[:]...))
}

// swarmLoop handles every peer the swarm connects.
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
			c.route(ev.Peer)
		}
	}
}

// route decides what a connection is for, since one protocol serves it: the inviter that a Join waits for, a
// peer of the conversation (chat and sync), or a person that has only an invite's topic with us, whose
// requests the inviter answers.
func (c *ChatService) route(p *koloswarm.Peer) {
	switch {
	case c.offerToJoin(p):
	case !c.isActive():
	case slices.Contains(p.Topics(), c.topic()):
		c.addPeer(p)
	default:
		c.wg.Go(func() { c.inv.ServePeer(c.ctx, p) })
	}
}

// addPeer starts the chat with a peer of the conversation's topic: our hello first, then what it sends.
func (c *ChatService) addPeer(p *koloswarm.Peer) {
	l := &link{p: p, mx: newMux(p.Conn()), key: hex.EncodeToString(p.PublicKey()), proven: make(chan struct{})}
	c.mu.Lock()
	c.links[l.key] = l
	c.mu.Unlock()
	log.Print("peer connected: ", l.key[:8])

	c.wg.Go(func() { c.serveAfterHello(l) })
	c.wg.Go(l.mx.acceptUni)
	c.wg.Go(func() {
		defer func() {
			c.mu.Lock()
			if c.links[l.key] == l {
				delete(c.links, l.key)
			}
			c.mu.Unlock()
			log.Print("peer gone: ", l.key[:8])
			c.emitPeers()
		}()
		if err := c.sendTo(l, c.hello(l, false)); err != nil {
			log.Print("hello: ", err)
		}
		c.receive(l)
	})
}

// receive handles what one peer sends on the connection's 'm' part, one stream at a time.
func (c *ChatService) receive(l *link) {
	part := l.mx.part(tagMsg)
	for {
		st, err := part.AcceptUniStream(c.ctx)
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
			c.onHello(l, m)
		case "msg":
			c.onMessage(l, m)
		case "dir":
			c.onDirectory(l)
		}
	}
}

func (c *ChatService) sendTo(l *link, m wireMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, sendTimeout)
	defer cancel()
	st, err := l.mx.part(tagMsg).OpenUniStream(ctx)
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

// registered lists the peers the chat talks to: proven, and on the roster.
func (c *ChatService) registered() []*link {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*link
	for _, l := range c.links {
		if l.registered {
			out = append(out, l)
		}
	}
	return out
}

// Send seals the text, with the room it is for, with the conversation key and appends it to our log. Then it
// hands it to every peer that is connected, and to the pin for those who are not. A pin that fails never fails
// the Send. The room must be one of Rooms, or Send returns ErrUnknownRoom.
func (c *ChatService) Send(room, text string) error {
	if len(text) > maxMessage {
		return errors.New("message too long")
	}
	if c.log == nil {
		return errors.New("not started")
	}
	if !c.isActive() {
		return ErrNotInConversation
	}
	if !slices.Contains(c.Rooms(), room) {
		return ErrUnknownRoom
	}
	sealed, err := c.seal(room, text)
	if err != nil {
		return err
	}
	index, err := c.log.Append(sealed)
	if err != nil {
		return err
	}
	for _, l := range c.registered() {
		if err := c.sendTo(l, wireMsg{Kind: "msg", Index: index, Sealed: sealed}); err != nil {
			log.Print("direct send: ", err) // the pin and the next sync still carry it
		}
	}
	c.pushSoon()
	return nil
}

// body is what a message holds before it is sealed: the room and the text. The room is inside the sealed bytes,
// so the pin and anyone with entry keys but not the conversation key cannot tell which room a message is for.
type body struct {
	Version int    `json:"v"`
	Room    string `json:"room"`
	Text    string `json:"text"`
}

// seal and open protect the room and the text with the conversation key (XChaCha20-Poly1305), bound to the
// author's key. This is the layer the pin and anyone with entry keys cannot remove. open takes an entry that
// is not a body, such as one written by step 4, for text in the default room.
func (c *ChatService) seal(room, text string) ([]byte, error) {
	plain, err := json.Marshal(body{Version: 1, Room: room, Text: text})
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, c.pub), nil
}

func (c *ChatService) open(author ed25519.PublicKey, sealed []byte) (room, text string, err error) {
	n := c.aead.NonceSize()
	if len(sealed) < n {
		return "", "", errors.New("sealed text too short")
	}
	plain, err := c.aead.Open(nil, sealed[:n], sealed[n:], author)
	if err != nil {
		return "", "", err
	}
	var b body
	if json.Unmarshal(plain, &b) != nil || b.Version != 1 {
		return defaultRoom, string(plain), nil
	}
	if validRoomName(b.Room) != nil {
		return "", "", errors.New("message for a room with a bad name")
	}
	return b.Room, b.Text, nil
}

// hello introduces us: our signed log header, which names our key and log ID, a proof that we know the conversation
// key, bound to both keys so it cannot be replayed to another peer, and our braid's ID. The directory header is how a
// peer learns which log holds our profile.
func (c *ChatService) hello(l *link, reply bool) wireMsg {
	return wireMsg{Kind: "hello", Header: c.log.Header().Encode(), DirHeader: c.dirLog.Header().Encode(),
		Proof: c.proof(c.pub, l.p.PublicKey()), Reply: reply, Group: c.group[:]}
}

func (c *ChatService) proof(from, to ed25519.PublicKey) []byte {
	mac := hmac.New(sha256.New, c.set.conversationKey[:])
	mac.Write([]byte("kolochat/hello/v1"))
	mac.Write(from)
	mac.Write(to)
	return mac.Sum(nil)
}

// onHello proves a peer's hello: it knows the conversation key and names our braid. A proven peer that is on the
// roster is registered and answered with our own hello; one that is not yet is kept, and accepted once the roster
// has it. The answer tells the peer that we registered it, so it may pull our log; a hello that is an answer tells
// us the same, so we may pull its log.
func (c *ChatService) onHello(l *link, m wireMsg) {
	pub := l.p.PublicKey()
	h, err := kololog.DecodeLogHeader(m.Header)
	if err == nil {
		err = kololog.VerifyLogHeader(pub, h)
	}
	if err == nil && !hmac.Equal(m.Proof, c.proof(pub, c.pub)) {
		err = errors.New("it does not prove the conversation key")
	}
	if err == nil {
		err = checkDirHeader(pub, m.DirHeader, h)
	}
	if err != nil {
		log.Print("hello refused from ", l.key[:8], ": ", err)
		return
	}
	c.mu.Lock()
	if !bytes.Equal(m.Group, c.group[:]) {
		first := !l.otherGroup
		l.otherGroup = true
		c.mu.Unlock()
		if first {
			log.Print("hello from ", l.key[:8], " names another member list")
		}
		return
	}
	l.hello = &m
	if l.since.IsZero() {
		l.since = time.Now()
	}
	l.answered = l.answered || m.Reply
	_, ok := c.accepted[l.key]
	c.mu.Unlock()
	l.once.Do(func() { close(l.proven) })
	if ok {
		c.accept(l, m, !m.Reply, m.Reply)
	}
}

// accept registers a proven peer that is on the roster. answer sends it a reply hello, and sync pulls from it.
func (c *ChatService) accept(l *link, m wireMsg, answer, sync bool) {
	if err := c.addMember(l.p.PublicKey(), m.Header, m.DirHeader); err != nil {
		log.Print("member: ", err)
		return
	}
	c.mu.Lock()
	if c.links[l.key] != l {
		c.mu.Unlock()
		return
	}
	was := l.registered
	l.registered = true
	c.mu.Unlock()
	if !was {
		c.emitPeers()
	}
	if answer {
		if err := c.sendTo(l, c.hello(l, true)); err != nil {
			log.Print("hello: ", err)
		}
	}
	if sync {
		c.wg.Go(func() { c.syncFromPeer(l) })
	}
}

// authorize lets the members on the roster read our two logs with their entry keys, and nobody else.
func (c *ChatService) authorize(peer, owner ed25519.PublicKey, logID kololog.LogID) kolosync.Access {
	if c.isAccepted(hex.EncodeToString(peer)) && owner.Equal(c.pub) && (logID == c.log.ID() || logID == c.dirLog.ID()) {
		return kolosync.Keys
	}
	return kolosync.NoAccess
}

// onMessage shows a message that arrives directly if it is the next one of its sender, and drops it if it was
// shown already. Only a peer whose hello was proven and who is on the roster is heard. A gap means we missed some:
// a sync from the sender shows them all, in order.
func (c *ChatService) onMessage(l *link, m wireMsg) {
	c.mu.Lock()
	_, ok := c.accepted[l.key]
	ok = ok && l.hello != nil
	c.mu.Unlock()
	if !ok {
		return
	}
	author := l.key
	c.showMu.Lock()
	defer c.showMu.Unlock()
	next, known := c.shown(author)
	switch {
	case !known: // its hello is not here yet, and the sync that follows it brings this message too
	case m.Index > next:
		c.wg.Go(func() { c.syncFromPeer(l) })
	case m.Index == next:
		room, text, err := c.open(l.p.PublicKey(), m.Sealed)
		if err != nil {
			log.Print("direct message refused: ", err)
			return
		}
		c.setShown(author, next+1)
		c.emit("chat:msg", ChatMsg{From: author, Room: room, Text: text, Via: "direct"})
	}
}

// syncFromPeer pulls the sender's log and its directory from the sender, with their entry keys, and shows what
// is new. It waits for the pull from the pin at start, so what the pin has is taken from the pin. A member who is
// not on the roster is not pulled.
func (c *ChatService) syncFromPeer(l *link) {
	conn, ok := c.afterPin(l)
	author := l.key
	if !ok || !c.isAccepted(author) {
		return
	}
	rep, err := c.replica(author)
	if err != nil {
		log.Print("replica: ", err)
	} else {
		ctx, cancel := context.WithTimeout(c.ctx, pinTimeout)
		c.pullMu.Lock()
		err = kolosync.Pull(ctx, conn, rep)
		c.pullMu.Unlock()
		cancel()
		if err != nil && c.ctx.Err() == nil {
			log.Print("sync from peer: ", err)
		} else {
			c.show(author)
		}
	}
	c.syncDirectory(conn, author)
}

// afterPin waits for the pull from the pin at start, and returns the 's' part of the peer's connection.
func (c *ChatService) afterPin(l *link) (kolosync.Conn, bool) {
	select {
	case <-c.pinDone:
	case <-c.ctx.Done():
		return nil, false
	}
	return l.mx.part(tagSync), true
}

// show gives the window the messages of an author that it has not seen, in the order of the author's log. It
// stops at the first entry whose key is missing: the pin sent the ciphertext, and the key comes only from the
// author, so the entry waits and nothing after it jumps ahead. One not on the roster shows nothing more.
func (c *ChatService) show(author string) {
	if !c.isAccepted(author) {
		return
	}
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
			var room, text string
			if room, text, err = c.open(pub, sealed); err == nil {
				c.mu.Lock()
				via := "peer"
				if index < c.pinned[author] {
					via = "pin"
				}
				c.mu.Unlock()
				c.emit("chat:msg", ChatMsg{From: author, Room: room, Text: text, Via: via})
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
	c.emit("chat:peers", len(c.registered()))
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

// addMember registers a member from its signed headers. A member we know gets its directory header when it
// first sends one.
func (c *ChatService) addMember(pub ed25519.PublicKey, header, dirHeader []byte) error {
	author := hex.EncodeToString(pub)
	c.mu.Lock()
	m, known := c.members[author]
	switch {
	case !known:
		c.members[author] = &member{Header: header, DirHeader: dirHeader}
	case len(m.DirHeader) == 0 && len(dirHeader) > 0:
		m.DirHeader = dirHeader
	default:
		c.mu.Unlock()
		return nil
	}
	err := saveMembers(c.set.DataDir, c.members)
	c.mu.Unlock()
	return err
}

func loadMembers(dir string) (map[string]*member, error) {
	members := map[string]*member{}
	data, err := readFile(filepath.Join(dir, "members.json"), maxStateFile)
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
