"""crosscheck: drive a running band3-rooms with band3's own Python codec.

Imports the encode/decode functions from band3's tools/liveless_rooms_mock.py (the
codec held to the same golden vectors as band3's C++ client), logs two games in,
and has the second join the first by code, then miss one. Exits non-zero on any
surprise.

  go build -o band3-rooms ./cmd/band3-rooms
  ./band3-rooms -address 127.0.0.1 -listen 127.0.0.1:19532
  python scripts/crosscheck.py ../band3_recomp --address 127.0.0.1

Standard library only.
"""

import argparse
import os
import socket
import sys


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("band3", help="a band3_recomp checkout (for tools/liveless_rooms_mock.py)")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=19532)
    parser.add_argument("--address", default="127.0.0.1",
                        help="what the logins are signed for: the server's -address")
    args = parser.parse_args(argv)
    sys.path.insert(0, os.path.join(args.band3, "tools"))
    import liveless_rooms_mock as rooms

    class Game:
        def __init__(self, name):
            self.name = name
            self.sock = socket.create_connection((args.host, args.port), timeout=5)
            self.reader = rooms.FrameReader()

        def send(self, data):
            print(f"{self.name} -> {data.hex().upper()[:80]}{'...' if len(data) > 40 else ''}")
            self.sock.sendall(data)

        def expect(self, type_, decode):
            while True:
                frame = self.reader.next()
                if frame is None:
                    data = self.sock.recv(4096)
                    if not data:
                        raise SystemExit(f"{self.name}: the server closed the connection")
                    self.reader.feed(data)
                    continue
                got, body = frame
                if got == rooms.PING and type_ != rooms.PING:
                    self.send(rooms.PONG_FRAME)
                    continue
                if got != type_:
                    raise SystemExit(f"{self.name}: wanted type {type_}, got {got} {body.hex()}")
                message = decode(body)
                print(f"{self.name} <- {message}")
                return message

        def login(self, gamertag, local_ipv4, version):
            self.send(rooms.encode_client_hello(rooms.ClientHello(True, "eng", version)))
            hello = self.expect(rooms.SERVER_HELLO, rooms.decode_server_hello)
            if not hello.allowed:
                raise SystemExit(f"{self.name}: turned away")
            xuid = rooms.rooms_xuid(gamertag)
            proof = rooms.login_proof(hello.proof_key, args.address, xuid) if hello.needs_proof else b""
            self.send(rooms.encode_client_login(rooms.ClientLogin(xuid, gamertag, local_ipv4, proof)))
            return self.expect(rooms.LOGGED_IN, rooms.decode_server_logged_in)

    host = Game("host")
    joiner = Game("joiner")
    host_in = host.login("crosshost", "192.168.1.2", "idio-sync/band3 crosscheck")
    joiner_in = joiner.login("crossjoin", "192.168.1.3", "3.2.0-crosscheck")
    if host_in.code == joiner_in.code:
        raise SystemExit("both games got the same code")

    # typed in lower case, as a player might
    joiner.send(rooms.encode_join_request(rooms.JoinRequest(host_in.code.lower())))
    punch = host.expect(rooms.NAT_PUNCH, rooms.decode_nat_punch)
    response = joiner.expect(rooms.JOIN_RESPONSE, rooms.decode_join_response)
    want = rooms.JoinResponse(rooms.JOIN_BY_CODE, "crosshost", rooms.rooms_xuid("crosshost"),
                              host_in.public_ipv4, "192.168.1.2")
    if response != want:
        raise SystemExit(f"join response {response}, wanted {want}")
    if punch.public_ipv4 != joiner_in.public_ipv4:
        raise SystemExit(f"NAT punch to {punch.public_ipv4}, wanted {joiner_in.public_ipv4}")
    same = response.public_ipv4 == joiner_in.public_ipv4
    print(f"joiner would go to {response.private_ipv4 if same else response.public_ipv4}"
          f" ({'same public address: LAN' if same else 'public'})")

    joiner.send(rooms.encode_join_request(rooms.JoinRequest("ZZZZZ")))
    joiner.expect(rooms.JOIN_DENIED, rooms.decode_join_denied)
    host.sock.close()
    joiner.sock.close()
    print("crosscheck passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
