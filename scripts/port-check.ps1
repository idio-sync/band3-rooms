<#
.SYNOPSIS
  Checks a band3-rooms server, and whether players elsewhere reach this PC's UDP port.

.DESCRIPTION
  Logs in to the server as a band3 client would ("idio-sync/band3 port-check"),
  shows the room code and the public address the server sees, and asks for a port
  check: the server sends one UDP packet to that address on -UdpPort, and this
  listens for it.

  Close band3 first: the check listens on the game's port itself. Without band3
  running, a port band3 opens on the router by itself (UPnP) is closed too, so
  this checks a port forwarded by hand. Windows may ask to let PowerShell through
  its firewall; allow it, or the packet is blocked and the port reads as closed.

.EXAMPLE
  .\port-check.ps1 -Server rooms.example.com
.EXAMPLE
  .\port-check.ps1 -Server 127.0.0.1 -UdpPort 19999
#>
param(
  [Parameter(Mandatory = $true)][string]$Server,
  [int]$Port = 19532,
  [int]$UdpPort = 9103,
  [int]$WaitSeconds = 5
)
$ErrorActionPreference = 'Stop'

# The protocol's frames: "LL", 0, the type, the body's size (big-endian), the body.
function New-Frame([byte]$Type, [byte[]]$Body) {
  $out = New-Object System.Collections.Generic.List[byte]
  $out.AddRange([byte[]](0x4C, 0x4C, 0, $Type, [byte]($Body.Length -shr 8), [byte]($Body.Length -band 0xFF)))
  $out.AddRange($Body)
  return , $out.ToArray()
}
# text in a field of `Size` bytes, NUL-padded; `Terminated` leaves room for a NUL
function Get-Field([string]$Text, [int]$Size, [bool]$Terminated = $true) {
  $field = New-Object byte[] $Size
  $bytes = [Text.Encoding]::ASCII.GetBytes($Text)
  $room = if ($Terminated) { $Size - 1 } else { $Size }
  [Array]::Copy($bytes, $field, [Math]::Min($bytes.Length, $room))
  return , $field
}
function Read-Exactly($Stream, [int]$Count) {
  $buffer = New-Object byte[] $Count
  $done = 0
  while ($done -lt $Count) {
    $got = $Stream.Read($buffer, $done, $Count - $done)
    if ($got -le 0) { throw "the server closed the connection" }
    $done += $got
  }
  return , $buffer
}
# the next frame as @(type, body), answering the server's pings on the way
function Read-Frame($Stream) {
  while ($true) {
    $header = Read-Exactly $Stream 6
    if ($header[0] -ne 0x4C -or $header[1] -ne 0x4C) { throw "not a Rooms server (bad magic)" }
    $size = [int]$header[4] * 256 + [int]$header[5]
    $body = if ($size -gt 0) { Read-Exactly $Stream $size } else { [byte[]]@() }
    if ($header[3] -eq 2) {  # ping: pong back
      $pong = New-Frame 2 ([byte[]]@())
      $Stream.Write($pong, 0, $pong.Length)
      continue
    }
    return @($header[3], $body)
  }
}
function Get-Random-Bytes([int]$Count) {
  $bytes = New-Object byte[] $Count
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  return , $bytes
}

# listen first: if the port is taken (band3 running), say so before logging in
try {
  $udp = New-Object System.Net.Sockets.UdpClient($UdpPort)
} catch {
  Write-Host "Can't listen on UDP $UdpPort ($($_.Exception.Message)). Close band3 and try again." -ForegroundColor Red
  exit 1
}
$udp.Client.ReceiveTimeout = $WaitSeconds * 1000

$tcp = New-Object System.Net.Sockets.TcpClient
try {
  $connect = $tcp.BeginConnect($Server, $Port, $null, $null)
  if (-not $connect.AsyncWaitHandle.WaitOne(5000)) { throw "no answer within 5 s" }
  $tcp.EndConnect($connect)
} catch {
  Write-Host "Can't reach $Server on TCP $Port ($($_.Exception.Message))." -ForegroundColor Red
  Write-Host "Check the name points at the server, the server is running, and TCP $Port is open in its firewall."
  $udp.Close(); exit 1
}
$tcp.ReceiveTimeout = 5000
$stream = $tcp.GetStream()
Write-Host "Connected to $Server on TCP $Port."

