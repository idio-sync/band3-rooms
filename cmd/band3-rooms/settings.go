package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/idio-sync/band3-rooms/internal/server"
)

// options is what the command line and environment set.
type options struct {
	cfg         server.Config
	listen      string
	health      string
	healthcheck bool
	version     bool
}

// envNames maps each flag that has one to its environment variable.
var envNames = map[string]string{
	"listen":           "ROOMS_LISTEN",
	"address":          "ROOMS_ADDRESS",
	"require-proof":    "ROOMS_REQUIRE_PROOF",
	"allow-consoles":   "ROOMS_ALLOW_CONSOLES",
	"ping":             "ROOMS_PING",
	"timeout":          "ROOMS_TIMEOUT",
	"login-timeout":    "ROOMS_LOGIN_TIMEOUT",
	"max-conns":        "ROOMS_MAX_CONNS",
	"max-conns-per-ip": "ROOMS_MAX_CONNS_PER_IP",
	"conn-join-misses": "ROOMS_CONN_JOIN_MISSES",
	"ip-join-misses":   "ROOMS_IP_JOIN_MISSES",
	"allow-versions":   "ROOMS_ALLOW_VERSIONS",
	"deny-versions":    "ROOMS_DENY_VERSIONS",
	"lan-public-ip":    "ROOMS_LAN_PUBLIC_IP",
	"health":           "ROOMS_HEALTH",
	"hex":              "ROOMS_HEX",
}

// parseArgs reads settings from the environment, then the command line, which wins.
func parseArgs(args []string, getenv func(string) string, out io.Writer) (options, error) {
	o := options{cfg: server.Defaults(), listen: fmt.Sprintf("0.0.0.0:%d", 19532)}
	fs := flag.NewFlagSet("band3-rooms", flag.ContinueOnError)
	fs.SetOutput(out)
	c := &o.cfg

	fs.StringVar(&o.listen, "listen", o.listen, "the TCP address to listen on, host:port (IPv4)")
	fs.Var((*listValue)(&c.Addresses), "address",
		"the server's host name exactly as players enter it in their game, which emulator login "+
			"proofs are signed with; comma-separated for more than one (needed with -require-proof)")
	fs.BoolVar(&c.RequireProof, "require-proof", c.RequireProof,
		"ask games for a login proof and check emulators' ones against -address")
	fs.BoolVar(&c.AllowConsoles, "allow-consoles", c.AllowConsoles,
		"take logins from real Xbox 360s, whose signed proofs this server can't check")
	fs.Var((*durationValue)(&c.Ping), "ping", "how often each game is pinged; 0: never (and no timeout)")
	fs.Var((*durationValue)(&c.Timeout), "timeout", "drop a game that has sent nothing for this long; 0: never")
	fs.Var((*durationValue)(&c.LoginTimeout), "login-timeout", "drop a connection not logged in after this long; 0: never")
	fs.IntVar(&c.MaxConns, "max-conns", c.MaxConns, "connections at once; 0: no limit")
	fs.IntVar(&c.MaxConnsPerIP, "max-conns-per-ip", c.MaxConnsPerIP, "connections at once from one IP address; 0: no limit")
	fs.IntVar(&c.ConnJoinMisses, "conn-join-misses", c.ConnJoinMisses,
		"failed joins (no such code) one connection may make per minute; 0: no limit")
	fs.IntVar(&c.IPJoinMisses, "ip-join-misses", c.IPJoinMisses,
		"failed joins all connections from one IP address may make per minute; 0: no limit")
	fs.Var((*listValue)(&c.AllowVersions), "allow-versions",
		"comma-separated client version prefixes; if set, only versions starting with one are let in")
	fs.Var((*listValue)(&c.DenyVersions), "deny-versions",
		"comma-separated client version prefixes to turn away (wins over -allow-versions)")
	fs.Var((*ipValue)(&c.LANPublicIP), "lan-public-ip",
		"the public IPv4 to give games connecting from a private or loopback address (the server's own)")
	fs.StringVar(&o.health, "health", "", "host:port for an HTTP health endpoint (GET /healthz); empty: off")
	fs.BoolVar(&c.Hex, "hex", false, "log every frame in and out in hex")
	fs.BoolVar(&o.healthcheck, "healthcheck", false, "check the -health endpoint of a running server and exit 0 if it's up")
	fs.BoolVar(&o.version, "version", false, "print the version and exit")

	fs.Usage = func() {
		fmt.Fprintf(out, "band3-rooms: a Liveless Rooms server.\n\nUsage: band3-rooms [flags]\n\n")
		fs.VisitAll(func(f *flag.Flag) {
			env := ""
			if name, ok := envNames[f.Name]; ok {
				env = " (" + name + ")"
			}
			def := ""
			if f.DefValue != "" && f.DefValue != "false" {
				def = " (default " + f.DefValue + ")"
			}
			fmt.Fprintf(out, "  -%s%s\n    \t%s%s\n", f.Name, env, f.Usage, def)
		})
		fmt.Fprintf(out, "\nTimes take Go durations (10s, 1m30s) or plain seconds.\n")
	}

	for name, env := range envNames {
		if value, ok := lookup(getenv, env); ok {
			if err := fs.Set(name, value); err != nil {
				return o, fmt.Errorf("%s=%q: %v", env, value, err)
			}
		}
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.version || o.healthcheck {
		return o, nil
	}
	if err := o.cfg.Validate(); err != nil {
		return o, err
	}
	return o, nil
}

// lookup is an environment variable, unless it's empty.
func lookup(getenv func(string) string, name string) (string, bool) {
	v := strings.TrimSpace(getenv(name))
	return v, v != ""
}

// durationValue takes a Go duration or plain seconds.
type durationValue time.Duration

func (d *durationValue) String() string { return time.Duration(*d).String() }

func (d *durationValue) Set(s string) error {
	s = strings.TrimSpace(s)
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		if secs < 0 {
			return errors.New("can't be negative")
		}
		*d = durationValue(time.Duration(secs * float64(time.Second)))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return errors.New("not a duration (10s, 1m) or seconds")
	}
	if v < 0 {
		return errors.New("can't be negative")
	}
	*d = durationValue(v)
	return nil
}

// listValue is comma-separated; each Set replaces the whole list.
type listValue []string

func (l *listValue) String() string { return strings.Join(*l, ",") }

func (l *listValue) Set(s string) error {
	var items []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	*l = items
	return nil
}

type ipValue netip.Addr

func (a *ipValue) String() string {
	if v := netip.Addr(*a); v.IsValid() {
		return v.String()
	}
	return ""
}

func (a *ipValue) Set(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*a = ipValue(netip.Addr{})
		return nil
	}
	v, err := netip.ParseAddr(s)
	if err != nil || !v.Is4() {
		return errors.New("not an IPv4 address")
	}
	*a = ipValue(v)
	return nil
}
