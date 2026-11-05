package hrtf

import (
	"fmt"
	"math"
)

// Binauralizer convolves each input channel with the HRIR pair for its
// virtual speaker and sums the result to stereo.
//
// It is a streaming processor: ProcessInto can be called repeatedly with
// consecutive blocks, and the convolution history carries over.
type Binauralizer struct {
	dataset  *Dataset
	speakers []Speaker
	irLeft   [][]float32
	irRight  [][]float32
	historyL [][]float32
	historyR [][]float32
	pos      int
}

// NewBinauralizer builds a processor for speakers using the embedded
// dataset. It returns an error if the dataset is unavailable or a speaker
// has no HRIR measurement.
func NewBinauralizer(speakers []Speaker) (*Binauralizer, error) {
	ds, err := Load()
	if err != nil {
		return nil, err
	}
	if len(speakers) == 0 {
		return nil, fmt.Errorf("hrtf: no speakers")
	}
	b := &Binauralizer{
		dataset:  ds,
		speakers: append([]Speaker(nil), speakers...),
		irLeft:   make([][]float32, len(speakers)),
		irRight:  make([][]float32, len(speakers)),
		historyL: make([][]float32, len(speakers)),
		historyR: make([][]float32, len(speakers)),
	}
	taps := ds.Taps
	for i, speaker := range speakers {
		left, right := ds.Lookup(speaker.Azimuth, speaker.Elevation)
		if len(left) == 0 || len(right) == 0 {
			return nil, fmt.Errorf("hrtf: no HRIR for speaker %s", speaker.Name)
		}
		b.irLeft[i] = left
		b.irRight[i] = right
		b.historyL[i] = make([]float32, taps)
		b.historyR[i] = make([]float32, taps)
	}
	return b, nil
}

// Channels returns the number of input channels this processor expects.
func (b *Binauralizer) Channels() int { return len(b.speakers) }

// SampleRate returns the dataset sample rate.
func (b *Binauralizer) SampleRate() int { return b.dataset.SampleRate }

// Reset clears the convolution history, as after a seek.
func (b *Binauralizer) Reset() {
	for i := range b.historyL {
		clear(b.historyL[i])
		clear(b.historyR[i])
	}
	b.pos = 0
}

// ProcessInto consumes frames interleaved source frames and writes frames
// stereo frames into dst. src must hold at least frames*Channels() samples
// and dst at least frames*2.
func (b *Binauralizer) ProcessInto(dst, src []float32, frames int) error {
	channels := len(b.speakers)
	if frames < 0 || len(src) < frames*channels || len(dst) < frames*2 {
		return fmt.Errorf("hrtf: short buffer (frames=%d, src=%d, dst=%d)", frames, len(src), len(dst))
	}
	taps := b.dataset.Taps
	gainScale := float32(1 / math.Sqrt(float64(channels)))
	pos := b.pos
	for f := 0; f < frames; f++ {
		var left, right float32
		base := f * channels
		for s, speaker := range b.speakers {
			x := src[base+s] * speaker.Gain * gainScale
			hl := b.historyL[s]
			hr := b.historyR[s]
			hl[pos] = x
			hr[pos] = x
			il := b.irLeft[s]
			ir := b.irRight[s]
			idx := pos
			var l, r float32
			for k := 0; k < taps; k++ {
				l += hl[idx] * il[k]
				r += hr[idx] * ir[k]
				idx--
				if idx < 0 {
					idx = taps - 1
				}
			}
			left += l
			right += r
		}
		dst[f*2] = clamp(left)
		dst[f*2+1] = clamp(right)
		pos++
		if pos == taps {
			pos = 0
		}
	}
	b.pos = pos
	return nil
}

func clamp(v float32) float32 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}
