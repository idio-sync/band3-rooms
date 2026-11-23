// Command band3-rooms is a Liveless Rooms server for band3 and RB3Enhanced:
// games log in for a code, and a game that sends another's code is told where
// to find it. It never carries game traffic.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/idio-sync/band3-rooms/internal/server"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "band3-rooms: %v\n", err)
		return 2
	}
	if o.version {
		fmt.Fprintln(stdout, buildVersion())
		return 0
	}
	if o.healthcheck {
		return healthcheck(o.health, stderr)
	}

	logger := newLogger(stdout)
	o.cfg.Version = buildVersion()
	srv, err := server.New(o.cfg, logger)
	if err != nil {
		fmt.Fprintf(stderr, "band3-rooms: %v\n", err)
		return 2
	}
	ln, err := net.Listen("tcp4", o.listen)
	if err != nil {
		fmt.Fprintf(stderr, "band3-rooms: can't listen on %s: %v\n", o.listen, err)
		return 1
	}
	logger(fmt.Sprintf("band3-rooms %s listening on %s; %s", buildVersion(), ln.Addr(), describe(o.cfg)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// the health and stats endpoints, each on its own address, or sharing one
	var webs []*http.Server
	webErr := make(chan error, 1)
	for _, w := range webEndpoints(o, srv) {
		wl, err := net.Listen("tcp", w.addr)
		if err != nil {
			for _, s := range webs {
				s.Close()
			}
			ln.Close()
			fmt.Fprintf(stderr, "band3-rooms: can't listen on %s for %s: %v\n", w.addr, w.what, err)
			return 1
		}
		web := &http.Server{Handler: w.handler, ReadHeaderTimeout: 5 * time.Second}
		webs = append(webs, web)
		for _, path := range w.paths {
			logger(fmt.Sprintf("%s on http://%s%s", w.what, wl.Addr(), path))
		}
		go func() { webErr <- web.Serve(wl) }()
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	code := 0
	select {
	case <-ctx.Done():
		logger("stopping")
	case err := <-served:
		logger(fmt.Sprintf("serving stopped: %v", err))
		code = 1
	case err := <-webErr:
		logger(fmt.Sprintf("HTTP endpoint stopped: %v", err))
		code = 1
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, web := range webs {
		_ = web.Shutdown(shutdown)
	}
	if err := srv.Shutdown(shutdown); err != nil {
		logger(fmt.Sprintf("connections still closing after 10s: %v", err))
		return 1
	}
	logger("stopped")
	return code
}

// newLogger writes one timestamped line per call, from any goroutine.
func newLogger(w io.Writer) func(string) {
	var mu sync.Mutex
	return func(line string) {
		stamp := time.Now().Format("2006-01-02 15:04:05.000")
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s %s\n", stamp, line)
	}
}

// describe is the settings that matter, for the startup line.
func describe(c server.Config) string {
	var parts []string
	if c.RequireProof {
		parts = append(parts, fmt.Sprintf("proofs signed for %q", strings.Join(c.Addresses, `", "`)))
	} else {
		parts = append(parts, "proofs off")
	}
	if !c.AllowConsoles {
		parts = append(parts, "no consoles")
	}
	if c.Ping > 0 {
		parts = append(parts, fmt.Sprintf("ping %v", c.Ping))
		if c.Timeout > 0 {
			parts = append(parts, fmt.Sprintf("timeout %v", c.Timeout))
		}
	} else {
		parts = append(parts, "no pings")
	}
	if len(c.AllowVersions) > 0 {
		parts = append(parts, fmt.Sprintf("only versions %q", c.AllowVersions))
	}
	if len(c.DenyVersions) > 0 {
		parts = append(parts, fmt.Sprintf("not versions %q", c.DenyVersions))
	}
	if c.LANPublicIP.IsValid() {
		parts = append(parts, fmt.Sprintf("LAN games' public IP %s", c.LANPublicIP))
	}
	return strings.Join(parts, ", ")
}

// webEndpoint is one HTTP listener: the health endpoint, the stats endpoint,
// or both when they're set to the same address.
type webEndpoint struct {
	addr, what string
	paths      []string
	handler    http.Handler
}

func webEndpoints(o options, srv *server.Server) []webEndpoint {
	var eps []webEndpoint
	mux := func(addr, what, path string, add func(*http.ServeMux)) {
		for i := range eps {
			if eps[i].addr == addr {
				add(eps[i].handler.(*http.ServeMux))
				eps[i].what += " and " + what
				eps[i].paths = append(eps[i].paths, path)
				return
			}
		}
		m := http.NewServeMux()
		add(m)
		eps = append(eps, webEndpoint{addr: addr, what: what, paths: []string{path}, handler: m})
	}
	if o.health != "" {
		mux(o.health, "health", "/healthz", func(m *http.ServeMux) { addHealth(m, srv) })
	}
	if o.stats != "" {
		mux(o.stats, "stats", "/stats", func(m *http.ServeMux) { addStats(m, srv, o.cfg.Version) })
	}
	return eps
}

func healthHandler(srv *server.Server) http.Handler {
	mux := http.NewServeMux()
	addHealth(mux, srv)
	return mux
}

func addHealth(mux *http.ServeMux, srv *server.Server) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
			server.Stats
		}{"ok", srv.Stats()})
	})
}

// publicStats is what the stats endpoint answers, for a public status page.
type publicStats struct {
	Version       string `json:"version"`
	PlayersOnline int    `json:"players_online"`
	OpenRooms     int    `json:"open_rooms"`
}

func statsHandler(srv *server.Server, version string) http.Handler {
	mux := http.NewServeMux()
	addStats(mux, srv, version)
	return mux
}

// addStats serves GET /stats: the server's version, the games logged in, and
// the rooms open. Each logged-in game holds one room code that others can join
// it by, so the two counts are the same today; they're kept apart so a page
// needn't change if that ever does. Nothing about who is on is given out.
func addStats(mux *http.ServeMux, srv *server.Server, version string) {
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		st := srv.Stats()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(publicStats{
			Version:       version,
			PlayersOnline: st.LoggedIn,
			OpenRooms:     st.LoggedIn,
		})
	})
}

// healthcheck asks a running server's health endpoint, for Docker's HEALTHCHECK.
func healthcheck(addr string, stderr io.Writer) int {
	if addr == "" {
		fmt.Fprintln(stderr, "band3-rooms: -healthcheck needs -health (ROOMS_HEALTH)")
		return 2
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(stderr, "band3-rooms: %v\n", err)
		return 2
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "band3-rooms: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "band3-rooms: health: %s\n", resp.Status)
		return 1
	}
	return 0
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return version
}
