package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Defaults of the settings that have one. Everything else must be configured.
const (
	defaultBootstrap      = "127.0.0.1:4000"
	defaultListen         = "127.0.0.1:0"
	defaultPinAddr        = "127.0.0.1:7450" // where `kolo-pin run` listens unless told otherwise
	defaultPinTTL         = 30 * 24 * time.Hour
	defaultPinQuota       = 16 << 20 // bytes
	defaultLookupInterval = 5 * time.Second
)

// ErrConfig matches every ConfigError, whatever setting it names.
var ErrConfig = errors.New("invalid configuration")

// Causes of a ConfigError, besides the errors of kolo-pin for a bad token.
var (
	ErrMissing   = errors.New("is required")
	ErrMalformed = errors.New("is malformed")

	errFixedPort = errors.New("has a fixed port, which only one conversation can use: use port 0")
)

// ConfigError names the setting that is wrong and why. errors.Is(err, ErrConfig) is true for every one, and
// errors.Is(err, ErrMissing), pin.ErrExpired and the like tell the cause.
type ConfigError struct {
	Setting string // the flag name, such as "pin-key"
	Err     error
}

func (e *ConfigError) Error() string { return fmt.Sprintf("kolo-chat: -%s %v", e.Setting, e.Err) }
func (e *ConfigError) Unwrap() error { return e.Err }
func (e *ConfigError) Is(target error) bool {
	return target == ErrConfig
}

// Config is everything the chat needs from outside. A zero field takes its default, if it has one.
// LoadConfig fills it from flags and environment variables.
type Config struct {
	DataDir         string        // -data, KOLO_CHAT_DATA: one folder per conversation, see conversations.go. Default: <user config dir>/kolo-chat
	Bootstrap       string        // -bootstrap, KOLO_CHAT_BOOTSTRAP: IP:PORT of a bootstrap node. Default 127.0.0.1:4000
	Listen          string        // -listen, KOLO_CHAT_LISTEN: local IP:PORT of every conversation. Default 127.0.0.1:0; a fixed port allows one conversation
	ConversationKey string        // -conversation-key, KOLO_CHAT_CONVERSATION_KEY: 64 hex characters (32 bytes), the key of one conversation, kept in its folder. Empty to start conversations in the window or join with an invite
	PinAddr         string        // -pin, KOLO_CHAT_PIN: IP:PORT of the pin. Default 127.0.0.1:7450. The pin of the conversations started here; a joiner gets the pin from the invite
	PinKey          string        // -pin-key, KOLO_CHAT_PIN_KEY: the pin's public key in hex (`kolo-pin id`). Required with a conversation key and to start conversations in the window
	PinToken        string        // -pin-token, KOLO_CHAT_PIN_TOKEN: a capability in hex for the -conversation-key conversation, issued by its kolo-id. Default: the app issues one
	PinTTL          time.Duration // -pin-ttl, KOLO_CHAT_PIN_TTL: lifetime of the capability the app issues. Default 720h
	PinQuota        uint64        // -pin-quota, KOLO_CHAT_PIN_QUOTA: bytes the pin may keep for you, in the capability the app issues. Default 16777216
	LookupInterval  time.Duration // -lookup-interval, KOLO_CHAT_LOOKUP_INTERVAL: time between topic lookups. Default 5s
}

// LoadConfig reads the flags in args. A setting that is not given as a flag comes from its environment variable
// (read with getenv, such as os.Getenv), and a flag wins over its variable.
func LoadConfig(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("kolo-chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	text := func(p *string) func(string) error {
		return func(v string) error { *p = v; return nil }
	}
	dur := func(p *time.Duration) func(string) error {
		return func(v string) (err error) { *p, err = time.ParseDuration(v); return err }
	}
	num := func(p *uint64) func(string) error {
		return func(v string) (err error) { *p, err = strconv.ParseUint(v, 10, 64); return err }
	}
	for _, s := range []struct {
		name, env, help string
		set             func(string) error
	}{
		{"data", "KOLO_CHAT_DATA", "data directory (default <user config dir>/kolo-chat)", text(&c.DataDir)},
		{"bootstrap", "KOLO_CHAT_BOOTSTRAP", "IP:PORT of the bootstrap node (default " + defaultBootstrap + ")", text(&c.Bootstrap)},
		{"listen", "KOLO_CHAT_LISTEN", "local IP:PORT to listen on (default " + defaultListen + ")", text(&c.Listen)},
		{"conversation-key", "KOLO_CHAT_CONVERSATION_KEY", "32-byte key of one conversation, in hex (leave it out to start conversations in the window or join with an invite)", text(&c.ConversationKey)},
		{"pin", "KOLO_CHAT_PIN", "IP:PORT of the pin (default " + defaultPinAddr + ")", text(&c.PinAddr)},
		{"pin-key", "KOLO_CHAT_PIN_KEY", "public key of the pin, in hex: the pin of the conversations you start (required with -conversation-key)", text(&c.PinKey)},
		{"pin-token", "KOLO_CHAT_PIN_TOKEN", "capability for the pin of the -conversation-key conversation, in hex (default: issued by the app)", text(&c.PinToken)},
		{"pin-ttl", "KOLO_CHAT_PIN_TTL", "lifetime of the capability the app issues (default 720h)", dur(&c.PinTTL)},
		{"pin-quota", "KOLO_CHAT_PIN_QUOTA", "bytes the pin may keep for you (default 16777216)", num(&c.PinQuota)},
		{"lookup-interval", "KOLO_CHAT_LOOKUP_INTERVAL", "time between lookups of the topic (default 5s)", dur(&c.LookupInterval)},
	} {
		if v := getenv(s.env); v != "" {
			if err := s.set(v); err != nil {
				return c, &ConfigError{Setting: s.name, Err: fmt.Errorf("%w: %s: %w", ErrMalformed, s.env, err)}
			}
		}
		fs.Func(s.name, s.help+" ("+s.env+")", s.set)
	}
	return c, fs.Parse(args)
}

