package vad

import (
	"context"
	"fmt"
)

type AudioGateConfig struct {
	PreRollMs          int
	AudioBufferSeconds int
	MaxSpeechMs        int
}

type AudioGate struct {
	inferer            Inferer
	cfg                StreamConfig
	windowSize         int
	minSilenceSamples  int
	minSpeechSamples   int
	preRollMs          int
	audioBufferSeconds int
	maxSpeechSamples   int

	sampleRate   int
	audio        sampleRing
	vadRead      int64
	sendCursor   int64
	active       bool
	tempStart    int64
	tempEnd      int64
	speechStart  int64
	preRollFloor int64
}

func NewAudioGate(inferer Inferer, streamCfg StreamConfig, cfg AudioGateConfig) (*AudioGate, error) {
	if inferer == nil {
		return nil, fmt.Errorf("vad: inferer is required")
	}
	streamCfg = normalizeStreamConfig(streamCfg)
	if err := validateStreamConfig(streamCfg); err != nil {
		return nil, err
	}
	if cfg.PreRollMs == 0 {
		cfg.PreRollMs = DefaultPreRollMs
	}
	if cfg.PreRollMs < 0 {
		return nil, fmt.Errorf("vad: pre-roll must be non-negative")
	}
	if cfg.AudioBufferSeconds == 0 {
		cfg.AudioBufferSeconds = DefaultAudioBufferSeconds
	}
	if cfg.AudioBufferSeconds <= 0 {
		return nil, fmt.Errorf("vad: audio buffer seconds must be positive")
	}
	if cfg.AudioBufferSeconds*1000 < cfg.PreRollMs {
		return nil, fmt.Errorf("vad: audio buffer must be at least as large as pre-roll")
	}
	if cfg.MaxSpeechMs == 0 {
		cfg.MaxSpeechMs = DefaultMaxSpeechMs
	}
	if cfg.MaxSpeechMs < 0 {
		return nil, fmt.Errorf("vad: max speech duration must be non-negative")
	}
	windowSize := 512
	if streamCfg.SampleRate == 8000 {
		windowSize = 256
	}
	maxSpeechSamples := cfg.MaxSpeechMs * streamCfg.SampleRate / 1000
	return &AudioGate{
		inferer:            inferer,
		cfg:                streamCfg,
		windowSize:         windowSize,
		minSilenceSamples:  streamCfg.MinSilenceDurationMs * streamCfg.SampleRate / 1000,
		minSpeechSamples:   streamCfg.MinSpeechDurationMs * streamCfg.SampleRate / 1000,
		preRollMs:          cfg.PreRollMs,
		audioBufferSeconds: cfg.AudioBufferSeconds,
		maxSpeechSamples:   maxSpeechSamples,
	}, nil
}

