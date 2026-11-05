package player

import (
	"io"
	"testing"
)

type testAudioStream struct {
	rate   int
	ch     int
	frames int
	pos    int
	at     func(frame, ch int) float32
}

func (s *testAudioStream) SampleRate() int { return s.rate }
func (s *testAudioStream) Channels() int   { return s.ch }
func (s *testAudioStream) Len() int64      { return int64(s.frames) }
func (s *testAudioStream) SetPosition(frame int64) error {
	s.pos = int(frame)
	return nil
}
func (s *testAudioStream) Read(dst []float32) (int, error) {
	if s.pos >= s.frames {
		return 0, io.EOF
	}
	n := len(dst) / s.ch
	if n > s.frames-s.pos {
		n = s.frames - s.pos
	}
	for i := 0; i < n; i++ {
		for c := 0; c < s.ch; c++ {
			dst[i*s.ch+c] = s.at(s.pos+i, c)
		}
	}
	s.pos += n
	return n * s.ch, nil
}
func (s *testAudioStream) Close() error                   { return nil }
func (s *testAudioStream) source() (*download, *spectrum) { return nil, nil }

func TestHRTFStreamRendersSixChannelsToStereo(t *testing.T) {
	src := &testAudioStream{rate: 44100, ch: 6, frames: 512, at: func(frame, ch int) float32 {
		if frame == 0 && ch == 0 {
			return 0.5
		}
		return 0
	}}
	h, err := newHRTFStream(src, "5.1(side)")
	if err != nil {
		t.Fatal(err)
	}
	if h.Channels() != 2 || h.SampleRate() != 44100 {
		t.Fatalf("hrtf stream = %d channels at %d Hz", h.Channels(), h.SampleRate())
	}
	buf := make([]float32, 256*2)
	n, err := h.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("Read = %d samples, want %d", n, len(buf))
	}
	var nonzero bool
	for _, v := range buf {
		if v != 0 {
			nonzero = true
			break
		}
	}
	if !nonzero {
		t.Fatal("HRTF output is silent")
	}
}

func TestHRTFStreamResamplesStereo(t *testing.T) {
	src := &testAudioStream{rate: 48000, ch: 2, frames: 480, at: func(frame, ch int) float32 {
		if frame == 0 {
			return 0.5
		}
		return 0
	}}
	h, err := newHRTFStream(src, "stereo")
	if err != nil {
		t.Fatal(err)
	}
	if h.Len() != 441 {
		t.Fatalf("Len = %d, want 441", h.Len())
	}
	buf := make([]float32, 441*2)
	n, err := h.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("Read = %d samples, want %d", n, len(buf))
	}
}

func TestHRTFStreamReadsSixChannelFLAC(t *testing.T) {
	dl := downloaded(encodeFLAC(t, 6, 64))
	src, err := openFLACStreamAllow(dl, true)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	h, err := newHRTFStream(src, "5.1(side)")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 32*2)
	n, err := h.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("Read = %d samples, want %d", n, len(buf))
	}
}

func TestHRTFLeftSourceFavoursLeftEar(t *testing.T) {
	src := &testAudioStream{rate: 44100, ch: 6, frames: 512, at: func(frame, ch int) float32 {
		if frame == 0 && ch == 0 { // front-left
			return 0.5
		}
		return 0
	}}
	h, err := newHRTFStream(src, "5.1(side)")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 512*2)
	if _, err := h.Read(buf); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	var left, right float32
	for i := 0; i < len(buf); i += 2 {
		left += abs(buf[i])
		right += abs(buf[i+1])
	}
	if left <= right {
		t.Fatalf("left energy %.6f <= right %.6f for a left source", left, right)
	}
}

func abs(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}
