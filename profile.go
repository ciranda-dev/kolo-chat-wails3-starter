package main

// Profiles, kept with kolo-kv.
//
// Every member has a second log in its store, the directory, and kolo-kv turns it into a key-value store that
// only that member writes. It holds one key, "profile": the member's name and avatar. Nobody writes another
// member's profile because nobody can write another member's log: a log has one signing key. The rooms and the
// members are shared state, which the braid keeps (see braid.go).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	koloid "github.com/ciranda-dev/kolo-id"
	kolokv "github.com/ciranda-dev/kolo-kv"
	kololog "github.com/ciranda-dev/kolo-log"
	kolostore "github.com/ciranda-dev/kolo-store"
	kolosync "github.com/ciranda-dev/kolo-sync"
)

const (
	defaultRoom = "general" // every conversation has it, and nobody has to write it
	keyProfile  = "profile"

	maxName     = 40   // characters in a display name
	maxAvatar   = 16   // characters in an avatar reference
	maxRoomName = 32   // characters in a room name
	maxRooms    = 64   // rooms admins may add
	maxMembers  = 64   // the braid's member bound, kolo-braid's default: one node's peer budget
	maxValue    = 4096 // bytes of a sealed value a reader accepts
)

// Errors of profiles and rooms. ErrRoomExists, ErrTooManyRooms and ErrUnknownRoom come as they are, and the other
// two come wrapped with the reason, so errors.Is tells the kind and the message is the sentence to show.
var (
	ErrInvalidProfile = errors.New("That profile cannot be used")
	ErrInvalidRoom    = errors.New("That room name cannot be used")
	ErrRoomExists     = errors.New("That room already exists.")
	ErrTooManyRooms   = errors.New("A conversation has at most 64 rooms.")
	ErrUnknownRoom    = errors.New("That room is not in the room list.")

	errNoDirectory = errors.New("the directory of that member is not known yet")
)

// Profile is what a member says about itself. Avatar is a reference, not an image: a short text such as an
// emoji or an id of the window's own, with no URL characters.
type Profile struct {
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

// Member is a member of the conversation as the window shows it. Name and Avatar are empty until the member's
// profile has been read. Admin may invite, approve, remove and add rooms; Seat is the member who settles changes to
// the lists (the founder), which nobody can remove. Pending says its admission is not settled yet.
type Member struct {
	Key     string `json:"key"` // the member's kolo-id, hex
	Name    string `json:"name"`
	Avatar  string `json:"avatar"`
	Admin   bool   `json:"admin"`
	Seat    bool   `json:"seat"`
	Pending bool   `json:"pending"`
}

// directoryView is what the window was told last: the payloads of "chat:profiles" and "chat:rooms".
type directoryView struct {
	Members []Member
	Rooms   []string
}

// directory is the read-only copy of another member's directory: its replica in our store, and the kolo-kv
// reader on it.
type directory struct {
	rep *kolostore.Replica
	db  *kolokv.DB
}

// SetProfile publishes our name and avatar in our directory. Every peer that is connected is told to pull it,
// and the pin gets it with the next push.
func (c *ChatService) SetProfile(name, avatar string) error {
	p := Profile{Name: strings.TrimSpace(name), Avatar: strings.TrimSpace(avatar)}
	if err := validProfile(p); err != nil {
		return err
	}
	if err := c.writeProfile(p); err != nil {
		return err
	}
	c.directoryChanged()
	return nil
}

// writeProfile puts our profile in our directory, sealed with the conversation key.
func (c *ChatService) writeProfile(p Profile) error {
	if !c.isActive() {
		return ErrNotInConversation
	}
	sealed, err := c.sealValue(keyProfile, p)
	if err != nil {
		return err
	}
	c.dirMu.Lock()
	defer c.dirMu.Unlock()
	return c.dir.Put([]byte(keyProfile), sealed)
}

// profileOf reads the profile in a member's own directory, and nothing else of it. It returns the zero Profile
// when there is none, or when it is not one we would have written.
func (c *ChatService) profileOf(author string) (p Profile) {
	db := c.readerOf(author)
	pub, err := hex.DecodeString(author)
	if db == nil || err != nil {
		return p
	}
	sealed, err := db.Get([]byte(keyProfile))
	if err != nil || c.openValue(pub, keyProfile, sealed, &p) != nil || validProfile(p) != nil {
		return Profile{}
	}
	return p
}

// readerOf returns the store of a member's directory: ours, which we write, or the read-only copy of another's.
func (c *ChatService) readerOf(author string) *kolokv.DB {
	if author == hex.EncodeToString(c.pub) {
		return c.dir
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.dirs[author]; ok {
		return d.db
	}
	return nil
}

// sealValue and openValue protect a value of the directory with the conversation key, bound to the author and
// to the key it is stored under, so the pin and anyone with entry keys but not the conversation key cannot read
// it, and a value cannot be moved to another key.
func (c *ChatService) sealValue(key string, v any) ([]byte, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, valueAD(c.pub, key)), nil
}

func (c *ChatService) openValue(author ed25519.PublicKey, key string, sealed []byte, out any) error {
	n := c.aead.NonceSize()
	if len(sealed) < n || len(sealed) > maxValue {
		return errors.New("sealed value has a bad size")
	}
	plain, err := c.aead.Open(nil, sealed[:n], sealed[n:], valueAD(author, key))
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, out)
}

