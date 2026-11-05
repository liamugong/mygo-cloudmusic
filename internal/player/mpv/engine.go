package mpv

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AudioDevice describes an audio output device enumerated by MPV.
type AudioDevice struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AudioStatus reports the audio formats MPV is actually decoding and sending
// to the output device. Zero values mean the property was unavailable, which
// is normal before a track has loaded; callers must not guess from the stream
// label alone.
type AudioStatus struct {
	SourceCodec    string
	InputChannels  int
	InputLayout    string
	OutputChannels int
	OutputLayout   string
	SampleRate     int
	BitDepth       int
	OutputDevice   string
	Passthrough    bool
	PropertyErrors []string
}

var (
	devsMu       sync.Mutex
	cachedDevs   []AudioDevice
	devsCachedAt time.Time
)

// AudioDevices returns the cached or probed list of audio devices.
func AudioDevices() []AudioDevice {
	devsMu.Lock()
	defer devsMu.Unlock()
	if time.Since(devsCachedAt) < 5*time.Second && cachedDevs != nil {
		return cachedDevs
	}
	if !Ready() {
		return cachedDevs
	}
	inst, err := NewInstance(map[string]string{"ao": "null"})
	if err != nil {
		return cachedDevs
	}
	defer inst.Close()
	raw := inst.GetProperty("audio-device-list")
	if raw != "" {
		var list []AudioDevice
		if err := json.Unmarshal([]byte(raw), &list); err == nil {
			cachedDevs = list
			devsCachedAt = time.Now()
		}
	}
	return cachedDevs
}

// Config holds user choices for MPV audio playback.
type Config struct {
	// Exclusive enables bit-perfect exclusive mode (e.g. WASAPI exclusive on Windows).
	Exclusive bool
	// ReplayGain sets audio volume normalization ("no", "album", "track").
	ReplayGain string
	// AudioDevice specifies output audio device, or "" for default.
	AudioDevice string
	// AudioChannels controls the channel layout MPV sends to the device.
	// "auto-safe" is MPV's default; an explicit speaker whitelist such as
	// "fl-fr-fc-lfe-bl-br-sl-sr-tfl-tfr-tbl-tbr,5.1(side),stereo" is for
	// multichannel HDMI/USB outputs.
	AudioChannels string
	// AudioSpdif passes these codecs through undecoded, such as "eac3" to
	// an HDMI receiver for Dolby Atmos.
	AudioSpdif string
	// SpatialMode is "auto", "speakers", "headphones" or "stereo". Only
	// headphones adds a binaural/crossfeed filter; the others leave the
	// channel layout to AudioChannels.
	SpatialMode string
	// HrtfProfile selects the headphone crossfeed width: "balanced" or
	// "wide".
	HrtfProfile string
	// Gapless enables gapless playback for consecutive tracks.
	Gapless bool
	// Equalizer settings
	EqualizerEnabled bool
	EqualizerPreamp  float64
	EqualizerGains   [10]float64
	// NullAO uses a dummy null audio output (for testing/headless environments).
	NullAO bool
}

// Engine wraps an active MPV instance to drive audio playback.
type Engine struct {
	mu     sync.RWMutex
	inst   *Instance
	cfg    Config
	closed bool

	onEnd   func()
	onError func(string)

	playing        bool
	position       time.Duration
	duration       time.Duration
	volume         float64
	eqEnabled      bool
	eqPreamp       float64
	eqGains        [10]float64
	spatialEnabled bool

	// lastSource/lastStart/lastHeaders remember the current load so a
	// failed compressed-passthrough attempt can be retried through MPV's
	// normal decoder without asking the caller to re-open the track.
	lastSource     string
	lastStart      time.Duration
	lastHeaders    map[string]string
	spdifRetryUsed bool

	// propErrors holds important MPV property set/read failures, newest
	// value per property. AudioStatus exposes them so the UI can explain
	// why output could not be confirmed. filterNote records why the active
	// filter chain is empty (passthrough cannot run EQ/spatial filters), and
	// fallbackReason records a runtime output fallback.
	propErrors     []string
	filterNote     string
	fallbackReason string

	eventDone chan struct{}
}

