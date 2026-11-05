package player

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/audio"
	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"

	"netease-music-mygo/internal/netease"
)

func TestPlayEmptyQueue(t *testing.T) {
	p := &Player{at: -1}
	p.PlayQueue(nil, 0) // an empty playlist's play button
	if p.Current() != nil {
		t.Error("playing from an empty queue")
	}
}

// silence is a long song of nothing.
type silence struct{}

func (silence) SampleRate() int               { return 48000 }
func (silence) Channels() int                 { return 2 }
func (silence) Len() int64                    { return 48000 * 600 }
func (silence) SetPosition(int64) error       { return nil }
func (silence) Read(b []float32) (int, error) { clear(b); return len(b), nil }

// posted collects what the player posts to the main thread, for the test
// to run there.
func posted() (chan func(), *Player) {
	ch := make(chan func(), 16)
	p := New(func(fn func()) { ch <- fn })
	p.Cue([]netease.Track{{ID: 1, Name: "一", Duration: 600000}})
	out := audio.NewPlayer(silence{})
	out.SetVolume(0)
	out.Play()
	p.Attach(out)
	p.fade.attach(audioPlayerOutput{out}, 1)
	return ch, p
}

func next(t *testing.T, ch chan func()) {
	t.Helper()
	select {
	case fn := <-ch:
		fn()
	case <-time.After(2 * time.Second):
		t.Fatal("nothing posted")
	}
}

// A track is reported once when it starts, not again when resumed.
func TestStartedReportsOnce(t *testing.T) {
	ch, p := posted()
	defer p.out.Close()
	var started []int64
	p.Started = func(tr *netease.Track) { started = append(started, tr.ID) }
	p.TogglePlay()
	next(t, ch)
	p.TogglePlay()
	if len(started) != 1 || started[0] != 1 {
		t.Fatalf("started = %v", started)
	}
	p.TogglePlay()
	next(t, ch)
	p.TogglePlay()
	if len(started) != 1 {
		t.Fatalf("resume reported again: %v", started)
	}
}

// Pausing fades out before the music stops; playing again fades it in.
func TestPauseFades(t *testing.T) {
	ch, p := posted()
	defer p.out.Close()
	p.TogglePlay()
	if p.Playing() || !p.out.Playing() {
		t.Fatal("paused at once, or not at all")
	}
	next(t, ch)
	if p.out.Playing() {
		t.Fatal("still playing after fading out")
	}
	if !p.fade.silent() {
		t.Error("faded out to something")
	}
	p.TogglePlay()
	if !p.Playing() || !p.out.Playing() {
		t.Fatal("did not play again")
	}
	time.Sleep(fadeIn + 50*time.Millisecond)
	p.fade.mu.Lock()
	level := p.fade.level(time.Now())
	p.fade.mu.Unlock()
	if level != 1 {
		t.Errorf("faded in to %v", level)
	}
}

// A seek playing pauses, which fades out over a few milliseconds, and
// plays again once there, its position the one asked for meanwhile.
func TestSeekWithoutClick(t *testing.T) {
	ch, p := posted()
	defer p.out.Close()
	p.Seek(90 * time.Second)
	if p.out.Playing() {
		t.Error("jumped while playing")
	}
	if p.Position() != 90*time.Second {
		t.Errorf("position %v while seeking", p.Position())
	}
	next(t, ch)
	if !p.out.Playing() || p.out.Position() < 90*time.Second {
		t.Errorf("after the seek: playing %v at %v", p.out.Playing(), p.out.Position())
	}
}

// Seeks one after the other, as the progress bar asks when let go, play
// on from the last.
func TestSeekTwicePlaysOn(t *testing.T) {
	ch, p := posted()
	defer p.out.Close()
	p.Seek(90 * time.Second)
	p.Seek(95 * time.Second)
	next(t, ch)
	for len(ch) > 0 {
		next(t, ch)
	}
	time.Sleep(seekGap * 2)
	for len(ch) > 0 {
		next(t, ch)
	}
	if !p.out.Playing() || p.out.Position() < 95*time.Second {
		t.Errorf("after two seeks: playing %v at %v", p.out.Playing(), p.out.Position())
	}
}

