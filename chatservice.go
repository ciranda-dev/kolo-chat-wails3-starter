package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/netip"
	"sync"
	"time"

	koloid "github.com/ciranda-dev/kolo-id"
	koloswarm "github.com/ciranda-dev/kolo-swarm"
	"github.com/wailsapp/wails/v3/pkg/application"
)

const maxMessage = 64 * 1024

// ChatMsg is the payload of the "chat:msg" event; From is the sender's kolo-id (hex public key).
type ChatMsg struct {
	From string `json:"from"`
	Text string `json:"text"`
}

type ChatService struct {
	mu      sync.Mutex
	started bool
	id      string
	ctx     context.Context
	peers   map[string]*koloswarm.Peer
}

// Start joins the network and returns this user's kolo-id (hex public key).
func (c *ChatService) Start() (string, error) {
	c.mu.Lock()
	if c.started {
		id := c.id
		c.mu.Unlock()
		return id, nil
	}
	c.started = true
	c.peers = map[string]*koloswarm.Peer{}
	c.mu.Unlock()

	ctx := context.Background()
	c.ctx = ctx

	id, err := koloid.Generate()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.id = id.PublicKeyString()
	c.mu.Unlock()
	sw, err := koloswarm.New(ctx, "127.0.0.1:0", id, &koloswarm.Config{LookupInterval: 5 * time.Second})
	if err != nil {
		return "", err
	}
	boot := netip.MustParseAddrPort("127.0.0.1:4000")
	if err := sw.Bootstrap(ctx, []netip.AddrPort{boot}); err != nil {
		return "", err
	}
	// swarm events
	go func() {
		for {
			ev, err := sw.Next(ctx)
			if err != nil {
				log.Print("swarm: ", err)
				return
			}
			if ev.Kind == koloswarm.Connected && ev.Peer != nil {
				c.addPeer(ev.Peer)
			}
		}
	}()

	// join the fixed chat topic
	sum := sha256.Sum256([]byte("kolochat/v1"))
	var topic koloswarm.Topic
	copy(topic[:], sum[:])
	go func() {
		if err := sw.Join(ctx, topic, &koloswarm.JoinOptions{Announce: true, Lookup: true}); err != nil {
			log.Print("join: ", err)
		}
	}()
	return id.PublicKeyString(), nil
}

func (c *ChatService) addPeer(p *koloswarm.Peer) {
	key := hex.EncodeToString(p.PublicKey())
	c.mu.Lock()
	c.peers[key] = p
	c.mu.Unlock()
	log.Print("peer connected: ", key[:8])
	c.emitPeers()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.peers, key)
			c.mu.Unlock()
			log.Print("peer gone: ", key[:8])
			c.emitPeers()
		}()
		for {
			s, err := p.Conn().AcceptStream(c.ctx)
			if err != nil {
				return
			}
			go func() {
				defer s.Close()
				data, err := io.ReadAll(io.LimitReader(s, maxMessage))
				if err != nil || len(data) == 0 {
					return
				}
				c.emit(key, data)
			}()
		}
	}()
}

func (c *ChatService) Send(text string) error {
	if len(text) > maxMessage {
		return errors.New("message too long")
	}
	c.mu.Lock()
	peers := make([]*koloswarm.Peer, 0, len(c.peers))
	for _, p := range c.peers {
		peers = append(peers, p)
	}
	c.mu.Unlock()
	if len(peers) == 0 {
		return errors.New("no peers connected")
	}
	var firstErr error
	for _, p := range peers {
		s, err := p.Conn().OpenStream(c.ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := s.Write([]byte(text)); err != nil && firstErr == nil {
			firstErr = err
		}
		s.Close()
	}
	return firstErr
}

// emit hands a received message to the window as the "chat:msg" event.
func (c *ChatService) emit(from string, data []byte) {
	application.Get().Event.Emit("chat:msg", ChatMsg{From: from, Text: string(data)})
}

// emitPeers tells the window how many peers are connected, as the "chat:peers" event.
func (c *ChatService) emitPeers() {
	c.mu.Lock()
	n := len(c.peers)
	c.mu.Unlock()
	application.Get().Event.Emit("chat:peers", n)
}