// NewEngine creates and configures a high-quality audio playback engine.
func NewEngine(cfg Config) (*Engine, error) {
	opts := map[string]string{
		"stream-lavf-o":  "seekable=1,reconnect=1,reconnect_streamed=1,reconnect_delay_max=5",
		"force-seekable": "yes",
	}
	if cfg.NullAO {
		opts["ao"] = "null"
	}
	inst, err := NewInstance(opts)
	if err != nil {
		return nil, err
	}

	e := &Engine{
		inst:           inst,
		cfg:            cfg,
		volume:         0.8,
		eqEnabled:      cfg.EqualizerEnabled,
		eqPreamp:       cfg.EqualizerPreamp,
		eqGains:        cfg.EqualizerGains,
		spatialEnabled: true,
		eventDone:      make(chan struct{}),
	}

	// Apply audiophile options through the recording helper so a rejected
	// value becomes a PropertyError instead of silently leaving defaults.
	_ = e.applyPropertyLocked("audio-exclusive", yesNo(cfg.Exclusive))
	if cfg.ReplayGain != "" && cfg.ReplayGain != "no" {
		_ = e.applyPropertyLocked("replaygain", cfg.ReplayGain)
		_ = e.applyPropertyLocked("replaygain-clip", "yes")
	}
	if cfg.Gapless {
		_ = e.applyPropertyLocked("gapless-audio", "yes")
	}
	if cfg.AudioDevice != "" {
		_ = e.applyPropertyLocked("audio-device", cfg.AudioDevice)
	}
	// Empty channels must actively reset audio-channels. Leaving the
	// property alone would keep a layout set for the previous output/device
	// and send stereo into a surround device map, or vice versa.
	_ = e.applyPropertyLocked("audio-channels", audioChannelsValue(cfg.AudioChannels))
	_ = e.applyPropertyLocked("audio-spdif", cfg.AudioSpdif)
	// Always normalize when MPV folds the 7.1.4 Vivid PCM into stereo or
	// headphones. The decoded stream often peaks at 0 dBFS; without this
	// MPV's default downmix clips and produces audible crackle.
	_ = e.applyPropertyLocked("audio-normalize-downmix", "yes")

	// Apply initial equalizer filters if enabled
	e.applyAudioFiltersLocked()

	// Set initial volume
	_ = e.applyPropertyLocked("volume", fmt.Sprintf("%.1f", e.volume*100))

	go e.runEvents()

	return e, nil
}

// Play loads and starts playing a stream or local file.
func (e *Engine) Play(sourceURL string, headers map[string]string, startPos time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed || e.inst == nil {
		return fmt.Errorf("mpv: engine closed")
	}

	// Reapply output options on every load. SetAudioOptions only runs when
	// the user changes settings, but MPV may reset audio properties after a
	// device reconfiguration or a failed passthrough retry. A failure to set
	// the channel layout is fatal for this load: playing with the wrong
	// layout while reporting success is worse than letting the caller pick
	// a fallback engine.
	if err := e.applyPropertyLocked("audio-channels", audioChannelsValue(e.cfg.AudioChannels)); err != nil {
		return fmt.Errorf("mpv: 无法应用声道布局：%w", err)
	}
	_ = e.applyPropertyLocked("audio-spdif", e.cfg.AudioSpdif)
	_ = e.applyPropertyLocked("audio-exclusive", yesNo(e.cfg.Exclusive))
	e.applyAudioFiltersLocked()
	e.fallbackReason = ""

	// Set HTTP request headers if any
	if len(headers) > 0 {
		var hList []string
		for k, v := range headers {
			hList = append(hList, fmt.Sprintf("%s: %s", k, v))
		}
		if err := e.applyPropertyLocked("http-header-fields", strings.Join(hList, ", ")); err != nil {
			return fmt.Errorf("mpv: 无法应用请求头：%w", err)
		}
	} else {
		// A previous track may have set authenticated headers; do not carry
		// them into a public URL on the same engine.
		_ = e.applyPropertyLocked("http-header-fields", "")
	}
	e.lastSource = sourceURL
	e.lastStart = startPos
	e.lastHeaders = cloneHeaders(headers)
	e.spdifRetryUsed = false

	// Load file
	code := mpvCode(e.inst.Command([]string{"loadfile", sourceURL, "replace"}))
	if code != 0 {
		return fmt.Errorf("mpv: loadfile failed with error %d", code)
	}

	// If start position is specified, seek right away
	if startPos > 0 {
		e.inst.Command([]string{"seek", fmt.Sprintf("%.3f", startPos.Seconds()), "absolute"})
	}

	e.inst.SetProperty("pause", "no")
	e.playing = true
	return nil
}

