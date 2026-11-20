package main

// One connection for the chat and the braid.
//
// kolo-swarm keeps one connection per peer key, and the chat's messages, its kolo-sync server and the braid's ServeConn
// each must be the only one that accepts streams on it. So every stream starts with a tag byte that says who it is
// for: 'm' the chat's messages (uni streams), 's' the chat's kolo-sync (bidi streams), 'b' the braid (both). The mux
// reads the tag of each stream on a goroutine of its own and hands the stream to the part of that tag, which looks
// like a whole connection to its user.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	koloswarm "github.com/ciranda-dev/kolo-swarm"
	kolotransport "github.com/ciranda-dev/kolo-transport"
)

const (
	tagMsg   = 'm'
	tagSync  = 's'
	tagBraid = 'b'
	// maxStreams is the incoming streams per direction a connection allows: 16 for the braid's pulls, 16 for the
	// chat's kolo-sync streams and 32 for the uni messages of both.
	maxStreams = 64
	// maxWaiting is how many streams a braid part may hold waiting; one more is a fault, which closes the part and
	// frees its streams at once. UNSOURCED: half of maxStreams, so a stuck braid never holds the credit the chat needs.
	maxWaiting = maxStreams / 2
	codeBadTag = 3
)

var errPartClosed = errors.New("mux: part closed")

// mux splits one connection into parts by the tag of each stream.
type mux struct {
	conn koloswarm.Conn
	wait time.Duration  // for a stream's tag: sendTimeout, shorter in tests
	wg   sync.WaitGroup // the handoff goroutines, which end with the connection

	mu    sync.Mutex
	parts map[byte]*muxConn
}

func newMux(c koloswarm.Conn) *mux {
	return &mux{conn: c, wait: sendTimeout, parts: map[byte]*muxConn{}}
}

// part returns the part of tag, a new one once the last was closed. A closed part cancels every stream of its tag until
// part is called again.
func (m *mux) part(tag byte) *muxConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.parts[tag]; p != nil && p.ctx.Err() == nil {
		return p
	}
	return m.newPartLocked(tag)
}

// waitingPart returns the part a stream of tag goes to: the open one, one made for it when none was asked for yet, or
// nil when the last one was closed.
func (m *mux) waitingPart(tag byte) *muxConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.parts[tag]
	switch {
	case p == nil:
		p = m.newPartLocked(tag)
	case p.ctx.Err() != nil:
		return nil
	}
	p.waiting.Add(1)
	return p
}

func (m *mux) newPartLocked(tag byte) *muxConn {
	ctx, cancel := context.WithCancel(m.conn.Context())
	p := &muxConn{m: m, tag: tag, ctx: ctx, cancel: cancel, limited: tag == tagBraid,
		uni: make(chan *kolotransport.ReceiveStream), bidi: make(chan *kolotransport.Stream)}
	m.parts[tag] = p
	return p
}

// acceptUni hands every uni stream of the connection to its part, until the connection ends.
func (m *mux) acceptUni() {
	defer m.wg.Wait()
	for {
		st, err := m.conn.AcceptUniStream(m.conn.Context())
		if err != nil {
			return
		}
		m.wg.Go(func() { handoff(m, st, false, func(p *muxConn) chan *kolotransport.ReceiveStream { return p.uni }) })
	}
}

// acceptBidi is acceptUni for bidi streams. It starts once the peer's hello is proven: before that, the inviter may
// need the connection's bidi streams.
func (m *mux) acceptBidi() {
	defer m.wg.Wait()
	for {
		st, err := m.conn.AcceptStream(m.conn.Context())
		if err != nil {
			return
		}
		m.wg.Go(func() { handoff(m, st, true, func(p *muxConn) chan *kolotransport.Stream { return p.bidi }) })
	}
}

// tagged is what handoff needs of a *kolotransport.Stream and a *kolotransport.ReceiveStream.
type tagged interface {
	io.Reader
	SetReadDeadline(time.Time) error
	CancelRead(code uint64) error
}

