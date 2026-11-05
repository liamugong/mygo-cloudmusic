// Package hrtf renders multichannel audio to stereo with a measured
// head-related impulse response (HRIR) dataset.
//
// The dataset is a compact, embedded conversion of the CIPIC subject 003
// HRIRs. It has 1250 directions over the full sphere, two ears and 200
// samples at 44.1 kHz. No HRTF data is downloaded at runtime.
package hrtf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	_ "embed"
)

//go:embed data/cipic_subject_003.hrtf
var embeddedDataset []byte

const datasetMagic = "MYHRTF1\x00"

// Direction is a SOFA-style horizontal coordinate: azimuth is measured
// counter-clockwise from the front (positive is left), elevation is above
// the horizontal plane. Both are degrees.
type Direction struct {
	Azimuth   float64
	Elevation float64
}

// Dataset is a measured HRIR set. Left and Right have one slice of Taps
// samples per direction; they share the same index as Directions.
type Dataset struct {
	SampleRate int
	Directions []Direction
	Left       [][]float32
	Right      [][]float32
	Taps       int
}

var (
	loadOnce sync.Once
	loaded   *Dataset
	loadErr  error
)

// Load returns the embedded dataset, reading it once.
func Load() (*Dataset, error) {
	loadOnce.Do(func() {
		loaded, loadErr = decodeDataset(embeddedDataset)
	})
	return loaded, loadErr
}

// Available reports whether the embedded dataset can be loaded. It is
// cheap after the first call.
func Available() bool {
	_, err := Load()
	return err == nil
}

func decodeDataset(b []byte) (*Dataset, error) {
	if len(b) < 24 || string(b[:8]) != datasetMagic {
		return nil, errors.New("hrtf: bad dataset header")
	}
	sampleRate := int(binary.LittleEndian.Uint32(b[8:]))
	m := int(binary.LittleEndian.Uint32(b[12:]))
	taps := int(binary.LittleEndian.Uint32(b[16:]))
	channels := int(binary.LittleEndian.Uint32(b[20:]))
	if sampleRate <= 0 || m <= 0 || taps <= 0 || channels != 2 {
		return nil, errors.New("hrtf: invalid dataset dimensions")
	}
	want := 24 + m*(8+2*taps*4)
	if len(b) != want {
		return nil, fmt.Errorf("hrtf: dataset is %d bytes, want %d", len(b), want)
	}
	d := &Dataset{
		SampleRate: sampleRate,
		Directions: make([]Direction, m),
		Left:       make([][]float32, m),
		Right:      make([][]float32, m),
		Taps:       taps,
	}
	off := 24
	for i := 0; i < m; i++ {
		d.Directions[i] = Direction{
			Azimuth:   float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))),
			Elevation: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off+4:]))),
		}
		off += 8
		d.Left[i] = make([]float32, taps)
		for j := range d.Left[i] {
			d.Left[i][j] = math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))
			off += 4
		}
		d.Right[i] = make([]float32, taps)
		for j := range d.Right[i] {
			d.Right[i][j] = math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))
			off += 4
		}
	}
	return d, nil
}

// Lookup returns the measured HRIR pair whose direction is closest to the
// requested azimuth/elevation. It returns nil slices when no measurement
// is available.
func (d *Dataset) Lookup(azimuth, elevation float64) (left, right []float32) {
	if d == nil || len(d.Directions) == 0 {
		return nil, nil
	}
	target := directionVector(azimuth, elevation)
	best := -2.0
	bestIndex := -1
	for i, dir := range d.Directions {
		score := dot(directionVector(dir.Azimuth, dir.Elevation), target)
		if score > best {
			best, bestIndex = score, i
		}
	}
	if bestIndex < 0 || bestIndex >= len(d.Left) {
		return nil, nil
	}
	return d.Left[bestIndex], d.Right[bestIndex]
}

// directionVector converts a SOFA azimuth/elevation to a unit vector with
// +X front, +Y left and +Z up.
func directionVector(azimuth, elevation float64) [3]float64 {
	az := azimuth * math.Pi / 180
	el := elevation * math.Pi / 180
	cosEl := math.Cos(el)
	return [3]float64{cosEl * math.Cos(az), cosEl * math.Sin(az), math.Sin(el)}
}

func dot(a, b [3]float64) float64 {
	return a[0]*b[0] + a[1]*b[1] + a[2]*b[2]
}
