package main

//lint:file-ignore ST1005 these errors are sentences the window shows as they are

// Several conversations in one app.
//
// Each conversation lives in a folder of its own, <data>/conversations/<id>, with its own engine (ChatService) and
// so its own kolo-id, swarm, logs, pin and invites. One engine per conversation follows the rules of the libraries
// as they are: a pin keeps the newest capability of each owner, and a connection to a peer key serves one protocol.
// Conversations lists the folders, starts an engine for each, and routes every call of the window by ID.

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	layoutVersion    = 3 // 3: each conversation has a braid
	layoutFile       = "layout.json"
	conversationDir  = "conversations"
	localFile        = "local.json"
	defaultConvName  = "Conversation"
	maxConvName      = 32 // characters in a conversation's name, as in a room's
	maxConversations = 8  // engines one app runs, each with its own socket, swarm and two stores
	maxSmallFile     = 4096
	maxStateFile     = 16 << 20 // conversation.json, members.json and a pin token: far above what the app writes
)

// oldLayoutNames are the files a data folder of step 5 or older held at its top, for its one conversation.
var oldLayoutNames = []string{"identity.key", "conversation.json", "members.json", "directory-log", "store", "invites", "pin-token"}

var (
	// ErrOldLayout matches the *DataDirError of a data folder of step 5 or older (Layout 1).
	ErrOldLayout            = errors.New("data folder made by Kolo Chat step 5 or older")
	ErrUnknownConversation  = errors.New("That conversation is not on this computer.")
	ErrTooManyConversations = errors.New("You have the most conversations this app keeps.")
	ErrNoPins               = errors.New("Start Kolo Chat with -pin-key to start a conversation. You can still join one with an invite.")
	ErrInvalidName          = errors.New("That name cannot be used")

	errNotStarted = errors.New("not started")
	errNotRegular = errors.New("not a regular file of a size this app writes")
)

// DataDirError is returned for a folder this version cannot use, and nothing in it is changed. Layout 1 is a data
// folder of step 5 or older, which held one conversation; Layout 2 one of step 6, whose conversations have no shared
// member list; another Layout is one of another version (-1: its layout.json is damaged), and Open one that other
// users can open. Without either, it is a conversation's folder that lost its conversation.json. Dir is that folder.
type DataDirError struct {
	Dir    string `json:"dir"`
	Layout int    `json:"layout,omitempty"`
	Open   bool   `json:"open,omitempty"`
}

func (e *DataDirError) Error() string {
	switch {
	case e.Open:
		return "The data folder " + e.Dir + " is open to other users, who could change what Kolo Chat keeps in it. " +
			"Run chmod 700 on it, or start with another -data. Nothing in the folder is changed."
	case e.Layout == 1:
		return "The data folder " + e.Dir + " holds one conversation of Kolo Chat step 5 or older, and this version keeps " +
			"each conversation in a folder of its own. Quit Kolo Chat, move or rename that folder (nothing in it is " +
			"deleted), then open Kolo Chat again: start a new conversation, or paste a new invite."
	case e.Layout == 2:
		return "The data folder " + e.Dir + " was made by Kolo Chat step 6, whose conversations have no shared member " +
			"list. Quit Kolo Chat, move or rename that folder (nothing in it is deleted), then open Kolo Chat again: start " +
			"a new conversation, or paste a new invite."
	case e.Layout == 0:
		return "The folder " + e.Dir + " holds a conversation whose conversation.json is gone. Quit Kolo Chat, move or " +
			"rename that folder (nothing in it is deleted), then open Kolo Chat again and paste a new invite."
	}
	return fmt.Sprintf("The data folder %s was made by another version of Kolo Chat (layout %d). Use that version, "+
		"or start with another -data. Nothing in the folder is changed.", e.Dir, e.Layout)
}

func (e *DataDirError) Is(target error) bool { return e.Layout == 1 && target == ErrOldLayout }

// localInfo is local.json: the name only we see, and when the conversation was added here, in Unix seconds.
type localInfo struct {
	Name    string `json:"name"`
	Created int64  `json:"created"`
}

