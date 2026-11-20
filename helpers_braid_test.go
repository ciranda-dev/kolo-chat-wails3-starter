package main

import (
	"context"
	"crypto/ed25519"
	"testing"

	kolobraid "github.com/ciranda-dev/kolo-braid"
	koloid "github.com/ciranda-dev/kolo-id"
	kolostore "github.com/ciranda-dev/kolo-store"
)

// admitKey puts id on a's member list, with roles, and a strand nobody writes: a member that never connects.
func admitKey(t *testing.T, a *ChatService, id *koloid.Identity, roles kolobraid.Role) {
	t.Helper()
	j, err := kolobraid.NewJoin(t.Context(), id, kolobraid.Options{Backend: kolostore.NewMemory(), App: stateApp, Apply: applyState})
	if err != nil {
		t.Fatal(err)
	}
	req, err := j.Request(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), braidWrite)
	defer cancel()
	if _, err := a.braid.Admit(ctx, id.PublicKey(), req, &kolobraid.AdmitOptions{Roles: &roles}); err != nil {
		t.Fatal(err)
	}
}

// member starts a chat in dir that joins a's conversation with an invite a approves, as a member.
func (n network) member(t *testing.T, a *ChatService, dir string) (*ChatService, *events) {
	t.Helper()
	b, ev := start(t, n.joiner(dir))
	joinVia(t, a, b, false)
	return b, ev
}

// joinVia makes b a member of a's conversation through a new invite, approved by a as a member or as an admin.
func joinVia(t *testing.T, a, b *ChatService, admin bool) {
	t.Helper()
	code := mustCreate(t, a, InviteOptions{TTLSeconds: 3600}).Code
	if err := waitJoin(t, approveJoin(t, a, b.pub, joinAsync(b, code), admin)); err != nil {
		t.Fatal(err)
	}
}

// approveJoin approves on a the request of joiner (any request when joiner is nil) and returns done.
func approveJoin(t *testing.T, a *ChatService, joiner ed25519.PublicKey, done <-chan error, admin bool) <-chan error {
	t.Helper()
	if err := a.Approve(requestOf(t, a, joiner), admin); err != nil {
		t.Fatal(err)
	}
	return done
}

// requestOf waits for the request of joiner on a, any request when joiner is nil.
func requestOf(t *testing.T, a *ChatService, joiner ed25519.PublicKey) uint64 {
	t.Helper()
	var id uint64
	eventually(t, "the join request", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		for rid, r := range a.requests {
			if joiner == nil || r.ev.Joiner.Equal(joiner) {
				id = uint64(rid)
				return true
			}
		}
		return false
	})
	return id
}

// joinConv joins bob's app to the conversation of the engine alice with code, approved by alice as a member.
func joinConv(t *testing.T, alice *ChatService, bob *Conversations, code, name string) ConversationInfo {
	t.Helper()
	type result struct {
		info ConversationInfo
		err  error
	}
	got := make(chan result, 1)
	go func() {
		info, err := bob.Join(code, name)
		got <- result{info, err}
	}()
	if err := alice.Approve(requestOf(t, alice, nil), false); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.info
}
