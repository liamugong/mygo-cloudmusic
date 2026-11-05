package player

import "strings"

// PlaybackStatus is what the listener can know about how the current track
// actually plays: the level asked for and used, the engine, and whatever
// the output device reports. An output channel count of zero means the
// output did not report one, so the UI must show "输出未确认" rather than
// guess.
type PlaybackStatus struct {
	RequestedLevel string
	EffectiveLevel string
	Engine         string // "builtin", "mpv", "dlna"
	SourceCodec    string
	InputChannels  int
	InputLayout    string
	OutputChannels int
	OutputLayout   string
	SampleRate     int
	BitDepth       int
	OutputDevice   string
	Passthrough    bool
	Hrtf           bool
	FallbackReason string
	ProcessingNote string
	PropertyErrors []string

	// ServedLevel is the level the server put in the stream response, and
	// LevelReported says whether the server actually reported it. The
	// eapi sometimes omits the level and the client fills in the requested
	// one; that fill-in is not proof of what was served.
	ServedLevel   string
	LevelReported bool
}

// Status reports how the current track actually plays. It is safe to call
// with no track loaded: it then reports the configured output engine and
// zero values for everything the output has not confirmed.
func (p *Player) Status() PlaybackStatus {
	if p == nil {
		return PlaybackStatus{Engine: EngineBuiltin.String()}
	}
	requested := p.requestedLevel()
	plan := p.plan
	if plan.EffectiveLevel == "" {
		plan = p.planFor(requested)
	}

	st := PlaybackStatus{
		RequestedLevel: requested,
		EffectiveLevel: plan.EffectiveLevel,
		FallbackReason: plan.FallbackReason,
	}

	// Start from what the plan expects, then trust the output that is
	// actually alive: after an MPV failure the player falls back to the
	// built-in decoder while the setting still prefers MPV.
	engine := plan.Engine
	switch {
	case p.IsCasting():
		engine = EngineDLNA
	case p.mpvOut != nil && !p.mpvOut.Closed():
		engine = EngineMPV
	case p.out != nil || p.stream != nil:
		engine = EngineBuiltin
	}
	st.Engine = engine.String()
	st.Hrtf = plan.HRTF && engine == EngineBuiltin
	if st.Hrtf {
		st.Engine = "hrtf"
	}

	if p.info != nil {
		st.SourceCodec = p.info.Type
		st.ServedLevel = p.info.ServedLevel
		st.LevelReported = p.info.LevelReported
		st.InputLayout = p.info.ChannelLayout
		if st.InputChannels == 0 {
			st.InputChannels = channelsFromLayout(p.info.ChannelLayout)
		}
		if st.SampleRate == 0 {
			st.SampleRate = p.info.SR
		}
	}

	switch engine {
	case EngineMPV:
		if p.mpvOut != nil && !p.mpvOut.Closed() {
			s := p.mpvOut.AudioStatus()
			if s.SourceCodec != "" {
				st.SourceCodec = s.SourceCodec
			}
			if s.InputChannels != 0 {
				st.InputChannels = s.InputChannels
			}
			if s.InputLayout != "" {
				st.InputLayout = s.InputLayout
			}
			st.OutputChannels = s.OutputChannels
			st.OutputLayout = s.OutputLayout
			if s.SampleRate != 0 {
				st.SampleRate = s.SampleRate
			}
			st.BitDepth = s.BitDepth
			st.OutputDevice = s.OutputDevice
			st.Passthrough = s.Passthrough
			st.PropertyErrors = append([]string(nil), s.PropertyErrors...)
			st.FallbackReason = joinReasons(st.FallbackReason, p.mpvOut.FallbackReason())
			st.ProcessingNote = p.mpvOut.FilterNote()
		}
	case EngineDLNA:
		// A renderer does not report its output format back to us.
		st.OutputChannels = 0
		st.OutputLayout = ""
		st.FallbackReason = joinReasons(st.FallbackReason, "DLNA 输出声道未确认")
	default:
		// The built-in decoder mixes to stereo. In HRTF mode it is a
		// binaural pair, not a plain stereo fold-down.
		st.OutputChannels = 2
		st.OutputLayout = "stereo"
		if st.Hrtf {
			st.ProcessingNote = joinReasons(st.ProcessingNote, "HRTF 双耳渲染（CIPIC 360° HRIR）")
		}
		if p.stream != nil {
			st.InputChannels = p.stream.Channels()
			if st.SampleRate == 0 {
				st.SampleRate = p.stream.SampleRate()
			}
			if st.InputLayout == "" {
				switch p.stream.Channels() {
				case 1:
					st.InputLayout = "mono"
				case 2:
					st.InputLayout = "stereo"
				}
			}
		}
	}
	return st
}

// joinReasons joins fallback notes without losing either.
func joinReasons(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "；" + b
	}
}

// channelsFromLayout turns a NetEase channel layout name into its channel
// count, or zero when the name is not one it knows.
func channelsFromLayout(layout string) int {
	layout = strings.ToLower(strings.TrimSpace(layout))
	switch {
	case strings.HasPrefix(layout, "7.1.4"):
		return 12
	case strings.HasPrefix(layout, "7.1"):
		return 8
	case strings.HasPrefix(layout, "5.1"):
		return 6
	case layout == "stereo":
		return 2
	case layout == "mono":
		return 1
	}
	return 0
}
