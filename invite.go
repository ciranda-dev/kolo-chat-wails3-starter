package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	kolobraid "github.com/ciranda-dev/kolo-braid"
	invite "github.com/ciranda-dev/kolo-invite"
	kololog "github.com/ciranda-dev/kolo-log"
	kolostore "github.com/ciranda-dev/kolo-store"
	koloswarm "github.com/ciranda-dev/kolo-swarm"
)

// Bounds of an invite, and how long Join waits.
const (
	grantVersion      = 2 // 2 carries the braid's welcome
	maxRequest        = 4096
	maxJoinData       = 6400 // a request and a log header in base64, with the JSON around them
	defaultInviteTTL  = 24 * time.Hour
	maxInviteTTL      = 30 * 24 * time.Hour
	maxInviteUses     = 100
	maxActiveInvites  = 16 // every active invite takes a topic of the swarm, see openInvites
	swarmTopics       = 32 // kolo-swarm's largest MaxTopics: the conversation's topic and the invites'
	joinFindTimeout   = 60 * time.Second
	joinAnswerTimeout = 10 * time.Minute
)

// InviteOptions is what an admin chooses for a new invite. A zero TTLSeconds is one day, a zero Uses is one. Every
// joiner waits for an admin's approval.
type InviteOptions struct {
	TTLSeconds int64 `json:"ttlSeconds"`
	Uses       int   `json:"uses"`
}

// InviteInfo describes an invite for the window. Code is the invite's secret: only the answer of CreateInvite
// has it, since kolo-invite does not give a code back later.
type InviteInfo struct {
	ID      string `json:"id"`
	Code    string `json:"code,omitempty"`
	Expires string `json:"expires"` // RFC 3339
	Uses    int    `json:"uses"`
	Used    int    `json:"used"`
	State   string `json:"state"` // "active", "expired", "used up" or "revoked"
}

// InviteRequest is the payload of the "chat:invite-request" event: Joiner (a kolo-id) asks to join through an
// invite. Withdrawn says the request is gone, because the joiner left or the invite ended.
type InviteRequest struct {
	Request   uint64 `json:"request"`
	Invite    string `json:"invite"`
	Joiner    string `json:"joiner"`
	Withdrawn bool   `json:"withdrawn"`
}

// JoinStatus is the payload of the "chat:join" event. Status is "joining", "waiting" (for an admin's answer),
// "joined" or "failed", and Message is the sentence to show.
type JoinStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// InviteError is an error of the invite flow. Message is the sentence the window shows, and Err the cause, one
// of the errors of kolo-invite.
type InviteError struct {
	Message string
	Err     error
}

func (e *InviteError) Error() string { return e.Message }
func (e *InviteError) Unwrap() error { return e.Err }

// ErrNotInConversation is returned by what needs a conversation before the user has joined one.
var ErrNotInConversation = errors.New("Join a conversation first: paste an invite code.")

// conversation is what makes a member: the key and where the pin is. It is kept in conversation.json. Who the
// members and the admins are is in the braid.
type conversation struct {
	Key     []byte `json:"key"`
	PinAddr string `json:"pinAddr"`
	PinKey  []byte `json:"pinKey"`
}

// grant is the payload of an accept. The invite encrypts it for the joiner alone (HPKE), and nothing else
// carries the conversation key. Header is the inviter's signed log header, with its key and log ID: what the
// joiner needs to meet the inviter and read its log from the pin. Welcome is the braid's answer to the join
// request, which makes the joiner's braid. There is no pin token in it: a capability is signed by the owner of a
// log and covers that log only, so the joiner issues its own (see capability).
type grant struct {
	Version int `json:"v"`
	conversation
	Header  []byte `json:"header"`
	Welcome []byte `json:"welcome"`
}

// joinData is what a joiner sends with its request: its signed log header and the braid's join request.
type joinData struct {
	Header  []byte `json:"header"`
	Request []byte `json:"request"`
}

// pendingRequest is a request that waits for an admin, with what the joiner sent.
type pendingRequest struct {
	ev   invite.Event
	data joinData
}

// check says whether the conversation is complete: a key, and a pin that can be dialed.
func (v conversation) check() error {
	if len(v.Key) != 32 || len(v.PinKey) != ed25519.PublicKeySize {
		return errors.New("wrong key size")
	}
	_, err := netip.ParseAddrPort(v.PinAddr)
	return err
}