// openLayout checks the data folder's layout.json, or writes it in a new folder. A folder of step 5 or older, or of
// another layout, is refused and nothing in it is changed: there is no migration.
func openLayout(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := privateDir(root); err != nil {
		return err
	}
	data, err := readSmall(filepath.Join(root, layoutFile))
	switch {
	case err == nil:
		var l struct {
			Layout int `json:"layout"`
		}
		if json.Unmarshal(data, &l) != nil || l.Layout < 2 { // no version wrote a layout below 2
			return &DataDirError{Dir: root, Layout: -1}
		}
		if l.Layout != layoutVersion {
			return &DataDirError{Dir: root, Layout: l.Layout}
		}
		if err := os.MkdirAll(filepath.Join(root, conversationDir), 0o700); err != nil { // deleted by hand: start empty
			return err
		}
		return privateDir(filepath.Join(root, conversationDir))
	case !errors.Is(err, fs.ErrNotExist): // a folder or a file we cannot read: no version wrote it
		return &DataDirError{Dir: root, Layout: -1}
	}
	for _, name := range oldLayoutNames {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
			return &DataDirError{Dir: root, Layout: 1}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, conversationDir), 0o700); err != nil {
		return err
	}
	if err := privateDir(filepath.Join(root, conversationDir)); err != nil {
		return err
	}
	return writeJSON(filepath.Join(root, layoutFile), map[string]int{"layout": layoutVersion})
}

// privateDir refuses a folder that other users can open, who could list, rename or replace what the app keeps
// there. Windows has no such mode bits; there the folder's place in the user's profile protects it.
func privateDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return &DataDirError{Dir: dir, Open: true}
	}
	return nil
}

// readSmall reads a regular file of at most maxSmallFile bytes.
func readSmall(path string) ([]byte, error) { return readFile(path, maxSmallFile) }

// readFile reads a regular file of at most limit bytes. The path is checked before the open, which does not wait as a
// FIFO's would, and the opened file again, so a link, a device or a FIFO is never read and a start never hangs on one.
func readFile(path string, limit int) ([]byte, error) {
	if fi, err := os.Lstat(path); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err == nil && len(b) > limit {
		err = errNotRegular
	}
	return b, err
}

// joinFiles are what a join that did not finish leaves in its folder.
var joinFiles = []string{"identity.key", localFile, "store", "braid", "invites", "directory-log"}

// onlyJoinFiles refuses a folder without conversation.json that holds anything a join that did not finish never makes,
// such as members.json or a pin token, or a kolo-id without the two stores its join makes with it: such a kolo-id may
// be on a member list. The engine then checks the stores.
func onlyJoinFiles(dir string) error {
	items, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	has := map[string]bool{}
	for _, it := range items {
		if !slices.Contains(joinFiles, it.Name()) {
			return &DataDirError{Dir: dir}
		}
		has[it.Name()] = true
	}
	if has["identity.key"] && !(has["store"] && has["braid"]) {
		return &DataDirError{Dir: dir}
	}
	return nil
}

// listConversations lists the conversation folders, oldest first. A folder without a conversation.json is a join that
// did not finish, or a member's folder that lost the file, which its engine refuses.
func listConversations(root string) ([]*convEntry, error) {
	dir := filepath.Join(root, conversationDir)
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var all []*convEntry
	for _, it := range items {
		if !it.IsDir() || !validID(it.Name()) {
			log.Printf("%s/%q: not a conversation, ignored", conversationDir, it.Name())
			continue
		}
		e := &convEntry{id: it.Name(), dir: filepath.Join(dir, it.Name())}
		e.local = loadLocal(e.dir)
		_, err := os.Lstat(filepath.Join(e.dir, "conversation.json"))
		e.joined = err == nil
		all = append(all, e)
	}
	slices.SortFunc(all, compareEntries)
	return all, nil
}

func compareEntries(x, y *convEntry) int {
	return cmp.Or(cmp.Compare(x.local.Created, y.local.Created), strings.Compare(x.id, y.id))
}

