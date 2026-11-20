package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	koloid "github.com/ciranda-dev/kolo-id"
	kolosecure "github.com/ciranda-dev/kolo-secure"
	kolotransport "github.com/ciranda-dev/kolo-transport"
)

const testWait = 300 * time.Millisecond

// muxPair is a kolo-secure connection on 127.0.0.1 with maxStreams per direction: the client's end as it is, and the
// server's end split by a mux whose accept loops run, with a short wait.
func muxPair(t *testing.T) (*kolosecure.Conn, *mux, chan struct{}) {
	t.Helper()
	ctx := t.Context()
	sid, _ := koloid.Generate()
	cid, _ := koloid.Generate()
	cfg := &kolosecure.Config{Transport: &kolotransport.Config{MaxIncomingStreams: maxStreams}}
	l, err := kolosecure.Listen(ctx, "127.0.0.1:0", sid, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	accepted := make(chan *kolosecure.Conn, 1)
	go func() {
		if c, err := l.Accept(ctx); err == nil {
			accepted <- c
		}
	}()
	client, err := kolosecure.Dial(ctx, l.Addr().String(), cid, sid.PublicKey(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server := <-accepted
	t.Cleanup(func() { server.Close() })
	m := newMux(server)
	m.wait = testWait
	done := make(chan struct{})
	go func() {
		ended := make(chan struct{})
		go func() { m.acceptBidi(); close(ended) }()
		m.acceptUni()
		<-ended
		close(done)
	}()
	return client, m, done
}

func sendUni(t *testing.T, c *kolosecure.Conn, data []byte) *kolotransport.SendStream {
	t.Helper()
	st, err := c.OpenUniStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(data); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return st
}

func openBidi(t *testing.T, c *kolosecure.Conn, data []byte) *kolotransport.Stream {
	t.Helper()
	st, err := c.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(data); err != nil {
		t.Fatal(err)
	}
	return st
}

// takeUni reads one uni stream of the part, or "" when none comes within d.
func takeUni(t *testing.T, p *muxConn, d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	st, err := p.AcceptUniStream(ctx)
	if err != nil {
		return ""
	}
	b, _ := io.ReadAll(st)
	return string(b)
}

func takeBidi(t *testing.T, p *muxConn, d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	st, err := p.AcceptStream(ctx)
	if err != nil {
		return ""
	}
	buf := make([]byte, 16)
	n, _ := st.Read(buf)
	return string(buf[:n])
}

// S7a: each tag reaches its part, on its kind of stream. A tag a kind of stream does not take, an unknown tag and a
// stream that ends before its tag cancel that stream only: the connection goes on.
func TestMuxRoutesByTag(t *testing.T) {
	c, m, _ := muxPair(t)
	sendUni(t, c, []byte("mhello"))
	sendUni(t, c, []byte("bbraid uni"))
	openBidi(t, c, []byte("ssync"))
	openBidi(t, c, []byte("bbraid bidi"))
	if got := takeUni(t, m.part(tagMsg), 5*time.Second); got != "hello" {
		t.Fatalf("'m': %q", got)
	}
	if got := takeUni(t, m.part(tagBraid), 5*time.Second); got != "braid uni" {
		t.Fatalf("'b' uni: %q", got)
	}
	if got := takeBidi(t, m.part(tagSync), 5*time.Second); got != "sync" {
		t.Fatalf("'s': %q", got)
	}
	if got := takeBidi(t, m.part(tagBraid), 5*time.Second); got != "braid bidi" {
		t.Fatalf("'b' bidi: %q", got)
	}

	sendUni(t, c, []byte("s on uni"))
	sendUni(t, c, []byte("xunknown"))
	sendUni(t, c, nil) // ends before its tag
	wrong := openBidi(t, c, []byte("m on bidi"))
	wrong.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := wrong.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Fatalf("an 'm' bidi stream was not cancelled: %v", err)
	}
	if got := takeUni(t, m.part(tagSync), testWait*3); got != "" {
		t.Fatalf("an 's' uni stream reached the part: %q", got)
	}
	sendUni(t, c, []byte("mstill here"))
	if got := takeUni(t, m.part(tagMsg), 5*time.Second); got != "still here" {
		t.Fatalf("the connection after bad streams: %q", got)
	}
	if c.Context().Err() != nil {
		t.Fatal("a bad stream closed the connection")
	}
}

// slowStream is a stream whose tag never comes: Read waits for the deadline, and for good without one.
type slowStream struct {
	deadline  time.Time
	cancelled chan struct{}
}

func (s *slowStream) Read([]byte) (int, error) {
	if s.deadline.IsZero() {
		select {}
	}
	time.Sleep(time.Until(s.deadline))
	return 0, os.ErrDeadlineExceeded
}
func (s *slowStream) SetReadDeadline(t time.Time) error { s.deadline = t; return nil }
func (s *slowStream) CancelRead(uint64) error           { close(s.cancelled); return nil }

// S7a: a stream whose tag comes late is cancelled after the mux's wait, on its own goroutine.
func TestMuxCancelsALateTag(t *testing.T) {
	m := &mux{wait: testWait, parts: map[byte]*muxConn{}}
	s := &slowStream{cancelled: make(chan struct{})}
	go handoff(m, s, false, func(*muxConn) chan *slowStream { return nil })
	select {
	case <-s.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("a stream without its tag waits for good")
	}
}

// S7a and N1: a burst of maxWaiting braid streams that nobody takes yet is kept, not dropped, and the chat's streams
// pass it at once; one more closes the braid part at once (the queue rule), its streams free their credit, and a chat
// message gets through within a second.
func TestMuxKeepsABurstAndBoundsAStuckBraid(t *testing.T) {
	c, m, _ := muxPair(t)
	for range maxWaiting {
		sendUni(t, c, []byte("bwaiting"))
	}
	time.Sleep(3 * testWait)
	sendUni(t, c, []byte("mhello"))
	if got := takeUni(t, m.part(tagMsg), sendTimeout); got != "hello" {
		t.Fatalf("a hello behind a busy braid: %q", got)
	}
	p := m.part(tagBraid)
	for i := range maxWaiting {
		if got := takeUni(t, p, 5*time.Second); got != "waiting" {
			t.Fatalf("braid stream %d was dropped: %q", i, got)
		}
	}
	if p.overflowed.Load() || p.ctx.Err() != nil {
		t.Fatal("maxWaiting streams broke the queue rule")
	}

	for range maxStreams { // the part closes on the way, and later writes may fail
		if st, err := c.OpenUniStream(t.Context()); err == nil {
			st.Write([]byte("bstuck"))
			st.Close()
		}
	}
	eventually(t, "the queue rule", func() bool { return p.overflowed.Load() && p.ctx.Err() != nil })
	began := time.Now()
	sendUni(t, c, []byte("mafter"))
	openBidi(t, c, []byte("ssync"))
	if got := takeUni(t, m.part(tagMsg), sendTimeout); got != "after" || time.Since(began) > time.Second {
		t.Fatalf("a chat message behind a stuck braid: %q after %v", got, time.Since(began))
	}
	if got := takeBidi(t, m.part(tagSync), sendTimeout); got != "sync" {
		t.Fatalf("a chat sync behind a stuck braid: %q", got)
	}
	if m.part(tagMsg).ctx.Err() != nil || c.Context().Err() != nil {
		t.Fatal("the queue rule closed more than the braid part")
	}
	if next := m.part(tagBraid); next == p || next.ctx.Err() != nil {
		t.Fatal("the braid is not served again after a closed part")
	}
}

// S7b: Close of a part cancels its context and its waiting streams, and every later stream of its tag until it is
// served again; the other parts and the connection go on.
func TestMuxPartCloseEndsThatPartOnly(t *testing.T) {
	c, m, _ := muxPair(t)
	p := m.part(tagBraid)
	waiting := openBidi(t, c, []byte("bwaits"))
	time.Sleep(testWait)
	p.Close()
	if p.Context().Err() == nil {
		t.Fatal("a closed part's context goes on")
	}
	waiting.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := waiting.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Fatalf("a waiting stream of a closed part: %v", err)
	}
	late := openBidi(t, c, []byte("blater"))
	late.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := late.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Fatalf("a later stream of a closed part: %v", err)
	}
	sendUni(t, c, []byte("mstill"))
	if got := takeUni(t, m.part(tagMsg), 5*time.Second); got != "still" || c.Context().Err() != nil {
		t.Fatalf("the chat after a part closed: %q", got)
	}
	if _, err := p.OpenStream(t.Context()); err == nil {
		t.Fatal("a closed part opens streams")
	}
}

// S7a and N9: the handoff goroutines end with the connection, and so do the accept loops.
func TestMuxEndsWithTheConnection(t *testing.T) {
	c, m, done := muxPair(t)
	for range 4 {
		sendUni(t, c, []byte("bnobody takes this"))
	}
	openBidi(t, c, []byte("bnor this"))
	time.Sleep(testWait)
	if m.part(tagBraid).waiting.Load() == 0 {
		t.Fatal("nothing waits")
	}
	m.conn.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a handoff goroutine outlived the connection")
	}
}