try {
  # hello: protocol 0, an emulator (no console signature), English, the version
  $hello = New-Object System.Collections.Generic.List[byte]
  $hello.AddRange([byte[]](0, 1))
  $hello.AddRange((Get-Field 'eng' 3 $false))
  $hello.AddRange((Get-Field 'idio-sync/band3 port-check' 0x30))
  $frame = New-Frame 0 $hello.ToArray()
  $stream.Write($frame, 0, $frame.Length)

  $reply = Read-Frame $stream
  if ($reply[0] -ne 0) { throw "expected the server's hello, got packet type $($reply[0])" }
  $body = $reply[1]
  if ($body[0] -eq 0) { throw "the server turned this client away (its version allow or deny list)" }
  $needsProof = $body[1] -ne 0
  $key = New-Object byte[] 16
  [Array]::Copy($body, 2, $key, 0, 16)

  # login: a throwaway XUID and name, no local address, the proof if asked for
  $xuid = Get-Random-Bytes 8
  $proof = New-Object byte[] 0x228
  if ($needsProof) {
    $hmac = [System.Security.Cryptography.HMACSHA1]::new($key)
    $signed = New-Object System.Collections.Generic.List[byte]
    $signed.AddRange([Text.Encoding]::ASCII.GetBytes($Server))
    $signed.AddRange($xuid)
    [Array]::Copy($hmac.ComputeHash($signed.ToArray()), $proof, 20)
  }
  $login = New-Object System.Collections.Generic.List[byte]
  $login.AddRange($xuid)
  $login.AddRange((Get-Field 'porttest' 16))
  $login.AddRange([byte[]](0, 0, 0, 0))
  $login.AddRange($proof)
  $frame = New-Frame 1 $login.ToArray()
  $stream.Write($frame, 0, $frame.Length)

  # band3-rooms 0.3.0 and later say what they are right after the hello
  $serverVersion = ''
  while ($true) {
    try {
      $reply = Read-Frame $stream
    } catch {
      throw "the server refused the login: -Server must be the name exactly as the server's ROOMS_ADDRESS has it"
    }
    if ($reply[0] -ne 0xB2) { break }
    $serverVersion = [Text.Encoding]::ASCII.GetString($reply[1], 4, 32).TrimEnd([char]0)
    Write-Host "Server: band3-rooms $serverVersion."
  }
  if ($reply[0] -ne 1) { throw "expected the login reply, got packet type $($reply[0])" }
  $body = $reply[1]
  $public = '{0}.{1}.{2}.{3}' -f $body[0], $body[1], $body[2], $body[3]
  $code = [Text.Encoding]::ASCII.GetString($body, 4, 8).TrimEnd([char]0)
  Write-Host "Logged in: room code $code, public address $public (as the server sees this PC)." -ForegroundColor Green

  # band3-rooms 0.2.0 and later say they can check ports, to band3 clients only
  $features = 0
  try {
    $reply = Read-Frame $stream
    if ($reply[0] -eq 0xB0) {
      $b = $reply[1]
      $features = ([int64]$b[0] -shl 24) -bor ([int64]$b[1] -shl 16) -bor ([int64]$b[2] -shl 8) -bor [int64]$b[3]
    }
  } catch { }
  if (($features -band 1) -eq 0) {
    Write-Host "This server doesn't check ports (band3-rooms before 0.2.0, or another Rooms server)." -ForegroundColor Yellow
    exit 2
  }

  $token = Get-Random-Bytes 16
  $request = New-Object System.Collections.Generic.List[byte]
  $request.AddRange([byte[]]([byte]($UdpPort -shr 8), [byte]($UdpPort -band 0xFF)))
  $request.AddRange($token)
  $frame = New-Frame 0xB1 $request.ToArray()
  $stream.Write($frame, 0, $frame.Length)
  Write-Host "Asked the server to send a packet to $public on UDP $UdpPort..."

  $open = $false
  $deadline = (Get-Date).AddSeconds($WaitSeconds)
  while ((Get-Date) -lt $deadline) {
    try {
      $from = New-Object System.Net.IPEndPoint([System.Net.IPAddress]::Any, 0)
      $data = $udp.Receive([ref]$from)
    } catch { break }
    if ($data.Length -eq 20 -and [Text.Encoding]::ASCII.GetString($data, 0, 4) -eq 'B3PC') {
      $match = $true
      for ($i = 0; $i -lt 16; $i++) { if ($data[4 + $i] -ne $token[$i]) { $match = $false } }
      if ($match) { $open = $true; break }
    }
  }
  if ($open) {
    Write-Host "UDP $UdpPort is OPEN: players elsewhere can reach this PC." -ForegroundColor Green
    exit 0
  }
  Write-Host "UDP $UdpPort looks CLOSED: no packet within $WaitSeconds s." -ForegroundColor Red
  Write-Host "Forward UDP $UdpPort on your router to this PC, and allow it through Windows Firewall."
  exit 3
} catch {
  Write-Host "Failed: $($_.Exception.Message)" -ForegroundColor Red
  exit 1
} finally {
  $tcp.Close()
  $udp.Close()
}
