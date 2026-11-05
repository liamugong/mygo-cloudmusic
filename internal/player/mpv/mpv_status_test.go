package mpv

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseAudioParams(t *testing.T) {
	tests := []struct {
		name     string
		props    map[string]string
		want     audioParamsInfo
		wantErrs int
	}{
		{
			name:  "full 5.1 s32",
			props: map[string]string{"format": "s32", "channel-count": "6", "channels": "5.1", "samplerate": "48000"},
			want:  audioParamsInfo{Format: "s32", Channels: 6, Layout: "5.1", SampleRate: 48000, BitDepth: 32},
		},
		{
			name: "empty before load",
			want: audioParamsInfo{},
		},
		{
			name:  "layout supplies missing channel count",
			props: map[string]string{"channels": "7.1.4"},
			want:  audioParamsInfo{Channels: 12, Layout: "7.1.4"},
		},
		{
			name:     "bad channel count falls back to layout",
			props:    map[string]string{"channel-count": "not-a-number", "channels": "stereo"},
			want:     audioParamsInfo{Channels: 2, Layout: "stereo"},
			wantErrs: 1,
		},
		{
			name:     "bad samplerate",
			props:    map[string]string{"samplerate": "fast"},
			want:     audioParamsInfo{},
			wantErrs: 1,
		},
		{
			name:  "float format",
			props: map[string]string{"format": "float"},
			want:  audioParamsInfo{Format: "float", BitDepth: 32},
		},
		{
			name:  "passthrough has no PCM bit depth",
			props: map[string]string{"format": "spdif-eac3", "channel-count": "2"},
			want:  audioParamsInfo{Format: "spdif-eac3", Channels: 2, BitDepth: 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, errs := parseAudioParams(tc.props)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseAudioParams() = %+v, want %+v", got, tc.want)
			}
			if len(errs) != tc.wantErrs {
				t.Fatalf("parseAudioParams() errors = %v, want %d", errs, tc.wantErrs)
			}
		})
	}
}

