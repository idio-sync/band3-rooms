package server

import (
	"errors"
	"net/netip"
	"strings"
	"time"
)

// Config is everything an operator can set, and a few things tests can.
type Config struct {
	// The server's host names as players type them in their game, which
	// emulator login proofs are signed with. The first is the one logs name.
	Addresses []string
	// Ask for a login proof (ServerHello requires_proof) and check emulators' ones.
	RequireProof bool
	// Take logins from real Xbox 360s, whose signed proofs can't be checked here.
	AllowConsoles bool

	// How often each game is pinged (0: never), and how long a game may go
	// without sending anything before it's dropped (0: never). Games hang up
	// on a server that's been silent for 30 s, so Ping should stay well under.
	Ping    time.Duration
	Timeout time.Duration
	// How long a connection has to log in (0: no limit).
	LoginTimeout time.Duration

	MaxConns      int // all connections at once (0: no limit)
	MaxConnsPerIP int // from one address at once (0: no limit)

	// Failed joins a connection, and all connections from one address together,
	// may make per minute (0: no limit). Codes are short, so this is what stops
	// anyone working through them.
	ConnJoinMisses int
	IPJoinMisses   int

	// Client version prefixes (the hello's version string, case-insensitive).
	// With AllowVersions set, only versions starting with one of them are let in;
	// DenyVersions turns away versions starting with one of them, and wins.
	AllowVersions []string
	DenyVersions  []string

	// The public IPv4 to give games that connect from a private, loopback or
	// link-local address: the server's own public address, when it shares a
	// network with some of its players. Unset, they're given the address they
	// came from.
	LANPublicIP netip.Addr

	// Log every frame in and out in hex.
	Hex bool

	// Frames to one game are sent at least this far apart, since RB3Enhanced
	// reads one packet per game frame and loses a second one that arrives with
	// it. 0 means the default; tests shorten it.
	FrameGap time.Duration
	// Makes codes; tests set it. nil means random ones.
	NewCode func() string
	// Now, for the join limits; tests set it. nil means time.Now.
	Now func() time.Time
}

const (
	DefaultFrameGap = 200 * time.Millisecond
	// how long a write may take before the game is given up on
	writeTimeout = 10 * time.Second
	// how long a game turned away has to hang up by itself
	lingerTimeout = 3 * time.Second
	// frames a game may have waiting to go out; more and it's too far behind
	outboxSize = 32
)

// Defaults are the settings the command line starts from.
func Defaults() Config {
	return Config{
		RequireProof:   true,
		AllowConsoles:  true,
		Ping:           10 * time.Second,
		Timeout:        30 * time.Second,
		LoginTimeout:   30 * time.Second,
		MaxConns:       1000,
		MaxConnsPerIP:  16,
		ConnJoinMisses: 5,
		IPJoinMisses:   20,
	}
}

// Validate checks the settings fit together.
func (c *Config) Validate() error {
	if c.RequireProof && len(c.Addresses) == 0 {
		return errors.New("proofs are on, so the server needs its address: the host name players " +
			"enter as their Rooms server (or turn proofs off)")
	}
	for _, a := range c.Addresses {
		if a == "" || strings.ContainsAny(a, " \t\r\n") {
			return errors.New("an address can't be blank or hold spaces")
		}
	}
	if c.Ping < 0 || c.Timeout < 0 || c.LoginTimeout < 0 {
		return errors.New("times can't be negative")
	}
	if c.Ping > 0 && c.Timeout > 0 && c.Timeout <= c.Ping {
		return errors.New("the timeout must be longer than the ping interval, or every game is dropped")
	}
	if c.MaxConns < 0 || c.MaxConnsPerIP < 0 || c.ConnJoinMisses < 0 || c.IPJoinMisses < 0 {
		return errors.New("limits can't be negative")
	}
	if c.LANPublicIP.IsValid() && !c.LANPublicIP.Is4() {
		return errors.New("the LAN public IP must be an IPv4 address")
	}
	return nil
}

// versionAllowed is whether a client's hello version is let in, and if not, why.
func (c *Config) versionAllowed(version string) (bool, string) {
	v := strings.ToLower(version)
	for _, p := range c.DenyVersions {
		if strings.HasPrefix(v, strings.ToLower(p)) {
			return false, "version is on the deny list (" + p + ")"
		}
	}
	if len(c.AllowVersions) == 0 {
		return true, ""
	}
	for _, p := range c.AllowVersions {
		if strings.HasPrefix(v, strings.ToLower(p)) {
			return true, ""
		}
	}
	return false, "version isn't on the allow list"
}
