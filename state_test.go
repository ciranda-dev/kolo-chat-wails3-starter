package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	kolobraid "github.com/ciranda-dev/kolo-braid"
	"github.com/ciranda-dev/kolo-braid/kolobraidtest"
	koloid "github.com/ciranda-dev/kolo-id"
	kolosecure "github.com/ciranda-dev/kolo-secure"
	kolostore "github.com/ciranda-dev/kolo-store"
)

var adminKey = bytes.Repeat([]byte{7}, 32)

func roomEntry(body []byte) kolobraid.Entry {
	return kolobraid.Entry{Writer: adminKey, Kind: kolobraid.KindData, Body: body}
}

// rawBody is a version 1 body with a length byte that need not fit.
func rawBody(version, op byte, n int, name string) []byte {
	return append([]byte{version, op, byte(n)}, name...)
}

// roomHistory adds rooms with duplicates in any case, bad names, the default room, unicode and the longest names.
func roomHistory() []kolobraid.Entry {
	var es []kolobraid.Entry
	for _, name := range []string{"design", "Design", "DESIGN", "dev", "general", "General", "café", "CAFÉ", "日本語",
		strings.Repeat("語", maxRoomName), strings.Repeat("x", maxRoomName+1), " lead", "a\x00b", "\xff\xfe", "", "dev"} {
		es = append(es, roomEntry(encodeAddRoom(name)))
	}
	return es
}

// S1: the room list is the same on every path, in every delivery order, whatever the batches.
func TestApplyStateIsDeterministic(t *testing.T) {
	es := roomHistory()
	kolobraidtest.CheckApply(t, applyState, es)
	for seed := range uint64(4) {
		if d, err := kolobraidtest.FindDivergence(applyState, es, 4, seed); d != nil || err != nil {
			t.Fatalf("seed %d: %+v, %v", seed, d, err)
		}
	}
}

// S1 and R1: a version 2 body, longer than any version 1 body and with bytes version 1 does not know, halts the old
// apply and is never rejected by it; positions before it apply alike.
func TestApplyStateHaltsOnALaterVersion(t *testing.T) {
	v2 := append([]byte{2, 9, 255}, bytes.Repeat([]byte{0xee}, 300)...)
	es := append(roomHistory(), roomEntry(v2), roomEntry(encodeAddRoom("after")))
	newer := func(tx *kolobraid.Tx, e *kolobraid.Entry) error {
		if e.Kind == kolobraid.KindData && len(e.Body) > 0 && e.Body[0] == 2 {
			return tx.Put([]byte("v2"), []byte{1})
		}
		return applyState(tx, e)
	}
	kolobraidtest.CheckUpgrade(t, applyState, newer, roomHistory(), len(roomHistory()))
	if _, err := kolobraidtest.FindDivergence(applyState, es, 2, 1); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("the old apply must halt on version 2, not reject it: %v", err)
	}
}

// S1b: every kind but data returns nil before it reads anything.
func TestApplyStateLeavesMembershipToTheBraid(t *testing.T) {
	for _, k := range []kolobraid.Kind{kolobraid.KindGenesis, kolobraid.KindAddMember, kolobraid.KindRemoveMember,
		kolobraid.KindSettlers, kolobraid.KindEvidence} {
		if err := applyState(nil, &kolobraid.Entry{Kind: k, Body: []byte{9}}); err != nil {
			t.Errorf("kind %d: %v", k, err)
		}
	}
}

// soloBraid is a braid with its founder alone, on memory, settling at once.
func soloBraid(t *testing.T, apply kolobraid.ApplyFunc) (*kolobraid.Braid, *koloid.Identity) {
	t.Helper()
	id, _ := koloid.Generate()
	b, err := kolobraid.Create(t.Context(), id, groupConfig, kolobraid.Options{Backend: kolostore.NewMemory(), App: stateApp, Apply: apply})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, id
}

// settledData waits until n data entries are settled and returns them in order.
func settledData(t *testing.T, b *kolobraid.Braid, n int) []kolobraid.Ordered {
	t.Helper()
	var got []kolobraid.Ordered
	eventually(t, "the entries to settle", func() bool {
		got = got[:0]
		for o, err := range b.Entries(0) {
			if err != nil {
				return false
			}
			if o.Kind == kolobraid.KindData && o.Settled {
				got = append(got, o)
			}
		}
		return len(got) >= n
	})
	return got
}