// Pause pauses playback.
func (e *Engine) Pause() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inst != nil {
		e.inst.SetProperty("pause", "yes")
		e.playing = false
	}
}

// Resume unpauses playback.
func (e *Engine) Resume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inst != nil {
		e.inst.SetProperty("pause", "no")
		e.playing = true
	}
}

// Playing reports whether the engine is currently playing.
func (e *Engine) Playing() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.playing
}

// Seek seeks to a target position.
func (e *Engine) Seek(pos time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inst != nil {
		e.inst.Command([]string{"seek", fmt.Sprintf("%.3f", pos.Seconds()), "absolute"})
		e.position = pos
	}
}

// Position returns current playback position.
func (e *Engine) Position() time.Duration {
	e.mu.RLock()
	inst := e.inst
	cached := e.position
	e.mu.RUnlock()

	if inst == nil {
		return cached
	}
	s := inst.GetProperty("time-pos")
	if s == "" {
		return cached
	}
	pos := parseSecs(s)
	if pos > 0 {
		e.mu.Lock()
		e.position = pos
		e.mu.Unlock()
	}
	return pos
}

// Duration returns total length of current track.
func (e *Engine) Duration() time.Duration {
	e.mu.RLock()
	inst := e.inst
	cached := e.duration
	e.mu.RUnlock()

	if inst == nil {
		return cached
	}
	s := inst.GetProperty("duration")
	if s == "" {
		return cached
	}
	d := parseSecs(s)
	if d > 0 {
		e.mu.Lock()
		e.duration = d
		e.mu.Unlock()
	}
	return d
}

// Volume returns the current volume between 0 and 1.
func (e *Engine) Volume() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.volume
}

// SetVolume sets the volume between 0 and 1.
func (e *Engine) SetVolume(vol float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.volume = vol
	if e.inst != nil {
		e.inst.SetProperty("volume", fmt.Sprintf("%.1f", vol*100))
	}
}

// SetReplayGainPreamp sets replaygain pre-amplification in dB.
// If db != 0, enables replaygain normalization with the given gain in dB.
// If db == 0, falls back to neutral or cfg.ReplayGain.
func (e *Engine) SetReplayGainPreamp(db float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inst != nil {
		if db != 0 {
			rgMode := e.cfg.ReplayGain
			if rgMode == "" || rgMode == "no" {
				rgMode = "track"
			}
			e.inst.SetProperty("replaygain", rgMode)
			e.inst.SetProperty("replaygain-preamp", fmt.Sprintf("%.2f", db))
			e.inst.SetProperty("replaygain-fallback", fmt.Sprintf("%.2f", db))
			e.inst.SetProperty("replaygain-clip", "yes")
		} else {
			if e.cfg.ReplayGain != "" && e.cfg.ReplayGain != "no" {
				e.inst.SetProperty("replaygain", e.cfg.ReplayGain)
				e.inst.SetProperty("replaygain-preamp", "0")
				e.inst.SetProperty("replaygain-fallback", "0")
			} else {
				e.inst.SetProperty("replaygain", "no")
				e.inst.SetProperty("replaygain-preamp", "0")
				e.inst.SetProperty("replaygain-fallback", "0")
			}
		}
	}
}

