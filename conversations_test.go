package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	invite "github.com/ciranda-dev/kolo-invite"
)

// convEvents records what Conversations tells the window: the events of each conversation, and every list.
type convEvents struct {
	mu     sync.Mutex
	byConv map[string]*events
	lists  [][]ConversationInfo
}

func (e *convEvents) emit(name string, data any) {
	if ev, ok := data.(ConvEvent); ok {
		e.of(ev.Conv).emit(name, ev.Data)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if l, ok := data.([]ConversationInfo); ok && name == "chat:conversations" {
		e.lists = append(e.lists, l)
	}
}

// of returns the events of one conversation.
func (e *convEvents) of(conv string) *events {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byConv == nil {
		e.byConv = map[string]*events{}
	}
	if e.byConv[conv] == nil {
		e.byConv[conv] = &events{}
	}
	return e.byConv[conv]
}

func (e *convEvents) lastList() []ConversationInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.lists) == 0 {
		return nil
	}
	return e.lists[len(e.lists)-1]
}

// app is the configuration of an app of the network n with its data in root, and the pin for new conversations.
func (n network) app(root string) Config {
	return Config{DataDir: root, Bootstrap: n.bootstrap, PinAddr: n.pin.Addr, PinKey: hex.EncodeToString(n.pin.ID), LookupInterval: time.Second}
}

// startApp runs the app's service with cfg and stops it when the test ends.
func startApp(t *testing.T, cfg Config) (*Conversations, *convEvents) {
	t.Helper()
	ev := &convEvents{}
	a := NewConversations(cfg, ev.emit)
	if _, err := a.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.ServiceShutdown() })
	return a, ev
}

func mustCreateConv(t *testing.T, a *Conversations, name string) ConversationInfo {
	t.Helper()
	info, err := a.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func engineOf(t *testing.T, a *Conversations, conv string) *ChatService {
	t.Helper()
	c, err := a.engine(conv)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// folders lists the conversation folders under root.
func folders(t *testing.T, root string) []string {
	t.Helper()
	items, err := os.ReadDir(filepath.Join(root, conversationDir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Name())
	}
	return out
}

// snapshot is every path under root with its mode and, for a file, its contents.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if d.Type().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			v += " " + string(b)
		}
		out[path] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func hasText(msgs []ChatMsg, text string) bool {
	return slices.ContainsFunc(msgs, func(m ChatMsg) bool { return m.Text == text })
}

// T1: a data folder of step 5 or older is refused with the folder named, whichever of its names it holds, and
// nothing in it changes.
func TestOldDataFolderIsRefused(t *testing.T) {
	// The names are written out here, not taken from oldLayoutNames, so a shorter list fails this test.
	for _, name := range []string{"identity.key", "conversation.json", "members.json", "directory-log", "store", "invites", "pin-token"} {
		t.Run(name, func(t *testing.T) {
			root := dataDir(t)
			path := filepath.Join(root, name)
			var err error
			if name == "store" || name == "invites" {
				if err = os.Mkdir(path, 0o700); err == nil {
					err = os.WriteFile(filepath.Join(path, "kept"), []byte("old data"), 0o600)
				}
			} else {
				err = os.WriteFile(path, []byte("old "+name), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, root)
			_, err = NewConversations(Config{DataDir: root}, (&convEvents{}).emit).Start()
			var de *DataDirError
			if !errors.As(err, &de) || de.Layout != 1 || de.Dir != root || !errors.Is(err, ErrOldLayout) ||
				!strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "step 5 or older") {
				t.Fatalf("got %v", err)
			}
			if after := snapshot(t, root); !mapsEqual(after, before) {
				t.Fatalf("the folder changed:\n%v\n%v", before, after)
			}
		})
	}
}

// T2: a layout.json of another version, or a damaged one, is refused with its layout and nothing changes.
func TestOtherLayoutIsRefused(t *testing.T) {
	for body, want := range map[string]int{`{"layout":4}`: 4, `{"layout":`: -1, `{}`: -1, `{"layout":1}`: -1} {
		root := dataDir(t)
		if err := os.WriteFile(filepath.Join(root, layoutFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, root)
		_, err := NewConversations(Config{DataDir: root}, (&convEvents{}).emit).Start()
		var de *DataDirError
		if !errors.As(err, &de) || de.Layout != want || de.Dir != root || errors.Is(err, ErrOldLayout) ||
			!strings.Contains(err.Error(), fmt.Sprintf("(layout %d)", want)) {
			t.Fatalf("%s: got %v", body, err)
		}
		if after := snapshot(t, root); !mapsEqual(after, before) {
			t.Fatalf("%s: the folder changed", body)
		}
	}
}

// A data folder, or its conversations/, that other users can open is refused, and nothing in it changes.
func TestOpenDataFolderIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
		sub  bool
	}{{"0755", 0o755, false}, {"0777", 0o777, false}, {"conversations/ 0750", 0o750, true}} {
		root := dataDir(t)
		dir := root
		if tc.sub {
			a := NewConversations(Config{DataDir: root}, (&convEvents{}).emit)
			if _, err := a.Start(); err != nil {
				t.Fatal(err)
			}
			a.ServiceShutdown()
			dir = filepath.Join(root, conversationDir)
		}
		if err := os.Chmod(dir, tc.mode); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, root)
		_, err := NewConversations(Config{DataDir: root}, (&convEvents{}).emit).Start()
		var de *DataDirError
		if !errors.As(err, &de) || !de.Open || de.Dir != dir || !strings.Contains(err.Error(), "chmod 700") {
			t.Fatalf("%s: got %v", tc.name, err)
		}
		if after := snapshot(t, root); !mapsEqual(after, before) {
			t.Fatalf("%s: the folder changed", tc.name)
		}
	}
}