// settings is a Config after defaults and checks: values the code can use as they are.
type settings struct {
	Config
	conversationKey [32]byte
	pinKey          ed25519.PublicKey
	token           []byte // nil: the app issues one
	bootstrap       netip.AddrPort
	inConversation  bool // the key and the pin are known: from the configuration, or later from an invite
}

// resolve applies the defaults and checks every setting, returning a *ConfigError for the first bad one.
// Without a conversation key a chat starts as a joiner: the key and the pin come from an invite (see
// useConversation). The pin is then the one of the conversations started in the window, and a token, which is
// for the key's conversation only, is refused. A token is checked later, in capability, because it needs the
// identity.
func (c Config) resolve() (settings, error) {
	s := settings{Config: c}
	if s.DataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return s, &ConfigError{Setting: "data", Err: fmt.Errorf("%w: no user config directory: %w", ErrMissing, err)}
		}
		s.DataDir = filepath.Join(dir, "kolo-chat")
	}
	s.Bootstrap = orDefault(s.Bootstrap, defaultBootstrap)
	s.Listen = orDefault(s.Listen, defaultListen)
	s.PinAddr = orDefault(s.PinAddr, defaultPinAddr)
	if s.PinTTL == 0 {
		s.PinTTL = defaultPinTTL
	}
	if s.PinQuota == 0 {
		s.PinQuota = defaultPinQuota
	}
	if s.LookupInterval == 0 {
		s.LookupInterval = defaultLookupInterval
	}

	boot, err := netip.ParseAddrPort(s.Bootstrap)
	if err != nil {
		return s, &ConfigError{Setting: "bootstrap", Err: fmt.Errorf("%w: %w", ErrMalformed, err)}
	}
	s.bootstrap = boot
	if _, err := netip.ParseAddrPort(s.PinAddr); err != nil {
		return s, &ConfigError{Setting: "pin", Err: fmt.Errorf("%w: %w", ErrMalformed, err)}
	}
	if c.ConversationKey == "" && c.PinToken != "" {
		return s, &ConfigError{Setting: "conversation-key", Err: ErrMissing}
	}
	if c.ConversationKey != "" || c.PinKey != "" {
		s.pinKey = make(ed25519.PublicKey, ed25519.PublicKeySize)
		if err := parseHex("pin-key", c.PinKey, s.pinKey); err != nil {
			return s, err
		}
	}
	if c.ConversationKey == "" {
		return s, nil
	}
	if err := parseHex("conversation-key", c.ConversationKey, s.conversationKey[:]); err != nil {
		return s, err
	}
	s.inConversation = true
	if c.PinToken != "" {
		if s.token, err = hex.DecodeString(c.PinToken); err != nil {
			return s, &ConfigError{Setting: "pin-token", Err: fmt.Errorf("%w: %w", ErrMalformed, err)}
		}
	}
	return s, nil
}

// fixedPort says whether the listen address names a port, which one socket only can hold.
func (s settings) fixedPort() bool {
	ap, err := netip.ParseAddrPort(s.Listen)
	return err == nil && ap.Port() != 0
}

// useConversation takes the key and the pin from an invite, or from the file that kept them.
func (s *settings) useConversation(conv conversation) {
	copy(s.conversationKey[:], conv.Key)
	s.pinKey = ed25519.PublicKey(conv.PinKey)
	s.PinAddr = conv.PinAddr
	s.inConversation = true
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// parseHex decodes the hex setting value into dst, which has the exact size wanted.
func parseHex(setting, value string, dst []byte) error {
	if value == "" {
		return &ConfigError{Setting: setting, Err: ErrMissing}
	}
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != len(dst) {
		return &ConfigError{Setting: setting, Err: fmt.Errorf("%w: want %d bytes in hex", ErrMalformed, len(dst))}
	}
	copy(dst, b)
	return nil
}