func (g *AudioGate) Process(ctx context.Context, chunk Chunk) (AudioOutput, error) {
	if g == nil || g.inferer == nil {
		return AudioOutput{}, fmt.Errorf("vad: nil audio gate")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	samples, sampleRate, err := chunkSamples(chunk, g.cfg.SampleRate)
	if err != nil {
		return AudioOutput{}, err
	}
	if err := g.ensureSampleRate(sampleRate); err != nil {
		return AudioOutput{}, err
	}

	if len(samples) > 0 {
		g.audio.Write(samples)
	}
	if g.vadRead < g.audio.Start() {
		return AudioOutput{}, fmt.Errorf("vad: unread audio overwritten by circular buffer")
	}

	var out []float32
	vadStart := false
	vadEnd := false
	inferenceFrames := 0
	probabilityMin := float32(1)
	probabilityMax := float32(0)
	for g.audio.End()-g.vadRead >= int64(g.windowSize) {
		if err := ctx.Err(); err != nil {
			return AudioOutput{}, err
		}
		frame := g.audio.CopyRange(g.vadRead, g.vadRead+int64(g.windowSize))
		probability, err := g.inferer.Infer(frame)
		if err != nil {
			return AudioOutput{}, fmt.Errorf("vad: infer frame: %w", err)
		}
		inferenceFrames++
		if probability < probabilityMin {
			probabilityMin = probability
		}
		if probability > probabilityMax {
			probabilityMax = probability
		}
		g.vadRead += int64(g.windowSize)
		started, ended := g.applyProbability(probability)
		vadStart = vadStart || started
		if started {
			start := g.preRollStart()
			out = append(out, g.audio.CopyRange(start, g.vadRead)...)
			g.sendCursor = g.vadRead
		}
		if g.active && !started {
			out = append(out, g.audio.CopyRange(g.sendCursor, g.vadRead)...)
			g.sendCursor = g.vadRead
		}
		forcedEnd := g.forceEndDueToMaxSpeech()
		if forcedEnd {
			if g.sendCursor < g.vadRead {
				out = append(out, g.audio.CopyRange(g.sendCursor, g.vadRead)...)
			}
			g.endActiveSpeech()
		}
		if ended {
			vadEnd = true
			g.endActiveSpeech()
		}
		if forcedEnd {
			vadEnd = true
		}
	}

	if chunk.Final && g.active {
		if g.sendCursor < g.audio.End() {
			out = append(out, g.audio.CopyRange(g.sendCursor, g.audio.End())...)
			g.sendCursor = g.audio.End()
		}
		vadEnd = true
		g.endActiveSpeech()
	}

	return AudioOutput{
		Chunk: Chunk{
			Samples:    out,
			SampleRate: sampleRate,
			Final:      chunk.Final,
		},
		VADEnd:          vadEnd,
		VADStart:        vadStart,
		InferenceFrames: inferenceFrames,
		ProbabilityMin:  probabilityMin,
		ProbabilityMax:  probabilityMax,
	}, nil
}

func (g *AudioGate) Reset() error {
	if g == nil || g.inferer == nil {
		return fmt.Errorf("vad: nil audio gate")
	}
	g.audio.Reset()
	g.vadRead = 0
	g.sendCursor = 0
	g.active = false
	g.tempStart = 0
	g.tempEnd = 0
	g.speechStart = 0
	g.preRollFloor = 0
	return g.inferer.Reset()
}

func (g *AudioGate) Close() error {
	return nil
}

func (g *AudioGate) ensureSampleRate(sampleRate int) error {
	if sampleRate == 0 {
		sampleRate = g.cfg.SampleRate
	}
	if sampleRate != g.cfg.SampleRate {
		return fmt.Errorf("vad: sample rate changed from %d to %d", g.cfg.SampleRate, sampleRate)
	}
	if g.sampleRate != 0 {
		return nil
	}
	g.sampleRate = sampleRate
	g.audio = newSampleRing(g.audioBufferSeconds * sampleRate)
	return nil
}

func (g *AudioGate) preRollSamples() int {
	return g.preRollMs * g.cfg.SampleRate / 1000
}

func (g *AudioGate) preRollStart() int64 {
	start := g.vadRead - int64(g.preRollSamples())
	if start < g.audio.Start() {
		start = g.audio.Start()
	}
	if start < g.preRollFloor {
		start = g.preRollFloor
	}
	return start
}

func (g *AudioGate) forceEndDueToMaxSpeech() bool {
	return g.maxSpeechSamples > 0 && g.active && g.speechStart != 0 && g.vadRead-g.speechStart >= int64(g.maxSpeechSamples)
}

func (g *AudioGate) endActiveSpeech() {
	g.active = false
	g.tempStart = 0
	g.tempEnd = 0
	g.speechStart = 0
	g.sendCursor = g.vadRead
	g.preRollFloor = g.vadRead
}

func (g *AudioGate) applyProbability(probability float32) (started bool, ended bool) {
	if probability >= g.cfg.Threshold && g.tempEnd != 0 {
		g.tempEnd = 0
	}
	if probability >= g.cfg.Threshold && g.tempStart == 0 && !g.active {
		g.tempStart = g.vadRead
		return false, false
	}
	if probability >= g.cfg.Threshold && g.tempStart != 0 && !g.active {
		if g.vadRead-g.tempStart < int64(g.minSpeechSamples) {
			return false, false
		}
		g.active = true
		g.speechStart = g.tempStart
		return true, false
	}
	if probability < g.cfg.Threshold && !g.active {
		g.tempStart = 0
		g.tempEnd = 0
		return false, false
	}
	if probability < g.cfg.Threshold-0.15 && g.active {
		if g.tempEnd == 0 {
			g.tempEnd = g.vadRead
		}
		if g.vadRead-g.tempEnd < int64(g.minSilenceSamples) {
			return false, false
		}
		g.tempStart = 0
		g.tempEnd = 0
		return false, true
	}
	return false, false
}

type sampleRing struct {
	buf   []float32
	start int64
	end   int64
}

func newSampleRing(capacity int) sampleRing {
	if capacity < 0 {
		capacity = 0
	}
	return sampleRing{buf: make([]float32, capacity)}
}

func (r *sampleRing) Write(samples []float32) {
	if len(r.buf) == 0 || len(samples) == 0 {
		return
	}
	if len(samples) >= len(r.buf) {
		base := samples[len(samples)-len(r.buf):]
		copy(r.buf, base)
		r.end += int64(len(samples))
		r.start = r.end - int64(len(r.buf))
		return
	}
	for _, sample := range samples {
		r.buf[int(r.end%int64(len(r.buf)))] = sample
		r.end++
		if r.end-r.start > int64(len(r.buf)) {
			r.start = r.end - int64(len(r.buf))
		}
	}
}

func (r *sampleRing) Start() int64 { return r.start }
func (r *sampleRing) End() int64   { return r.end }

func (r *sampleRing) CopyRange(start, end int64) []float32 {
	if start < r.start {
		start = r.start
	}
	if end > r.end {
		end = r.end
	}
	if start >= end || len(r.buf) == 0 {
		return nil
	}
	out := make([]float32, end-start)
	for i := range out {
		out[i] = r.buf[int((start+int64(i))%int64(len(r.buf)))]
	}
	return out
}

func (r *sampleRing) Reset() {
	r.start = 0
	r.end = 0
}