// SetEqualizer configures and applies the 10-band graphic equalizer filter.
func (e *Engine) SetEqualizer(preamp float64, gains [10]float64, enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.eqPreamp = preamp
	e.eqGains = gains
	e.eqEnabled = enabled
	e.applyAudioFiltersLocked()
}

func (e *Engine) applyAudioFiltersLocked() {
	if e.inst == nil {
		return
	}
	eqFilter := ""
	if e.eqEnabled {
		eqFilter = BuildEqualizerFilter(e.eqPreamp, e.eqGains)
	}
	spatialFilter := ""
	if e.spatialEnabled {
		spatialFilter = BuildSpatialFilter(e.cfg.SpatialMode, e.cfg.HrtfProfile)
	}
	decision := resolvePipeline(e.cfg.AudioSpdif != "", eqFilter, spatialFilter)
	e.filterNote = decision.Reason
	_ = e.applyPropertyLocked("af", decision.Filters)
}

// OnEnd sets the callback to invoke when a track reaches its natural end.
func (e *Engine) OnEnd(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onEnd = fn
}

// OnError sets the callback to invoke when an audio loading or decoding failure occurs.
func (e *Engine) OnError(fn func(string)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onError = fn
}

func (e *Engine) runEvents() {
	defer close(e.eventDone)
	for {
		e.mu.RLock()
		inst := e.inst
		closed := e.closed
		e.mu.RUnlock()

		if closed || inst == nil {
			return
		}

		evID, _, endReason := inst.WaitEvent(0.2)
		switch evID {
		case EventShutdown:
			return
		case EventEndFile:
			if endReason == EndFileReasonError {
				e.mu.Lock()
				// A device can reject compressed passthrough or exclusive
				// CoreAudio after loadfile has already returned success.
				// Retry once through MPV's normal PCM decoder (and without
				// hog mode) instead of leaving the UI in a fake "playing"
				// state. This is especially important for E-AC-3 on built-in
				// speakers, where exclusive CoreAudio can fail before the
				// E-AC-3 core is decoded. Work on a copy of cfg: the retry
				// must not permanently forget the user's passthrough or
				// exclusive preference for the next track.
				inst := e.inst
				retryCfg := e.cfg
				retry := (retryCfg.AudioSpdif != "" || retryCfg.Exclusive) && !e.spdifRetryUsed && e.lastSource != "" && inst != nil && !e.closed
				var source string
				var start time.Duration
				var headers map[string]string
				if retry {
					e.spdifRetryUsed = true
					_ = e.applyPropertyLocked("audio-spdif", "")
					_ = e.applyPropertyLocked("audio-exclusive", "no")
					source, start, headers = e.lastSource, e.lastStart, cloneHeaders(e.lastHeaders)
					if len(headers) > 0 {
						var hList []string
						for k, v := range headers {
							hList = append(hList, fmt.Sprintf("%s: %s", k, v))
						}
						_ = e.applyPropertyLocked("http-header-fields", strings.Join(hList, ", "))
					}
				}
				e.mu.Unlock()
				if retry {
					fmt.Printf("[mpv] audio output failed; retrying with PCM decode\n")
					if code := mpvCode(inst.Command([]string{"loadfile", source, "replace"})); code == 0 {
						if start > 0 {
							inst.Command([]string{"seek", fmt.Sprintf("%.3f", start.Seconds()), "absolute"})
						}
						e.mu.Lock()
						e.playing = true
						e.fallbackReason = "音频输出失败，已回退 PCM"
						e.mu.Unlock()
						continue
					}
				}
				e.mu.Lock()
				e.playing = false
				onError := e.onError
				e.mu.Unlock()
				if onError != nil {
					onError("音频加载或解码失败")
				}
				continue
			}
			if endReason == EndFileReasonEOF {
				e.mu.RLock()
				onEnd := e.onEnd
				e.mu.RUnlock()
				if onEnd != nil {
					onEnd()
				}
			}
		case EventFileLoaded, EventPlaybackRestart:
			e.mu.Lock()
			e.playing = true
			e.mu.Unlock()
		}
	}
}

