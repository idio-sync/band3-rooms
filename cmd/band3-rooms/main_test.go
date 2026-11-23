package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/idio-sync/band3-rooms/internal/protocol"
	"github.com/idio-sync/band3-rooms/internal/server"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	o, err := parseArgs([]string{"-address", "rooms.example.com"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := server.Defaults()
	want.Addresses = []string{"rooms.example.com"}
	if !reflect.DeepEqual(o.cfg, want) || o.listen != "0.0.0.0:19532" || o.health != "" || o.stats != "" {
		t.Fatalf("%+v", o)
	}
}

func TestEnvironmentAndFlags(t *testing.T) {
	o, err := parseArgs([]string{"-ping", "5", "-deny-versions", "x"}, env(map[string]string{
		"ROOMS_LISTEN":           "127.0.0.1:2000",
		"ROOMS_ADDRESS":          " rooms.example.com , 203.0.113.7 ",
		"ROOMS_REQUIRE_PROOF":    "true",
		"ROOMS_ALLOW_CONSOLES":   "0",
		"ROOMS_PING":             "20s", // the flag wins
		"ROOMS_TIMEOUT":          "1m",
		"ROOMS_LOGIN_TIMEOUT":    "15",
		"ROOMS_MAX_CONNS":        "50",
		"ROOMS_MAX_CONNS_PER_IP": "4",
		"ROOMS_CONN_JOIN_MISSES": "3",
		"ROOMS_IP_JOIN_MISSES":   "9",
		"ROOMS_ALLOW_VERSIONS":   "idio-sync/band3,3.",
		"ROOMS_DENY_VERSIONS":    "y", // the flag replaces it
		"ROOMS_LAN_PUBLIC_IP":    "198.51.100.2",
		"ROOMS_HEALTH":           "127.0.0.1:19580",
		"ROOMS_STATS":            "127.0.0.1:19533",
		"ROOMS_HEX":              "true",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c := o.cfg
	if o.listen != "127.0.0.1:2000" || o.health != "127.0.0.1:19580" || o.stats != "127.0.0.1:19533" {
		t.Fatalf("%+v", o)
	}
	if !reflect.DeepEqual(c.Addresses, []string{"rooms.example.com", "203.0.113.7"}) ||
		!c.RequireProof || c.AllowConsoles || c.Ping != 5*time.Second || c.Timeout != time.Minute ||
		c.LoginTimeout != 15*time.Second || c.MaxConns != 50 || c.MaxConnsPerIP != 4 ||
		c.ConnJoinMisses != 3 || c.IPJoinMisses != 9 || !c.Hex ||
		!reflect.DeepEqual(c.AllowVersions, []string{"idio-sync/band3", "3."}) ||
		!reflect.DeepEqual(c.DenyVersions, []string{"x"}) ||
		c.LANPublicIP != netip.MustParseAddr("198.51.100.2") {
		t.Fatalf("%+v", c)
	}
}

func TestEveryFlagWithAnEnvironmentVariableExists(t *testing.T) {
	var out bytes.Buffer
	if _, err := parseArgs([]string{"-h"}, env(nil), &out); err == nil {
		t.Fatal("-h")
	}
	for flag, name := range envNames {
		if !strings.Contains(out.String(), "-"+flag+" ("+name+")") {
			t.Errorf("usage is missing -%s (%s)", flag, name)
		}
	}
}

func TestBadSettings(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{nil, nil, "needs its address"},
		{[]string{"-require-proof=false", "-ping", "-1"}, nil, "negative"},
		{[]string{"-require-proof=false", "-ping", "soon"}, nil, "not a duration"},
		{[]string{"-require-proof=false", "-ping", "30s", "-timeout", "30s"}, nil, "longer than the ping"},
		{[]string{"-require-proof=false"}, map[string]string{"ROOMS_LAN_PUBLIC_IP": "::1"}, "ROOMS_LAN_PUBLIC_IP"},
		{[]string{"-require-proof=false"}, map[string]string{"ROOMS_MAX_CONNS": "lots"}, "ROOMS_MAX_CONNS"},
		{[]string{"-require-proof=false", "extra"}, nil, "unexpected argument"},
	} {
		_, err := parseArgs(tc.args, env(tc.env), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v %v: %v, want %q", tc.args, tc.env, err, tc.want)
		}
	}
}

func TestProofsOffNeedNoAddress(t *testing.T) {
	o, err := parseArgs(nil, env(map[string]string{"ROOMS_REQUIRE_PROOF": "false"}), io.Discard)
	if err != nil || o.cfg.RequireProof {
		t.Fatal(err)
	}
}

func TestRunRejectsBadSettings(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(nil, env(nil), io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "address") {
		t.Fatalf("%d %q", code, stderr.String())
	}
	var stdout bytes.Buffer
	if code := run([]string{"-version"}, env(nil), &stdout, io.Discard); code != 0 || stdout.Len() == 0 {
		t.Fatal(code)
	}
}

func TestHealth(t *testing.T) {
	srv, err := server.New(server.Config{FrameGap: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	ts := httptest.NewServer(healthHandler(srv))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || got["status"] != "ok" || got["connections"] != 0.0 || got["logged_in"] != 0.0 {
		t.Fatalf("%d %v", resp.StatusCode, got)
	}
	addr := strings.TrimPrefix(ts.URL, "http://")
	if code := healthcheck(addr, io.Discard); code != 0 {
		t.Fatal(code)
	}
	_, port, _ := net.SplitHostPort(addr)
	if code := healthcheck("0.0.0.0:"+port, io.Discard); code != 0 {
		t.Fatal("0.0.0.0 isn't asked as 127.0.0.1")
	}
	ts.Close()
	if code := healthcheck(addr, io.Discard); code != 1 {
		t.Fatal(code)
	}
	if code := healthcheck("", io.Discard); code != 2 {
		t.Fatal(code)
	}
}

func getStats(t *testing.T, url string) publicStats {
	t.Helper()
	resp, err := http.Get(url + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	var got publicStats
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// waitStats asks until the stats are want, for a second at most.
func waitStats(t *testing.T, url string, want publicStats) {
	t.Helper()
	var got publicStats
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got = getStats(t, url); got == want {
			return
		}
	}
	t.Fatalf("%+v, want %+v", got, want)
}

func TestStats(t *testing.T) {
	srv, err := server.New(server.Config{FrameGap: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	ts := httptest.NewServer(statsHandler(srv, "v9.9.9"))
	defer ts.Close()

	waitStats(t, ts.URL, publicStats{Version: "v9.9.9"})

	// a game that's connected but hasn't logged in isn't counted
	nc, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_, _ = nc.Write(protocol.EncodeClientHello(protocol.ClientHello{Emulator: true, Language: "eng", Version: "rb3e-test"}))
	var r protocol.FrameReader
	next := func() (uint8, []byte) {
		buf := make([]byte, 1024)
		_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if typ, body, ok := r.Next(); ok {
				return typ, body
			}
			n, err := nc.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			r.Feed(buf[:n])
		}
	}
	if typ, _ := next(); typ != protocol.ServerHelloType {
		t.Fatal(typ)
	}
	waitStats(t, ts.URL, publicStats{Version: "v9.9.9"})

	login := protocol.ClientLogin{XUID: protocol.RoomsXUID("host"), Gamertag: "host", LocalIP: netip.MustParseAddr("192.168.1.2")}
	_, _ = nc.Write(protocol.EncodeClientLogin(login))
	if typ, _ := next(); typ != protocol.LoggedInType {
		t.Fatal(typ)
	}
	waitStats(t, ts.URL, publicStats{Version: "v9.9.9", PlayersOnline: 1, OpenRooms: 1})

	nc.Close()
	waitStats(t, ts.URL, publicStats{Version: "v9.9.9"})

	// it only reads
	resp, err := http.Post(ts.URL+"/stats", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal(resp.Status)
	}
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatal("the stats endpoint answers /healthz:", resp.Status)
	}
}

func TestWebEndpoints(t *testing.T) {
	srv, err := server.New(server.Config{FrameGap: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	o := options{health: "127.0.0.1:1", stats: "127.0.0.1:2"}
	o.cfg.Version = "v1"
	if eps := webEndpoints(o, srv); len(eps) != 2 || eps[0].what != "health" || eps[1].what != "stats" {
		t.Fatalf("%+v", eps)
	}
	if eps := webEndpoints(options{}, srv); len(eps) != 0 {
		t.Fatalf("on by default: %+v", eps)
	}
	// on one address, one listener answers both
	o.stats = o.health
	eps := webEndpoints(o, srv)
	if len(eps) != 1 || eps[0].what != "health and stats" {
		t.Fatalf("%+v", eps)
	}
	ts := httptest.NewServer(eps[0].handler)
	defer ts.Close()
	if got := getStats(t, ts.URL); got != (publicStats{Version: "v1"}) {
		t.Fatalf("%+v", got)
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
}
