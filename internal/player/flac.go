package player

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// ErrMultichannelFLAC reports FLAC with more channels than the built-in
// decoder can play: taking only the first two would silently discard the
// rest of the mix. MPV can play such a stream; the built-in player cannot.
type ErrMultichannelFLAC struct {
	Channels int
}

func (e *ErrMultichannelFLAC) Error() string {
	return fmt.Sprintf("该音源为 %d 声道，需要启用 MPV 或使用立体声版本", e.Channels)
}

// flacStream decodes a FLAC download for package audio: the lossless and
// Hi-Res streams. Its frames stand alone, borrowing nothing from those
// before, so a seek lands on the very sample: decoding from the frame that
// holds it, found by its header near where the bytes say it is.
type flacStream struct {
	dl       *download
	info     *meta.StreamInfo
	channels int   // the real channel count, never a fixed stereo pair
	start    int64 // the first frame's byte, after the metadata
	r        *bufio.Reader
	pos      int64     // the frame Read returns next
	skip     int64     // frames to drop before it, after a seek
	pend     []float32 // decoded, and not yet read: interleaved frames
	spec     *spectrum
	drop     func() // lets the download go, as the stream closes
	once     sync.Once
}

func openFLACStream(dl *download) (*flacStream, error) {
	return openFLACStreamAllow(dl, false)
}

// openFLACStreamAllow opens a FLAC stream. When allowMultichannel is true
// the frame decoder keeps every channel; the HRTF renderer then consumes
// them. Without it, more than two channels are rejected rather than
// silently truncated.
func openFLACStreamAllow(dl *download, allowMultichannel bool) (*flacStream, error) {
	start, err := flacDataStart(dl)
	if err != nil {
		return nil, err
	}
	from := id3Size(dl)
	st, err := flac.New(bytes.NewReader(dl.peek(from, int(start-from))))
	if err != nil {
		return nil, err
	}
	s := &flacStream{dl: dl, info: st.Info, start: start, drop: dl.Close}
	if s.info.NChannels == 0 || s.info.SampleRate == 0 {
		return nil, errors.New("flac: no stream info")
	}
	s.channels = int(s.info.NChannels)
	if s.channels > 2 && !allowMultichannel {
		return nil, &ErrMultichannelFLAC{Channels: s.channels}
	}
	if s.channels <= 2 {
		s.spec = newSpectrum(int(s.info.SampleRate), int64(s.info.NSamples))
	}
	s.r = bufio.NewReaderSize(&downloadReader{d: dl, pos: start}, 64<<10)
	return s, nil
}

// flacDataStart returns where a FLAC file's first frame starts, after its
// ID3 tag, if any, and its metadata blocks.
func flacDataStart(dl *download) (int64, error) {
	off := id3Size(dl)
	if dl.waitFor(off+4) < off+4 || string(dl.peek(off, 4)) != "fLaC" {
		return 0, errors.New("flac: not a FLAC file")
	}
	off += 4
	for {
		if dl.waitFor(off+4) < off+4 {
			return 0, io.ErrUnexpectedEOF
		}
		h := dl.peek(off, 4)
		last := h[0]&0x80 != 0
		off += 4 + (int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3]))
		if last {
			if dl.waitFor(off) < off {
				return 0, io.ErrUnexpectedEOF
			}
			return off, nil
		}
	}
}

func (s *flacStream) SampleRate() int { return int(s.info.SampleRate) }

func (s *flacStream) Channels() int {
	if s.channels > 0 {
		return s.channels
	}
	return int(s.info.NChannels)
}

func (s *flacStream) Len() int64 {
	if s.info.NSamples == 0 {
		return -1
	}
	return int64(s.info.NSamples)
}