// S1b: the body layout is checked whole. Each malformed body is rejected on the braid, which goes on.
func TestApplyStateRejectsMalformedBodies(t *testing.T) {
	b, _ := soloBraid(t, applyState)
	bad := [][]byte{
		{},                                   // empty
		rawBody(0, opAddRoom, 3, "dev"),      // version 0
		rawBody(roomV1, 2, 3, "dev"),         // another op
		rawBody(roomV1, opAddRoom, 2, "dev"), // length one short
		rawBody(roomV1, opAddRoom, 4, "dev"), // length one long
		append(encodeAddRoom("dev"), 0),      // a trailing byte
		{roomV1, opAddRoom},                  // too short
		encodeAddRoom("\xff\xfe"),            // not UTF-8
	}
	ctx, cancel := context.WithTimeout(t.Context(), braidWrite)
	defer cancel()
	for _, body := range append(bad, encodeAddRoom("ok")) {
		if _, err := b.Append(ctx, body); err != nil {
			t.Fatal(err)
		}
	}
	got := settledData(t, b, len(bad)+1)
	for i, o := range got[:len(bad)] {
		if !o.Rejected {
			t.Errorf("body %d %x was applied", i, bad[i])
		}
	}
	if got[len(bad)].Rejected || b.Status().Halted != nil {
		t.Fatalf("a good body after bad ones: rejected %v, halted %v", got[len(bad)].Rejected, b.Status().Halted)
	}
	v := b.View()
	defer v.Close()
	if r := roomsOf(v); !slices.Equal(r, []string{"ok"}) {
		t.Fatalf("rooms %v", r)
	}
}

// S1: the room list keeps the first of each name in ASCII case, refuses the default room and bad names, keeps unicode
// as it is, and stops at maxRooms.
func TestApplyStateRoomList(t *testing.T) {
	b, _ := soloBraid(t, applyState)
	ctx, cancel := context.WithTimeout(t.Context(), braidWrite)
	defer cancel()
	es := roomHistory()
	for i := range maxRooms + 2 {
		es = append(es, roomEntry(encodeAddRoom("r"+strings.Repeat("0", i%3)+string(rune('a'+i/26))+string(rune('a'+i%26)))))
	}
	for _, e := range es {
		if _, err := b.Append(ctx, e.Body); err != nil {
			t.Fatal(err)
		}
	}
	settledData(t, b, len(es))
	v := b.View()
	rooms := roomsOf(v)
	v.Close()
	want := []string{"CAFÉ", "café", "design", "dev", "日本語", strings.Repeat("語", maxRoomName)}
	for _, w := range want {
		if !slices.Contains(rooms, w) {
			t.Errorf("%q is missing from %v", w, rooms)
		}
	}
	for _, no := range []string{"Design", "DESIGN", "general", "General", " lead", strings.Repeat("x", maxRoomName+1)} {
		if slices.Contains(rooms, no) {
			t.Errorf("%q is in the list", no)
		}
	}
	if len(rooms) != maxRooms || !slices.IsSorted(rooms) {
		t.Fatalf("%d rooms, sorted %v", len(rooms), slices.IsSorted(rooms))
	}
}

// braidPair is a founder braid and a member admitted with roles, synced over kolo-secure on 127.0.0.1.
func braidPair(t *testing.T, founder, member kolobraid.ApplyFunc, roles kolobraid.Role) (a, b *kolobraid.Braid) {
	t.Helper()
	a, idA := soloBraid(t, founder)
	idB, _ := koloid.Generate()
	b = joinBraid(t, a, idB, member, roles)
	connectBraids(t, a, idA, b, idB)
	return a, b
}

// joinBraid admits id to a with roles and returns its braid.
func joinBraid(t *testing.T, a *kolobraid.Braid, id *koloid.Identity, apply kolobraid.ApplyFunc, roles kolobraid.Role) *kolobraid.Braid {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), braidWrite)
	defer cancel()
	j, err := kolobraid.NewJoin(ctx, id, kolobraid.Options{Backend: kolostore.NewMemory(), App: stateApp, Apply: apply})
	if err != nil {
		t.Fatal(err)
	}
	req, err := j.Request(nil)
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := a.Admit(ctx, id.PublicKey(), req, &kolobraid.AdmitOptions{Roles: &roles})
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.Complete(ctx, welcome)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// connectBraids syncs a and b, whose identities are idA and idB, over one kolo-secure connection on 127.0.0.1.
func connectBraids(t *testing.T, a *kolobraid.Braid, idA *koloid.Identity, b *kolobraid.Braid, idB *koloid.Identity) {
	t.Helper()
	connectBraidsCtx(t, t.Context(), a, idA, b, idB)
}

