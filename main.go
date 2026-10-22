// ams-webrtc-test is a native (pure Go) port of Ant Media's Linux-only
// webrtc-load-test tool. It speaks the Ant Media Server websocket signalling
// protocol and uses Pion WebRTC for media, so it builds to a single .exe.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/gorilla/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"
	"github.com/pion/webrtc/v4/pkg/media/ivfwriter"
)

// boolArg takes an explicit value ("-q true"), matching the original tool's flag syntax.
type boolArg bool

func (b *boolArg) String() string { return strconv.FormatBool(bool(*b)) }
func (b *boolArg) Set(s string) error {
	v, err := strconv.ParseBool(s)
	*b = boolArg(v)
	return err
}

var (
	file                                 = flag.String("f", "", "publisher: source MP4 (H264 + Opus), default test.mp4; player: optional output file (.h264/.ivf) for connection #1")
	server                               = flag.String("s", "localhost", "server IP or domain name")
	logLevel                             = flag.Int("l", 3, "log level 0:VERBOSE 1:INFO 2:WARNING 3:ERROR 4:NONE")
	streamID                             = flag.String("i", "myStream", "stream id")
	mode                                 = flag.String("m", "player", "publisher or player")
	port                                 = flag.Int("p", 5080, "websocket port")
	count                                = flag.Int("n", 1, "number of player/publisher connections")
	kafka                                = flag.String("k", "", "Kafka broker (not ported, ignored)")
	codec                                = flag.String("c", "h264", "publisher codec (only h264 is ported)")
	app                                  = flag.String("a", "WebRTCAppEE", "application name")
	stun                                 = flag.String("stun", "stun:stun.l.google.com:19302", "client STUN server, empty for none")
	ramp                                 = flag.Duration("ramp", 20*time.Millisecond, "delay between starting connections")
	duration                             = flag.Duration("duration", 0, "stop after this long, e.g. 10m (0 = until Ctrl+C)")
	bindIP                               = flag.String("ip", "", "use only this local IP for media (default: all interfaces)")
	wss, gui, verbose, loop, dataChannel boolArg
)

func init() {
	flag.Var(&wss, "q", "true (wss) or false (ws)")
	flag.Var(&gui, "u", "show GUI (not ported, always headless)")
	flag.Var(&verbose, "v", "verbose, same as -l 0")
	flag.Var(&loop, "r", "publisher: loop the file")
	flag.Var(&dataChannel, "d", "data channel (not ported, ignored)")
}

func logf(level int, format string, a ...any) {
	if level >= *logLevel {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000 ")+format+"\n", a...)
	}
}

const (
	stConnecting int32 = iota
	stUp
	stFailed
	stEnded
)

type stats struct {
	state                                                    atomic.Int32
	bytes, pkts, gaps, repaired, frames, stalls, nacks, plis atomic.Int64
}

// ponytail: fixed threshold, not Chrome's freezeCount formula (max(3*avg frame time, avg+150ms)).
const stallGap = 500 * time.Millisecond

func main() {
	flag.Parse()
	if verbose {
		*logLevel = 0
	}
	if gui {
		logf(2, "-u true: GUI not ported, running headless")
	}
	if *kafka != "" {
		logf(2, "-k: Kafka not ported, ignored")
	}
	if dataChannel {
		logf(2, "-d: data channel not ported, ignored")
	}
	publisher := *mode == "publisher"
	if !publisher && *mode != "player" {
		fatal("-m must be publisher or player")
	}
	if *count < 1 {
		fatal("-n must be >= 1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *duration > 0 {
		time.AfterFunc(*duration, cancel)
	}

	api, err := newAPI(publisher)
	if err != nil {
		fatal(err.Error())
	}

	pump := &stats{}
	var tracks []webrtc.TrackLocal
	if publisher {
		if !strings.EqualFold(*codec, "h264") {
			fatal("-c " + *codec + ": only h264 publishing is ported")
		}
		src := *file
		if src == "" {
			src = "test.mp4"
		}
		video, audio, err := loadMP4(src)
		if err != nil {
			fatal(err.Error())
		}
		var pumps sync.WaitGroup
		vt, _ := webrtc.NewTrackLocalStaticSample(h264Cap, "video", "ams-webrtc-test")
		tracks = append(tracks, vt)
		pumps.Add(1)
		go func() { defer pumps.Done(); runPump(ctx, vt, video, pump) }()
		if len(audio) > 0 {
			at, _ := webrtc.NewTrackLocalStaticSample(opusCap, "audio", "ams-webrtc-test")
			tracks = append(tracks, at)
			pumps.Add(1)
			go func() { defer pumps.Done(); runPump(ctx, at, audio, pump) }()
		}
		go func() { pumps.Wait(); cancel() }() // file played out without -r true
	}

	conns := make([]*stats, *count)
	for i := range conns {
		conns[i] = &stats{}
	}
	start := time.Now()
	go report(ctx, conns, pump, publisher)

	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() { defer wg.Done(); runConn(ctx, i, api, conns[i], tracks, publisher) }()
		select {
		case <-ctx.Done():
		case <-time.After(*ramp):
		}
		if ctx.Err() != nil {
			conns = conns[:i+1]
			break
		}
	}
	wg.Wait()
	fmt.Println(line("TOTAL", conns, sum(conns, pump), time.Since(start), publisher))
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}