func valueAD(author ed25519.PublicKey, key string) []byte {
	return append(append([]byte("kolochat/directory/v1"), author...), key...)
}

// directory returns the read-only copy of a member's directory, opening it the first time: a replica of its
// log in our store, from the header the member sent, and a kolo-kv reader on it.
func (c *ChatService) directory(author string) (*directory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.dirs[author]; ok {
		return d, nil
	}
	m, ok := c.members[author]
	if !ok || len(m.DirHeader) == 0 {
		return nil, errNoDirectory
	}
	h, err := kololog.DecodeLogHeader(m.DirHeader)
	if err != nil {
		return nil, err
	}
	rep, err := c.store.OpenReplica(h.LogKey, h.LogID, m.DirHeader)
	if err != nil {
		return nil, err
	}
	db, err := kolokv.OpenReader(rep)
	if err != nil {
		rep.Close()
		return nil, err
	}
	d := &directory{rep: rep, db: db}
	c.dirs[author] = d
	return d, nil
}

// openDirectories opens the copies of the directories of the members we know, from what the last run kept, so
// the window has names before anyone is connected.
func (c *ChatService) openDirectories() {
	for _, author := range c.authors() {
		if _, err := c.directory(author); err != nil && !errors.Is(err, errNoDirectory) {
			log.Printf("directory of %s: %v", author[:8], err)
		}
	}
	c.publishDirectory()
}

func (c *ChatService) closeDirectories() {
	c.mu.Lock()
	dirs := c.dirs
	c.dirs = map[string]*directory{}
	c.mu.Unlock()
	for _, d := range dirs {
		d.db.Close() // also closes its replica
	}
	if c.dir != nil {
		c.dir.Close() // also closes the directory log
	}
}

// pullDirectory brings a member's directory up to date from conn, which is the member itself (with entry keys)
// or the pin (ciphertext only), and shows what changed. A reader replays the entries it has keys for, up to the
// first one it does not: from the pin that can be none of the new ones, so the old profile stays until the
// member itself is reachable.
func (c *ChatService) pullDirectory(ctx context.Context, conn kolosync.Conn, author string, fromPin bool) error {
	d, err := c.directory(author)
	if err != nil {
		return err
	}
	pull := kolosync.Pull
	if fromPin {
		pull = kolosync.PullCiphertext
	}
	c.pullMu.Lock()
	err = pull(ctx, conn, d.rep)
	c.pullMu.Unlock()
	if rerr := d.db.Refresh(); rerr != nil {
		err = errors.Join(err, rerr)
	}
	c.publishDirectory()
	return err
}

// syncDirectory pulls a peer's directory from the peer itself, if it is on the roster.
func (c *ChatService) syncDirectory(conn kolosync.Conn, author string) {
	if !c.isAccepted(author) {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, pinTimeout)
	defer cancel()
	err := c.pullDirectory(ctx, conn, author, false)
	if err != nil && !errors.Is(err, errNoDirectory) && c.ctx.Err() == nil {
		log.Print("sync of directory: ", err)
	}
}

// onDirectory handles a peer saying that its directory changed: it pulls it. The peer decides nothing else.
func (c *ChatService) onDirectory(l *link) {
	c.wg.Go(func() {
		if conn, ok := c.afterPin(l); ok {
			c.syncDirectory(conn, l.key)
		}
	})
}

// directoryChanged is what follows a write to our directory: the window sees it, the peers that are connected
// are told to pull it, and the pin gets it.
func (c *ChatService) directoryChanged() {
	c.publishDirectory()
	for _, l := range c.registered() {
		c.wg.Go(func() {
			if err := c.sendTo(l, wireMsg{Kind: "dir"}); err != nil {
				log.Print("directory notice: ", err)
			}
		})
	}
	c.pushSoon()
}

