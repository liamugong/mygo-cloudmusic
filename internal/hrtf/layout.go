package hrtf

import (
	"fmt"
	"strings"
)

// Speaker is a virtual loudspeaker position in degrees and a linear gain.
// The positions follow the listener-relative SOFA convention: azimuth 0 is
// front, +90 is left, -90 is right.
type Speaker struct {
	Name      string
	Azimuth   float64
	Elevation float64
	Gain      float32
}

// LayoutFor maps a channel count and a NetEase/FLAC layout name to virtual
// speaker positions. Unknown names fall back to the standard order for the
// channel count.
func LayoutFor(layout string, channels int) ([]Speaker, error) {
	name := strings.ToLower(strings.TrimSpace(layout))
	switch channels {
	case 1:
		return []Speaker{{Name: "C", Azimuth: 0, Gain: 1}}, nil
	case 2:
		return []Speaker{
			{Name: "L", Azimuth: 30, Gain: 1},
			{Name: "R", Azimuth: -30, Gain: 1},
		}, nil
	case 6:
		side := strings.Contains(name, "side")
		left, right := 110.0, -110.0
		if side {
			left, right = 90, -90
		}
		return []Speaker{
			{Name: "FL", Azimuth: 30, Gain: 1},
			{Name: "FR", Azimuth: -30, Gain: 1},
			{Name: "FC", Azimuth: 0, Gain: 1},
			{Name: "LFE", Azimuth: 0, Gain: 0.35},
			{Name: "SL", Azimuth: left, Gain: 0.85},
			{Name: "SR", Azimuth: right, Gain: 0.85},
		}, nil
	case 8:
		return []Speaker{
			{Name: "FL", Azimuth: 30, Gain: 1},
			{Name: "FR", Azimuth: -30, Gain: 1},
			{Name: "FC", Azimuth: 0, Gain: 1},
			{Name: "LFE", Azimuth: 0, Gain: 0.35},
			{Name: "BL", Azimuth: 135, Gain: 0.85},
			{Name: "BR", Azimuth: -135, Gain: 0.85},
			{Name: "SL", Azimuth: 90, Gain: 0.85},
			{Name: "SR", Azimuth: -90, Gain: 0.85},
		}, nil
	case 12:
		return []Speaker{
			{Name: "FL", Azimuth: 30, Gain: 1},
			{Name: "FR", Azimuth: -30, Gain: 1},
			{Name: "FC", Azimuth: 0, Gain: 1},
			{Name: "LFE", Azimuth: 0, Gain: 0.35},
			{Name: "BL", Azimuth: 135, Gain: 0.8},
			{Name: "BR", Azimuth: -135, Gain: 0.8},
			{Name: "SL", Azimuth: 90, Gain: 0.8},
			{Name: "SR", Azimuth: -90, Gain: 0.8},
			{Name: "TFL", Azimuth: 45, Elevation: 45, Gain: 0.75},
			{Name: "TFR", Azimuth: -45, Elevation: 45, Gain: 0.75},
			{Name: "TBL", Azimuth: 135, Elevation: 45, Gain: 0.7},
			{Name: "TBR", Azimuth: -135, Elevation: 45, Gain: 0.7},
		}, nil
	default:
		return nil, fmt.Errorf("hrtf: no speaker layout for %d channels", channels)
	}
}
