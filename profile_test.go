package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	kolokv "github.com/ciranda-dev/kolo-kv"
	kololog "github.com/ciranda-dev/kolo-log"
	"github.com/ciranda-dev/kolo-pin"
)

// group is an owner, A, and two members that joined by invite, B and C, all connected.
type group struct {
	n          network
	a, b, c    *ChatService
	aev, bev   *events
	cev        *events
	bdir, cdir string
}

func newGroup(t *testing.T) group {
	t.Helper()
	g := group{n: newNetwork(t), bdir: dataDir(t), cdir: dataDir(t)}
	g.a, g.aev = start(t, g.n.config(dataDir(t)))
	g.b, g.bev = start(t, g.n.joiner(g.bdir))
	g.c, g.cev = start(t, g.n.joiner(g.cdir))
	info := mustCreate(t, g.a, InviteOptions{TTLSeconds: 3600, Uses: 2})
	for _, m := range []*ChatService{g.b, g.c} {
		if err := m.Join(info.Code); err != nil {
			t.Fatal(err)
		}
	}
	linked(t, g.a, g.b)
	linked(t, g.a, g.c)
	linked(t, g.b, g.c)
	return g
}

func keyOf(c *ChatService) string { return hex.EncodeToString(c.pub) }

// nameOn is the name that c shows for who.
func nameOn(c, who *ChatService) string {
	for _, m := range c.Members() {
		if m.Key == keyOf(who) {
			return m.Name
		}
	}
	return ""
}

func sees(t *testing.T, c, who *ChatService, name string) {
	t.Helper()
	eventually(t, fmt.Sprintf("%s to be %q", keyOf(who)[:8], name), func() bool { return nameOn(c, who) == name })
}

func seesRooms(t *testing.T, c *ChatService, want ...string) {
	t.Helper()
	eventually(t, fmt.Sprintf("rooms %v", want), func() bool { return slices.Equal(c.Rooms(), want) })
}

func (e *events) lastMembers() []Member {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.profiles) == 0 {
		return nil
	}
	return e.profiles[len(e.profiles)-1]
}

func (e *events) lastRooms() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.rooms) == 0 {
		return nil
	}
	return e.rooms[len(e.rooms)-1]
}

func told(members []Member, who *ChatService) Member {
	for _, m := range members {
		if m.Key == keyOf(who) {
			return m
		}
	}
	return Member{}
}