// A radio asks for more as its queue nears its end, and adds what is not
// in it already.
func TestRadioRefills(t *testing.T) {
	p := &Player{at: -1}
	p.Cue([]netease.Track{{ID: 1}, {ID: 2}})
	asked := 0
	p.AttachRadio("fm:DEFAULT", func(got func([]netease.Track)) {
		asked++
		got([]netease.Track{{ID: 2}, {ID: 3}})
	})
	if asked != 1 || len(p.Queue()) != 3 || p.Queue()[2].ID != 3 {
		t.Fatalf("asked %d, queue %v", asked, p.Queue())
	}
	if p.Radio() != "fm:DEFAULT" {
		t.Errorf("radio %q", p.Radio())
	}
	// Playing something else ends the radio.
	p.endRadio()
	if p.Radio() != "" || p.feed != nil {
		t.Error("radio still on")
	}
}

func TestNormGain(t *testing.T) {
	for _, c := range []struct {
		st   *netease.Stream
		want float64
	}{
		{nil, 1},
		{&netease.Stream{}, 1},
		{&netease.Stream{Gain: -6.6, Peak: 1}, math.Pow(10, -2.6/20)},
		// Quiet, it is lifted, but not past its peak.
		{&netease.Stream{Gain: 3, Peak: 0.8}, 1.25},
		{&netease.Stream{Gain: -4, Peak: 0}, 1},
	} {
		if g := normGain(c.st); math.Abs(g-c.want) > 1e-9 {
			t.Errorf("%+v: %v, want %v", c.st, g, c.want)
		}
	}
}

// The cache gives back what it kept, at the level asked for or a better
// one, and drops what was played longest ago once over its limit.
func TestCache(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir, 400)
	store := func(id int64, st *netease.Stream, n int) {
		f := c.part(st)
		f.Write(make([]byte, n))
		f.Close()
		c.keep(id, st, f.Name(), int64(n))
	}
	store(1, &netease.Stream{Level: "lossless", Type: "flac", Gain: -3}, 100)
	dl, st, ok := c.lookup(1, "exhigh")
	if !ok || dl.size != 100 || st.Type != "flac" || st.Gain != -3 {
		t.Fatalf("lookup: %v %+v", ok, st)
	}
	dl.Close()
	if _, _, ok := c.lookup(1, "hires"); ok {
		t.Error("found a lossless song for Hi-Res")
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "1-lossless.flac"), old, old)
	store(2, &netease.Stream{Level: "exhigh", Type: "mp3"}, 100)
	store(3, &netease.Stream{Level: "exhigh", Type: "mp3"}, 100)
	store(4, &netease.Stream{Level: "exhigh", Type: "mp3"}, 100)
	store(5, &netease.Stream{Level: "exhigh", Type: "mp3"}, 100)
	if _, _, ok := c.lookup(1, "standard"); ok {
		t.Error("kept the song played longest ago")
	}
	if dl, _, ok := c.lookup(5, "standard"); !ok {
		t.Error("dropped the song just kept")
	} else {
		dl.Close()
	}
	if n := c.Size(); n > 400 {
		t.Errorf("takes %d of 400", n)
	}
	c.Clear()
	if n := c.Size(); n != 0 {
		t.Errorf("cleared to %d", n)
	}
}

// Leaving a radio keeps the song playing and those before it, drops those
// it queued after, and asks it for no more.
func TestEndRadio(t *testing.T) {
	p := &Player{at: -1}
	p.Cue([]netease.Track{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}})
	p.at, p.track = 1, &p.queue[1]
	asked := 0
	p.AttachRadio("heart", func(got func([]netease.Track)) { asked++ })
	p.EndRadio()
	if p.Radio() != "" || p.Current().ID != 2 {
		t.Fatalf("radio %q, current %v", p.Radio(), p.Current())
	}
	if q := p.Queue(); len(q) != 2 || q[0].ID != 1 || q[1].ID != 2 {
		t.Fatalf("queue %v", q)
	}
	p.refill()
	if asked != 0 {
		t.Error("asked the radio after leaving it")
	}
}

// A Dolby or surround stream can be kept in the cache as an MP4, not only
// as MP3 or FLAC.
func TestCacheKeepsMP4(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir, 400)
	f := c.part(&netease.Stream{Level: "dolby", Type: "mp4"})
	f.Write(make([]byte, 100))
	f.Close()
	if _, ok := c.keep(1, &netease.Stream{Level: "dolby", Type: "mp4"}, f.Name(), 100); !ok {
		t.Fatal("did not keep an MP4 stream")
	}
	dl, st, ok := c.lookup(1, "dolby")
	if !ok || st.Type != "mp4" {
		t.Fatalf("lookup: %v %+v", ok, st)
	}
	dl.Close()
}

