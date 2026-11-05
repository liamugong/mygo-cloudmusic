package player

import (
	"fmt"
	"strings"

	"netease-music-mygo/internal/hrtf"
	"netease-music-mygo/internal/netease"
	"netease-music-mygo/internal/player/av3a"
)

// EngineKind is an output engine a track can play through.
type EngineKind int

const (
	EngineBuiltin EngineKind = iota
	EngineMPV
	EngineDLNA
)

// String names the engine as PlaybackStatus reports it.
func (k EngineKind) String() string {
	switch k {
	case EngineMPV:
		return "mpv"
	case EngineDLNA:
		return "dlna"
	default:
		return "builtin"
	}
}

// PlaybackRequest is what the listener asks for, before output limits are
// applied: the level picked in the settings, the spatial mode, and which
// engine will play it.
type PlaybackRequest struct {
	RequestedLevel string
	SpatialMode    string
	EqualizerOn    bool
	SpdifPreferred bool
	Engine         EngineKind
	AV3AReady      bool
	HRTFAvailable  bool
}

// PlaybackPlan is how a track will actually play: the level after output
// limits lower it, the target layout, whether passthrough or spatial
// processing is used, and why it differs from what was asked for.
type PlaybackPlan struct {
	EffectiveLevel string
	Engine         EngineKind
	TargetLayout   string
	Passthrough    string // "" means PCM
	Spatial        bool
	Exclusive      bool
	HRTF           bool
	FallbackReason string
}

// PlanPlayback decides how a track plays. It is pure: the same request
// always gives the same plan, away from the audio device and the network.
func PlanPlayback(req PlaybackRequest) PlaybackPlan {
	plan := PlaybackPlan{
		Engine: req.Engine,
	}
	level := req.RequestedLevel
	var reasons []string

	// A real HRTF renderer runs in the built-in path: it can consume a
	// multichannel FLAC and output binaural stereo itself. Dolby and Vivid
	// are not decodable there, so they use the 5.1 FLAC master instead.
	if req.HRTFAvailable && req.Engine != EngineDLNA && req.SpatialMode == "headphones" && netease.Spatial(level) && level != "jyeffect" {
		switch level {
		case "vivid":
			reasons = append(reasons, "Vivid 需要 AV3A 解码，HRTF 模式改用 sky 5.1 FLAC")
			level = "sky"
		case "dolby":
			reasons = append(reasons, "杜比 E-AC-3 无法在 Go HRTF 路径解码，改用 sky 5.1 FLAC")
			level = "sky"
		}
		return PlaybackPlan{
			EffectiveLevel: level,
			Engine:         EngineBuiltin,
			TargetLayout:   hrtfLayout(level),
			Spatial:        true,
			HRTF:           true,
			FallbackReason: strings.Join(reasons, "；"),
		}
	}

	// Audio Vivid needs the arm64 helper, whatever the output.
	if level == "vivid" && !req.AV3AReady {
		level = "sky"
		reasons = append(reasons, "Vivid 需要 macOS arm64 helper，已降为 sky")
	}

	switch req.Engine {
	case EngineDLNA:
		// A DLNA receiver is only known to render jyeffect's stereo
		// master; do not ask it to decode a surround stream.
		if netease.Spatial(level) && level != "jyeffect" {
			reasons = append(reasons, fmt.Sprintf("DLNA 不支持 %s，已降为 jyeffect", layoutName(level)))
			level = "jyeffect"
		}
	case EngineBuiltin:
		// The built-in decoder plays mono and stereo; surround levels
		// come as multichannel FLAC and would lose every channel after
		// the first two. Use the stereo spatial master instead.
		switch level {
		case "vivid", "dolby", "sky":
			reasons = append(reasons, fmt.Sprintf("内置播放器不支持 %s，已降为 jyeffect", layoutName(level)))
			level = "jyeffect"
		}
	}

	plan.EffectiveLevel = level
	plan.TargetLayout = targetLayout(req, level)
	plan.Spatial = req.SpatialMode == "headphones" && netease.Spatial(level) && level != "jyeffect"

	// Compressed passthrough skips MPV's filter chain, so EQ and headphone
	// spatialisation cannot coexist with it. The listener's processing
	// choice wins; passthrough is only used when nothing needs filters.
	if req.SpdifPreferred && req.Engine == EngineMPV {
		if req.EqualizerOn || plan.Spatial || req.SpatialMode == "stereo" {
			reasons = append(reasons, "为应用 EQ/下混/空间化已切换 PCM")
		} else {
			plan.Passthrough = "eac3"
		}
	}

	// Hog mode is for ordinary stereo PCM only: spatial streams need the
	// CoreAudio mixer so the system volume keeps working, and DLNA has no
	// local device to take exclusive control of.
	plan.Exclusive = req.Engine == EngineMPV && !netease.Spatial(level)
	plan.FallbackReason = strings.Join(reasons, "；")
	return plan
}

// targetLayout is the channel layout the output is asked to render. It is
// only a request: PlaybackStatus reports what the device actually accepted.
func targetLayout(req PlaybackRequest, level string) string {
	if req.Engine != EngineMPV || req.SpatialMode == "headphones" || req.SpatialMode == "stereo" {
		return "stereo"
	}
	switch level {
	case "vivid":
		return "7.1.4"
	case "dolby", "sky":
		return "5.1"
	}
	return "stereo"
}

// hrtfLayout names the multichannel FLAC layout used by the HRTF renderer.
func hrtfLayout(level string) string {
	if level == "sky" {
		return "5.1(side)"
	}
	return "stereo"
}

func layoutName(level string) string {
	switch level {
	case "vivid":
		return "Audio Vivid 7.1.4"
	case "dolby":
		return "杜比全景声"
	case "sky":
		return "5.1 环绕声"
	}
	return level
}

// playbackRequest builds the request for the current player settings.
func (p *Player) playbackRequest(level string) PlaybackRequest {
	engine := EngineBuiltin
	switch {
	case p.IsCasting():
		engine = EngineDLNA
	case p.UsingMPV():
		engine = EngineMPV
	}
	cfg := p.mpvConfig()
	return PlaybackRequest{
		RequestedLevel: level,
		SpatialMode:    cfg.SpatialMode,
		EqualizerOn:    cfg.EqualizerEnabled,
		SpdifPreferred: cfg.AudioSpdif != "",
		Engine:         engine,
		AV3AReady:      av3a.Ready(),
		HRTFAvailable:  hrtf.Available(),
	}
}

// planFor computes the plan for level under the current settings.
func (p *Player) planFor(level string) PlaybackPlan {
	return PlanPlayback(p.playbackRequest(level))
}

// planForEngine computes the plan as if engine were the output.
func (p *Player) planForEngine(level string, engine EngineKind) PlaybackPlan {
	req := p.playbackRequest(level)
	req.Engine = engine
	return PlanPlayback(req)
}

// requestedLevel is the level picked in the settings, before output limits.
func (p *Player) requestedLevel() string {
	if p.Quality != nil {
		if level := p.Quality(); level != "" {
			return level
		}
	}
	return "exhigh"
}