// loadLocal reads local.json. A damaged or missing one gives the name "Conversation", and the conversation opens.
func loadLocal(dir string) localInfo {
	var l localInfo
	data, err := readSmall(filepath.Join(dir, localFile))
	if err == nil {
		err = json.Unmarshal(data, &l)
	}
	if err != nil {
		return localInfo{Name: defaultConvName}
	}
	if l.Name, err = convName(l.Name, defaultConvName); err != nil {
		l.Name = defaultConvName
	}
	return l
}

// convName checks a conversation's name: plain text of 1 to maxConvName characters. An empty one is def, or refused
// when def is empty.
func convName(name, def string) (string, error) {
	if name = strings.TrimSpace(name); name == "" {
		name = def
	}
	if name == "" || utf8.RuneCountInString(name) > maxConvName || !plainText(name) {
		return "", fmt.Errorf("%w: a name has 1 to %d characters, with no control characters.", ErrInvalidName, maxConvName)
	}
	return name, nil
}

// validID says whether id is one the app makes: 32 lowercase hex characters.
func validID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

// writeJSON replaces the file at path with v in one step, mode 0600.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ConversationInfo is one conversation as the window lists it. Me is our kolo-id in it, Joined is false for the
// join that waits for an invite's answer, and Error says why it did not start, with the Folder it is in.
type ConversationInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Me     string `json:"me"`
	Joined bool   `json:"joined"`
	Admin  bool   `json:"admin"`
	Error  string `json:"error"`
	Folder string `json:"folder"`
	// Network is "public" or "own" when the conversation did not start because no entry node of that network answered.
	Network string `json:"network,omitempty"`
}

// AppInfo is what Start returns. CanCreate says the app has a pin, which a new conversation needs, and Network which
// network the app joins.
type AppInfo struct {
	Conversations    []ConversationInfo `json:"conversations"`
	MaxConversations int                `json:"maxConversations"`
	CanCreate        bool               `json:"canCreate"`
	Network          NetworkInfo        `json:"network"`
}

// ConvEvent is the payload of every chat event: the conversation it is about, and the engine's payload.
type ConvEvent struct {
	Conv string `json:"conv"`
	Data any    `json:"data"`
}

// convEntry is one conversation folder and its engine, nil when it did not start.
type convEntry struct {
	id, dir string
	local   localInfo
	joined  bool
	chat    *ChatService
	me      string
	err     error
}

// Conversations is the window's service: it keeps one engine per conversation folder and routes every call by ID.
type Conversations struct {
	cfg  Config
	emit func(name string, data any)

	opMu sync.Mutex // one Start, Create, Join or ServiceShutdown at a time
	set  settings
	root string

	mu      sync.Mutex // guards the fields below
	started bool
	convs   map[string]*convEntry
}

// NewConversations returns the service that reads cfg when it starts and tells the window what happens through emit.
func NewConversations(cfg Config, emit func(name string, data any)) *Conversations {
	return &Conversations{cfg: cfg, emit: emit}
}

// engineConfig is the configuration of one engine: the app's, in the conversation's folder. The key, the pin and
// the token are only for the -conversation-key conversation; every other one reads its own conversation.json.
func (a *Conversations) engineConfig(dir string, configured bool) Config {
	cfg := a.cfg
	cfg.DataDir = dir
	if !configured {
		cfg.ConversationKey, cfg.PinAddr, cfg.PinKey, cfg.PinToken = "", "", "", ""
	}
	return cfg
}

// startEngine starts the conversation's engine, whose events go to the window tagged with the conversation's ID.
func (a *Conversations) startEngine(e *convEntry, cfg Config) {
	c := NewChatService(cfg, func(name string, data any) { a.emit(name, ConvEvent{Conv: e.id, Data: data}) })
	if e.me, e.err = c.Start(); e.err == nil {
		e.chat, e.joined = c, c.Joined()
	} else {
		log.Printf("conversation %s did not start: %v", e.id, e.err)
	}
}

func (a *Conversations) add(e *convEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.convs[e.id] = e
}

