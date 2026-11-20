package main

// What the app adds around the braid, which a first version can leave out: the window's "chat:state" and the pause
// of a member whose braid broke the protocol. braid.go has the braid itself.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"time"

	kolobraid "github.com/ciranda-dev/kolo-braid"
)

const (
	// pauseFirst is how long a braid fault stops the braid with that member, doubling to 16 times as long.
	// UNSOURCED: ten times the braid's 60 s answer timeout, so a faulty braid costs one reconnect per pause.
	pauseFirst = 10 * time.Minute
	// noAnswerAfter is how long proven peers may go without accepting us before the window says so: the braid's
	// 60 s grace for an unknown key.
	noAnswerAfter = 60 * time.Second
)

// StateStatus is the payload of "chat:state": what the window says about the member and room lists.
type StateStatus struct {
	SingleSettler    bool     `json:"singleSettler"`
	Settler          bool     `json:"settler"`
	Pending          int      `json:"pending"`
	PendingRooms     []string `json:"pendingRooms"`
	Paused           int      `json:"paused"`
	Halted           bool     `json:"halted"`
	SettlementHalted bool     `json:"settlementHalted"`
	Diverged         bool     `json:"diverged"`
	Unconfirmed      bool     `json:"unconfirmed"`
	StrandLost       bool     `json:"strandLost"`
	NoAnswer         bool     `json:"noAnswer"`
	OtherGroup       bool     `json:"otherGroup"`
	NotListed        bool     `json:"notListed"` // our braid does not list us yet: a joiner talks to nobody until a member syncs it
}

// pause is a member whose braid broke the protocol: the braid with it stops until until, then starts fresh.
type pause struct {
	until  time.Time
	faults int
	served time.Time
}

func (c *ChatService) pausedFor(key string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pauses[key]; p != nil {
		return time.Until(p.until)
	}
	return 0
}

func (c *ChatService) served(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pauses[key]; p != nil {
		p.served = time.Now()
	}
}

// braidFault starts or doubles the pause of a member's braid. Faults in a row double it; a braid that ran for a
// whole first pause before the fault starts again from it.
func (c *ChatService) braidFault(key string) {
	c.mu.Lock()
	p := c.pauses[key]
	if p == nil {
		p = &pause{}
		c.pauses[key] = p
	}
	if !p.served.IsZero() && time.Since(p.served) >= pauseFirst {
		p.faults = 0
	}
	p.faults++
	wait := min(pauseFirst<<(p.faults-1), 16*pauseFirst)
	p.until = time.Now().Add(wait)
	c.mu.Unlock()
	log.Printf("member list with %s paused for %s: its braid broke the protocol", key[:8], wait)
}

func (c *ChatService) unpause(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.pauses[key]; p != nil {
		p.until = time.Time{}
	}
}

// state is what "chat:state" says now.
func (c *ChatService) state(st kolobraid.Status) StateStatus {
	view, settled := c.braid.View(), c.braid.Settled()
	rooms, done := roomsOf(view), roomsOf(settled)
	view.Close()
	settled.Close()
	s := StateStatus{SingleSettler: st.SingleSettler, PendingRooms: []string{}, StrandLost: st.StrandLost,
		SettlementHalted: st.SettlementHalted != nil, Diverged: errors.Is(st.Halted, kolobraid.ErrDiverged)}
	s.Halted = st.Halted != nil && !s.Diverged
	for _, r := range rooms {
		if !slices.Contains(done, r) {
			s.PendingRooms = append(s.PendingRooms, r)
		}
	}
	for o, err := range c.braid.Entries(st.Settled) {
		if err != nil {
			break
		}
		switch o.Kind {
		case kolobraid.KindData, kolobraid.KindAddMember, kolobraid.KindRemoveMember, kolobraid.KindSettlers:
			if !o.Settled && !o.Void {
				s.Pending++
			}
		}
	}
	me := hex.EncodeToString(c.pub)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, listed := c.accepted[me]
	s.Settler, s.NotListed = c.accepted[me].Seat, !listed
	if !st.Unconfirmed {
		c.unconfirmed = false
	}
	s.Unconfirmed = c.unconfirmed
	proven, answered := 0, false
	for key, l := range c.links {
		if p := c.pauses[key]; p != nil && time.Now().Before(p.until) && l.registered {
			s.Paused++
		}
		s.OtherGroup = s.OtherGroup || l.otherGroup
		if !l.since.IsZero() && time.Since(l.since) >= noAnswerAfter {
			proven++
		}
		answered = answered || l.answered
	}
	s.NoAnswer = proven > 0 && !answered
	return s
}

// emitState sends "chat:state" when it changed. Only stateLoop calls it, so the events come in order.
func (c *ChatService) emitState(st kolobraid.Status) {
	s := c.state(st)
	b, _ := json.Marshal(s)
	if bytes.Equal(b, c.lastState) {
		return
	}
	c.lastState = b
	c.emit("chat:state", s)
}