// handoff reads the tag of one stream and waits until its part takes it, the part closes or the connection ends.
// Nothing is dropped while the part runs; a braid part with more than maxWaiting streams waiting is closed.
func handoff[S tagged](m *mux, st S, bidi bool, ch func(*muxConn) chan S) {
	cancel := func() {
		st.CancelRead(codeBadTag)
		if s, ok := any(st).(*kolotransport.Stream); ok {
			s.CancelWrite(codeBadTag)
		}
	}
	st.SetReadDeadline(time.Now().Add(m.wait))
	var tag [1]byte
	if _, err := io.ReadFull(st, tag[:]); err != nil || !allowedTag(tag[0], bidi) {
		cancel()
		return
	}
	st.SetReadDeadline(time.Time{})
	p := m.waitingPart(tag[0])
	if p == nil {
		cancel()
		return
	}
	defer p.waiting.Add(-1)
	if p.limited && p.waiting.Load() > maxWaiting {
		p.overflowed.Store(true)
		p.Close()
	}
	select {
	case ch(p) <- st:
	case <-p.ctx.Done():
		cancel()
	}
}

// allowedTag says which tags each kind of stream takes: uni streams 'm' and 'b', bidi streams 's' and 'b'.
func allowedTag(tag byte, bidi bool) bool {
	return tag == tagBraid || bidi && tag == tagSync || !bidi && tag == tagMsg
}

// muxConn is one part of a connection. It has the methods of a koloswarm.Conn, so kolo-sync and kolo-braid take it as
// a connection; Close ends the part only.
type muxConn struct {
	m          *mux
	tag        byte
	ctx        context.Context
	cancel     context.CancelFunc
	limited    bool         // the queue rule applies
	waiting    atomic.Int32 // streams of this tag that wait to be taken
	overflowed atomic.Bool  // closed by the queue rule
	uni        chan *kolotransport.ReceiveStream
	bidi       chan *kolotransport.Stream
}

func (p *muxConn) RemotePublicKey() ed25519.PublicKey { return p.m.conn.RemotePublicKey() }
func (p *muxConn) Context() context.Context           { return p.ctx }
func (p *muxConn) Close() error                       { p.cancel(); return nil }

func (p *muxConn) CloseWithError(uint64, string) error { return p.Close() }

func (p *muxConn) OpenStream(ctx context.Context) (*kolotransport.Stream, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, errPartClosed
	}
	st, err := p.m.conn.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	st.SetWriteDeadline(time.Now().Add(sendTimeout))
	if _, err := st.Write([]byte{p.tag}); err != nil {
		st.CancelWrite(codeBadTag)
		st.CancelRead(codeBadTag)
		return nil, err
	}
	st.SetWriteDeadline(time.Time{})
	return st, nil
}

func (p *muxConn) OpenUniStream(ctx context.Context) (*kolotransport.SendStream, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, errPartClosed
	}
	st, err := p.m.conn.OpenUniStream(ctx)
	if err != nil {
		return nil, err
	}
	st.SetWriteDeadline(time.Now().Add(sendTimeout))
	if _, err := st.Write([]byte{p.tag}); err != nil {
		st.CancelWrite(codeBadTag)
		return nil, err
	}
	st.SetWriteDeadline(time.Time{})
	return st, nil
}

func (p *muxConn) AcceptStream(ctx context.Context) (*kolotransport.Stream, error) {
	return accept(ctx, p, p.bidi)
}

func (p *muxConn) AcceptUniStream(ctx context.Context) (*kolotransport.ReceiveStream, error) {
	return accept(ctx, p, p.uni)
}

func accept[S any](ctx context.Context, p *muxConn, ch chan S) (S, error) {
	var zero S
	select {
	case st := <-ch:
		return st, nil
	case <-p.ctx.Done():
		return zero, errPartClosed
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}