// openInvites opens the inviter, which keeps its invites in a store of its own, and runs what it needs next to
// the swarm: Announce, which joins the topic of every live invite, and the loop of its events. The swarm has
// room for the conversation's topic and for maxActiveInvites invites.
func (c *ChatService) openInvites() (err error) {
	if c.invDB, err = kolostore.OpenPebble(filepath.Join(c.set.DataDir, "invites")); err != nil {
		return err
	}
	if c.inv, err = invite.Open(c.invDB, c.id, &invite.Options{MaxData: maxJoinData}); err != nil {
		return err
	}
	c.requests = map[invite.RequestID]pendingRequest{}
	c.wg.Go(func() {
		if err := c.inv.Announce(c.ctx, c.sw); err != nil && c.ctx.Err() == nil && !errors.Is(err, invite.ErrClosed) {
			log.Print("invite announce: ", err)
		}
	})
	c.wg.Go(c.inviteLoop)
	return nil
}

// CreateInvite makes an invite for the conversation and returns it with its code, which the admin gives to the
// person to invite. Every request through it waits for an admin's Approve: each joiner needs a welcome of its own.
func (c *ChatService) CreateInvite(opts InviteOptions) (InviteInfo, error) {
	if c.inv == nil {
		return InviteInfo{}, errors.New("not started")
	}
	if !c.isActive() {
		return InviteInfo{}, ErrNotInConversation
	}
	if !c.IsAdmin() {
		return InviteInfo{}, ErrNotAdmin
	}
	create := invite.CreateOptions{TTL: defaultInviteTTL, MaxUses: max(opts.Uses, 1), Manual: true}
	if opts.TTLSeconds != 0 {
		create.TTL = time.Duration(opts.TTLSeconds) * time.Second
	}
	if create.TTL < time.Second || create.TTL > maxInviteTTL || create.MaxUses > maxInviteUses {
		return InviteInfo{}, fmt.Errorf("An invite lasts from 1 second to %d days and takes 1 to %d uses.", maxInviteTTL/(24*time.Hour), maxInviteUses)
	}
	infos, err := c.inv.List()
	if err != nil {
		return InviteInfo{}, err
	}
	if n := countActive(infos); n >= maxActiveInvites {
		return InviteInfo{}, fmt.Errorf("You have %d active invites. Revoke one, or wait for one to expire.", n)
	}
	code, info, err := c.inv.Create(&create)
	if err != nil {
		return InviteInfo{}, err
	}
	out := inviteInfo(info)
	out.Code = code
	return out, nil
}

// Invites lists the invites, oldest first, ended ones included. It has no codes.
func (c *ChatService) Invites() ([]InviteInfo, error) {
	if c.inv == nil {
		return nil, errors.New("not started")
	}
	infos, err := c.inv.List()
	out := make([]InviteInfo, len(infos))
	for i, info := range infos {
		out[i] = inviteInfo(info)
	}
	return out, err
}

// Revoke ends an invite: a joiner that uses its code from now on is refused, and the requests that wait for an
// answer are refused too. Members that joined stay.
func (c *ChatService) Revoke(id string) error {
	if c.inv == nil {
		return errors.New("not started")
	}
	var inviteID invite.ID
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != len(inviteID) {
		return errors.New("That is not an invite ID.")
	}
	copy(inviteID[:], b)
	return ownerError(c.inv.Revoke(inviteID))
}

func (c *ChatService) Deny(request uint64) error {
	if c.inv != nil && !c.IsAdmin() {
		return ErrNotAdmin
	}
	return ownerError(c.answer(request, func(id invite.RequestID) error { return c.inv.Deny(id) }))
}

func (c *ChatService) answer(request uint64, do func(invite.RequestID) error) error {
	if c.inv == nil {
		return errors.New("not started")
	}
	id := invite.RequestID(request)
	err := do(id)
	if err == nil || errors.Is(err, invite.ErrNotFound) || errors.Is(err, invite.ErrExpired) ||
		errors.Is(err, invite.ErrUsedUp) || errors.Is(err, invite.ErrRevoked) {
		c.mu.Lock()
		delete(c.requests, id) // the request is over, however it ended
		c.mu.Unlock()
		c.emit("chat:invite-request", InviteRequest{Request: request, Withdrawn: true})
	}
	return err
}

// grantPayload is what an accept carries: the conversation, our signed log header and the braid's welcome.
func (c *ChatService) grantPayload(welcome []byte) ([]byte, error) {
	if !c.isActive() {
		return nil, ErrNotInConversation
	}
	return json.Marshal(grant{
		Version:      grantVersion,
		conversation: conversation{Key: c.set.conversationKey[:], PinAddr: c.set.PinAddr, PinKey: c.set.pinKey},
		Header:       c.log.Header().Encode(),
		Welcome:      welcome,
	})
}