// T3: a new data folder gets layout.json and conversations/, both private, and starts again as it is, also after
// conversations/ was deleted. The lists reach the window as lists, never null.
func TestNewDataFolderGetsTheLayout(t *testing.T) {
	root := filepath.Join(dataDir(t), "new")
	for range 2 {
		ev := &convEvents{}
		a := NewConversations(Config{DataDir: root}, ev.emit)
		info, err := a.Start()
		if err != nil || len(info.Conversations) != 0 || info.Conversations == nil || info.CanCreate ||
			info.MaxConversations != maxConversations || ev.lastList() == nil {
			t.Fatalf("%+v, %v", info, err)
		}
		a.ServiceShutdown()
	}
	if err := os.Remove(filepath.Join(root, conversationDir)); err != nil {
		t.Fatal(err)
	}
	a := NewConversations(Config{DataDir: root}, (&convEvents{}).emit)
	if _, err := a.Start(); err != nil {
		t.Fatalf("without conversations/: %v", err)
	}
	a.ServiceShutdown()
	if b, err := os.ReadFile(filepath.Join(root, layoutFile)); err != nil || string(b) != `{"layout":3}` {
		t.Fatalf("layout.json: %q, %v", b, err)
	}
	for path, mode := range map[string]os.FileMode{layoutFile: 0o600, conversationDir: os.ModeDir | 0o700} {
		if st, err := os.Stat(filepath.Join(root, path)); err != nil || st.Mode() != mode {
			t.Fatalf("%s: %v, %v", path, st.Mode(), err)
		}
	}
}