var (
	h264Cap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}
	opusCap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		SDPFmtpLine: "minptime=10;useinbandfec=1"}
)

func newAPI(publisher bool) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if publisher {
		// Offer only what we send, so the server cannot pick another codec.
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: h264Cap, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opusCap, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, err
		}
	} else if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	if *bindIP != "" {
		ip := net.ParseIP(*bindIP)
		if ip == nil {
			return nil, fmt.Errorf("-ip %q is not an IP address", *bindIP)
		}
		se.SetIPFilter(func(candidate net.IP) bool { return candidate.Equal(ip) })
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

type message struct {
	Command    string `json:"command"`
	Type       string `json:"type"`
	SDP        string `json:"sdp"`
	Candidate  string `json:"candidate"`
	ID         string `json:"id"`
	Label      uint16 `json:"label"`
	Definition string `json:"definition"`
}

// runConn runs one player or publisher session until it ends or ctx is cancelled.
func runConn(ctx context.Context, idx int, api *webrtc.API, st *stats, tracks []webrtc.TrackLocal, publisher bool) {
	id := *streamID
	if publisher && *count > 1 {
		id = *streamID + strconv.Itoa(idx+1)
	}
	var once sync.Once
	done := make(chan struct{})
	finish := func(state int32) {
		once.Do(func() {
			if state == stEnded && st.state.Load() != stUp {
				state = stFailed
			}
			st.state.Store(state)
			close(done)
		})
	}

	scheme := "ws"
	if wss {
		scheme = "wss"
	}
	url := fmt.Sprintf("%s://%s:%d/%s/websocket", scheme, *server, *port, *app)
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	ws, _, err := websocket.DefaultDialer.DialContext(dialCtx, url, nil)
	dialCancel()
	if err != nil {
		logf(3, "#%d %s: %v", idx+1, url, err)
		finish(stFailed)
		return
	}
	defer ws.Close()
	var wmu sync.Mutex
	send := func(m map[string]any) {
		m["streamId"] = id
		wmu.Lock()
		defer wmu.Unlock()
		if err := ws.WriteJSON(m); err != nil {
			logf(1, "#%d send %s: %v", idx+1, m["command"], err)
		}
	}

	cfg := webrtc.Configuration{}
	if *stun != "" {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: []string{*stun}}}
	}
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		logf(3, "#%d peerconnection: %v", idx+1, err)
		finish(stFailed)
		return
	}
	defer pc.Close()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		send(map[string]any{"command": "takeCandidate", "label": deref(j.SDPMLineIndex), "id": deref(j.SDPMid), "candidate": j.Candidate})
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logf(1, "#%d %s", idx+1, s)
		switch s {
		case webrtc.PeerConnectionStateConnected:
			st.state.Store(stUp)
		case webrtc.PeerConnectionStateFailed:
			finish(stFailed)
		case webrtc.PeerConnectionStateClosed:
			finish(stEnded)
		}
	})

	if publisher {
		for _, t := range tracks {
			sender, err := pc.AddTrack(t)
			if err != nil {
				logf(3, "#%d add track: %v", idx+1, err)
				finish(stFailed)
				return
			}
			go readRTCP(sender, st)
		}
		send(map[string]any{"command": "publish", "video": true, "audio": len(tracks) > 1})
	} else {
		out := ""
		if idx == 0 {
			out = *file
		}
		pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { readTrack(t, st, out) })
		send(map[string]any{"command": "play"})
	}

	var lastMsg atomic.Value
	lastMsg.Store("none")
	go func() {
		var pending []webrtc.ICECandidateInit
		fail := func(what string, err error) {
			logf(3, "#%d %s: %v", idx+1, what, err)
			finish(stFailed)
		}
		for {
			var m message
			if err := ws.ReadJSON(&m); err != nil {
				if ctx.Err() == nil {
					logf(1, "#%d websocket closed: %v", idx+1, err)
				}
				finish(stEnded)
				return
			}
			logf(0, "#%d <- %s %s%s", idx+1, m.Command, m.Type, m.Definition)
			lastMsg.Store(strings.TrimSpace(m.Command + " " + m.Type + m.Definition))
			switch m.Command {
			case "start": // publisher: server is ready for our offer
				offer, err := pc.CreateOffer(nil)
				if err == nil {
					err = pc.SetLocalDescription(offer)
				}
				if err != nil {
					fail("offer", err)
					return
				}
				send(map[string]any{"command": "takeConfiguration", "type": "offer", "sdp": offer.SDP})
			case "takeConfiguration":
				sdpType := webrtc.NewSDPType(m.Type)
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: sdpType, SDP: m.SDP}); err != nil {
					fail("remote description", err)
					return
				}
				if sdpType == webrtc.SDPTypeOffer {
					answer, err := pc.CreateAnswer(nil)
					if err == nil {
						err = pc.SetLocalDescription(answer)
					}
					if err != nil {
						fail("answer", err)
						return
					}
					send(map[string]any{"command": "takeConfiguration", "type": "answer", "sdp": answer.SDP})
				}
				for _, c := range pending {
					_ = pc.AddICECandidate(c)
				}
				pending = nil
			case "takeCandidate":
				c := webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMid: &m.ID, SDPMLineIndex: &m.Label}
				if pc.RemoteDescription() == nil {
					pending = append(pending, c)
				} else if err := pc.AddICECandidate(c); err != nil {
					logf(1, "#%d candidate: %v", idx+1, err)
				}
			case "notification":
				logf(1, "#%d %s", idx+1, m.Definition)
				if m.Definition == "play_finished" || m.Definition == "publish_finished" {
					finish(stEnded)
					return
				}
			case "error":
				fail("server error", fmt.Errorf("%s", m.Definition))
				return
			}
		}
	}()

	ping := time.NewTicker(5 * time.Second)
	defer ping.Stop()
	connectTimeout := time.After(30 * time.Second)
	for {
		select {
		case <-connectTimeout:
			if st.state.Load() == stConnecting {
				logf(3, "#%d not connected after 30s (last message: %s, ICE: %s)", idx+1, lastMsg.Load(), pc.ICEConnectionState())
				finish(stFailed)
				return
			}
		case <-ctx.Done():
			send(map[string]any{"command": "stop"})
			finish(stEnded)
			return
		case <-done:
			return
		case <-ping.C:
			send(map[string]any{"command": "ping"})
		}
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// readTrack counts received media; packet gaps are measured before NACK repair.
func readTrack(t *webrtc.TrackRemote, st *stats, out string) {
	video := t.Kind() == webrtc.RTPCodecTypeVideo
	var w interface {
		WriteRTP(*rtp.Packet) error
		Close() error
	}
	if video && out != "" {
		var err error
		if t.Codec().MimeType == webrtc.MimeTypeH264 {
			w, err = h264writer.New(out)
		} else {
			w, err = ivfwriter.New(out, ivfwriter.WithCodec(t.Codec().MimeType))
		}
		if err != nil {
			logf(3, "output %s: %v", out, err)
			w = nil
		} else {
			defer w.Close()
		}
	}
	var last uint16
	var lastFrame time.Time
	first := true
	missing := map[uint16]struct{}{}
	for {
		p, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		st.bytes.Add(int64(len(p.Payload)))
		d := p.SequenceNumber - last
		inOrder := first || (d > 0 && d < 0x8000) // false for NACK retransmits and duplicates
		if inOrder {
			st.pkts.Add(1)
			if !first && d > 1 {
				st.gaps.Add(int64(d - 1))
				for s := last + 1; s != p.SequenceNumber; s++ {
					missing[s] = struct{}{}
				}
				if len(missing) > 4096 { // ponytail: stale entries just count as unrepaired
					clear(missing)
				}
			}
			last, first = p.SequenceNumber, false
		} else if _, ok := missing[p.SequenceNumber]; ok {
			delete(missing, p.SequenceNumber)
			st.repaired.Add(1)
		}
		if video && p.Marker && inOrder {
			now := time.Now()
			if !lastFrame.IsZero() && now.Sub(lastFrame) > stallGap {
				st.stalls.Add(1)
			}
			lastFrame = now
			st.frames.Add(1)
		}
		if w != nil {
			_ = w.WriteRTP(p)
		}
	}
}

func readRTCP(s *webrtc.RTPSender, st *stats) {
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication:
				st.plis.Add(1)
			case *rtcp.TransportLayerNack:
				st.nacks.Add(1)
			}
		}
	}
}