// Start opens the data folder and starts every conversation in it, each on its own. It fails only for a bad
// setting (*ConfigError) or a data folder it cannot use (*DataDirError): a conversation that does not start
// shows its Error in the list, and the others run.
func (a *Conversations) Start() (AppInfo, error) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if a.isStarted() {
		return a.info(), nil
	}
	set, err := a.cfg.resolve()
	if err != nil {
		return AppInfo{}, err
	}
	if err := openLayout(set.DataDir); err != nil {
		return AppInfo{}, err
	}
	entries, err := listConversations(set.DataDir)
	if err != nil {
		return AppInfo{}, err
	}
	a.set, a.root = set, set.DataDir
	configured, err := a.findConfigured(entries)
	if err != nil {
		return AppInfo{}, err
	}
	isNew := configured != nil && !slices.Contains(entries, configured)
	if isNew {
		entries = append(entries, configured)
	}
	if set.fixedPort() && len(entries) > 1 {
		if isNew {
			a.dropFolder(configured)
		}
		return AppInfo{}, &ConfigError{Setting: "listen", Err: errFixedPort}
	}

	a.mu.Lock()
	a.started, a.convs = true, map[string]*convEntry{}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range entries {
		if !e.joined {
			if e.err = onlyJoinFiles(e.dir); e.err != nil { // left as it is, with its row
				continue
			}
		}
		wg.Go(func() { a.startEngine(e, a.engineConfig(e.dir, e == configured)) })
	}
	wg.Wait()
	for _, e := range entries {
		if !e.joined && e.chat != nil { // a join that did not finish: its engine found nothing of a member
			log.Printf("%s/%s: a join that did not finish, removed", conversationDir, e.id)
			if err := e.chat.ServiceShutdown(); err != nil {
				log.Print("join: ", err)
			}
			a.dropFolder(e)
			continue
		}
		a.add(e)
	}
	if isNew && configured.err != nil { // a bad setting, such as a -pin-token, as before there were several
		a.dropFolder(configured)
		a.shutdown()
		return AppInfo{}, configured.err
	}
	a.emitList()
	return a.info(), nil
}

// findConfigured returns the conversation that -conversation-key names: the folder whose conversation.json holds
// the key, or a new folder for it. It is nil without the setting.
func (a *Conversations) findConfigured(entries []*convEntry) (*convEntry, error) {
	if !a.set.inConversation {
		return nil, nil
	}
	for _, e := range entries {
		if v, found, err := loadConversation(e.dir); err == nil && found && slices.Equal(v.Key, a.set.conversationKey[:]) {
			return e, nil
		}
	}
	return a.newFolder(defaultConvName)
}

// Create starts a new conversation that we own, with a key from crypto/rand and the app's pin. name is only ours.
func (a *Conversations) Create(name string) (ConversationInfo, error) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	name, err := convName(name, defaultConvName)
	if err != nil {
		return ConversationInfo{}, err
	}
	if err := a.hasRoom(); err != nil {
		return ConversationInfo{}, err
	}
	if len(a.set.pinKey) == 0 {
		return ConversationInfo{}, ErrNoPins
	}
	e, err := a.newFolder(name)
	if err != nil {
		return ConversationInfo{}, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		a.dropFolder(e)
		return ConversationInfo{}, err
	}
	cfg := a.engineConfig(e.dir, false)
	cfg.ConversationKey, cfg.PinAddr, cfg.PinKey = hex.EncodeToString(key), a.cfg.PinAddr, a.cfg.PinKey
	clear(key)
	if a.startEngine(e, cfg); e.err != nil {
		a.dropFolder(e)
		return ConversationInfo{}, e.err
	}
	a.add(e)
	a.emitList()
	return a.infoOf(e), nil
}

// newFolder makes the folder of a new conversation, with a random ID that has nothing to do with its key.
func (a *Conversations) newFolder(name string) (*convEntry, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b)
	e := &convEntry{id: id, dir: filepath.Join(a.root, conversationDir, id), local: localInfo{Name: name, Created: time.Now().Unix()}}
	if err := os.Mkdir(e.dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(e.dir, localFile), e.local); err != nil {
		os.RemoveAll(e.dir)
		return nil, err
	}
	return e, nil
}

// dropFolder removes the folder of a conversation that failed before it had a conversation.json: nothing in it is
// worth keeping.
func (a *Conversations) dropFolder(e *convEntry) {
	if _, err := os.Lstat(filepath.Join(e.dir, "conversation.json")); errors.Is(err, fs.ErrNotExist) {
		os.RemoveAll(e.dir)
	}
}