// inviteLoop reports to the window what happens to the invites, and meets the people that join.
func (c *ChatService) inviteLoop() {
	for {
		ev, err := c.inv.Next(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil && !errors.Is(err, invite.ErrClosed) {
				log.Print("invite events: ", err)
			}
			return
		}
		switch ev.Kind {
		case invite.Requested:
			c.onRequested(ev)
		case invite.Withdrawn:
			c.mu.Lock()
			delete(c.requests, ev.Request)
			c.mu.Unlock()
			c.emit("chat:invite-request", InviteRequest{Request: uint64(ev.Request), Invite: ev.Invite.String(), Joiner: hex.EncodeToString(ev.Joiner), Withdrawn: true})
		case invite.Joined:
			c.onJoined(ev)
		case invite.Ended:
			c.emitInvite(ev.Invite)
		}
	}
}

// onRequested shows the window a request that needs an answer. The joiner sends its signed log header and the
// braid's join request as join data, so a request without them, or one that an admin cannot answer, is refused.
func (c *ChatService) onRequested(ev invite.Event) {
	d, err := readJoinData(ev.Joiner, ev.Data)
	if err == nil && !c.IsAdmin() {
		err = ErrNotAdmin
	}
	if err != nil {
		log.Print("invite request refused: ", err)
		if err := c.inv.Deny(ev.Request); err != nil {
			log.Print("invite deny: ", err)
		}
		return
	}
	c.mu.Lock()
	c.requests[ev.Request] = pendingRequest{ev: ev, data: d}
	c.mu.Unlock()
	c.emit("chat:invite-request", InviteRequest{Request: uint64(ev.Request), Invite: ev.Invite.String(), Joiner: hex.EncodeToString(ev.Joiner)})
}

// onJoined registers the person that joined as a member, from the log header it sent, so we may read its log and
// give it ours before its first hello. The hello registers it too, so a lost event costs nothing.
func (c *ChatService) onJoined(ev invite.Event) {
	c.mu.Lock()
	delete(c.requests, ev.Request)
	c.mu.Unlock()
	d, err := readJoinData(ev.Joiner, ev.Data)
	if err == nil {
		err = c.addMember(ev.Joiner, d.Header, nil)
	}
	if err != nil {
		log.Print("invite joiner: ", err)
	}
	log.Print("member joined: ", hex.EncodeToString(ev.Joiner)[:8])
	c.emitInvite(ev.Invite)
}

func (c *ChatService) emitInvite(id invite.ID) {
	infos, err := c.inv.List()
	if err != nil {
		return
	}
	for _, info := range infos {
		if info.ID == id {
			c.emit("chat:invite", inviteInfo(info))
		}
	}
}

// joinerHeader returns the signed log header a joiner sent as its join data, if it is the joiner's own.
func joinerHeader(joiner ed25519.PublicKey, data []byte) ([]byte, error) {
	h, err := kololog.DecodeLogHeader(data)
	if err == nil {
		err = kololog.VerifyLogHeader(joiner, h)
	}
	if err != nil {
		return nil, errors.New("the join data is not the joiner's signed log header")
	}
	return data, nil
}

// readJoinData checks what a joiner sent: its own signed log header and a braid join request of at most maxRequest.
func readJoinData(joiner ed25519.PublicKey, raw []byte) (joinData, error) {
	var d joinData
	if json.Unmarshal(raw, &d) != nil || len(d.Request) > maxRequest {
		return d, errors.New("not a join request of this version")
	}
	if _, err := joinerHeader(joiner, d.Header); err != nil {
		return d, err
	}
	if _, err := kolobraid.ParseJoinRequest(d.Request); err != nil {
		return d, errors.New("not a braid join request")
	}
	return d, nil
}

// joinAttempt is a Join that waits for the connection to the inviter.
type joinAttempt struct {
	code invite.Code
	peer chan *koloswarm.Peer
}

// offerToJoin gives the peer to the Join that waits for it, if the peer is the inviter of its code.
func (c *ChatService) offerToJoin(p *koloswarm.Peer) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.joining == nil || !c.joining.code.Matches(p.PublicKey()) {
		return false
	}
	select {
	case c.joining.peer <- p:
	default:
	}
	return true
}