// The whole story with three instances joined by invite. Names and rooms reach everybody, also a member that
// was closed (from the pin, as far as the pin can give it), and nobody writes what it does not own.
func TestProfilesAndRooms(t *testing.T) {
	g := newGroup(t)
	a, b, c := g.a, g.b, g.c

	// Who is the owner is known from the start, to the owner and to the members the owner invited.
	if !a.IsOwner() || b.IsOwner() || c.IsOwner() || !bytes.Equal(b.ownerKey(), a.pub) || !bytes.Equal(c.ownerKey(), a.pub) {
		t.Fatal("A is the owner, and B and C know it")
	}
	for _, m := range []*ChatService{a, b, c} {
		if !slices.Equal(m.Rooms(), []string{defaultRoom}) {
			t.Fatalf("every conversation starts with the default room, got %v", m.Rooms())
		}
	}

	// Everybody publishes a profile, and everybody sees every name. Names come from each member's own directory.
	for _, p := range []struct {
		who          *ChatService
		name, avatar string
	}{{a, "Ana", "octopus"}, {b, "Bo", "fox"}, {c, "Cy", "cat-3"}} {
		if err := p.who.SetProfile(p.name, p.avatar); err != nil {
			t.Fatal(err)
		}
	}
	for _, viewer := range []*ChatService{a, b, c} {
		sees(t, viewer, a, "Ana")
		sees(t, viewer, b, "Bo")
		sees(t, viewer, c, "Cy")
	}
	members := b.Members()
	if len(members) != 3 || !members[0].Owner || members[0].Key != keyOf(a) || members[0].Avatar != "octopus" {
		t.Fatalf("the owner comes first, got %+v", members)
	}
	// The owner's member list has all three, written by the owner when each joined.
	eventually(t, "the member list", func() bool { return len(c.roster()) == 3 && len(a.roster()) == 3 })

	// A change of name reaches the owner and C with an event, while everybody is online.
	if err := b.SetProfile("Bob", "fox"); err != nil {
		t.Fatal(err)
	}
	sees(t, a, b, "Bob")
	sees(t, c, b, "Bob")
	eventually(t, "the events", func() bool {
		return told(g.aev.lastMembers(), b).Name == "Bob" && told(g.cev.lastMembers(), b).Name == "Bob"
	})

	// The value is sealed: even with the key of the entry, the name is not in the clear.
	sealed, err := b.dir.Get([]byte(keyProfile))
	if err != nil || bytes.Contains(sealed, []byte("Bob")) {
		t.Fatalf("a profile must be stored sealed, got %q, %v", sealed, err)
	}

	// A profile that is not valid is refused with a typed error, and nothing is written.
	for name, p := range map[string]Profile{
		"no name":     {Name: "  ", Avatar: "fox"},
		"long name":   {Name: strings.Repeat("n", maxName+1)},
		"control":     {Name: "a\x00b"},
		"a URL":       {Name: "Bob", Avatar: "https://example.com/bob.png"},
		"long avatar": {Name: "Bob", Avatar: strings.Repeat("a", maxAvatar+1)},
	} {
		if err := b.SetProfile(p.Name, p.Avatar); !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if nameOn(b, b) != "Bob" {
		t.Fatal("a refused profile changes nothing")
	}

	// Rooms: the owner adds one and everybody sees it, with an event. A member cannot, and gets a typed error.
	if err := a.AddRoom("dev"); err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []*ChatService{a, b, c} {
		seesRooms(t, viewer, defaultRoom, "dev")
	}
	eventually(t, "the room events", func() bool {
		return slices.Equal(g.bev.lastRooms(), []string{defaultRoom, "dev"}) && slices.Equal(g.cev.lastRooms(), []string{defaultRoom, "dev"})
	})
	if err := b.AddRoom("random"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a member adds no room, got %v", err)
	}
	if err := c.AddRoom("random"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("got %v", err)
	}
	for name, want := range map[string]error{"DEV": ErrRoomExists, "general": ErrRoomExists, "": ErrInvalidRoom, "x\ty": ErrInvalidRoom, strings.Repeat("r", maxRoomName+1): ErrInvalidRoom} {
		if err := a.AddRoom(name); !errors.Is(err, want) {
			t.Errorf("room %q: got %v", name, err)
		}
	}
	if !slices.Equal(b.Rooms(), []string{defaultRoom, "dev"}) {
		t.Fatalf("a refused room changes nothing, got %v", b.Rooms())
	}

	// A message carries its room inside the sealed payload, and only a listed room takes messages.
	if err := b.Send("nowhere", "hi"); !errors.Is(err, ErrUnknownRoom) {
		t.Fatalf("got %v", err)
	}
	if err := b.Send("dev", "in dev"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the message in dev", func() bool { return len(g.aev.messages()) == 1 })
	if m := g.aev.messages()[0]; m.Room != "dev" || m.Text != "in dev" || m.From != keyOf(b) {
		t.Fatalf("got %+v", m)
	}
	stored, _ := b.log.Get(0)
	if bytes.Contains(stored, []byte("dev")) {
		t.Fatal("the room must be inside the sealed payload")
	}

	// C is closed while B changes its name. B puts it on the pin, then B is closed too.
	if err := c.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	if err := b.SetProfile("Bobby", "fox"); err != nil {
		t.Fatal(err)
	}
	sees(t, a, b, "Bobby")
	eventually(t, "the pin to hold B's directory", func() bool {
		info, err := pin.Status(t.Context(), b.id, g.n.pin, b.dirLog.ID())
		return err == nil && info.Entries == 3 // Bo, Bob and Bobby
	})
	if err := b.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}

	// C comes back with B offline. What it kept shows at once. The pin gives it the ciphertext of the new entry
	// and no key for it, so the reader shows what it can read: the name before. The owner is online, but it
	// does not hand out entry keys of B's log.
	c2, c2ev := start(t, g.n.joiner(g.cdir))
	if nameOn(c2, b) != "Bob" || nameOn(c2, a) != "Ana" {
		t.Fatalf("C shows what it kept: %q, %q", nameOn(c2, b), nameOn(c2, a))
	}
	<-c2.pinDone
	linked(t, a, c2)
	time.Sleep(2 * time.Second)
	d, err := c2.directory(keyOf(b))
	if err != nil {
		t.Fatal(err)
	}
	if d.rep.Len() != 3 || d.db.Len() != 2 || nameOn(c2, b) != "Bob" {
		t.Fatalf("the pin has 3 entries of B, C can read 2 of them and shows %q: have %d, read %d", nameOn(c2, b), d.rep.Len(), d.db.Len())
	}
	if msg := c2ev.pin().Error; msg != "" {
		t.Fatalf("the pull from the pin worked, but the window says %q", msg)
	}

	// B comes back: its keys reach C, and the name changes.
	b2, _ := start(t, g.n.joiner(g.bdir))
	sees(t, c2, b2, "Bobby")
	sees(t, a, b2, "Bobby")
	if d.db.Len() != 3 {
		t.Fatalf("C reads all 3 entries now, got %d", d.db.Len())
	}
	linked(t, b2, c2)

	// Nobody writes a key it does not own. B cannot write in C's directory: it holds a read-only copy.
	theirs, err := b2.directory(keyOf(c2))
	if err != nil {
		t.Fatal(err)
	}
	if err := theirs.db.Put([]byte(keyProfile), []byte("Mallory")); !errors.Is(err, kolokv.ErrReadOnly) {
		t.Fatalf("got %v", err)
	}
	// B cannot write the owner's keys through the app either.
	if _, err := b2.write(prefixRoom+"evil", stamp{}, false); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("got %v", err)
	}
	// Writing them in its own directory by hand, past the app, achieves nothing: the log is B's, and a room or
	// a member is read only from the owner's log. The profile in B's log is B's, whatever it says.
	evil, _ := b2.sealValue(prefixRoom+"evil", stamp{At: 1})
	fake, _ := b2.sealValue(prefixMember+strings.Repeat("ab", 32), stamp{At: 1})
	forged, _ := b2.sealValue(keyProfile, Profile{Name: "Mallory"})
	for key, value := range map[string][]byte{prefixRoom + "evil": evil, prefixMember + strings.Repeat("ab", 32): fake, keyProfile: forged} {
		if err := b2.dir.Put([]byte(key), value); err != nil {
			t.Fatal(err)
		}
	}
	b2.directoryChanged()
	eventually(t, "A to hold the forged keys", func() bool {
		db := a.readerOf(keyOf(b2))
		_, err := db.Get([]byte(prefixRoom + "evil"))
		return err == nil
	})
	eventually(t, "C to hold the forged keys", func() bool {
		_, err := c2.readerOf(keyOf(b2)).Get([]byte(prefixRoom + "evil"))
		return err == nil
	})
	for _, viewer := range []*ChatService{a, c2, b2} {
		if !slices.Equal(viewer.Rooms(), []string{defaultRoom, "dev"}) {
			t.Fatalf("a room written by B shows up: %v", viewer.Rooms())
		}
		if r := viewer.roster(); len(r) != 3 || slices.Contains(r, strings.Repeat("ab", 32)) {
			t.Fatalf("a member written by B shows up: %v", r)
		}
	}
	sees(t, a, b2, "Mallory") // what B writes in its own directory is B's profile, and only B's
	if nameOn(a, c2) != "Cy" || nameOn(c2, c2) != "Cy" || nameOn(b2, c2) != "Cy" {
		t.Fatal("C's profile is C's own")
	}
	if !a.IsOwner() || b2.IsOwner() {
		t.Fatal("ownership does not move")
	}
	// A value cannot be moved to another key or another author.
	if err := a.openValue(b.pub, keyProfile, sealed, &Profile{}); err != nil {
		t.Fatalf("the value opens where it was written: %v", err)
	}
	if a.openValue(b.pub, prefixRoom+"x", sealed, &stamp{}) == nil || a.openValue(c.pub, keyProfile, sealed, &Profile{}) == nil {
		t.Fatal("a value must not open under another key or author")
	}
}

