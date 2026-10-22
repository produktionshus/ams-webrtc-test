# ams-webrtc-test

WebRTC load tester for [Ant Media Server](https://antmedia.io), as a single native Windows x64 `.exe`.

It is a port of Ant Media's `webrtc-load-test` (Java), which only runs on Linux and macOS because it depends on Ant Media's own patched build of `libjingle_peerconnection_so` (no Windows build exists). This port is written in Go on top of [Pion WebRTC](https://github.com/pion/webrtc) and talks the same Ant Media websocket signalling protocol, so it needs no Java, no native libraries and no WSL/Docker.

## Build

Requires Go 1.26 or newer. `go.mod` pins the toolchain to go1.27.3; an older local Go downloads it automatically from the official Go distribution.

```powershell
go build -trimpath -ldflags "-s -w" -o ams-webrtc-test.exe .
```

Linux: `GOOS=linux GOARCH=amd64 go build -trimpath -o ams-webrtc-test .`

`go version -m ams-webrtc-test.exe` lists the Go version and every module compiled into a binary, so a binary can be checked against this source.

## Usage

Flags match the original tool, including the `-q true` style for booleans.

Play a stream with 100 viewers through WSS on port 5443 for 10 minutes:

```powershell
.\ams-webrtc-test.exe -m player -n 100 -i myStream -s media.example.com -q true -p 5443 -a LiveApp -duration 10m
```

Publish `test.mp4` in a loop:

```powershell
.\ams-webrtc-test.exe -m publisher -f test.mp4 -i myStream -s media.example.com -q true -p 5443 -a LiveApp -r true
```

Stop with Ctrl+C or `-duration`; a `TOTAL` line is printed on exit.

| Flag | Default | Description |
|------|---------|-------------|
| `-f` | `test.mp4` (publisher) | Publisher: source MP4. Player: optional output file (`.h264` or `.ivf`) for the video of connection #1 |
| `-s` | `localhost` | Server IP or domain name |
| `-q` | `false` | `true` = wss, `false` = ws |
| `-p` | `5080` | Websocket port |
| `-a` | `WebRTCAppEE` | Application name |
| `-i` | `myStream` | Stream id. With `-m publisher -n N` (N > 1) the ids are `myStream1` ... `myStreamN` |
| `-m` | `player` | `player` or `publisher` |
| `-n` | `1` | Number of connections |
| `-r` | `false` | Publisher: loop the file |
| `-l` | `3` | Log level 0:VERBOSE 1:INFO 2:WARNING 3:ERROR 4:NONE |
| `-v` | `false` | Same as `-l 0` |
| `-duration` | `0` | Stop after this long, e.g. `10m`. 0 = until Ctrl+C (new) |
| `-ip` | all | Use only this local IP address for media (new) |
| `-stun` | `stun:stun.l.google.com:19302` | Client STUN server, empty for none (new) |
| `-ramp` | `20ms` | Delay between starting connections (new) |

### Output

Every 5 seconds:

```
12:00:05 up 100/100 connecting 0 failed 0 ended 0 | 4480 kbps/conn | 25.0 fps/conn | gaps 0.01% unrepaired 0.00% | stalls 0
```

- **kbps/conn, fps/conn**: average per connected viewer over the last interval, including retransmissions. fps counts in-order RTP frames (marker bit); nothing is decoded.
- **gaps**: RTP sequence gaps as a share of expected packets, measured *before* NACK retransmission repair.
- **unrepaired**: gaps that no retransmission filled, i.e. what a viewer actually lost.
- **stalls**: video frame gaps longer than 500 ms. This is not the same metric as Chrome's `freezeCount`.
- A connection that is not up after 30 s is marked failed and logged with the last signalling message and ICE state.
- Publisher lines show the bitrate of the published stream and the PLI/NACK requests received from the server.

## Capacity planning

### Resource use

Measured on Windows 11 (player mode, 1080p stream):

| | Per process | Per viewer |
|---|---|---|
| Memory | ~40 MB | ~1.6 MB |
| CPU | | ~0.5 core per 100 Mbit/s received |
| TCP | | 1 outbound websocket |
| UDP | | 1 socket per local IP address (use `-ip` to limit to one) |

So 1000 viewers of a 4.5 Mbit/s stream need roughly 2 GB of memory, 25-30 cores and 4.5 Gbit/s of downlink. The tool listens on no TCP port and writes no files unless `-f` is given in player mode.

### Network

Every simulated viewer really receives the stream, so the limit is usually the network path to the test machine, not CPU. All viewers on one machine get each video frame at the same moment, so routers and ISP shapers see bursts that real, spread-out viewers never cause, and a TCP speed test overstates what gets through.

Measured from one office PC to a server about 5 ms away, 4.5 Mbit/s 1080p stream:

| Link | Viewers | Result |
|------|---------|--------|
| Wi-Fi, 144 Mbit/s | 10 | clean |
| Wi-Fi, 144 Mbit/s | 25 | 30 % gaps |
| 1 Gbit/s internet (up to 800 Mbit/s TCP) | 50 | 24.3 fps, ~1 % unrepaired |
| 1 Gbit/s internet (up to 800 Mbit/s TCP) | 75 | 22-23 fps, 2-10 % unrepaired |
| 1 Gbit/s internet (up to 800 Mbit/s TCP) | 100 | all connected, ~23 fps, 4-9 % unrepaired, router peaks at line rate |

The server's WebRTC pacer may send each viewer up to about 2.5 × the stream bitrate in short bursts (keyframes). Synchronised across all viewers on one line, 100 × 4.5 Mbit/s × 2.5 ≈ 1.1 Gbit/s overflows a 1 Gbit/s link even though the average is under half of it. Plan for roughly `line speed / (2.5 × stream bitrate) / 2` viewers per network port for clean results. Router features that inspect every packet (IPS, DPI, smart queues) lower this further.

When `gaps` rise on every connection at the same time, check the test machine's network first.

### Several machines

Scaling out is usually easier than scaling up. Run the same command on each machine with the same `-duration` and start them at about the same time. Each machine prints its own `TOTAL` line for the viewer side; the server's own statistics show the combined load.

On one machine with several network ports, run one process per port with `-ip` set to that port's address.

### Large tests on Windows Server

- Allow the program through Windows Firewall up front (elevated PowerShell), so no prompt blocks a headless run:
  `New-NetFirewallRule -DisplayName ams-webrtc-test -Program C:\path\to\ams-webrtc-test.exe -Direction Inbound -Protocol UDP -Action Allow`
- Make sure Receive Side Scaling is on for the NICs (`Get-NetAdapterRss`), so receive work spreads over cores.
- Use the High performance power plan (`powercfg /setactive SCHEME_MIN`).
- Exclude the folder from real-time antivirus scanning if CPU use looks high.
- Step up (100, 300, 600, 1000) and watch CPU, `unrepaired` and fps. If CPU is fine but gaps grow while the NIC is not full, split the load over several processes (for example 4 × `-n 250`).

## Security notes

For reviewing the tool before it runs in a test environment:

- **Network connections.** Only to the hosts given on the command line: a websocket to `-s`:`-p` (TLS with certificate verification against the system store when `-q true`), UDP to the STUN server in `-stun` (default `stun.l.google.com:19302`; `-stun ""` turns it off), and UDP media to the addresses the server offers as ICE candidates. No telemetry, update checks or other hosts.
- **Local addresses.** Like any WebRTC client, it sends its local IP addresses to the server as ICE host candidates. `-ip` limits this to one address (for example to keep VPN addresses out).
- **Privileges.** Runs as a normal user. No installation, services or registry changes. Only the optional firewall rule above needs elevation.
- **Files.** Reads the MP4 given with `-f` (publisher). Writes only the file given with `-f` (player).
- **Dependencies.** All permissive licenses: Pion WebRTC and its sub-modules (MIT), Eyevinn/mp4ff (MIT), gorilla/websocket (BSD-2-Clause), google/uuid and golang.org/x/{crypto,net,sys,time} (BSD-3-Clause), wlynxg/anet (BSD-3-Clause). Exact versions are in `go.mod`/`go.sum`.
- **Vulnerabilities.** `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` reported 0 vulnerabilities affecting the code at the time of writing.

## Media file

The publisher sends the file as-is, without transcoding (like the original). It needs a progressive (non-fragmented) MP4 with H264 video and optional Opus audio. Use Baseline/Constrained Baseline H264 without B-frames for WebRTC. To make one with ffmpeg:

```powershell
ffmpeg -i input.mov -c:v libx264 -profile:v baseline -bf 0 -g 50 -c:a libopus -ar 48000 -ac 2 test.mp4
```

Ant Media's own `test.mp4` from the original tool also works.

## Differences from the original

Not ported: GUI (`-u`, always headless), saving frames as PNG, Kafka stats (`-k`), data channel (`-d`), VP8/H265 publishing (`-c`). The player accepts whatever codec the server offers.

## Test

```powershell
$env:AMS_TEST_MP4 = "C:\path\to\test.mp4"; go test -v .
```

## License

MIT, see [LICENSE](LICENSE). Not affiliated with Ant Media.