// T4, T5 and T12. Alice starts two conversations: two folders, two kolo-ids, two topics. Bob joins the second while he
// is in the first, as Alice's other kolo-id is a stranger to him. A message goes to its own conversation only, with
// its ID on the event. Both conversations keep their logs on the one pin, and Bob, back after missing messages, gets
// each from it in its own conversation.
func TestConversationsStayApart(t *testing.T) {
	n := newNetwork(t)
	aroot, broot := dataDir(t), dataDir(t)
	alice, _ := startApp(t, n.app(aroot))
	fam, work := mustCreateConv(t, alice, "Family"), mustCreateConv(t, alice, "Work")
	af, aw := engineOf(t, alice, fam.ID), engineOf(t, alice, work.ID)
	if fam.ID == work.ID || fam.Me == work.Me || af.topic() == aw.topic() || af.set.conversationKey == aw.set.conversationKey ||
		af.set.DataDir != filepath.Join(aroot, conversationDir, fam.ID) || aw.set.DataDir != filepath.Join(aroot, conversationDir, work.ID) {
		t.Fatalf("the conversations share something: %+v %+v", fam, work)
	}

	bob, bev := startApp(t, n.joiner(broot))
	join := func(conv, name string) ConversationInfo {
		t.Helper()
		inv, err := alice.CreateInvite(conv, InviteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return joinConv(t, engineOf(t, alice, conv), bob, inv.Code, name)
	}
	bfam := join(fam.ID, "Family")
	began := time.Now()
	bwork := join(work.ID, "Work")
	if d := time.Since(began); d > joinFindTimeout {
		t.Fatalf("the second join took %v", d)
	}
	if bfam.ID == bwork.ID || bfam.Me == bwork.Me || !bfam.Joined || bwork.Name != "Work" {
		t.Fatalf("Bob's conversations: %+v %+v", bfam, bwork)
	}
	linked(t, af, engineOf(t, bob, bfam.ID))
	linked(t, aw, engineOf(t, bob, bwork.ID))

	if err := alice.Send(fam.ID, defaultRoom, "for the family"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Send(work.ID, defaultRoom, "for work"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "each message in its conversation", func() bool {
		return hasText(bev.of(bfam.ID).messages(), "for the family") && hasText(bev.of(bwork.ID).messages(), "for work")
	})
	time.Sleep(time.Second)
	if hasText(bev.of(bfam.ID).messages(), "for work") || hasText(bev.of(bwork.ID).messages(), "for the family") ||
		len(bev.of("").messages()) != 0 {
		t.Fatal("a message reached another conversation")
	}

	bob.ServiceShutdown()
	for conv, text := range map[string]string{fam.ID: "family while Bob is away", work.ID: "work while Bob is away"} {
		if err := alice.Send(conv, defaultRoom, text); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the pin to hold both conversations", func() bool { return pinned(t, n, af).Entries == 2 && pinned(t, n, aw).Entries == 2 })

	_, b2ev := startApp(t, n.joiner(broot))
	fromPin := func(conv, text string) bool {
		return slices.ContainsFunc(b2ev.of(conv).messages(), func(m ChatMsg) bool { return m.Text == text && m.Via == "pin" })
	}
	eventually(t, "each conversation from the pin", func() bool {
		return fromPin(bfam.ID, "family while Bob is away") && fromPin(bwork.ID, "work while Bob is away")
	})
	if hasText(b2ev.of(bfam.ID).messages(), "work while Bob is away") {
		t.Fatal("the pin's message reached another conversation")
	}
}

// T7: -conversation-key starts the folder that holds that key, or one new folder for it, once.
func TestConversationKeySetting(t *testing.T) {
	n := newNetwork(t)
	root := dataDir(t)
	cfg := n.app(root)
	cfg.ConversationKey = testKey
	keyOf := func(id string) string {
		v, _, err := loadConversation(filepath.Join(root, conversationDir, id))
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(v.Key)
	}

	first, _ := startApp(t, cfg)
	list := first.List()
	if len(list) != 1 || !list[0].Joined || !list[0].Admin || list[0].Name != defaultConvName || keyOf(list[0].ID) != testKey {
		t.Fatalf("first start: %+v", list)
	}
	id := list[0].ID
	first.ServiceShutdown()

	second, _ := startApp(t, cfg)
	other := mustCreateConv(t, second, "Other")
	second.ServiceShutdown()
	if got := folders(t, root); len(got) != 2 || !slices.Contains(got, id) {
		t.Fatalf("folders after two starts: %v", got)
	}

	third, _ := startApp(t, cfg)
	if engineOf(t, third, id).set.conversationKey == engineOf(t, third, other.ID).set.conversationKey || keyOf(other.ID) == testKey {
		t.Fatal("the key went to another conversation")
	}
	if len(folders(t, root)) != 2 {
		t.Fatal("a third start made a folder")
	}
}

// freeUDPPort returns a UDP port of 127.0.0.1 that nothing listens on now.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// A fixed -listen port holds one conversation: a second is refused at Create, at Join and at Start.
func TestFixedListenPortHoldsOneConversation(t *testing.T) {
	n := newNetwork(t)
	cfg := n.app(dataDir(t))
	cfg.Listen = fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	a, _ := startApp(t, cfg)
	mustCreateConv(t, a, "One")
	if _, err := a.Create("Two"); !isListenError(err) {
		t.Fatalf("Create: %v", err)
	}
	if _, err := a.Join("not a code", ""); !isListenError(err) {
		t.Fatalf("Join: %v", err)
	}
	a.ServiceShutdown()

	second := filepath.Join(cfg.DataDir, conversationDir, strings.Repeat("ab", 16))
	if err := os.Mkdir(second, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "conversation.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConversations(cfg, (&convEvents{}).emit).Start(); !isListenError(err) {
		t.Fatalf("Start with two: %v", err)
	}
	cfg.Listen = "127.0.0.1:0"
	if b, _ := startApp(t, cfg); len(b.List()) != 2 {
		t.Fatalf("port 0: %+v", b.List())
	}
}

func isListenError(err error) bool {
	var ce *ConfigError
	return errors.As(err, &ce) && ce.Setting == "listen" && errors.Is(err, errFixedPort)
}

// T13: Create makes a conversation we own, with a fresh random key, private files and the name only we see. A bad
// name, no pin, or an engine that does not start, adds nothing.
func TestCreateConversation(t *testing.T) {
	n := newNetwork(t)
	root := dataDir(t)
	a, ev := startApp(t, n.app(root))
	x, y := mustCreateConv(t, a, "Family"), mustCreateConv(t, a, "  ")
	if !x.Admin || !x.Joined || x.Name != "Family" || y.Name != defaultConvName || x.Error != "" {
		t.Fatalf("created: %+v %+v", x, y)
	}
	keys := map[string]bool{}
	for _, id := range []string{x.ID, y.ID} {
		dir := filepath.Join(root, conversationDir, id)
		v, found, err := loadConversation(dir)
		if err != nil || !found || len(v.Key) != 32 {
			t.Fatalf("%s: %+v, %v", id, v, err)
		}
		keys[string(v.Key)] = true
		for path, mode := range map[string]os.FileMode{"": os.ModeDir | 0o700, "conversation.json": 0o600, localFile: 0o600} {
			if st, err := os.Stat(filepath.Join(dir, path)); err != nil || st.Mode() != mode {
				t.Fatalf("%s/%s: %v, %v", id, path, st.Mode(), err)
			}
		}
	}
	if len(keys) != 2 {
		t.Fatal("two conversations got one key")
	}
	if l := ev.lastList(); len(l) != 2 || l[0].ID != x.ID && l[1].ID != x.ID {
		t.Fatalf("the window's list: %+v", l)
	}
	for _, bad := range []string{strings.Repeat("a", maxConvName+1), "a\x00b", "\xff"} {
		if _, err := a.Create(bad); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if len(folders(t, root)) != 2 || len(a.List()) != 2 {
		t.Fatal("a refused Create added a conversation")
	}

	joiner, _ := startApp(t, n.joiner(dataDir(t)))
	if info, err := joiner.Start(); err != nil || info.CanCreate {
		t.Fatalf("an app without a pin: %+v, %v", info, err)
	}
	if _, err := joiner.Create("Family"); !errors.Is(err, ErrNoPins) || len(folders(t, joiner.root)) != 0 {
		t.Fatalf("no pin: %v", err)
	}

	cfg := n.app(dataDir(t))
	cfg.Listen = "256.0.0.1:0"
	broken, _ := startApp(t, cfg)
	if _, err := broken.Create("Family"); err == nil || len(folders(t, cfg.DataDir)) != 0 || len(broken.List()) != 0 {
		t.Fatalf("a Create that failed: %v, %v", err, folders(t, cfg.DataDir))
	}
}

// S18: a Join that fails, or is cancelled, removes its folder, kolo-id included, and the next Join starts in a new
// folder with a new kolo-id: an admin may have admitted the old key already. A restart removes a join a crash stopped.
// The bound counts the join in progress.
func TestFailedJoinStartsAfresh(t *testing.T) {
	n := newNetwork(t)
	owner, _ := startApp(t, n.app(dataDir(t)))
	code, err := owner.CreateInvite(mustCreateConv(t, owner, "Family").ID, InviteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	owner.ServiceShutdown() // nobody answers, so the join waits

	root := dataDir(t)
	a, _ := startApp(t, n.app(root))
	var ie *InviteError
	if _, err := a.Join("not a code", ""); !errors.As(err, &ie) || !errors.Is(err, invite.ErrInvalidCode) {
		t.Fatalf("a bad code: %v", err)
	}
	if got := folders(t, root); len(got) != 0 || len(a.List()) != 0 {
		t.Fatalf("a failed join left %v", got)
	}
	for range maxConversations - 1 {
		mustCreateConv(t, a, "")
	}
	done := make(chan error, 1)
	go func() { _, err := a.Join(code.Code, "Family"); done <- err }()
	eventually(t, "the join to start", func() bool { return len(folders(t, root)) == maxConversations })
	if _, err := a.Create("One more"); !errors.Is(err, ErrTooManyConversations) {
		t.Fatalf("the join in progress does not count: %v", err)
	}
	i := slices.IndexFunc(a.List(), func(c ConversationInfo) bool { return !c.Joined })
	if i < 0 {
		t.Fatalf("the join in progress is not listed as one: %+v", a.List())
	}
	c := engineOf(t, a, a.List()[i].ID)
	eventually(t, "the join to wait", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.joining != nil })
	a.CancelJoin()
	if err := waitJoin(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled join: %v", err)
	}
	if got := folders(t, root); len(got) != maxConversations-1 {
		t.Fatalf("a cancelled join left %v", got)
	}
	a.ServiceShutdown()

	stale := filepath.Join(root, conversationDir, strings.Repeat("ef", 16)) // a join a crash stopped
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := startApp(t, n.app(root))
	if l := b.List(); len(l) != maxConversations-1 || exists(t, stale) {
		t.Fatalf("after a restart: %+v", l)
	}
}

// A member's folder that lost conversation.json is not a join that did not finish: a start refuses it with the folder
// to move aside and deletes nothing, whether the member wrote or not.
func TestMemberFolderWithoutConversationIsKept(t *testing.T) {
	n := newNetwork(t)
	alice, _ := startApp(t, n.app(dataDir(t)))
	conv := mustCreateConv(t, alice, "Kept")
	af := engineOf(t, alice, conv.ID)
	for _, writes := range []bool{true, false} {
		inv, err := alice.CreateInvite(conv.ID, InviteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cfg := n.joiner(dataDir(t))
		app, _ := startApp(t, cfg)
		info := joinConv(t, af, app, inv.Code, "Kept")
		if writes {
			if err := app.Send(info.ID, defaultRoom, "a message to keep"); err != nil {
				t.Fatal(err)
			}
		}
		app.ServiceShutdown()
		dir := filepath.Join(cfg.DataDir, conversationDir, info.ID)
		if err := os.Remove(filepath.Join(dir, "conversation.json")); err != nil {
			t.Fatal(err)
		}
		again, _ := startApp(t, cfg)
		l := again.List()
		if len(l) != 1 || l[0].ID != info.ID || l[0].Folder != dir || !strings.Contains(l[0].Error, "conversation.json is gone") {
			t.Fatalf("writes %v, the row: %+v", writes, l)
		}
		for _, name := range []string{"identity.key", "store", "braid", "members.json", localFile} {
			if !exists(t, filepath.Join(dir, name)) {
				t.Fatalf("%s/%s was deleted", dir, name)
			}
		}
	}
}

// A conversation.json that is a FIFO, a dangling link or a file nobody may read never hangs a start: the row says why,
// and the folder and the file stay.
func TestSpecialConversationFileKeepsTheFolder(t *testing.T) {
	n := newNetwork(t)
	root := dataDir(t)
	a, _ := startApp(t, n.app(root))
	a.ServiceShutdown()
	kinds := map[string]func(string) error{
		"fifo":     mkfifo,
		"dangling": func(p string) error { return os.Symlink(filepath.Join(filepath.Dir(p), "nowhere"), p) },
		"mode 000": func(p string) error { return os.WriteFile(p, []byte("{}"), 0) },
	}
	made := map[string]string{}
	for kind, create := range kinds {
		dir := filepath.Join(root, conversationDir, fmt.Sprintf("%032x", len(made)+1))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := create(filepath.Join(dir, "conversation.json")); err != nil {
			t.Logf("%s: %v", kind, err) // no FIFO on this system
			os.RemoveAll(dir)
			continue
		}
		made[kind] = dir
	}
	// S7-11: a folder with members.json and no conversation.json is no join that did not finish.
	members := filepath.Join(root, conversationDir, fmt.Sprintf("%032x", 9))
	if err := os.Mkdir(members, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(members, "members.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewConversations(n.app(root), (&convEvents{}).emit)
	started := make(chan error, 1)
	go func() { _, err := b.Start(); started <- err }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.ServiceShutdown() }) // only once the start returned: a hung one holds its lock
	case <-time.After(30 * time.Second):
		t.Fatal("the start hangs on a special conversation.json")
	}
	if !exists(t, filepath.Join(members, "members.json")) {
		t.Fatal("a folder with members.json was deleted")
	}
	var rows []ConversationInfo
	for _, r := range b.List() {
		if r.ID != filepath.Base(members) {
			rows = append(rows, r)
		}
	}
	if len(rows) != len(made) {
		t.Fatalf("rows %+v", rows)
	}
	for _, r := range rows {
		if !strings.Contains(r.Error, "conversation.json") || r.Folder == "" {
			t.Errorf("a special conversation.json started: %+v", r)
		}
	}
	for kind, dir := range made {
		if _, err := os.Lstat(filepath.Join(dir, "conversation.json")); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

// S8: a data folder of step 6, layout 2, is refused with its layout and the sentence to move it, and nothing in it
// changes: its conversations have no braid, and there is no migration.
func TestLayoutTwoIsRefused(t *testing.T) {
	root := dataDir(t)
	if err := writeJSON(filepath.Join(root, layoutFile), map[string]int{"layout": 2}); err != nil {
		t.Fatal(err)
	}
	conv := filepath.Join(root, conversationDir, strings.Repeat("ab", 16))
	if err := os.MkdirAll(conv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, "conversation.json"), []byte(`{"key":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	_, err := NewConversations(Config{DataDir: root}, (&convEvents{}).emit).Start()
	var de *DataDirError
	if !errors.As(err, &de) || de.Layout != 2 || de.Dir != root || !strings.Contains(err.Error(), "step 6") {
		t.Fatalf("got %v", err)
	}
	if after := snapshot(t, root); !mapsEqual(after, before) {
		t.Fatal("the folder changed")
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// T16: every call with an unknown conversation ID fails with ErrUnknownConversation and reaches no conversation.
func TestUnknownConversation(t *testing.T) {
	n := newNetwork(t)
	a, _ := startApp(t, n.app(dataDir(t)))
	known := mustCreateConv(t, a, "Known")
	c := engineOf(t, a, known.ID)
	for _, conv := range []string{"", "nope", strings.Repeat("0", 32), "../" + known.ID, strings.ToUpper(known.ID)} {
		_, members := a.Members(conv)
		_, rooms := a.Rooms(conv)
		_, admin := a.IsAdmin(conv)
		_, inv := a.CreateInvite(conv, InviteOptions{})
		_, invs := a.Invites(conv)
		for what, err := range map[string]error{
			"Send": a.Send(conv, defaultRoom, "x"), "Members": members, "Rooms": rooms, "IsAdmin": admin,
			"RemoveMember": a.RemoveMember(conv, keyOf(c)), "SetProfile": a.SetProfile(conv, "Name", ""), "AddRoom": a.AddRoom(conv, "room"),
			"CreateInvite": inv, "Invites": invs, "Revoke": a.Revoke(conv, "00"), "Approve": a.Approve(conv, 1, false),
			"Deny": a.Deny(conv, 1),
		} {
			if !errors.Is(err, ErrUnknownConversation) {
				t.Errorf("%s(%q): %v", what, conv, err)
			}
		}
	}
	if c.log.Len() != 0 || len(c.Rooms()) != 1 || c.profileOf(keyOf(c)).Name != "" {
		t.Fatal("a call for another ID reached the conversation")
	}
	if infos, _ := c.Invites(); len(infos) != 0 {
		t.Fatalf("invites: %+v", infos)
	}
}
