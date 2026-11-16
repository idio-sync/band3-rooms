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
	if !reflect.DeepEqual(o.cfg, want) || o.listen != "0.0.0.0:19532" || o.health != "" {
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
		"ROOMS_HEX":              "true",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c := o.cfg
	if o.listen != "127.0.0.1:2000" || o.health != "127.0.0.1:19580" {
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
