package hrtf

import (
	"math"
	"testing"
)

func TestEmbeddedDataset(t *testing.T) {
	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if d.SampleRate != 44100 || len(d.Directions) != 1250 || d.Taps != 200 {
		t.Fatalf("dataset = rate %d, directions %d, taps %d", d.SampleRate, len(d.Directions), d.Taps)
	}
	if len(d.Left) != len(d.Directions) || len(d.Right) != len(d.Directions) {
		t.Fatal("left/right HRIR arrays do not match directions")
	}
}

func TestLayoutFor(t *testing.T) {
	tests := []struct {
		layout   string
		channels int
		want     int
	}{
		{"", 1, 1},
		{"", 2, 2},
		{"5.1(side)", 6, 6},
		{"5.1", 6, 6},
		{"7.1", 8, 8},
		{"7.1.4", 12, 12},
	}
	for _, tt := range tests {
		got, err := LayoutFor(tt.layout, tt.channels)
		if err != nil {
			t.Fatalf("LayoutFor(%q, %d): %v", tt.layout, tt.channels, err)
		}
		if len(got) != tt.want {
			t.Fatalf("LayoutFor(%q, %d) = %d speakers, want %d", tt.layout, tt.channels, len(got), tt.want)
		}
	}
	if _, err := LayoutFor("", 7); err == nil {
		t.Fatal("LayoutFor accepted an unsupported channel count")
	}
}

func TestBinauralizerKeepsHistoryAcrossBlocks(t *testing.T) {
	speakers, err := LayoutFor("5.1(side)", 6)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBinauralizer(speakers)
	if err != nil {
		t.Fatal(err)
	}
	src := make([]float32, 512*6)
	for i := range src {
		src[i] = float32(math.Sin(float64(i)*0.01)) * 0.1
	}
	whole := make([]float32, 512*2)
	if err := b.ProcessInto(whole, src, 512); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	first := make([]float32, 256*2)
	second := make([]float32, 256*2)
	if err := b.ProcessInto(first, src[:256*6], 256); err != nil {
		t.Fatal(err)
	}
	if err := b.ProcessInto(second, src[256*6:], 256); err != nil {
		t.Fatal(err)
	}
	for i := range whole {
		var got float32
		if i < 256*2 {
			got = first[i]
		} else {
			got = second[i-256*2]
		}
		if math.Abs(float64(got-whole[i])) > 1e-6 {
			t.Fatalf("split block differs at %d: %g != %g", i, got, whole[i])
		}
	}
}

func TestDatasetLeftEarDominantAtLeft(t *testing.T) {
	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	left, right := d.Lookup(90, 0)
	var le, re float64
	for i := range left {
		le += float64(left[i] * left[i])
		re += float64(right[i] * right[i])
	}
	if le <= re {
		t.Fatalf("left measurement energy %g <= right %g", le, re)
	}
}

func BenchmarkBinauralizer12Ch(b *testing.B) {
	speakers, err := LayoutFor("7.1.4", 12)
	if err != nil {
		b.Fatal(err)
	}
	proc, err := NewBinauralizer(speakers)
	if err != nil {
		b.Fatal(err)
	}
	src := make([]float32, 256*12)
	dst := make([]float32, 256*2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := proc.ProcessInto(dst, src, 256); err != nil {
			b.Fatal(err)
		}
	}
}