// connectBraidsCtx is connectBraids until ctx ends, which cuts the connection.
func connectBraidsCtx(t *testing.T, ctx context.Context, a *kolobraid.Braid, idA *koloid.Identity, b *kolobraid.Braid, idB *koloid.Identity) {
	t.Helper()
	l, err := kolosecure.Listen(ctx, "127.0.0.1:0", idA, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		if c, err := l.Accept(ctx); err == nil {
			go a.ServeConn(ctx, c)
			<-ctx.Done()
			c.Close()
		}
	}()
	conn, err := kolosecure.Dial(ctx, l.Addr().String(), idB, idA.PublicKey(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() { <-ctx.Done(); conn.Close() }()
	go b.ServeConn(ctx, conn)
}

// S1b and S5: a member who is not an admin writes room entries past the app, of version 1 and 2: every peer rejects
// them, and nobody halts. An admin's entry still applies.
func TestNonAdminEntriesAreRejectedEverywhere(t *testing.T) {
	if testing.Short() {
		t.Skip("syncs two braids")
	}
	a, b := braidPair(t, applyState, applyState, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, body := range [][]byte{encodeAddRoom("mine"), {2, 1, 0}} {
		if _, err := b.Append(ctx, body); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Append(ctx, encodeAddRoom("ours")); err != nil {
		t.Fatal(err)
	}
	for _, br := range []*kolobraid.Braid{a, b} {
		for _, o := range settledData(t, br, 3) {
			if o.Rejected == bytes.Equal(o.Body, encodeAddRoom("ours")) {
				t.Fatalf("entry %x: rejected %v", o.Body, o.Rejected)
			}
		}
		v := br.View()
		rooms := roomsOf(v)
		v.Close()
		if !slices.Equal(rooms, []string{"ours"}) || br.Status().Halted != nil {
			t.Fatalf("rooms %v, halted %v", rooms, br.Status().Halted)
		}
	}
}

// L6: an app that a version 2 entry halted stops following the roster too: a member removed after it stays on its
// member list until the app is updated, which docs/shared-state.md and state.halted say.
func TestHaltedAppKeepsItsMemberList(t *testing.T) {
	if testing.Short() {
		t.Skip("syncs braids")
	}
	newer := func(tx *kolobraid.Tx, e *kolobraid.Entry) error {
		if e.Kind == kolobraid.KindData && len(e.Body) > 0 && e.Body[0] == 2 {
			return tx.Put([]byte("v2"), []byte{1})
		}
		return applyState(tx, e)
	}
	a, b := braidPair(t, newer, applyState, 0)
	idC, _ := koloid.Generate()
	joinBraid(t, a, idC, applyState, 0)
	eventually(t, "B to list C", func() bool { return listed(b, idC.PublicKey()) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := a.Append(ctx, []byte{2, 1, 0}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "B to halt", func() bool { return b.Status().Halted != nil })
	removal, err := a.RemoveMember(ctx, idC.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the founder to drop C", func() bool { return !listed(a, idC.PublicKey()) })
	eventually(t, "B to hold the removal", func() bool { return holds(b, removal.ID) })
	if !listed(b, idC.PublicKey()) {
		t.Fatal("the halted app learned of the removal: docs/shared-state.md and state.halted say it does not")
	}
}

func listed(b *kolobraid.Braid, key []byte) bool {
	v := b.View()
	defer v.Close()
	return slices.ContainsFunc(v.Members(), func(m kolobraid.Member) bool { return bytes.Equal(m.Key, key) })
}

// holds says whether b has the entry id in its order, applied or not.
func holds(b *kolobraid.Braid, id kolobraid.EntryID) bool {
	for o, err := range b.Entries(0) {
		if err != nil {
			return false
		}
		if o.ID == id {
			return true
		}
	}
	return false
}
