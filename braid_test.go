package main

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kolobraid "github.com/ciranda-dev/kolo-braid"
	koloid "github.com/ciranda-dev/kolo-id"
	invite "github.com/ciranda-dev/kolo-invite"
	kolostore "github.com/ciranda-dev/kolo-store"
	kolosync "github.com/ciranda-dev/kolo-sync"
)

// rosterOf is the sorted member keys c shows.
func rosterOf(c *ChatService) []string {
	var keys []string
	for _, m := range c.Members() {
		keys = append(keys, m.Key)
	}
	slices.Sort(keys)
	return keys
}

func roleOf(c *ChatService, who string) (kolobraid.Role, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.accepted[who]
	return m.Roles, ok
}

func registeredWith(c *ChatService, who string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.links[who]
	return l != nil && l.registered
}

// S2, S6, S15: Alice founds, Bob is approved as an admin and Carol as a member. Alice and Bob add "design" at the same
// time: one room everywhere. Bob removes Carol while Alice approves Dave: every member
// ends with the same list. The log holds no key, room name or text.
func TestTwoAdminsConverge(t *testing.T) {
	logs := captureLog(t)
	n := newNetwork(t)
	a, _ := start(t, n.config(dataDir(t)))
	b, _ := start(t, n.joiner(dataDir(t)))
	joinVia(t, a, b, true)
	c, _ := n.member(t, a, dataDir(t))
	introduced(t, a, b)
	introduced(t, a, c)
	introduced(t, b, c)
	if r, _ := roleOf(a, keyOf(b)); r != kolobraid.RoleAdmin {
		t.Fatalf("Bob approved as an admin has roles %d", r)
	}
	if r, ok := roleOf(a, keyOf(c)); r != 0 || !ok {
		t.Fatalf("Carol approved as a member has roles %d", r)
	}
	eventually(t, "Bob to be an admin", b.IsAdmin)

	var wg sync.WaitGroup
	for _, x := range []*ChatService{a, b} {
		wg.Go(func() {
			if err := x.AddRoom("design"); err != nil && !errors.Is(err, ErrRoomExists) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for _, x := range []*ChatService{a, b, c} {
		seesRooms(t, x, defaultRoom, "design")
	}
	if _, err := b.braid.Append(t.Context(), encodeAddRoom("DESIGN")); err != nil { // an admin's duplicate, refused
		t.Fatal(err)
	}
	eventually(t, "the adds to settle", func() bool {
		rejected := 0
		for o, err := range a.braid.Entries(0) {
			if err == nil && o.Settled && o.Rejected {
				rejected++
			}
		}
		return rejected > 0 && len(a.state(a.braid.Status()).PendingRooms) == 0
	})

	d, _ := start(t, n.joiner(dataDir(t)))
	code := mustCreate(t, a, InviteOptions{}).Code
	done := joinAsync(d, code)
	request := requestOf(t, a, d.pub)
	wg.Go(func() {
		if err := b.RemoveMember(keyOf(c)); err != nil {
			t.Error(err)
		}
	})
	if err := a.Approve(request, false); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := waitJoin(t, done); err != nil {
		t.Fatal(err)
	}
	want := []string{keyOf(a), keyOf(b), keyOf(d)}
	slices.Sort(want)
	for _, x := range []*ChatService{a, b, d} {
		eventually(t, "the same member list", func() bool { return slices.Equal(rosterOf(x), want) })
	}

	key, _ := hex.DecodeString(testKey)
	for _, s := range []string{testKey, string(key), "design"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("the log holds %q", s)
		}
	}
}

// S3, S3c, S3d, S4g, S5, S11: Carol is removed. Every app that holds the removal stops talking to her: no
// registration, NoAccess, her new messages are neither shown nor pulled, and what she wrote before stays. She keeps
// the key and the ciphertext on the pin, but an app that holds the removal gives her no entry key. A member is no
// admin, and the settler cannot be removed.
func TestRemovedMemberIsNotTalkedTo(t *testing.T) {
	n := newNetwork(t)
	a, aev := start(t, n.config(dataDir(t)))
	bdir := dataDir(t)
	b, bev := n.member(t, a, bdir)
	cdir := dataDir(t)
	c, _ := n.member(t, a, cdir)
	introduced(t, a, c)
	introduced(t, b, c)
	if err := c.Send(defaultRoom, "before"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Carol's message", func() bool { return hasText(aev.messages(), "before") && hasText(bev.messages(), "before") })

	if err := b.RemoveMember(keyOf(c)); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("a member removes: %v", err)
	}
	if err := a.RemoveMember(keyOf(a)); !errors.Is(err, ErrSettler) {
		t.Fatalf("the settler is removed: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), braidWrite)
	ref, err := a.braid.RemoveMember(ctx, a.pub) // past the app: the library keeps its one settler
	cancel()
	if err == nil {
		eventually(t, "the removal of the settler to settle", func() bool { return settledEntry(a.braid, ref.ID) })
		if !a.isAccepted(keyOf(a)) {
			t.Fatal("the library removed the settler")
		}
	}

	if err := a.RemoveMember(keyOf(c)); err != nil {
		t.Fatal(err)
	}
	for _, x := range []*ChatService{a, b} {
		eventually(t, "the removal", func() bool { return !x.isAccepted(keyOf(c)) && !registeredWith(x, keyOf(c)) })
		if got := x.authorize(c.pub, x.pub, x.log.ID()); got != kolosync.NoAccess {
			t.Fatalf("a removed member gets %v", got)
		}
	}
	shown, _ := b.shown(keyOf(c))
	dirBefore := dirLen(a, keyOf(c))
	rep, err := b.replica(keyOf(c))
	if err != nil {
		t.Fatal(err)
	}
	held := rep.Len()
	if err := c.Send(defaultRoom, "after"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetProfile("Carol again", ""); err != nil { // S7-8: tells her links to pull her directory
		t.Fatal(err)
	}
	for _, x := range []*ChatService{a, b} {
		barrier(t, x, c)
	}
	if dirLen(a, keyOf(c)) != dirBefore {
		t.Fatal("a removed member's directory was pulled")
	}
	if hasText(aev.messages(), "after") || hasText(bev.messages(), "after") {
		t.Fatal("a removed member's new message was shown")
	}
	if now, _ := b.shown(keyOf(c)); now != shown {
		t.Fatalf("Bob pulled Carol: shown %d then %d", shown, now)
	}
	if rep.Len() != held {
		t.Fatalf("Bob's copy of Carol's log grew from %d to %d", held, rep.Len())
	}
	if !hasText(bev.messages(), "before") {
		t.Fatal("what Carol wrote before is gone")
	}
	eventually(t, "the pin to hold Carol's new message", func() bool { return pinned(t, n, c).Entries == 2 })
	b.ServiceShutdown()
	b2, b2ev := start(t, n.joiner(bdir))
	<-b2.pinDone
	if rep, err := b2.replica(keyOf(c)); err != nil || rep.Len() != held || hasText(b2ev.messages(), "after") {
		t.Fatalf("Bob pulled removed Carol from the pin: %v", err)
	}

	c.ServiceShutdown()
	if err := a.Send(defaultRoom, "new for everyone"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pin to hold it", func() bool { return pinned(t, n, a).Entries == 1 })
	_, c2ev := start(t, n.joiner(cdir))
	eventually(t, "removed Carol to hold the ciphertext from the pin", func() bool { return c2ev.pin().Locked >= 1 })
	time.Sleep(3 * time.Second) // Alice is online, and would give the key to a member
	if hasText(c2ev.messages(), "new for everyone") {
		t.Fatal("an app that holds the removal gave removed Carol an entry key")
	}
}

// S12, D17: a restart opens the braid from braid/ with the same lists. An admin that is not the settler and restarts
// alone cannot confirm its strand: AddRoom returns within the write bound with the waiting sentence, and the window
// says so.
func TestRestartKeepsTheMemberList(t *testing.T) {
	n := newNetwork(t)
	adir, bdir := dataDir(t), dataDir(t)
	a, _ := start(t, n.config(adir))
	b, _ := start(t, n.joiner(bdir))
	joinVia(t, a, b, true)
	introduced(t, a, b)
	if err := a.AddRoom("kept"); err != nil {
		t.Fatal(err)
	}
	seesRooms(t, b, defaultRoom, "kept")
	members := rosterOf(a)
	a.ServiceShutdown()
	b.ServiceShutdown()

	a2, _ := start(t, n.joiner(adir))
	if !slices.Equal(a2.Rooms(), []string{defaultRoom, "kept"}) || !slices.Equal(rosterOf(a2), members) || !a2.IsAdmin() {
		t.Fatalf("after a restart: rooms %v, members %v", a2.Rooms(), rosterOf(a2))
	}
	a2.ServiceShutdown()

	b2, b2ev := start(t, n.joiner(bdir))
	added := make(chan error, 1)
	go func() { added <- b2.AddRoom("alone") }()
	select {
	case err := <-added:
		if !errors.Is(err, ErrWaitingForMember) {
			t.Fatalf("an admin alone after a restart: %v", err)
		}
	case <-time.After(braidWrite + 10*time.Second):
		t.Fatal("AddRoom of an admin alone does not return")
	}
	eventually(t, "the window to say it waits", func() bool { return b2ev.state().Unconfirmed })
}

// pendingRequestFor is a request in c's list, as onRequested keeps it, from a new identity with a real braid join
// request. kolo-invite does not know its ID, so its Approve fails after Admit.
func pendingRequestFor(t *testing.T, c *ChatService, id uint64) *koloid.Identity {
	t.Helper()
	joiner, _ := koloid.Generate()
	j, err := kolobraid.NewJoin(t.Context(), joiner, kolobraid.Options{Backend: kolostore.NewMemory(), App: stateApp, Apply: applyState})
	if err != nil {
		t.Fatal(err)
	}
	req, err := j.Request(nil)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.requests[invite.RequestID(id)] = pendingRequest{ev: invite.Event{Request: invite.RequestID(id), Joiner: joiner.PublicKey()},
		data: joinData{Request: req}}
	c.mu.Unlock()
	return joiner
}

// S6, S13, D10: an approval whose invite cannot carry the welcome removes the joiner again. At 64 members Approve is
// refused with the full sentence before anything is written.
func TestApproveBoundsAndUndo(t *testing.T) {
	n := newNetwork(t)
	a, _ := start(t, n.config(dataDir(t)))
	joiner := pendingRequestFor(t, a, 999)
	if err := a.Approve(999, true); !errors.Is(err, invite.ErrNotFound) {
		t.Fatalf("an approval the invite cannot carry: %v", err)
	}
	eventually(t, "the joiner to be removed again", func() bool { return !a.isAccepted(joiner.PublicKeyString()) })

	for len(rosterOf(a)) < maxMembers {
		id, _ := koloid.Generate()
		admitKey(t, a, id, 0)
		eventually(t, "the admission", func() bool { return a.isAccepted(id.PublicKeyString()) })
	}
	full := pendingRequestFor(t, a, 1000)
	before := a.braid.Status().Tentative
	if err := a.Approve(1000, false); !errors.Is(err, ErrFull) {
		t.Fatalf("Approve at 64: %v", err)
	}
	if a.braid.Status().Tentative != before || a.isAccepted(full.PublicKeyString()) || len(rosterOf(a)) != maxMembers {
		t.Fatal("Approve at 64 wrote an AddMember")
	}
}

// settledEntry says whether the entry id is settled in b's order.
func settledEntry(b *kolobraid.Braid, id kolobraid.EntryID) bool {
	for o, err := range b.Entries(0) {
		if err != nil {
			return false
		}
		if o.ID == id {
			return o.Settled
		}
	}
	return false
}

// dirLen is how many entries of who's directory a holds, 0 when it holds none.
func dirLen(a *ChatService, who string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d := a.dirs[who]; d != nil {
		return d.rep.Len()
	}
	return 0
}

// barrier sends a hello from c to a on their link and waits until a handled it: a reads the link's messages one by
// one, in the order they come, so what c sent before was handled too.
func barrier(t *testing.T, a, c *ChatService) {
	t.Helper()
	hello := func() *wireMsg {
		a.mu.Lock()
		defer a.mu.Unlock()
		if l := a.links[keyOf(c)]; l != nil {
			return l.hello
		}
		return nil
	}
	before := hello()
	c.mu.Lock()
	l := c.links[keyOf(a)]
	c.mu.Unlock()
	if l == nil {
		t.Fatal("no link for the barrier")
	}
	if err := c.sendTo(l, c.hello(l, false)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the barrier", func() bool { h := hello(); return h != nil && h != before })
}
