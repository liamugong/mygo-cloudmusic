package player

import (
	"errors"
	"io"
	"math"

	"netease-music-mygo/internal/hrtf"
)

// hrtfStream turns a decoded multichannel stream into binaural stereo.
// It resamples to the dataset's rate before the convolution so the HRIR
// timing stays correct.
type hrtfStream struct {
	src     stream
	b       *hrtf.Binauralizer
	outRate int
	inRate  int
	inCh    int
	res     *linearResampler
	eof     bool
}

func newHRTFStream(src stream, layout string) (*hrtfStream, error) {
	if src == nil || src.Channels() < 1 {
		return nil, errors.New("hrtf: source has no channels")
	}
	speakers, err := hrtf.LayoutFor(layout, src.Channels())
	if err != nil {
		return nil, err
	}
	b, err := hrtf.NewBinauralizer(speakers)
	if err != nil {
		return nil, err
	}
	h := &hrtfStream{src: src, b: b, outRate: b.SampleRate(), inRate: src.SampleRate(), inCh: src.Channels()}
	if h.outRate <= 0 || h.inRate <= 0 {
		return nil, errors.New("hrtf: invalid sample rate")
	}
	if h.inRate != h.outRate {
		h.res = newLinearResampler(src, h.inRate, h.outRate)
	}
	return h, nil
}

func (h *hrtfStream) SampleRate() int { return h.outRate }
func (h *hrtfStream) Channels() int   { return 2 }

func (h *hrtfStream) Len() int64 {
	n := h.src.Len()
	if n < 0 {
		return -1
	}
	return n * int64(h.outRate) / int64(h.inRate)
}

func (h *hrtfStream) SetPosition(frame int64) error {
	h.eof = false
	h.b.Reset()
	if h.res != nil {
		return h.res.SeekFrame(frame)
	}
	return h.src.SetPosition(frame)
}

func (h *hrtfStream) Read(dst []float32) (int, error) {
	if len(dst) < 2 {
		return 0, io.ErrShortBuffer
	}
	frames := len(dst) / 2
	src := make([]float32, frames*h.inCh)
	var nFrames int
	var err error
	if h.res != nil {
		nFrames, err = h.res.ReadFrames(src, frames)
	} else {
		var n int
		n, err = h.src.Read(src)
		nFrames = n / h.inCh
		if n%h.inCh != 0 {
			nFrames = 0
			err = io.ErrUnexpectedEOF
		}
	}
	if nFrames == 0 {
		if err != nil {
			h.eof = errors.Is(err, io.EOF)
			return 0, err
		}
		return 0, nil
	}
	if err := h.b.ProcessInto(dst[:nFrames*2], src, nFrames); err != nil {
		return 0, err
	}
	if err == io.EOF {
		h.eof = true
		err = nil
	}
	return nFrames * 2, err
}

func (h *hrtfStream) Close() error { return h.src.Close() }

func (h *hrtfStream) source() (*download, *spectrum) { return h.src.source() }

// linearResampler streams frames from src at inRate to outRate. It keeps
// one source frame of history so interpolation across Read calls is
// continuous.
type linearResampler struct {
	src     stream
	inCh    int
	inRate  int
	outRate int
	buf     []float32 // interleaved source frames
	start   int64     // source frame index of buf[0]
	next    int64     // next source frame to read into buf
	out     int64     // next output frame
	eof     bool
	eofErr  error
}

func newLinearResampler(src stream, inRate, outRate int) *linearResampler {
	return &linearResampler{src: src, inCh: src.Channels(), inRate: inRate, outRate: outRate}
}

func (r *linearResampler) SeekFrame(outFrame int64) error {
	srcFrame := outFrame * int64(r.inRate) / int64(r.outRate)
	if err := r.src.SetPosition(srcFrame); err != nil {
		return err
	}
	r.buf = r.buf[:0]
	// The next read from src begins at srcFrame; record that as the
	// buffer's first frame so frame(n) is not shifted by a seek.
	r.start, r.next, r.out = srcFrame, srcFrame, outFrame
	r.eof, r.eofErr = false, nil
	return nil
}

func (r *linearResampler) ReadFrames(dst []float32, frames int) (int, error) {
	if len(dst) < frames*r.inCh {
		return 0, io.ErrShortBuffer
	}
	nOut := 0
	for nOut < frames {
		pos := float64(r.out) * float64(r.inRate) / float64(r.outRate)
		base := int64(math.Floor(pos))
		frac := pos - float64(base)
		if !r.ensure(base + 1) {
			if err := r.eofErr; err != nil && !errors.Is(err, io.EOF) {
				return nOut, err
			}
			break
		}
		f0 := r.frame(base)
		f1 := r.frame(base + 1)
		if f1 == nil {
			f1 = f0
		}
		for c := 0; c < r.inCh; c++ {
			dst[nOut*r.inCh+c] = float32(float64(f0[c])*(1-frac) + float64(f1[c])*frac)
		}
		nOut++
		r.out++
	}
	if nOut == 0 && r.eof {
		return 0, io.EOF
	}
	return nOut, nil
}

func (r *linearResampler) ensure(frame int64) bool {
	for r.start+int64(len(r.buf)/r.inCh) <= frame {
		if r.eof {
			return false
		}
		block := make([]float32, 4096*r.inCh)
		n, err := r.src.Read(block)
		n -= n % r.inCh
		if n > 0 {
			r.buf = append(r.buf, block[:n]...)
		}
		if err != nil {
			r.eof = true
			r.eofErr = err
			if n == 0 {
				return false
			}
		}
	}
	return true
}

func (r *linearResampler) frame(n int64) []float32 {
	off := int(n-r.start) * r.inCh
	if off < 0 || off+r.inCh > len(r.buf) {
		return nil
	}
	return r.buf[off : off+r.inCh]
}