// Append adds a track to the end of MPV's internal playlist for gapless prefetching.
func (e *Engine) Append(sourceURL string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.inst == nil {
		return fmt.Errorf("mpv: engine closed")
	}
	code := mpvCode(e.inst.Command([]string{"loadfile", sourceURL, "append"}))
	if code != 0 {
		return fmt.Errorf("mpv: loadfile append failed with code %d", code)
	}
	return nil
}

// Stop stops playback and clears playlist without destroying the engine.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inst != nil && !e.closed {
		e.playing = false
		e.inst.Command([]string{"stop"})
	}
}

// Closed reports whether the engine has been shut down.
func (e *Engine) Closed() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.closed || e.inst == nil
}

// SetOption sets an MPV option.
func (e *Engine) SetOption(name, value string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inst != nil {
		return e.inst.SetOption(name, value)
	}
	return -1
}

// RequestLogMessages requests MPV log messages as events.
func (e *Engine) RequestLogMessages(minLevel string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inst != nil {
		e.inst.RequestLogMessages(minLevel)
	}
}

// AudioDevices returns the list of audio output devices reported by MPV.
func (e *Engine) AudioDevices() []AudioDevice {
	e.mu.RLock()
	inst := e.inst
	e.mu.RUnlock()
	if inst == nil {
		return AudioDevices()
	}
	devsMu.Lock()
	defer devsMu.Unlock()
	if time.Since(devsCachedAt) < 3*time.Second && cachedDevs != nil {
		return cachedDevs
	}
	raw := inst.GetProperty("audio-device-list")
	if raw != "" {
		var list []AudioDevice
		if err := json.Unmarshal([]byte(raw), &list); err == nil {
			cachedDevs = list
			devsCachedAt = time.Now()
			return list
		}
	}
	return cachedDevs
}

// SetAudioDevice dynamically sets the audio output device (e.g. "auto", or device identifier).
func (e *Engine) SetAudioDevice(dev string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.AudioDevice = dev
	if e.inst != nil {
		if dev == "" {
			dev = "auto"
		}
		_ = e.applyPropertyLocked("audio-device", dev)
	}
}

// SetSpatialOptions updates the spatial filter while the engine is playing.
// enabled is false for stereo sources and for jyeffect, which is already
// spatialised and must not be processed twice.
func (e *Engine) SetSpatialOptions(mode, profile string, enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.SpatialMode, e.cfg.HrtfProfile, e.spatialEnabled = mode, profile, enabled
	e.applyAudioFiltersLocked()
}

// SetAudioOptions updates the channel layout and SP-DIF passthrough while
// the engine is playing. Empty channels explicitly resets MPV to "auto-safe"
// instead of leaving a layout configured for a previous output device.
func (e *Engine) SetAudioOptions(channels, spdif string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.AudioChannels, e.cfg.AudioSpdif = channels, spdif
	if e.inst == nil {
		return
	}
	_ = e.applyPropertyLocked("audio-channels", audioChannelsValue(channels))
	_ = e.applyPropertyLocked("audio-spdif", spdif)
	e.applyAudioFiltersLocked()
}