type sample struct {
	data []byte
	dur  time.Duration
}

// loadMP4 reads a progressive MP4 into memory: H264 as Annex-B (SPS/PPS before
// each keyframe) and Opus packets. Media is sent as-is, no transcoding.
func loadMP4(path string) (video, audio []sample, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := mp4.DecodeFile(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Moov == nil {
		return nil, nil, fmt.Errorf("%s: no moov box (fragmented MP4 not supported)", path)
	}
	for _, trak := range f.Moov.Traks {
		stbl := trak.Mdia.Minf.Stbl
		ts := float64(trak.Mdia.Mdhd.Timescale)
		var paramSets []byte
		switch {
		case stbl.Stsd.AvcX != nil:
			for _, nalu := range append(stbl.Stsd.AvcX.AvcC.SPSnalus, stbl.Stsd.AvcX.AvcC.PPSnalus...) {
				paramSets = append(append(paramSets, 0, 0, 0, 1), nalu...)
			}
		case stbl.Stsd.Opus != nil:
		default:
			logf(2, "%s: skipping track %d (not H264/Opus)", path, trak.Tkhd.TrackID)
			continue
		}
		for nr := uint32(1); nr <= stbl.Stsz.GetNrSamples(); nr++ {
			r, err := trak.GetRangesForSampleInterval(nr, nr)
			if err != nil {
				return nil, nil, err
			}
			data := raw[r[0].Offset : r[0].Offset+r[0].Size]
			s := sample{data: data, dur: time.Duration(float64(stbl.Stts.GetDur(nr)) / ts * float64(time.Second))}
			if paramSets == nil {
				audio = append(audio, s)
				continue
			}
			s.data = avc.ConvertSampleToByteStream(data)
			if stbl.Stss == nil || stbl.Stss.IsSyncSample(nr) {
				s.data = append(append([]byte{}, paramSets...), s.data...)
			}
			video = append(video, s)
		}
	}
	if len(video) == 0 {
		return nil, nil, fmt.Errorf("%s: no H264 video track", path)
	}
	if len(audio) == 0 {
		logf(2, "%s: no Opus audio track, publishing video only", path)
	}
	return video, audio, nil
}

// runPump plays samples in real time into a track shared by all publisher connections.
func runPump(ctx context.Context, t *webrtc.TrackLocalStaticSample, samples []sample, st *stats) {
	next := time.Now()
	for {
		for _, s := range samples {
			if err := t.WriteSample(media.Sample{Data: s.data, Duration: s.dur}); err != nil {
				logf(1, "write sample: %v", err)
			}
			st.bytes.Add(int64(len(s.data)))
			next = next.Add(s.dur)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
		}
		if !loop {
			return
		}
	}
}

type totals struct{ bytes, pkts, gaps, repaired, frames, stalls, nacks, plis int64 }

func sum(conns []*stats, pump *stats) totals {
	var t totals
	for _, c := range conns {
		if c == nil {
			continue
		}
		t.pkts += c.pkts.Load()
		t.gaps += c.gaps.Load()
		t.repaired += c.repaired.Load()
		t.frames += c.frames.Load()
		t.stalls += c.stalls.Load()
		t.nacks += c.nacks.Load()
		t.plis += c.plis.Load()
		t.bytes += c.bytes.Load()
	}
	t.bytes += pump.bytes.Load() // publisher: bytes of the one shared stream
	return t
}

func line(label string, conns []*stats, t totals, dt time.Duration, publisher bool) string {
	var n [4]int
	for _, c := range conns {
		if c != nil {
			n[c.state.Load()]++
		}
	}
	secs := dt.Seconds()
	head := fmt.Sprintf("%s up %d/%d connecting %d failed %d ended %d |", label, n[stUp], len(conns), n[stConnecting], n[stFailed], n[stEnded])
	if publisher {
		return fmt.Sprintf("%s stream %.0f kbps | PLI %d NACK %d", head, float64(t.bytes)*8/1000/secs, t.plis, t.nacks)
	}
	up := float64(max(n[stUp]+n[stEnded], 1)) // ended = was up before it stopped
	var loss, lost float64
	if expected := float64(t.pkts + t.gaps); expected > 0 {
		loss = float64(t.gaps) * 100 / expected
		lost = float64(max(t.gaps-t.repaired, 0)) * 100 / expected
	}
	return fmt.Sprintf("%s %.0f kbps/conn | %.1f fps/conn | gaps %.2f%% unrepaired %.2f%% | stalls %d",
		head, float64(t.bytes)*8/1000/secs/up, float64(t.frames)/secs/up, loss, lost, t.stalls)
}

func report(ctx context.Context, conns []*stats, pump *stats, publisher bool) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	prev, prevT := totals{}, time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			cur := sum(conns, pump)
			d := totals{cur.bytes - prev.bytes, cur.pkts - prev.pkts, cur.gaps - prev.gaps, cur.repaired - prev.repaired,
				cur.frames - prev.frames, cur.stalls - prev.stalls, cur.nacks - prev.nacks, cur.plis - prev.plis}
			fmt.Println(line(now.Format("15:04:05"), conns, d, now.Sub(prevT), publisher))
			prev, prevT = cur, now
		}
	}
}
