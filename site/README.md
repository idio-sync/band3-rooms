# rockband.idiosynchronic.com status page

One static page that shows whether band3's two servers on the VM are up, GoCentral's
numbers and most popular songs, and how to connect. Caddy serves it and passes two
read-only JSON calls through to the servers on the same machine:

| Path | Goes to | |
|---|---|---|
| `/api/stats` | GoCentral's `GET /stats` on `127.0.0.1:8080` | scores, machines, setlists, characters, bands, active gatherings, top 3 songs |
| `/api/rooms` | band3-rooms' `GET /stats` on `127.0.0.1:19533` (`ROOMS_STATS`) | version, players online, open rooms |

Nothing else of GoCentral's REST API (which has admin endpoints) is reachable: any other
`/api/` path is a 404, anything but GET under `/api/` is a 405, and the upstreams only ever
see `GET /stats` with no query, cookies or `Authorization`.

| File | |
|---|---|
| `index.html` | the page: inline CSS and JS, no build step. Polls both calls every 30 s; a failed call shows that server offline and leaves the rest |
| `songs.json` | song names for GoCentral's numeric IDs; an ID not in it shows as "Song #ID" |
| `Caddyfile` | `/etc/caddy/Caddyfile` on the VM |
| `deploy.ps1` | copies the page (and the Caddyfile) to the VM and reloads Caddy |
| `tools/make_songs_json.py` | makes `songs.json` |

## Running it locally

Caddy (a single binary from https://github.com/caddyserver/caddy/releases), the production
Caddyfile with its placeholders pointed at local servers, and anything answering `/stats`
for GoCentral (a real GoCentral, or a small mock):

```powershell
# band3-rooms with its stats endpoint on
$env:ROOMS_REQUIRE_PROOF = "false"; $env:ROOMS_LISTEN = "127.0.0.1:18532"; $env:ROOMS_STATS = "127.0.0.1:18089"
go run ./cmd/band3-rooms

# in another window: Caddy on plain HTTP, port 18080
$env:SITE_ADDRESS = "http://127.0.0.1:18080"
$env:SITE_ROOT = "$PWD\site"
$env:GOCENTRAL_UPSTREAM = "127.0.0.1:18088"   # GoCentral's REST port, or a mock
$env:ROOMS_UPSTREAM = "127.0.0.1:18089"
$env:CADDY_ADMIN = "localhost:18019"
caddy run --config site\Caddyfile --adapter caddyfile
```

Then open http://127.0.0.1:18080/. `caddy validate --config site\Caddyfile --adapter caddyfile`
checks the file as production has it.

## songs.json

The disc's 83 songs come from `songs/gen/songs.dtb` in the game's archive, read through
band3_recomp's `tools/read_game_config.py`:

```sh
python site/tools/make_songs_json.py --band3-recomp ../band3_recomp
```

Official DLC isn't in that archive. To add it, pass each package's `songs/songs.dta` (text
DTA, extracted from the package), after the disc's; later files win for the same ID:

```sh
python site/tools/make_songs_json.py --band3-recomp ../band3_recomp dlc/*/songs.dta
```

Entries need their own `name`, `artist` and numeric `song_id`; fakes (lessons, test songs),
anything under `#ifndef _SHIP` and customs' text IDs are left out. Tests:
`cd site/tools && python -m unittest test_make_songs_json`.

## Deploying

Once, on the VM (not done yet):

1. Install Caddy from its Debian repository (https://caddyserver.com/docs/install#debian-ubuntu-raspbian).
2. Open the web ports: a firewall rule `allow-web`, tcp:80,443, target tag `band3-rooms`.
   Caddy gets its certificate for `rockband.idiosynchronic.com` by itself once 80 and 443 reach it.
3. Turn on band3-rooms' stats endpoint: add `ROOMS_STATS=127.0.0.1:19533` to
   `/etc/band3-rooms.env`, with a band3-rooms build that has it, then
   `sudo systemctl restart band3-rooms`.

Then, from Windows, at the repo root:

```powershell
.\site\deploy.ps1            # page, songs.json and Caddyfile; validates, then reloads Caddy
.\site\deploy.ps1 -PageOnly  # page and songs.json only
```
