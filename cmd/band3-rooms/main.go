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

	var health *http.Server
	healthErr := make(chan error, 1)
	if o.health != "" {
		hl, err := net.Listen("tcp", o.health)
		if err != nil {
			ln.Close()
			fmt.Fprintf(stderr, "band3-rooms: can't listen on %s for health: %v\n", o.health, err)
			return 1
		}
		health = &http.Server{Handler: healthHandler(srv), ReadHeaderTimeout: 5 * time.Second}
		logger(fmt.Sprintf("health on http://%s/healthz", hl.Addr()))
		go func() { healthErr <- health.Serve(hl) }()
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
	case err := <-healthErr:
		logger(fmt.Sprintf("health endpoint stopped: %v", err))
		code = 1
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if health != nil {
		_ = health.Shutdown(shutdown)
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

func healthHandler(srv *server.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
			server.Stats
		}{"ok", srv.Stats()})
	})
	return mux
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
