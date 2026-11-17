# band3-rooms

A Liveless Rooms server for [band3](https://github.com/idio-sync/band3_recomp), the PC
recompilation of Rock Band 3, and for RB3Enhanced. It speaks RB3Enhanced's Liveless Rooms
protocol, so band3 and RB3Enhanced (on Xenia or a real Xbox 360) can both use it.

RBEnhanced runs a Rooms server for RB3Enhanced players. band3 players need one of their
own: this is it, small enough to run at home.

## What it does, and what it doesn't

Each game logs in over TCP and gets a short room code (five letters and digits, like
`K7MQX`). A player who enters someone's code is told that game's public address and its
address on its own network; the host's game is told to send a packet towards the joiner.
The games then connect to each other directly, on UDP 9103.

That's all it does, but for two things for band3 (the [port check](#the-port-check) and
[connection test](#the-connection-test)). It never carries game traffic, so:

- **Whoever hosts needs UDP 9103 reachable** from the players joining them: forwarded on
  their router (band3 can do it by itself with UPnP), as without Rooms. band3 asks the
  server to check that it is.
- Players behind the same public address as the host are sent to the host's LAN address.
  The server sends both addresses and the game picks: band3 and RB3Enhanced both use the
  LAN one when the host's public address is their own.
- Nothing is saved. Codes, names and addresses live in memory while a game is connected
  and in the log; a restart forgets everything, and every game gets a new code when it
  logs in again (band3 reconnects by itself).

## Running it

The server listens on **TCP 19532**. Forward that port from your router to it.

### Docker

```sh
cp .env.example .env    # set ROOMS_ADDRESS
docker compose up -d
```

The image is `ghcr.io/idio-sync/band3-rooms` (amd64 and arm64): a static binary on
distroless, running as a non-root user, with nothing to mount.

The server tells each game the address it came from, so it has to see games' real
addresses. Docker's published ports keep them for connections from other machines. If the
log shows every game coming from one `172.x` address, use `network_mode: host` (in
`docker-compose.yml`) instead.

### A Linux server, without Docker

The lightest way onto a small VPS (a 1 GB free-tier VM included): each
[release](https://github.com/idio-sync/band3-rooms/releases) has the static binary for
`amd64` and `arm64`, a systemd unit and the settings file. As root, on `amd64` (`arm64`
for ARM machines):

```sh
base=https://github.com/idio-sync/band3-rooms/releases/latest/download
curl -fL -o /usr/local/bin/band3-rooms $base/band3-rooms-linux-amd64
chmod 755 /usr/local/bin/band3-rooms
curl -fL -o /etc/systemd/system/band3-rooms.service $base/band3-rooms.service
curl -fL -o /etc/band3-rooms.env $base/band3-rooms.env
nano /etc/band3-rooms.env         # set ROOMS_ADDRESS
systemctl daemon-reload
systemctl enable --now band3-rooms
journalctl -u band3-rooms         # "listening on 0.0.0.0:19532"
```

It runs as a throwaway user with no access to the rest of the system, restarts if it
stops, and starts at boot. The release's `SHA256SUMS` lists each file's checksum, to
compare with `sha256sum /usr/local/bin/band3-rooms`. To update,
download the binary again and `systemctl restart band3-rooms`; after changing the settings
file, restart it too.

### Unraid

Docker tab → **Add Container** → **Template**: paste
`https://raw.githubusercontent.com/idio-sync/band3-rooms/main/unraid/band3-rooms.xml`
in the template URL box (or copy the file to
`/boot/config/plugins/dockerMan/templates-user/my-band3-rooms.xml`). Fill in **Address**,
and **LAN public IP** if you'll play on the same network as the server, then Apply.

### The binary

```sh
go build -o band3-rooms ./cmd/band3-rooms
./band3-rooms -address rooms.example.com
```

Go 1.24 or newer, no other dependencies. `./band3-rooms -h` lists every setting.

## Pointing games at it

**band3:** set `liveless_rooms_server` (**Rooms server**, Settings > Online) to the server's
host name **exactly as the server has it in `ROOMS_ADDRESS`**, plus `:port` if it isn't on
19532 (`rooms.example.com`, or `rooms.example.com:20000`). Restart band3.

**RB3Enhanced:** set `LivelessAddress` in `rb3.ini` to the same name. RB3Enhanced always
connects on port 19532, so a server for RB3Enhanced players has to be on 19532.

### Why the name has to match

Each game signs its login with the server name it was given (an HMAC of the name and its
XUID, keyed with a random key the server sends). The server checks the signature against
its own `ROOMS_ADDRESS` and hangs up if it doesn't match. If players connect to
`rooms.example.com` but the server is set to `203.0.113.7`, every login is refused; band3
shows *login refused: check that liveless_rooms_server matches the server's address*.

The match is exact, case included. If players will use more than one name (a host name and
an IP address, say), list them all: `ROOMS_ADDRESS=rooms.example.com,203.0.113.7`.

This isn't a password: anyone who knows the name can make the signature. It catches games
pointed at the server under a name it doesn't answer to. Turn it off with
`ROOMS_REQUIRE_PROOF=false`, and `ROOMS_ADDRESS` isn't needed.

A real Xbox 360 signs with its console key, which this server can't check; such logins are
taken unless `ROOMS_ALLOW_CONSOLES=false`. Emulators (band3, Xenia) sign with the plain
hash, which is checked.

### Players on the server's own network

A game that connects from the server's LAN shows up with its LAN address, which is no use
to a player joining from outside. Set `ROOMS_LAN_PUBLIC_IP` to your public IPv4 and games
connecting from a private, loopback or link-local address are given that instead. Players
outside then reach the host through your router, and players inside still go over the LAN
(their public address matches the host's).

## Choosing which clients get in

Every game sends a version string when it connects, and the log shows it:

- band3 sends `idio-sync/band3 ` and its build tag (`git describe` of its source), cut to
  47 characters
- RB3Enhanced sends its build tag

`ROOMS_ALLOW_VERSIONS` and `ROOMS_DENY_VERSIONS` are comma-separated prefixes of that
string, compared without regard to case:

| To | Set |
|---|---|
| let only band3 in | `ROOMS_ALLOW_VERSIONS=idio-sync/band3` |
| keep band3 out | `ROOMS_DENY_VERSIONS=idio-sync/band3` |
| let everyone in | neither (the default) |

The deny list wins over the allow list. A game that isn't let in is told so in the
protocol's own way (a server hello that doesn't allow it) and the server hangs up. band3
shows *the server turned the connection away* and doesn't try again. The version string is
whatever the client says it is, so this sorts honest clients; it doesn't stop anyone who
sets out to get around it.

## The port check

A host whose UDP port is closed only finds out when a friend's join fails on the friend's
side. So band3 asks the server to check: each time the game goes online (and when the
player picks **CHECK PORT** in its Rooms menu), it sends the port it plays on and a random
16-byte token, and the server sends one UDP datagram, `B3PC` and the token, to that port
**on the address the game's connection comes from**, never any other. The game says the
port is open if the datagram arrives within 3 seconds, and closed if it doesn't.

- **Only band3.** It's band3's own addition to RB3Enhanced's protocol, which has no room for
  it: right after logging a game in, the server tells it it checks ports (a packet of type
  `0xB0`), but only when the game's version starts with `idio-sync/band3` (any case).
  RB3Enhanced closes its connection on a packet it doesn't know, so it's never sent one,
  and band3 asks (type `0xB1`) only a server that said it checks, so it never asks
  RBEnhanced's server or an older band3-rooms. Anything else that sends the request gets
  what any unknown packet gets: a log line, and nothing else.
- **Limits.** One check per connection per 30 seconds; more are ignored (and logged). Port
  0 is ignored. The datagram goes out from a port the system picks; nothing answers it, so
  the server needs no inbound port beyond TCP 19532.
- **On the server's own network** a game connects from its LAN address, so the datagram goes
  to that and arrives whatever the router does: the check means something for players
  outside.

The log has a line for each one: `port check #1 ("alice") -> 198.51.100.20:9103`.

**Checking without the game:** `scripts/port-check.ps1` (Windows PowerShell 5.1 or
PowerShell 7) logs in as a band3 client, shows the room code and the public address the
server sees, and runs the check on this PC:

```powershell
powershell -ExecutionPolicy Bypass -File scripts\port-check.ps1 -Server rooms.example.com
```

It listens on UDP 9103 itself (`-UdpPort` for another), so close band3 first. That also
means a port band3 opens on the router by itself (UPnP) isn't open while it runs: it checks
a port forwarded by hand. Allow PowerShell through Windows Firewall if asked, or the port
reads as closed. It says when the server doesn't check ports, and when `-Server` isn't the
name the server signs logins for.

## The connection test

band3's Settings has **Test connection** under its Rooms server: it connects, says hello as
the game does, and never logs in, so it takes no code and registers no name. The server
tells band3 what it is right after the hello (a packet of type `0xB2`: its version, as
`-version` prints it, and what it does beyond RB3Enhanced's protocol), and band3 times a
few echoes (it sends type `0xB3` with a number, the server sends it straight back as type
`0xB4`). band3 shows the round trips' median as its ping, with the version.

As with the port check, only a game whose version starts with `idio-sync/band3` is sent
the server's info, and band3 sends echoes only to a server that said it answers them; from
anything else an echo is an unknown packet. Older band3 builds that log in are sent the info
too, and ignore it. A connection is answered 32 echoes; more are ignored.

## Settings

Each setting is a flag and an environment variable; the flag wins. Times take Go durations
(`10s`, `1m30s`) or plain seconds. A `0` limit or time means none.

| Flag | Variable | Default | |
|---|---|---|---|
| `-listen` | `ROOMS_LISTEN` | `0.0.0.0:19532` | TCP address to listen on (IPv4) |
| `-address` | `ROOMS_ADDRESS` | | the server's host name as players enter it, which login proofs are signed with; comma-separated for more than one. Needed while proofs are on |
| `-require-proof` | `ROOMS_REQUIRE_PROOF` | `true` | ask games for a login proof and check emulators' ones |
| `-allow-consoles` | `ROOMS_ALLOW_CONSOLES` | `true` | take logins from real Xbox 360s, whose proofs can't be checked |
| `-lan-public-ip` | `ROOMS_LAN_PUBLIC_IP` | | public IPv4 given to games connecting from a private or loopback address |
| `-allow-versions` | `ROOMS_ALLOW_VERSIONS` | | only let in client versions starting with one of these |
| `-deny-versions` | `ROOMS_DENY_VERSIONS` | | turn away client versions starting with one of these |
| `-ping` | `ROOMS_PING` | `10s` | how often each game is pinged; `0` turns pings and the timeout off |
| `-timeout` | `ROOMS_TIMEOUT` | `30s` | drop a game that has sent nothing for this long; longer than `-ping` |
| `-login-timeout` | `ROOMS_LOGIN_TIMEOUT` | `30s` | drop a connection that hasn't logged in after this long |
| `-max-conns` | `ROOMS_MAX_CONNS` | `1000` | connections at once |
| `-max-conns-per-ip` | `ROOMS_MAX_CONNS_PER_IP` | `16` | connections at once from one IP address |
| `-conn-join-misses` | `ROOMS_CONN_JOIN_MISSES` | `5` | failed joins (no such code) one connection may make per minute |
| `-ip-join-misses` | `ROOMS_IP_JOIN_MISSES` | `20` | failed joins all connections from one IP address may make per minute |
| `-health` | `ROOMS_HEALTH` | | `host:port` for the health endpoint; off when blank (the Docker image sets `127.0.0.1:19580`) |
| `-hex` | `ROOMS_HEX` | `false` | log every packet in and out in hex |
| `-healthcheck` | | | ask a running server's health endpoint and exit 0 if it answers (Docker's `HEALTHCHECK`) |
| `-version` | | | print the version and exit |

Games hang up on a server they haven't heard from in 30 seconds, so keep `-ping` well under
that.

## Limits

Codes are short, so the server stops anyone working through them: a join to a code that
doesn't exist counts against the connection (`-conn-join-misses`) and against its IP address
across all its connections and reconnections (`-ip-join-misses`). Each allows that many a
minute, refilling steadily. Over either, joins are refused without looking the code up, so
guessing on learns nothing. Joins that find their game don't count.

Port checks are one per connection per 30 seconds ([The port check](#the-port-check)), and
echoes 32 per connection ([The connection test](#the-connection-test)).

Also: packets over 774 bytes (the protocol's limit), or anything that isn't the protocol,
close the connection. So do anything but a hello first, no login within `-login-timeout`,
nothing heard within `-timeout`, and a game so far behind that 32 packets wait for it.
Connections over `-max-conns` or `-max-conns-per-ip` are closed as they arrive; band3 tries
again later by itself.

## Logs

One line per event on stdout, nothing on disk:

```
2026-11-16 11:51:56.324 band3-rooms v1.0.0 listening on 0.0.0.0:19532; proofs signed for "rooms.example.com", ping 10s, timeout 30s
2026-11-16 11:51:56.920 connect #1 from 198.51.100.20:54480
2026-11-16 11:51:56.921 hello #1: emulator, "eng", "idio-sync/band3 v0.4.0", protocol 0
2026-11-16 11:51:56.922 login #1 ("alice"): xuid 0009DF84DB89BDC5 local 192.168.1.2 public 198.51.100.20 code 2M3ER
2026-11-16 11:51:58.130 port check #1 ("alice") -> 198.51.100.20:9103
2026-11-16 11:51:57.337 join #2 ("bob") -> 2M3ER #1 ("alice"): public 198.51.100.20 local 192.168.1.2
2026-11-16 11:51:57.542 deny #2 ("bob") -> "ZZZZZ": no such code
2026-11-16 11:52:40.101 close #1 ("alice"): client disconnected
```

Other lines: `refuse` (over a connection limit), `turn away` (version), `bad proof`,
`ping timeout`, `port check from ...: too soon` (and the like, for checks ignored), and
`close` with its reason.

## Health

With `-health` set, `GET /healthz` answers `{"status":"ok","connections":3,"logged_in":2}`.
The Docker image turns it on inside the container only, for its `HEALTHCHECK`.

## Not in this version

- UDP hole punching or relaying for hosts who can't open UDP 9103. The server passes on the
  protocol's NAT punch request (the host's game sends one packet towards the joiner),
  checks band3's port ([The port check](#the-port-check)) and answers its connection test
  ([The connection test](#the-connection-test)), and that's all.
- Presence, friends lists, or joining from outside the game (the protocol's join type 1).
- Persistence: codes that survive a reconnect or a restart.
- Checking real Xbox 360s' signed proofs.

## Development

```sh
go vet ./... && go test ./...
```

`testdata/liveless_rooms_packets.txt` is band3's `tests/golden/liveless_rooms_packets.txt`,
copied unchanged: the packet vectors band3's C++ client and its Python mock server are held
to. The tests check every one, then run the server in-process against scripted clients.

`scripts/crosscheck.py` drives a running server with band3's own Python codec
(`tools/liveless_rooms_mock.py` from a band3 checkout): two logins (band3 and RB3Enhanced),
the server's info and an echo, a port check on this machine, and a join by code.

```sh
./band3-rooms -address 127.0.0.1 -listen 127.0.0.1:19532 &
python scripts/crosscheck.py ../band3_recomp
```

Images are built by GitHub Actions and pushed to `ghcr.io/idio-sync/band3-rooms`: `latest`
and `sha-<commit>` from `main`, and `1.2.3` and `1.2` from a `v1.2.3` tag. A `v1.2.3` tag
also makes that GitHub release, with the Linux binaries, `contrib/band3-rooms.service`,
`.env.example` as `band3-rooms.env`, and `SHA256SUMS`.

## License

[GPL-2.0](LICENSE), as band3.