// publishDirectory tells the window the members ("chat:profiles") and the rooms ("chat:rooms") if they are not
// what it was told last.
func (c *ChatService) publishDirectory() {
	c.pubMu.Lock()
	defer c.pubMu.Unlock()
	view := directoryView{Members: c.Members(), Rooms: c.Rooms()}
	if !slices.Equal(view.Members, c.lastView.Members) {
		c.emit("chat:profiles", view.Members)
	}
	if !slices.Equal(view.Rooms, c.lastView.Rooms) {
		c.emit("chat:rooms", view.Rooms)
	}
	c.lastView = view
}

// checkDirHeader checks the directory header in a hello: it is the sender's own signed header, for a kolo-kv
// log (the bytes encoding) that is not its messages log. A hello without one, from a peer that has no
// directory, is fine.
func checkDirHeader(pub ed25519.PublicKey, raw []byte, messages kololog.LogHeader) error {
	if len(raw) == 0 {
		return nil
	}
	h, err := kololog.DecodeLogHeader(raw)
	if err == nil {
		err = kololog.VerifyLogHeader(pub, h)
	}
	if err == nil && (h.LogID == messages.LogID || h.Encoding != kololog.Bytes) {
		err = errors.New("not a directory log")
	}
	if err != nil {
		return fmt.Errorf("directory header: %w", err)
	}
	return nil
}

func validProfile(p Profile) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s.", ErrInvalidProfile, why) }
	switch {
	case p.Name == "" || utf8.RuneCountInString(p.Name) > maxName:
		return bad(fmt.Sprintf("a name has 1 to %d characters", maxName))
	case !plainText(p.Name) || !plainText(p.Avatar):
		return bad("it has control characters or is not valid text")
	case utf8.RuneCountInString(p.Avatar) > maxAvatar || strings.ContainsAny(p.Avatar, `/:\<>`):
		return bad(fmt.Sprintf("an avatar is a short reference of up to %d characters, such as an emoji, not a URL", maxAvatar))
	}
	return nil
}

func validRoomName(name string) error {
	switch {
	case name == "" || utf8.RuneCountInString(name) > maxRoomName:
		return fmt.Errorf("%w: a name has 1 to %d characters.", ErrInvalidRoom, maxRoomName)
	case !plainText(name) || strings.TrimSpace(name) != name:
		return fmt.Errorf("%w: it has control characters, or spaces at its ends.", ErrInvalidRoom)
	}
	return nil
}

// plainText says that s is valid UTF-8 with no control characters.
func plainText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// openOwnLogs opens the two logs we write, or creates them the first time: the messages, and the directory.
// Both are ours and look alike, so the directory's ID is kept in the data directory. A store from step 4 has
// the messages log only, and gets a directory.
func (c *ChatService) openOwnLogs() (err error) {
	own, err := ownLogs(c.store, c.id)
	if err != nil {
		return err
	}
	dirID, saved := loadDirectoryID(c.set.DataDir)
	saved = saved && slices.Contains(own, dirID)
	messages := slices.DeleteFunc(own, func(id kololog.LogID) bool { return saved && id == dirID })
	switch {
	case len(messages) > 1:
		return errors.New("the store holds two logs of ours and does not say which one is the directory")
	case len(messages) == 1:
		c.log, err = c.store.Open(c.id, messages[0])
	default:
		c.log, err = c.store.Create(c.id)
	}
	if err != nil {
		return err
	}
	if saved {
		c.dirLog, err = c.store.Open(c.id, dirID)
	} else if c.dirLog, err = c.store.Create(c.id); err == nil {
		err = saveDirectoryID(c.set.DataDir, c.dirLog.ID())
	}
	if err != nil {
		return err
	}
	c.dir, err = kolokv.Open(c.dirLog) // from now on only the DB writes to the log
	return err
}

// ownLogs lists the logs in the store that are ours, not replicas of someone else's.
func ownLogs(st *kolostore.Store, id *koloid.Identity) (own []kololog.LogID, err error) {
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
			own = append(own, logID)
		}
	}
	return own, nil
}

func loadDirectoryID(dir string) (id kololog.LogID, ok bool) {
	data, err := readSmall(filepath.Join(dir, "directory-log"))
	if b, err2 := hex.DecodeString(strings.TrimSpace(string(data))); err == nil && err2 == nil && len(b) == len(id) {
		copy(id[:], b)
		return id, true
	}
	return id, false
}

// saveDirectoryID writes directory-log, mode 0600, in one step.
func saveDirectoryID(dir string, id kololog.LogID) error {
	tmp := filepath.Join(dir, "directory-log.tmp")
	if err := os.WriteFile(tmp, []byte(id.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "directory-log"))
}