// Join uses an invite code: it finds the inviter on the invite's topic, asks to join with a new strand of the braid,
// and waits for an admin's answer. With the answer it takes the conversation key, the pin and the braid's welcome
// from the payload of the accept, keeps them in the data directory, and joins the conversation. A refusal comes back as an *InviteError whose Message is the sentence to show and
// whose cause is an error of kolo-invite: ErrDenied, ErrExpired, ErrUsedUp, ErrRevoked, ErrInvalidCode,
// ErrInvalid, ErrUnreachable and the like.
func (c *ChatService) Join(text string) error {
	if c.sw == nil {
		return errors.New("not started")
	}
	err := c.join(strings.TrimSpace(text))
	if err == nil {
		c.emit("chat:join", JoinStatus{Status: "joined", Message: "You joined the conversation."})
		return nil
	}
	var ie *InviteError
	if !errors.As(err, &ie) {
		ie = &InviteError{Message: describeJoin(err), Err: err}
	}
	c.emit("chat:join", JoinStatus{Status: "failed", Message: ie.Message})
	return ie
}

func (c *ChatService) join(text string) error {
	code, err := invite.Parse(text)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	att := &joinAttempt{code: code, peer: make(chan *koloswarm.Peer, 1)}
	c.mu.Lock()
	switch {
	case c.active:
		c.mu.Unlock()
		return &InviteError{Message: "You are already in a conversation."}
	case c.joining != nil:
		c.mu.Unlock()
		return &InviteError{Message: "A join is already in progress."}
	}
	c.joining, c.cancelJoin = att, cancel
	c.mu.Unlock()
	left := false
	leave := func() {
		if !left {
			left = true
			c.sw.Leave(code.Topic())
		}
	}
	defer func() {
		leave()
		c.mu.Lock()
		c.joining, c.cancelJoin = nil, nil
		c.mu.Unlock()
	}()

	c.emit("chat:join", JoinStatus{Status: "joining", Message: "Looking for the person who invited you..."})
	if err := c.sw.Join(ctx, code.Topic(), &koloswarm.JoinOptions{Lookup: true}); err != nil {
		return err
	}
	var peer *koloswarm.Peer
	select {
	case peer = <-att.peer:
	case <-time.After(joinFindTimeout):
		return fmt.Errorf("%w: not found in %s", invite.ErrUnreachable, joinFindTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}

	j, err := kolobraid.NewJoin(ctx, c.id, braidOptions(c.braidDB))
	if err != nil {
		return err
	}
	request, err := j.Request(nil)
	if err != nil {
		return err
	}
	data, err := json.Marshal(joinData{Header: c.log.Header().Encode(), Request: request})
	if err != nil {
		return err
	}
	c.emit("chat:join", JoinStatus{Status: "waiting", Message: "Waiting for an admin to approve your request..."})
	actx, acancel := context.WithTimeout(ctx, joinAnswerTimeout)
	defer acancel()
	payload, err := invite.JoinConn(actx, peer.Conn(), c.id, code, &invite.JoinOptions{Data: data})
	if err != nil {
		return err
	}
	g, err := readGrant(payload, peer.PublicKey())
	if err != nil {
		return err
	}
	leave() // before the conversation's topic: the invite-only connection closes, and the group gets its own
	b, err := j.Complete(ctx, g.Welcome)
	if err != nil {
		return fmt.Errorf("%w: the welcome: %w", invite.ErrProtocol, err)
	}
	return c.becomeMember(g, peer.PublicKey(), b)
}

// CancelJoin stops a Join that waits, for the inviter or for its answer.
func (c *ChatService) CancelJoin() {
	c.mu.Lock()
	cancel := c.cancelJoin
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Joined says whether this chat is in a conversation: it started with a conversation key, or joined with an invite.
func (c *ChatService) Joined() bool { return c.isActive() }

func (c *ChatService) isActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// readGrant checks the payload of an accept, which only the inviter could have made: a conversation that is
// complete, the inviter's signed log header and a welcome.
func readGrant(payload []byte, owner ed25519.PublicKey) (g grant, err error) {
	malformed := func(why string) error {
		return fmt.Errorf("%w: the accept payload is %s", invite.ErrProtocol, why)
	}
	if json.Unmarshal(payload, &g) != nil || g.Version != grantVersion {
		return g, malformed("not one this version understands")
	}
	if g.conversation.check() != nil {
		return g, malformed("incomplete")
	}
	if len(g.Welcome) == 0 {
		return g, malformed("missing the welcome")
	}
	if _, err := joinerHeader(owner, g.Header); err != nil {
		return g, malformed("missing the inviter's log header")
	}
	return g, nil
}

// becomeMember keeps the conversation the invite gave and enters it with the braid Complete made. It is saved once
// the braid holds the welcome, so a restart finds both and goes on.
func (c *ChatService) becomeMember(g grant, inviter ed25519.PublicKey, b *kolobraid.Braid) error {
	c.braid = b
	if err := saveConversation(c.set.DataDir, g.conversation); err != nil {
		return err
	}
	if err := c.addMember(inviter, g.Header, nil); err != nil {
		return err
	}
	return c.enter(g.conversation, false)
}

// savedConversation returns the conversation this chat is in at start: the one in the configuration, or the one
// the data directory keeps, from an invite or from an earlier start with a key. A joiner that has none yet returns
// false. enter keeps the configured one, so the app finds it again after a restart.
func (c *ChatService) savedConversation() (conversation, bool, error) {
	saved, found, err := loadConversation(c.set.DataDir)
	if err != nil {
		return saved, false, err
	}
	if !c.set.inConversation {
		return saved, found, nil
	}
	mine := conversation{Key: slices.Clone(c.set.conversationKey[:]), PinAddr: c.set.PinAddr, PinKey: slices.Clone(c.set.pinKey)}
	if found && !slices.Equal(saved.Key, mine.Key) {
		err := errors.New("is not the conversation this data directory joined with an invite: use another -data")
		return mine, false, &ConfigError{Setting: "conversation-key", Err: err}
	}
	return mine, true, nil
}

func loadConversation(dir string) (v conversation, found bool, err error) {
	data, err := readFile(filepath.Join(dir, "conversation.json"), maxStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("conversation.json: %w", err)
	}
	if err = json.Unmarshal(data, &v); err == nil {
		err = v.check()
	}
	if err != nil {
		return v, false, fmt.Errorf("conversation.json: %w", errors.New("damaged"))
	}
	return v, true, nil
}

// saveConversation writes conversation.json, mode 0600, in one step.
func saveConversation(dir string, v conversation) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "conversation.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "conversation.json"))
}

func inviteInfo(i invite.Info) InviteInfo {
	out := InviteInfo{ID: i.ID.String(), Uses: i.MaxUses, Used: i.Used, State: i.State.String()}
	if !i.Expires.IsZero() {
		out.Expires = i.Expires.UTC().Format(time.RFC3339)
	}
	return out
}

func countActive(infos []invite.Info) (n int) {
	for _, i := range infos {
		if i.State == invite.Active {
			n++
		}
	}
	return n
}

// describeJoin turns an error of kolo-invite into the sentence the joiner's window shows.
func describeJoin(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "Joining was cancelled."
	case errors.Is(err, invite.ErrInvalidCode):
		return "That is not an invite code. Paste the whole code, exactly as you received it."
	case errors.Is(err, invite.ErrDenied):
		return "An admin denied your request to join."
	case errors.Is(err, invite.ErrExpired):
		return "This invite has expired. Ask for a new one."
	case errors.Is(err, invite.ErrUsedUp):
		return "This invite has no uses left. Ask for a new one."
	case errors.Is(err, invite.ErrRevoked):
		return "This invite was revoked. Ask for a new one."
	case errors.Is(err, invite.ErrInvalid):
		return "The inviter does not recognize this invite. Check that you pasted the whole code."
	case errors.Is(err, invite.ErrRateLimited):
		return "Too many attempts. Wait a minute and try again."
	case errors.Is(err, invite.ErrBusy):
		return "The inviter is busy. Try again in a moment."
	case errors.Is(err, invite.ErrNotAnswered):
		return "The inviter received your request, but the connection ended before it answered. Try again: asking twice is safe."
	case errors.Is(err, invite.ErrUnreachable):
		return "Could not reach the person who invited you. They must be online with the app open. Try again later."
	case errors.Is(err, invite.ErrTooLarge), errors.Is(err, invite.ErrProtocol):
		return "The inviter sent an answer this app cannot use. Ask for a new invite."
	}
	return "Could not join: " + err.Error()
}

// ownerError turns an error of kolo-invite into the sentence the owner's window shows, and keeps the cause.
func ownerError(err error) error {
	var msg string
	switch {
	case err == nil:
		return nil
	case errors.Is(err, invite.ErrExpired):
		msg = "The invite expired before it was answered."
	case errors.Is(err, invite.ErrUsedUp):
		msg = "The invite has no uses left."
	case errors.Is(err, invite.ErrRevoked):
		msg = "The invite was revoked."
	case errors.Is(err, invite.ErrNotFound):
		msg = "That request or invite is gone: the person left, or the app restarted."
	default:
		return err
	}
	return &InviteError{Message: msg, Err: err}
}