// PlanPlayback applies output limits: the built-in player and DLNA get
// stereo spatial masters, vivid needs its helper, and passthrough yields
// to EQ or headphone spatialisation.
func TestPlanPlayback(t *testing.T) {
	tests := []struct {
		name        string
		req         PlaybackRequest
		level       string
		layout      string
		passthrough string
		spatial     bool
		exclusive   bool
		hrtf        bool
		reason      bool
	}{
		{
			name:  "builtin sky falls to jyeffect",
			req:   PlaybackRequest{RequestedLevel: "sky", Engine: EngineBuiltin},
			level: "jyeffect", layout: "stereo", reason: true,
		},
		{
			name:  "builtin dolby falls to jyeffect",
			req:   PlaybackRequest{RequestedLevel: "dolby", Engine: EngineBuiltin},
			level: "jyeffect", layout: "stereo", reason: true,
		},
		{
			name:  "builtin vivid falls to sky then jyeffect",
			req:   PlaybackRequest{RequestedLevel: "vivid", Engine: EngineBuiltin, AV3AReady: true},
			level: "jyeffect", layout: "stereo", reason: true,
		},
		{
			name:  "vivid without helper falls to sky",
			req:   PlaybackRequest{RequestedLevel: "vivid", Engine: EngineMPV, AV3AReady: false},
			level: "sky", layout: "5.1", reason: true,
		},
		{
			name:  "dlna spatial falls to jyeffect",
			req:   PlaybackRequest{RequestedLevel: "dolby", Engine: EngineDLNA},
			level: "jyeffect", layout: "stereo", reason: true,
		},
		{
			name:  "headphones sky uses HRTF",
			req:   PlaybackRequest{RequestedLevel: "sky", SpatialMode: "headphones", Engine: EngineBuiltin, HRTFAvailable: true},
			level: "sky", layout: "5.1(side)", spatial: true, hrtf: true,
		},
		{
			name:  "headphones dolby uses HRTF sky",
			req:   PlaybackRequest{RequestedLevel: "dolby", SpatialMode: "headphones", Engine: EngineMPV, HRTFAvailable: true},
			level: "sky", layout: "5.1(side)", spatial: true, hrtf: true, reason: true,
		},
		{
			name:  "headphones vivid uses HRTF sky",
			req:   PlaybackRequest{RequestedLevel: "vivid", SpatialMode: "headphones", Engine: EngineMPV, AV3AReady: true, HRTFAvailable: true},
			level: "sky", layout: "5.1(side)", spatial: true, hrtf: true, reason: true,
		},
		{
			name:  "mpv vivid keeps 7.1.4",
			req:   PlaybackRequest{RequestedLevel: "vivid", Engine: EngineMPV, AV3AReady: true},
			level: "vivid", layout: "7.1.4", exclusive: false,
		},
		{
			name:  "mpv stereo keeps hog mode",
			req:   PlaybackRequest{RequestedLevel: "lossless", Engine: EngineMPV},
			level: "lossless", layout: "stereo", exclusive: true,
		},
		{
			name:  "headphones spatialise surround",
			req:   PlaybackRequest{RequestedLevel: "sky", Engine: EngineMPV, SpatialMode: "headphones", AV3AReady: true},
			level: "sky", layout: "stereo", spatial: true, exclusive: false,
		},
		{
			name:  "jyeffect is not spatialised twice",
			req:   PlaybackRequest{RequestedLevel: "jyeffect", Engine: EngineMPV, SpatialMode: "headphones"},
			level: "jyeffect", layout: "stereo",
		},
		{
			name:  "spdif passes through only without processing",
			req:   PlaybackRequest{RequestedLevel: "lossless", Engine: EngineMPV, SpdifPreferred: true},
			level: "lossless", layout: "stereo", passthrough: "eac3", exclusive: true,
		},
		{
			name:  "eq wins over spdif",
			req:   PlaybackRequest{RequestedLevel: "lossless", Engine: EngineMPV, SpdifPreferred: true, EqualizerOn: true},
			level: "lossless", layout: "stereo", passthrough: "", exclusive: true, reason: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := PlanPlayback(tt.req)
			if plan.EffectiveLevel != tt.level {
				t.Errorf("level = %q, want %q", plan.EffectiveLevel, tt.level)
			}
			if plan.TargetLayout != tt.layout {
				t.Errorf("layout = %q, want %q", plan.TargetLayout, tt.layout)
			}
			if plan.Passthrough != tt.passthrough {
				t.Errorf("passthrough = %q, want %q", plan.Passthrough, tt.passthrough)
			}
			if plan.Spatial != tt.spatial {
				t.Errorf("spatial = %v, want %v", plan.Spatial, tt.spatial)
			}
			if plan.Exclusive != tt.exclusive {
				t.Errorf("exclusive = %v, want %v", plan.Exclusive, tt.exclusive)
			}
			if plan.HRTF != tt.hrtf {
				t.Errorf("hrtf = %v, want %v", plan.HRTF, tt.hrtf)
			}
			if (plan.FallbackReason != "") != tt.reason {
				t.Errorf("fallback reason = %q, want present=%v", plan.FallbackReason, tt.reason)
			}
		})
	}
}