// Join uses an invite code in a new folder: each invite is a conversation of its own, with a new kolo-id. A Join
// that fails or is cancelled removes that folder, kolo-id included: an admin may have admitted that key already, and
// the braid admits a key once, so the next Join starts with a new one.
func (a *Conversations) Join(code, name string) (ConversationInfo, error) {
	name, err := convName(name, defaultConvName)
	if err != nil {
		return ConversationInfo{}, err
	}
	a.opMu.Lock()
	e, err := a.pendingJoin()
	a.opMu.Unlock()
	if err != nil {
		return ConversationInfo{}, err
	}
	if err := e.chat.Join(code); err != nil {
		a.opMu.Lock()
		a.discardJoin(e)
		a.opMu.Unlock()
		return ConversationInfo{}, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	a.mu.Lock()
	e.local.Name, e.joined = name, true
	local := e.local
	a.mu.Unlock()
	if err := writeJSON(filepath.Join(e.dir, localFile), local); err != nil {
		log.Print("conversation name: ", err) // the conversation works, and shows as "Conversation" after a restart
	}
	a.emitList()
	return a.infoOf(e), nil
}

// pendingJoin returns the conversation of the join in progress, or a new one made and started for this join.
func (a *Conversations) pendingJoin() (*convEntry, error) {
	a.mu.Lock()
	var pending *convEntry
	for _, e := range a.convs {
		if !e.joined && e.chat != nil {
			pending = e
		}
	}
	a.mu.Unlock()
	if pending != nil {
		return pending, nil // its Join says that a join is in progress
	}
	if err := a.hasRoom(); err != nil {
		return nil, err
	}
	e, err := a.newFolder(defaultConvName)
	if err != nil {
		return nil, err
	}
	if a.startEngine(e, a.engineConfig(e.dir, false)); e.err != nil {
		a.dropFolder(e)
		return nil, e.err
	}
	a.add(e)
	a.emitList() // the window shows the kolo-id the join uses, for an invite that needs approval
	return e, nil
}

// discardJoin stops the engine of a Join that failed and removes its folder, which never held a conversation.
func (a *Conversations) discardJoin(e *convEntry) {
	a.mu.Lock()
	if a.convs[e.id] != e {
		a.mu.Unlock()
		return
	}
	delete(a.convs, e.id)
	a.mu.Unlock()
	if err := e.chat.ServiceShutdown(); err != nil {
		log.Print("join: ", err)
	}
	a.dropFolder(e)
	a.emitList()
}

// CancelJoin stops the Join that waits, for the inviter or for its answer.
func (a *Conversations) CancelJoin() {
	a.mu.Lock()
	var c *ChatService
	for _, e := range a.convs {
		if !e.joined && e.chat != nil {
			c = e.chat
		}
	}
	a.mu.Unlock()
	if c != nil {
		c.CancelJoin()
	}
}

// hasRoom says whether one more conversation may be added: the bound counts the join that waits and the ones that
// did not start, and a fixed -listen port holds one conversation only.
func (a *Conversations) hasRoom() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch n := len(a.convs); {
	case !a.started:
		return errNotStarted
	case n >= maxConversations:
		return ErrTooManyConversations
	case n >= 1 && a.set.fixedPort():
		return &ConfigError{Setting: "listen", Err: errFixedPort}
	}
	return nil
}

// List returns the conversations in the order they were added, never nil.
func (a *Conversations) List() []ConversationInfo {
	a.mu.Lock()
	entries := make([]convEntry, 0, len(a.convs))
	for _, e := range a.convs {
		entries = append(entries, *e)
	}
	a.mu.Unlock()
	slices.SortFunc(entries, func(x, y convEntry) int { return compareEntries(&x, &y) })
	out := make([]ConversationInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.info())
	}
	return out
}

func (e convEntry) info() ConversationInfo {
	info := ConversationInfo{ID: e.id, Name: e.local.Name, Me: e.me, Joined: e.joined}
	if e.chat != nil {
		info.Admin = e.chat.IsAdmin()
	}
	if e.err != nil {
		info.Error, info.Folder = e.err.Error(), e.dir
		if ne := (*NetworkError)(nil); errors.As(e.err, &ne) { // nothing is wrong with the folder
			info.Folder, info.Network = "", ne.Network.Mode
		}
	}
	return info
}