func TestResolvePipeline(t *testing.T) {
	const (
		eq      = "equalizer=f=1000:t=q:w=1:g=3"
		spatial = "lavfi=[crossfeed]"
	)
	tests := []struct {
		name        string
		passthrough bool
		eq          string
		spatial     string
		want        audioPipeline
	}{
		{
			name:        "passthrough ignores both filters",
			passthrough: true,
			eq:          eq,
			spatial:     spatial,
			want:        audioPipeline{Passthrough: true, IgnoredFilter: true, Reason: "直通模式已忽略 EQ/空间化"},
		},
		{
			name: "pcm keeps equalizer",
			eq:   eq,
			want: audioPipeline{Filters: eq},
		},
		{
			name:    "pcm keeps spatial filter",
			spatial: spatial,
			want:    audioPipeline{Filters: spatial},
		},
		{
			name: "pcm multichannel needs no filter",
			want: audioPipeline{},
		},
		{
			name:    "pcm combines both filters",
			eq:      eq,
			spatial: spatial,
			want:    audioPipeline{Filters: eq + "," + spatial},
		},
		{
			name:        "passthrough without filters is not an ignored setting",
			passthrough: true,
			want:        audioPipeline{Passthrough: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePipeline(tc.passthrough, tc.eq, tc.spatial); got != tc.want {
				t.Fatalf("resolvePipeline() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoopFileValue(t *testing.T) {
	if got := loopFileValue(true); got != "inf" {
		t.Fatalf("loopFileValue(true) = %q, want inf", got)
	}
	if got := loopFileValue(false); got != "no" {
		t.Fatalf("loopFileValue(false) = %q, want no", got)
	}
}

func TestReadyCache(t *testing.T) {
	origProbe, origNow, origTTL := readyProbe, readyNow, readyTTL
	defer func() {
		readyProbe, readyNow, readyTTL = origProbe, origNow, origTTL
		resetReadyCache()
	}()

	now := time.Unix(1_700_000_000, 0)
	readyNow = func() time.Time { return now }
	readyTTL = 10 * time.Second
	calls := 0
	readyProbe = func() bool {
		calls++
		return true
	}
	resetReadyCache()

	if !Ready() || !Ready() {
		t.Fatal("Ready() should return the cached positive probe result")
	}
	if calls != 1 {
		t.Fatalf("probe called %d times within TTL, want 1", calls)
	}

	now = now.Add(11 * time.Second)
	if !Ready() {
		t.Fatal("Ready() should probe again after TTL")
	}
	if calls != 2 {
		t.Fatalf("probe called %d times after TTL, want 2", calls)
	}
}

func TestIsDownloadedValidatesSizeAndSHA(t *testing.T) {
	origAsset, hadAsset := NativeAssets[Platform()]
	origCache := cacheDirOverride
	defer func() {
		if hadAsset {
			NativeAssets[Platform()] = origAsset
		} else {
			delete(NativeAssets, Platform())
		}
		SetCacheDir(origCache)
	}()

	content := []byte(strings.Repeat("0123456789abcdef", 128)) // 2048 bytes, above the stub-file floor
	sum := sha256.Sum256(content)
	asset := origAsset
	asset.Size = int64(len(content))
	asset.SHA256 = hex.EncodeToString(sum[:])
	NativeAssets[Platform()] = asset

	t.Run("matching file", func(t *testing.T) {
		dir := t.TempDir()
		SetCacheDir(dir)
		path := filepath.Join(dir, asset.Name)
		if err := os.WriteFile(path, content, 0644); err != nil {
			t.Fatal(err)
		}
		if !IsDownloaded() {
			t.Fatal("IsDownloaded() = false for matching size and SHA256")
		}
	})

	t.Run("wrong size", func(t *testing.T) {
		dir := t.TempDir()
		SetCacheDir(dir)
		path := filepath.Join(dir, asset.Name)
		if err := os.WriteFile(path, content[:8], 0644); err != nil {
			t.Fatal(err)
		}
		if IsDownloaded() {
			t.Fatal("IsDownloaded() = true for truncated file")
		}
	})

	t.Run("wrong digest", func(t *testing.T) {
		dir := t.TempDir()
		SetCacheDir(dir)
		path := filepath.Join(dir, asset.Name)
		bad := []byte(strings.Repeat("fedcba9876543210", 128))
		if err := os.WriteFile(path, bad, 0644); err != nil {
			t.Fatal(err)
		}
		if IsDownloaded() {
			t.Fatal("IsDownloaded() = true for file with wrong SHA256")
		}
	})
}

func TestPlayReappliesAudioOptions(t *testing.T) {
	if !Ready() {
		t.Skip("mpv unavailable")
	}
	path := filepath.Join(t.TempDir(), "reapply.wav")
	writePCM16WAV(t, path, 1)

	eng, err := NewEngine(Config{NullAO: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	eng.SetAudioOptions("stereo", "")
	// Simulate MPV dropping the setting after a device change; Play must
	// restore it from e.cfg before loading.
	eng.inst.SetProperty("audio-channels", "auto-safe")
	if err := eng.Play(path, nil, 0); err != nil {
		t.Fatal(err)
	}
	if got := eng.GetProperty("audio-channels"); got != "stereo" {
		t.Fatalf("audio-channels = %q after Play, want stereo", got)
	}
}

func TestSetAudioOptionsResetsChannels(t *testing.T) {
	if !Ready() {
		t.Skip("mpv unavailable")
	}
	eng, err := NewEngine(Config{NullAO: true, AudioChannels: "stereo", AudioSpdif: "eac3"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Empty settings must restore MPV's auto-safe channel layout, not keep
	// the layout configured for the previous output.
	eng.SetAudioOptions("", "")
	if got := eng.GetProperty("audio-channels"); got != "auto-safe" {
		t.Fatalf("audio-channels = %q, want auto-safe", got)
	}
	if got := eng.GetProperty("audio-spdif"); got != "" {
		t.Fatalf("audio-spdif = %q, want empty", got)
	}
}

func TestSetLoopFileRuntime(t *testing.T) {
	if !Ready() {
		t.Skip("mpv unavailable")
	}
	eng, err := NewEngine(Config{NullAO: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.SetLoopFile(true); err != nil {
		t.Fatalf("SetLoopFile(true): %v", err)
	}
	if got := eng.GetProperty("loop-file"); got != "inf" {
		t.Fatalf("loop-file = %q, want inf", got)
	}
	if err := eng.SetLoopFile(false); err != nil {
		t.Fatalf("SetLoopFile(false): %v", err)
	}
	if got := eng.GetProperty("loop-file"); got != "no" {
		t.Fatalf("loop-file = %q, want no", got)
	}
}

func TestAudioStatusRuntime(t *testing.T) {
	if !Ready() {
		t.Skip("mpv unavailable")
	}
	path := filepath.Join(t.TempDir(), "status.wav")
	writePCM16WAV(t, path, 3)

	eng, err := NewEngine(Config{NullAO: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Play(path, nil, 0); err != nil {
		t.Fatal(err)
	}

	var st AudioStatus
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st = eng.AudioStatus()
		if st.OutputChannels > 0 && st.SampleRate > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.OutputChannels != 2 {
		t.Fatalf("OutputChannels = %d, want 2 (status %+v)", st.OutputChannels, st)
	}
	if st.InputChannels != 2 {
		t.Fatalf("InputChannels = %d, want 2", st.InputChannels)
	}
	if st.SampleRate != 44100 {
		t.Fatalf("SampleRate = %d, want 44100", st.SampleRate)
	}
	if st.BitDepth != 16 {
		t.Fatalf("BitDepth = %d, want 16", st.BitDepth)
	}
	if !strings.Contains(strings.ToLower(st.SourceCodec), "pcm") {
		t.Fatalf("SourceCodec = %q, want PCM", st.SourceCodec)
	}
	if st.OutputDevice == "" {
		t.Fatal("OutputDevice is empty while output is active")
	}
	if st.Passthrough {
		t.Fatal("Passthrough = true for a PCM WAV")
	}
}

func TestAudioStatusWithoutTrack(t *testing.T) {
	if !Ready() {
		t.Skip("mpv unavailable")
	}
	eng, err := NewEngine(Config{NullAO: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	st := eng.AudioStatus()
	if st.InputChannels != 0 || st.OutputChannels != 0 || st.SampleRate != 0 {
		t.Fatalf("AudioStatus() before any load = %+v, want zero format fields", st)
	}
}

func TestEngineRecordsPropertyErrors(t *testing.T) {
	if !Ready() {
		t.Skip("mpv unavailable")
	}
	eng, err := NewEngine(Config{NullAO: true, AudioChannels: "definitely-not-a-layout"})
	if err != nil {
		t.Fatalf("NewEngine should still return an engine so the error is visible: %v", err)
	}
	defer eng.Close()

	st := eng.AudioStatus()
	found := false
	for _, msg := range st.PropertyErrors {
		if strings.Contains(msg, "audio-channels") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("PropertyErrors = %v, want an audio-channels entry", st.PropertyErrors)
	}
	if err := eng.Play(filepath.Join(t.TempDir(), "missing.wav"), nil, 0); err == nil {
		t.Fatal("Play should fail when the requested channel layout cannot be applied")
	}
}

// writePCM16WAV writes a silent stereo 16-bit/44.1kHz WAV. The existing tiny
// 8-bit fixture is too short for MPV to report audio-out-params reliably.
func writePCM16WAV(t *testing.T, path string, seconds int) {
	t.Helper()
	const (
		rate      = 44100
		channels  = 2
		bytesEach = 2
	)
	dataSize := rate * channels * bytesEach * seconds
	buf := make([]byte, 44+dataSize)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataSize))
	copy(buf[8:], "WAVE")
	copy(buf[12:], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1)
	binary.LittleEndian.PutUint16(buf[22:], channels)
	binary.LittleEndian.PutUint32(buf[24:], rate)
	binary.LittleEndian.PutUint32(buf[28:], rate*channels*bytesEach)
	binary.LittleEndian.PutUint16(buf[32:], channels*bytesEach)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataSize))
	if err := os.WriteFile(path, buf, 0644); err != nil {
		t.Fatal(err)
	}
}