// SetExclusive enables or disables CoreAudio hog mode for the next load.
// Dolby/E-AC-3 needs to be decoded to PCM on normal CoreAudio outputs; hog
// mode can fail before the decoder gets a usable substream. Callers therefore
// disable it for E-AC-3 while keeping the user's preference for PCM tracks.
func (e *Engine) SetExclusive(enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg.Exclusive = enabled
	if e.inst == nil {
		return
	}
	_ = e.applyPropertyLocked("audio-exclusive", yesNo(enabled))
}

// GetProperty returns the string value of an MPV property.
func (e *Engine) GetProperty(prop string) string {
	e.mu.RLock()
	inst := e.inst
	e.mu.RUnlock()
	if inst == nil {
		return ""
	}
	return inst.GetProperty(prop)
}

// SetLoopFile makes MPV repeat the current file forever when enabled and
// clears the repeat otherwise. It returns the MPV error so a failed repeat
// switch can be reported instead of silently ignored.
func (e *Engine) SetLoopFile(enabled bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.inst == nil {
		return fmt.Errorf("mpv: engine closed")
	}
	return e.applyPropertyLocked("loop-file", loopFileValue(enabled))
}

func loopFileValue(enabled bool) string {
	if enabled {
		return "inf"
	}
	return "no"
}

// AudioStatus reads the actual decoder and output formats from MPV. Every
// property is optional: before a file loads, or when a property is missing,
// the corresponding field is zero rather than guessed. Important set/read
// failures are collected in PropertyErrors.
func (e *Engine) AudioStatus() AudioStatus {
	e.mu.RLock()
	inst := e.inst
	closed := e.closed
	spdif := e.cfg.AudioSpdif
	propErrs := append([]string(nil), e.propErrors...)
	e.mu.RUnlock()

	st := AudioStatus{PropertyErrors: propErrs}
	if inst == nil || closed {
		return st
	}

	// MPV's audio-params and audio-out-params are node properties, which
	// mpv_get_property_string cannot serialize. Read their scalar sub-paths
	// instead; each is a plain string property.
	in, inErrs := parseAudioParams(map[string]string{
		"format":        inst.GetProperty("audio-params/format"),
		"channel-count": inst.GetProperty("audio-params/channel-count"),
		"channels":      inst.GetProperty("audio-params/channels"),
		"samplerate":    inst.GetProperty("audio-params/samplerate"),
	})
	out, outErrs := parseAudioParams(map[string]string{
		"format":        inst.GetProperty("audio-out-params/format"),
		"channel-count": inst.GetProperty("audio-out-params/channel-count"),
		"channels":      inst.GetProperty("audio-out-params/channels"),
		"samplerate":    inst.GetProperty("audio-out-params/samplerate"),
	})
	st.PropertyErrors = appendUnique(st.PropertyErrors, inErrs...)
	st.PropertyErrors = appendUnique(st.PropertyErrors, outErrs...)
	// A format string without a channel count means MPV answered the node
	// partially; that is the read failure that matters for output
	// confirmation. All-empty properties simply mean no track is loaded.
	if out.Format != "" && out.Channels == 0 {
		st.PropertyErrors = appendUnique(st.PropertyErrors, "audio-out-params/channel-count: 读取为空")
	}
	if in.Format != "" && in.Channels == 0 {
		st.PropertyErrors = appendUnique(st.PropertyErrors, "audio-params/channel-count: 读取为空")
	}

	// audio-codec is a node (human readable description) and cannot be read
	// with the string API; audio-codec-name is the stable identifier.
	st.SourceCodec = firstNonEmpty(
		inst.GetProperty("audio-codec-name"),
		inst.GetProperty("current-tracks/audio/codec"),
		inst.GetProperty("audio-codec"),
	)
	st.InputChannels = in.Channels
	if n := positiveInt(inst.GetProperty("current-tracks/audio/demux-channel-count")); n > 0 {
		// With compressed passthrough audio-params reports the decoder's
		// stereo carrier, not the real source layout; current-tracks keeps
		// the demuxed channel count.
		st.InputChannels = n
	}
	st.InputLayout = in.Layout
	if st.InputLayout == "" {
		st.InputLayout = layoutFromChannels(st.InputChannels)
	}
	st.OutputChannels = out.Channels
	st.OutputLayout = out.Layout
	if st.OutputLayout == "" {
		st.OutputLayout = layoutFromChannels(st.OutputChannels)
	}
	st.SampleRate = firstPositive(out.SampleRate, in.SampleRate)
	st.BitDepth = out.BitDepth
	if st.BitDepth == 0 {
		st.BitDepth = in.BitDepth
	}
	st.OutputDevice = strings.TrimSpace(inst.GetProperty("audio-device"))
	st.Passthrough = isPassthroughFormat(out.Format) && spdif != ""
	if st.InputChannels > 2 && (st.InputLayout == "" || st.InputLayout == "stereo") {
		// A downmix can make audio-params report the output carrier layout
		// while current-tracks still knows the real source channel count.
		st.InputLayout = layoutFromChannels(st.InputChannels)
	}
	return st
}