func TestCacheKeepsVividWAV(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir, 400)
	path := filepath.Join(dir, "decoded.wav")
	if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := c.keepVivid(7, path)
	if !ok || got != c.VividPath(7) {
		t.Fatalf("keepVivid = %q, %v; VividPath = %q", got, ok, c.VividPath(7))
	}
	if n := c.Size(); n != 100 {
		t.Fatalf("size = %d, want 100", n)
	}
	c.Clear()
	if p := c.VividPath(7); p != "" {
		t.Fatalf("VividPath after Clear = %q", p)
	}
}

// flacChannels maps a channel count to the FLAC channel assignment.
func flacChannels(n int) frame.Channels {
	switch n {
	case 1:
		return frame.ChannelsMono
	case 2:
		return frame.ChannelsLR
	case 3:
		return frame.ChannelsLRC
	case 4:
		return frame.ChannelsLRLsRs
	case 5:
		return frame.ChannelsLRCLsRs
	case 6:
		return frame.ChannelsLRCLfeLsRs
	}
	return frame.ChannelsMono
}

// encodeFLAC makes a short FLAC file with n real channels.
func encodeFLAC(t *testing.T, n, frames int) []byte {
	t.Helper()
	var buf bytes.Buffer
	info := &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  16,
		SampleRate:    48000,
		NChannels:     uint8(n),
		BitsPerSample: 16,
		NSamples:      uint64(frames),
	}
	enc, err := flac.NewEncoder(&buf, info)
	if err != nil {
		t.Fatal(err)
	}
	for at := 0; at < frames; at += 16 {
		subs := make([]*frame.Subframe, n)
		for c := 0; c < n; c++ {
			samples := make([]int32, 16)
			for i := range samples {
				samples[i] = int32((at+i)*100 + c*17 - 5000)
			}
			subs[c] = &frame.Subframe{Pred: frame.PredVerbatim, Samples: samples, NSamples: 16}
		}
		f := &frame.Frame{
			Header: frame.Header{
				BlockSize:     16,
				SampleRate:    48000,
				Channels:      flacChannels(n),
				BitsPerSample: 16,
			},
			Subframes: subs,
		}
		if err := enc.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The decoder reports a FLAC file's real channel count: a mono file must
// not be presented as stereo silence, and a stereo file keeps both sides.
func TestFLACChannels(t *testing.T) {
	for _, n := range []int{1, 2} {
		dl := downloaded(encodeFLAC(t, n, 32))
		s, err := openFLACStream(dl)
		if err != nil {
			t.Fatalf("%d channels: %v", n, err)
		}
		if got := s.Channels(); got != n {
			t.Errorf("%d channels: Channels() = %d", n, got)
		}
		buf := make([]float32, 16*n)
		if got, err := s.Read(buf); err != nil || got != len(buf) {
			t.Errorf("%d channels: Read = %d, %v", n, got, err)
		}
		s.Close()
	}
}

// A surround FLAC must be rejected with its real channel count instead of
// being downmixed to the first two channels and played as if complete.
func TestMultichannelFLACRejected(t *testing.T) {
	dl := downloaded(encodeFLAC(t, 6, 32))
	_, err := openFLACStream(dl)
	var mc *ErrMultichannelFLAC
	if !errors.As(err, &mc) {
		t.Fatalf("err = %v, want ErrMultichannelFLAC", err)
	}
	if mc.Channels != 6 {
		t.Errorf("channels = %d, want 6", mc.Channels)
	}
	p := &Player{}
	if got := p.loadErrorMessage(err); got != "该音源为 6 声道，需要启用 MPV 或使用立体声版本" {
		t.Errorf("message = %q", got)
	}
	dl.Close()
}

// RepeatOne restarts on the engine that owns playback: the built-in
// decoder seeks, MPV loops itself, DLNA replays the track, and a missing
// output does nothing.
func TestRepeatOneStepByOutput(t *testing.T) {
	for _, tt := range []struct {
		kind outputKind
		want repeatOneStep
	}{
		{outputNone, repeatOneNothing},
		{outputBuiltin, repeatOneBuiltin},
		{outputMPV, repeatOneMPV},
		{outputDLNA, repeatOneDLNA},
	} {
		if got := repeatOneStepFor(tt.kind); got != tt.want {
			t.Errorf("output %d: step = %d, want %d", tt.kind, got, tt.want)
		}
	}
}

// statusStream is a decoder with a real channel count, for Status.
type statusStream struct{ channels int }

func (s statusStream) SampleRate() int                { return 48000 }
func (s statusStream) Channels() int                  { return s.channels }
func (s statusStream) Len() int64                     { return 0 }
func (s statusStream) SetPosition(int64) error        { return nil }
func (s statusStream) Read([]float32) (int, error)    { return 0, io.EOF }
func (s statusStream) Close() error                   { return nil }
func (s statusStream) source() (*download, *spectrum) { return nil, nil }

// Status reports the built-in decoder's real input channels and the
// stereo output it mixes to, not a guessed surround count.
func TestStatusBuiltin(t *testing.T) {
	p := &Player{
		info:   &netease.Stream{Type: "flac", Level: "lossless", ServedLevel: "lossless", LevelReported: true},
		stream: statusStream{channels: 1},
		plan:   PlanPlayback(PlaybackRequest{RequestedLevel: "lossless", Engine: EngineBuiltin}),
	}
	st := p.Status()
	if st.Engine != "builtin" || st.SourceCodec != "flac" {
		t.Fatalf("engine/codec = %q/%q", st.Engine, st.SourceCodec)
	}
	if st.InputChannels != 1 || st.OutputChannels != 2 || st.OutputLayout != "stereo" {
		t.Fatalf("channels = %d -> %d (%q)", st.InputChannels, st.OutputChannels, st.OutputLayout)
	}
	if !st.LevelReported || st.ServedLevel != "lossless" {
		t.Errorf("served level = %q, reported %v", st.ServedLevel, st.LevelReported)
	}
}

// A server that answers a vivid request with exhigh must not leave the
// HRTF plan in place: exhigh is stereo, not a 5.1 master.
func TestPlanForServedDropsHRTFWhenServerReturnsStereo(t *testing.T) {
	p := &Player{}
	p.mpvCfg.SpatialMode = "headphones"
	plan := PlanPlayback(PlaybackRequest{
		RequestedLevel: "vivid",
		SpatialMode:    "headphones",
		Engine:         EngineBuiltin,
		HRTFAvailable:  true,
	})
	if !plan.HRTF || plan.EffectiveLevel != "sky" {
		t.Fatalf("initial plan = %+v", plan)
	}
	got := p.planForServed(plan, &netease.Stream{Level: "exhigh"})
	if got.HRTF || got.EffectiveLevel != "exhigh" {
		t.Fatalf("served exhigh plan = %+v, want stereo exhigh", got)
	}
	if got := p.planForServed(plan, &netease.Stream{Level: "sky"}); !got.HRTF || got.EffectiveLevel != "sky" {
		t.Fatalf("served sky plan = %+v, want HRTF sky", got)
	}
}

// A player with nothing configured still reports a usable engine name.
func TestStatusZeroPlayer(t *testing.T) {
	var p Player
	if st := p.Status(); st.Engine != "builtin" {
		t.Errorf("engine = %q, want builtin", st.Engine)
	}
}

// Leaving a track cancels the load context, so its download and Vivid
// decode do not keep running for a song nobody hears.
func TestStopCancelsLoad(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Player{loadCtx: ctx, loadCancel: cancel}
	p.stop()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("load context err = %v, want canceled", ctx.Err())
	}
}
