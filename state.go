package main

// The shared state of a conversation: the room list, kept by kolo-braid.
//
// The braid keeps the member list itself (Snapshot.Members), so the view holds only the rooms. A data entry of the chat
// is a fixed byte layout: byte 0 the version, byte 1 the op, byte 2 the name's length in bytes, then the name. Version 1
// has one op, "add a room".

import (
	"errors"
	"fmt"
	"slices"

	kolobraid "github.com/ciranda-dev/kolo-braid"
)

const (
	stateApp  = "kolochat/state/v1"
	roomV1    = 1
	opAddRoom = 1
)

var keyRooms = []byte("rooms")

// applyState keeps the view key "rooms", the sorted list of rooms admins added. It reads only the view, the entry and
// the roster at the entry's position, so every peer computes the same list.
func applyState(tx *kolobraid.Tx, e *kolobraid.Entry) error {
	if e.Kind != kolobraid.KindData {
		return nil // the library applied the membership effect
	}
	// Before the version: old and new apps agree on a non-admin's entry, so nobody can halt every view.
	if m, ok := tx.Member(e.Writer); !ok || m.Roles&kolobraid.RoleAdmin == 0 {
		return kolobraid.Reject("not an admin")
	}
	b := e.Body
	switch {
	case len(b) == 0 || b[0] == 0:
		return kolobraid.Reject("no version")
	case b[0] > roomV1: // a later version halts this app until it is updated, and is never rejected
		return fmt.Errorf("room entry of version %d", b[0])
	case len(b) < 3 || b[1] != opAddRoom || int(b[2]) != len(b)-3:
		return kolobraid.Reject("not a version 1 room entry")
	}
	name := string(b[3:])
	raw, err := tx.Get(keyRooms)
	if err != nil && !errors.Is(err, kolobraid.ErrNotFound) {
		return err
	}
	rooms, err := decodeRooms(raw)
	if err != nil {
		return err
	}
	switch {
	case validRoomName(name) != nil, sameRoom(name, defaultRoom):
		return kolobraid.Reject("bad room name")
	case slices.ContainsFunc(rooms, func(r string) bool { return sameRoom(r, name) }):
		return kolobraid.Reject("room exists")
	case len(rooms) >= maxRooms:
		return kolobraid.Reject("too many rooms")
	}
	rooms = append(rooms, name)
	slices.Sort(rooms)
	return tx.Put(keyRooms, encodeRooms(rooms))
}

// encodeAddRoom is the body of an entry that adds the room name, which validRoomName accepts: at most 128 bytes.
func encodeAddRoom(name string) []byte {
	return append([]byte{roomV1, opAddRoom, byte(len(name))}, name...)
}

// encodeRooms writes the room list as length-prefixed names.
func encodeRooms(rooms []string) []byte {
	var out []byte
	for _, r := range rooms {
		out = append(append(out, byte(len(r))), r...)
	}
	return out
}

func decodeRooms(b []byte) ([]string, error) {
	var rooms []string
	for len(b) > 0 {
		n := int(b[0])
		if n == 0 || len(b) < 1+n {
			return nil, errors.New("damaged room list")
		}
		rooms, b = append(rooms, string(b[1:1+n])), b[1+n:]
	}
	return rooms, nil
}

// roomsOf reads the rooms admins added from a snapshot of the braid's view.
func roomsOf(s *kolobraid.Snapshot) []string {
	raw, err := s.Get(keyRooms)
	if err != nil {
		return nil
	}
	rooms, _ := decodeRooms(raw)
	return rooms
}

// sameRoom compares two names with ASCII letters folded and every other byte as is: Go's Unicode tables never decide
// what a peer applies.
func sameRoom(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