func (a *Conversations) info() AppInfo {
	return AppInfo{Conversations: a.List(), MaxConversations: maxConversations, CanCreate: len(a.set.pinKey) > 0, Network: a.set.network()}
}

func (a *Conversations) infoOf(e *convEntry) ConversationInfo {
	a.mu.Lock()
	snapshot := *e
	a.mu.Unlock()
	return snapshot.info()
}

func (a *Conversations) emitList() { a.emit("chat:conversations", a.List()) }

// Send, Members and the methods down to Deny are the engine's, for the conversation conv.

func (a *Conversations) Send(conv, room, text string) error {
	return call(a, conv, func(c *ChatService) error { return c.Send(room, text) })
}

func (a *Conversations) Members(conv string) ([]Member, error) {
	return get(a, conv, (*ChatService).Members)
}

func (a *Conversations) Rooms(conv string) ([]string, error) {
	return get(a, conv, (*ChatService).Rooms)
}

func (a *Conversations) IsAdmin(conv string) (bool, error) {
	return get(a, conv, (*ChatService).IsAdmin)
}

func (a *Conversations) SetProfile(conv, name, avatar string) error {
	return call(a, conv, func(c *ChatService) error { return c.SetProfile(name, avatar) })
}

func (a *Conversations) AddRoom(conv, name string) error {
	return call(a, conv, func(c *ChatService) error { return c.AddRoom(name) })
}

func (a *Conversations) RemoveMember(conv, key string) error {
	return call(a, conv, func(c *ChatService) error { return c.RemoveMember(key) })
}

func (a *Conversations) CreateInvite(conv string, opts InviteOptions) (InviteInfo, error) {
	c, err := a.engine(conv)
	if err != nil {
		return InviteInfo{}, err
	}
	return c.CreateInvite(opts)
}

func (a *Conversations) Invites(conv string) ([]InviteInfo, error) {
	c, err := a.engine(conv)
	if err != nil {
		return nil, err
	}
	return c.Invites()
}

func (a *Conversations) Revoke(conv, id string) error {
	return call(a, conv, func(c *ChatService) error { return c.Revoke(id) })
}

func (a *Conversations) Approve(conv string, request uint64, admin bool) error {
	return call(a, conv, func(c *ChatService) error { return c.Approve(request, admin) })
}

func (a *Conversations) Deny(conv string, request uint64) error {
	return call(a, conv, func(c *ChatService) error { return c.Deny(request) })
}

// engine returns the running engine of conv. Only IDs the app listed or made are in the map, so no path is ever
// built from what the window sends.
func (a *Conversations) engine(conv string) (*ChatService, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.convs[conv]; e != nil && e.chat != nil {
		return e.chat, nil
	}
	return nil, ErrUnknownConversation
}

func call(a *Conversations, conv string, f func(*ChatService) error) error {
	c, err := a.engine(conv)
	if err != nil {
		return err
	}
	return f(c)
}

func get[T any](a *Conversations, conv string, f func(*ChatService) T) (T, error) {
	c, err := a.engine(conv)
	if err != nil {
		var zero T
		return zero, err
	}
	return f(c), nil
}

// ServiceShutdown stops every conversation. Wails calls it when the app quits.
func (a *Conversations) ServiceShutdown() error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	return a.shutdown()
}

func (a *Conversations) shutdown() error {
	a.mu.Lock()
	engines := a.running()
	a.started, a.convs = false, nil
	a.mu.Unlock()
	errs := make([]error, len(engines))
	var wg sync.WaitGroup
	for i, c := range engines {
		wg.Go(func() { errs[i] = c.ServiceShutdown() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// running returns the engines that run. The caller holds mu.
func (a *Conversations) running() []*ChatService {
	var out []*ChatService
	for _, e := range a.convs {
		if e.chat != nil {
			out = append(out, e.chat)
		}
	}
	return out
}

func (a *Conversations) isStarted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started
}