// FilterNote explains why EQ/spatial filters are currently inactive. It is
// empty when the normal filter chain is active.
func (e *Engine) FilterNote() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.filterNote
}

// FallbackReason reports a runtime output fallback, such as PCM after MPV
// rejected compressed passthrough or exclusive mode.
func (e *Engine) FallbackReason() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.fallbackReason
}

// applyPropertyLocked sets an MPV property and records important failures.
// Callers must hold e.mu (NewEngine may call it before the engine is shared).
func (e *Engine) applyPropertyLocked(name, value string) error {
	if e.inst == nil {
		return fmt.Errorf("mpv: engine closed")
	}
	if code := mpvCode(e.inst.SetProperty(name, value)); code != 0 {
		err := fmt.Errorf("%s: 设置 %q 失败（MPV error %d）", name, value, code)
		e.recordPropertyErrorLocked(name, err.Error())
		return err
	}
	e.clearPropertyErrorLocked(name)
	return nil
}

func (e *Engine) recordPropertyErrorLocked(name, msg string) {
	prefix := name + ":"
	for i, existing := range e.propErrors {
		if strings.HasPrefix(existing, prefix) {
			e.propErrors[i] = msg
			return
		}
	}
	e.propErrors = append(e.propErrors, msg)
}

func (e *Engine) clearPropertyErrorLocked(name string) {
	prefix := name + ":"
	kept := e.propErrors[:0]
	for _, existing := range e.propErrors {
		if !strings.HasPrefix(existing, prefix) {
			kept = append(kept, existing)
		}
	}
	e.propErrors = kept
}

// audioParamsInfo is one decoded view of MPV's audio-params or
// audio-out-params node, flattened into scalar properties.
type audioParamsInfo struct {
	Format     string
	Channels   int
	Layout     string
	SampleRate int
	BitDepth   int
}

// parseAudioParams converts MPV's scalar property strings into a typed view.
// Bad numbers are reported but never fatal: a partially readable status is
// still more useful than none.
func parseAudioParams(props map[string]string) (audioParamsInfo, []string) {
	var info audioParamsInfo
	var errs []string
	info.Format = strings.ToLower(strings.TrimSpace(props["format"]))
	info.Layout = strings.TrimSpace(props["channels"])
	if raw := strings.TrimSpace(props["channel-count"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			info.Channels = n
		} else {
			errs = append(errs, fmt.Sprintf("channel-count: 无法解析 %q", raw))
		}
	}
	if info.Channels == 0 {
		info.Channels = channelsFromLayout(info.Layout)
	}
	if raw := strings.TrimSpace(props["samplerate"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			info.SampleRate = n
		} else {
			errs = append(errs, fmt.Sprintf("samplerate: 无法解析 %q", raw))
		}
	}
	info.BitDepth = bitDepthFromFormat(info.Format)
	return info, errs
}