// A member that is invited by another member knows the real owner, and the owner writes the new member in its
// list when they meet.
func TestInvitedByAMember(t *testing.T) {
	g := newGroup(t)
	info := mustCreate(t, g.b, InviteOptions{TTLSeconds: 3600})
	d, _ := start(t, g.n.joiner(dataDir(t)))
	if err := d.Join(info.Code); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.ownerKey(), g.a.pub) || d.IsOwner() {
		t.Fatal("D takes the owner from the member that invited it, who names the owner")
	}
	linked(t, g.a, d)
	eventually(t, "the owner to list D", func() bool { return slices.Contains(g.a.roster(), keyOf(d)) })
	if err := d.AddRoom("x"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("got %v", err)
	}
	if err := d.SetProfile("Dee", "d"); err != nil {
		t.Fatal(err)
	}
	sees(t, g.a, d, "Dee")
}

// The directory is in the data directory: a restart opens the same log, with the profile in it, and a store
// that cannot tell its two logs apart is refused rather than guessed.
func TestDirectorySurvivesARestart(t *testing.T) {
	n := newNetwork(t)
	dir := dataDir(t)
	a, _ := start(t, n.config(dir))
	if err := a.SetProfile("Ana", "octopus"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddRoom("dev"); err != nil {
		t.Fatal(err)
	}
	id := a.dirLog.ID()
	if err := a.ServiceShutdown(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "directory-log")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("directory-log: %v, %v", fi, err)
	}
	a2, _ := start(t, n.config(dir))
	if a2.dirLog.ID() != id || nameOn(a2, a2) != "Ana" || !slices.Equal(a2.Rooms(), []string{defaultRoom, "dev"}) {
		t.Fatalf("the directory should be the same: %v, %q, %v", a2.dirLog.ID() == id, nameOn(a2, a2), a2.Rooms())
	}
	if m := a2.Members(); len(m) != 1 || !m[0].Owner {
		t.Fatalf("got %+v", m)
	}
	a2.ServiceShutdown()

	if err := os.Remove(filepath.Join(dir, "directory-log")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewChatService(n.config(dir), (&events{}).emit).Start(); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("got %v", err)
	}
}

// A pin capability from step 4 covers the messages log only, and is replaced by one for both logs.
func TestCapabilityCoversBothLogs(t *testing.T) {
	n := newNetwork(t)
	dir := dataDir(t)
	a, _ := start(t, n.config(dir))
	c, err := pin.ParseCapability(a.token)
	if err != nil || !c.Covers(a.log.ID()) || !c.Covers(a.dirLog.ID()) || len(c.Logs) != 2 {
		t.Fatalf("got %+v, %v", c, err)
	}
	messages := a.log.ID()
	a.ServiceShutdown()

	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	old, err := pin.Issue(id, pin.Capability{Peer: n.pin.ID, Logs: []kololog.LogID{messages}, Expires: time.Now().Add(time.Hour), Quota: defaultPinQuota})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pin-token"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the pin refuses two capabilities of the same second
	a2, _ := start(t, n.config(dir))
	if c, err := pin.ParseCapability(a2.token); err != nil || !c.Covers(a2.dirLog.ID()) {
		t.Fatalf("got %+v, %v", c, err)
	}
}