// parse parses the frame r is at: its header, then its samples, taking
// what the header leaves to the stream info from there.
func (s *flacStream) parse(r io.Reader) (*frame.Frame, error) {
	f, err := frame.New(r)
	if err != nil {
		return nil, err
	}
	if f.BitsPerSample == 0 {
		f.BitsPerSample = s.info.BitsPerSample
	}
	if f.SampleRate == 0 {
		f.SampleRate = s.info.SampleRate
	}
	if err := f.Parse(); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *flacStream) Read(dst []float32) (int, error) {
	ch := max(s.Channels(), 1)
	if len(dst) < ch {
		return 0, io.ErrShortBuffer
	}
	for len(s.pend) == 0 {
		f, err := s.parse(s.r)
		if err != nil {
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			// When parsing fails at or near the end of stream (e.g. trailing ID3 tags or padding),
			// treat it as clean EOF if we have already read all expected samples.
			if s.info.NSamples > 0 && s.pos >= int64(s.info.NSamples) {
				return 0, io.EOF
			}
			if strings.Contains(err.Error(), "invalid sync-code") && s.info.NSamples > 0 && s.pos+int64(s.info.BlockSizeMax) >= int64(s.info.NSamples) {
				return 0, io.EOF
			}
			return 0, err
		}
		s.decode(f)
	}
	n := copy(dst[:len(dst)/ch*ch], s.pend)
	s.pend = s.pend[n:]
	// The spectrum measures a mono signal as an average of two equal
	// channels; duplicate it rather than feeding an odd number of samples.
	if s.spec != nil {
		if ch == 1 {
			stereo := make([]float32, 0, n*2)
			for _, v := range dst[:n] {
				stereo = append(stereo, v, v)
			}
			s.spec.feed(s.pos, stereo)
		} else {
			s.spec.feed(s.pos, dst[:n])
		}
	}
	s.pos += int64(n / ch)
	return n, nil
}

// decode adds a frame's samples to those pending, in the stream's own
// channel order, but those to skip.
func (s *flacStream) decode(f *frame.Frame) {
	n := int64(f.BlockSize)
	from := min(s.skip, n)
	s.skip -= from
	if from == n {
		return
	}
	if len(f.Subframes) < s.Channels() {
		return
	}
	scale := float32(1 / math.Exp2(float64(f.BitsPerSample-1)))
	if cap(s.pend) < int(n)*s.Channels() {
		s.pend = make([]float32, 0, int(n)*s.Channels())
	}
	out := s.pend[:0]
	for i := from; i < n; i++ {
		for c := 0; c < s.Channels(); c++ {
			out = append(out, float32(f.Subframes[c].Samples[i])*scale)
		}
	}
	s.pend = out
}

// SetPosition goes to frame: to the frame of FLAC holding it, then drops
// what comes before it there. It finds that frame by the secant method,
// guessing where in the bytes it is from the frames found on either side,
// until one is found less than a second before it.
func (s *flacStream) SetPosition(target int64) error {
	total := int64(s.info.NSamples)
	size := s.dl.size
	if size <= 0 {
		size = s.dl.waitFor(math.MaxInt64)
	}
	loOff, lo := s.start, int64(0)
	hiOff, hi := size, total
	for tries := 0; tries < 8 && target-lo > int64(s.info.SampleRate) && hi > lo; tries++ {
		guess := loOff + int64(float64(hiOff-loOff)*float64(target-lo)/float64(hi-lo))
		off, at, ok := s.frameAfter(max(guess, loOff+1), hiOff)
		if !ok {
			break
		}
		if at > target {
			hiOff, hi = off, at
		} else {
			loOff, lo = off, at
		}
	}
	s.r = bufio.NewReaderSize(&downloadReader{d: s.dl, pos: loOff}, 64<<10)
	s.pos, s.skip, s.pend = target, target-lo, s.pend[:0]
	return nil
}

// frameAfter returns where the first whole frame at or after off and
// before end starts, and the frame of sound it starts with. Bytes that
// only look like a frame's header within the audio fail its checksums.
func (s *flacStream) frameAfter(off, end int64) (int64, int64, bool) {
	w := window{dl: s.dl}
	for ; off+2 < end; off++ {
		if s.dl.waitFor(off+2) < off+2 {
			return 0, 0, false
		}
		b := w.at(off, 2)
		if b[0] != 0xff || b[1]&0xfe != 0xf8 {
			continue
		}
		if at, ok := s.frameAt(off); ok {
			return off, at, true
		}
	}
	return 0, 0, false
}

// frameAt returns the frame of sound the frame of FLAC at off starts with,
// if a whole one is there. The decoder may panic on bytes that only
// passed the header's checksum.
func (s *flacStream) frameAt(off int64) (at int64, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	f, err := s.parse(bufio.NewReaderSize(&downloadReader{d: s.dl, pos: off}, 16<<10))
	if err != nil || f.Channels.Count() != int(s.info.NChannels) {
		return 0, false
	}
	return int64(f.SampleNumber()), true
}

func (s *flacStream) Close() error {
	s.once.Do(s.drop)
	return nil
}

func (s *flacStream) source() (*download, *spectrum) { return s.dl, s.spec }