func bitDepthFromFormat(format string) int {
	f := strings.ToLower(strings.TrimSpace(format))
	if strings.HasPrefix(f, "spdif") {
		return 0
	}
	f = strings.TrimSuffix(f, "p")
	switch f {
	case "u8", "s8":
		return 8
	case "u16", "s16":
		return 16
	case "u24", "s24":
		return 24
	case "u32", "s32":
		return 32
	case "float", "float32":
		return 32
	case "double", "float64", "s64", "u64":
		return 64
	}
	return 0
}

func channelsFromLayout(layout string) int {
	l := strings.ToLower(strings.TrimSpace(layout))
	if l == "" {
		return 0
	}
	switch l {
	case "mono":
		return 1
	case "stereo":
		return 2
	case "2.1", "3.0":
		return 3
	case "4.0", "quad":
		return 4
	case "5.0":
		return 5
	case "5.1", "5.1(side)":
		return 6
	case "6.1":
		return 7
	case "7.1":
		return 8
	case "7.1.4":
		return 12
	}
	// MPV can also report a dash separated speaker list such as
	// "fl-fr-fc-lfe-bl-br". Count non-empty tokens.
	n := 0
	for _, part := range strings.Split(l, "-") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

func layoutFromChannels(n int) string {
	switch n {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	case 3:
		return "2.1"
	case 4:
		return "4.0"
	case 5:
		return "5.0"
	case 6:
		return "5.1"
	case 7:
		return "6.1"
	case 8:
		return "7.1"
	case 12:
		return "7.1.4"
	}
	return ""
}

func isPassthroughFormat(format string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(format)), "spdif")
}

// audioPipeline is the filter decision for one load. Passthrough and the
// decoded filter chain are mutually exclusive: a compressed bitstream cannot
// be equalized or spatialised without decoding it first.
type audioPipeline struct {
	Filters       string
	Passthrough   bool
	IgnoredFilter bool
	Reason        string
}

// resolvePipeline decides the af value from the user's options. When
// passthrough is active filters are cleared deliberately and the ignored
// reason is recorded so the UI can explain why EQ/space controls do nothing.
func resolvePipeline(passthrough bool, equalizerFilter, spatialFilter string) audioPipeline {
	if passthrough {
		decision := audioPipeline{Passthrough: true}
		if equalizerFilter != "" || spatialFilter != "" {
			decision.IgnoredFilter = true
			decision.Reason = "直通模式已忽略 EQ/空间化"
		}
		return decision
	}
	filters := make([]string, 0, 2)
	if equalizerFilter != "" {
		filters = append(filters, equalizerFilter)
	}
	if spatialFilter != "" {
		filters = append(filters, spatialFilter)
	}
	return audioPipeline{Filters: strings.Join(filters, ",")}
}

func audioChannelsValue(channels string) string {
	if strings.TrimSpace(channels) == "" {
		return "auto-safe"
	}
	return channels
}

// mpvCode recovers the signed MPV error code from a raw purego return.
// purego widens a C int return into a positive Go int on some platforms, so
// -9 arrives as 4294967287; int32 restores it. Non-zero means failure for
// every MPV call used here (success is 0).
func mpvCode(code int) int32 {
	return int32(code)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func appendUnique(dst []string, values ...string) []string {
	for _, v := range values {
		if v == "" {
			continue
		}
		found := false
		for _, existing := range dst {
			if existing == v {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, v)
		}
	}
	return dst
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func positiveInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Close terminates the engine.
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	inst := e.inst
	e.inst = nil
	e.mu.Unlock()

	// Let the event goroutine leave mpv_wait_event before the native handle
	// is destroyed; destroying it underneath a waiting call can crash.
	select {
	case <-e.eventDone:
	case <-time.After(time.Second):
	}
	if inst != nil {
		inst.Close()
	}
}

func cloneHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
