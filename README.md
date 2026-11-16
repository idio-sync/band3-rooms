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

That's all it does. It never carries game traffic, so:

- **Whoever hosts needs UDP 9103 reachable** from the players joining them: forwarded on
  their router (band3 can do it by itself with UPnP), as without Rooms.
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
2026-11-16 11:51:57.337 join #2 ("bob") -> 2M3ER #1 ("alice"): public 198.51.100.20 local 192.168.1.2
2026-11-16 11:51:57.542 deny #2 ("bob") -> "ZZZZZ": no such code
2026-11-16 11:52:40.101 close #1 ("alice"): client disconnected
```

Other lines: `refuse` (over a connection limit), `turn away` (version), `bad proof`,
`ping timeout`, and `close` with its reason.

## Health

With `-health` set, `GET /healthz` answers `{"status":"ok","connections":3,"logged_in":2}`.
The Docker image turns it on inside the container only, for its `HEALTHCHECK`.

## Not in this version

- UDP hole punching or relaying for hosts who can't open UDP 9103. The server passes on the
  protocol's NAT punch request (the host's game sends one packet towards the joiner), and
  that's all.
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
(`tools/liveless_rooms_mock.py` from a band3 checkout): two logins and a join by code.

```sh
./band3-rooms -address 127.0.0.1 -listen 127.0.0.1:19532 &
python scripts/crosscheck.py ../band3_recomp
```

Images are built by GitHub Actions and pushed to `ghcr.io/idio-sync/band3-rooms`: `latest`
and `sha-<commit>` from `main`, and `1.2.3` and `1.2` from a `v1.2.3` tag.

## License

[GPL-2.0](LICENSE), as band3.
