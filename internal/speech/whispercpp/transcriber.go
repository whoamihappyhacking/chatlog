//go:build cgo

package whispercpp

/*
#cgo CFLAGS: -I${SRCDIR}/../../../third_party/whisper/include
#cgo LDFLAGS: -L${SRCDIR}/../../../third_party/whisper/lib -lwhisper -lggml -lstdc++ -lm
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"

	whisper "github.com/ggerganov/whisper.cpp/bindings/go/pkg/whisper"

	"github.com/sjzar/chatlog/internal/speech"
	silkutil "github.com/sjzar/chatlog/pkg/util/silk"
)

// Config describes how to initialise the whisper.cpp backend.
type Config struct {
	ModelPath      string
	DefaultOptions speech.Options
}

// Transcriber wraps a whisper.cpp model instance.
type Transcriber struct {
	mu    sync.Mutex
	model whisper.Model
	cfg   Config
}

// New instantiates a whisper.cpp backed transcriber.
func New(cfg Config) (*Transcriber, error) {
	path := strings.TrimSpace(cfg.ModelPath)
	if path == "" {
		return nil, errors.New("whisper model path is required")
	}

	model, err := whisper.New(path)
	if err != nil {
		return nil, fmt.Errorf("load whisper model: %w", err)
	}

	cfg.DefaultOptions = normalizeDefaults(cfg.DefaultOptions)

	return &Transcriber{model: model, cfg: cfg}, nil
}

// Close releases the underlying model resources.
func (t *Transcriber) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.model != nil {
		_ = t.model.Close()
		t.model = nil
	}
}

// TranscribePCM transcribes mono PCM samples.
func (t *Transcriber) TranscribePCM(ctx context.Context, samples []float32, sampleRate int, opts speech.Options) (*speech.Result, error) {
	if len(samples) == 0 {
		return nil, errors.New("empty audio samples")
	}

	processed := resampleFloat32(samples, sampleRate, int(whisper.SampleRate))
	return t.process(ctx, processed, opts)
}

// TranscribeSilk decodes a Silk encoded voice clip and transcribes it.
func (t *Transcriber) TranscribeSilk(ctx context.Context, silkData []byte, opts speech.Options) (*speech.Result, error) {
	pcm16, pcmRate, err := silkutil.Silk2PCM16(silkData)
	if err != nil {
		return nil, err
	}
	floatSamples := int16ToFloat32(pcm16)
	return t.TranscribePCM(ctx, floatSamples, pcmRate, opts)
}

func (t *Transcriber) process(ctx context.Context, samples []float32, override speech.Options) (*speech.Result, error) {
	t.mu.Lock()
	model := t.model
	cfg := t.cfg
	t.mu.Unlock()
	if model == nil {
		return nil, errors.New("transcriber closed")
	}

	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	wctx, err := model.NewContext()
	if err != nil {
		return nil, fmt.Errorf("create whisper context: %w", err)
	}

	effective := mergeOptions(cfg.DefaultOptions, override)

	threads := effective.Threads
	if !effective.ThreadsSet || threads <= 0 {
		threads = runtime.NumCPU()
	}
	wctx.SetThreads(uint(threads))

	languageOpt := "auto"
	if effective.LanguageSet && strings.TrimSpace(effective.Language) != "" {
		languageOpt = strings.TrimSpace(effective.Language)
	} else if cfg.DefaultOptions.LanguageSet && strings.TrimSpace(cfg.DefaultOptions.Language) != "" {
		languageOpt = strings.TrimSpace(cfg.DefaultOptions.Language)
	}
	if err := wctx.SetLanguage(languageOpt); err != nil {
		return nil, err
	}

	translate := cfg.DefaultOptions.Translate
	if cfg.DefaultOptions.TranslateSet {
		translate = cfg.DefaultOptions.Translate
	}
	if effective.TranslateSet {
		translate = effective.Translate
	}
	wctx.SetTranslate(translate)

	prompt := ""
	if cfg.DefaultOptions.InitialPromptSet {
		prompt = cfg.DefaultOptions.InitialPrompt
	}
	if effective.InitialPromptSet {
		prompt = effective.InitialPrompt
	}
	if prompt != "" {
		wctx.SetInitialPrompt(prompt)
	}

	if cfg.DefaultOptions.TemperatureSet {
		wctx.SetTemperature(cfg.DefaultOptions.Temperature)
	}
	if effective.TemperatureSet {
		wctx.SetTemperature(effective.Temperature)
	}
	if cfg.DefaultOptions.TemperatureFloorSet {
		wctx.SetTemperatureFallback(cfg.DefaultOptions.TemperatureFloor)
	}
	if effective.TemperatureFloorSet {
		wctx.SetTemperatureFallback(effective.TemperatureFloor)
	}

	var encoderCb whisper.EncoderBeginCallback
	if ctx != nil {
		encoderCb = func() bool {
			return ctx.Err() == nil
		}
	}

	if err := wctx.Process(samples, encoderCb, nil, nil); err != nil {
		return nil, err
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	segments := make([]speech.Segment, 0)
	var textBuilder strings.Builder
	for {
		seg, err := wctx.NextSegment()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		converted := speech.Segment{
			ID:    seg.Num,
			Start: seg.Start,
			End:   seg.End,
			Text:  seg.Text,
		}
		segments = append(segments, converted)
		if textBuilder.Len() > 0 {
			textBuilder.WriteByte(' ')
		}
		textBuilder.WriteString(strings.TrimSpace(seg.Text))
	}

	duration := time.Duration(float64(len(samples)) / float64(whisper.SampleRate) * float64(time.Second))
	detected := wctx.DetectedLanguage()
	if detected == "" {
		detected = languageOpt
	}

	return &speech.Result{
		Text:     strings.TrimSpace(textBuilder.String()),
		Language: detected,
		Duration: duration,
		Segments: segments,
	}, nil
}

func mergeOptions(base, override speech.Options) speech.Options {
	result := base

	if override.LanguageSet {
		result.Language = override.Language
		result.LanguageSet = true
	}
	if override.TranslateSet {
		result.Translate = override.Translate
		result.TranslateSet = true
	}
	if override.ThreadsSet {
		result.Threads = override.Threads
		result.ThreadsSet = true
	}
	if override.InitialPromptSet {
		result.InitialPrompt = override.InitialPrompt
		result.InitialPromptSet = true
	}
	if override.TemperatureSet {
		result.Temperature = override.Temperature
		result.TemperatureSet = true
	}
	if override.TemperatureFloorSet {
		result.TemperatureFloor = override.TemperatureFloor
		result.TemperatureFloorSet = true
	}

	return result
}

func int16ToFloat32(src []int16) []float32 {
	const scale = 1.0 / 32768.0
	out := make([]float32, len(src))
	for i, s := range src {
		out[i] = float32(float64(s) * scale)
	}
	return out
}

func resampleFloat32(src []float32, srcRate, dstRate int) []float32 {
	if len(src) == 0 {
		return nil
	}
	if srcRate <= 0 {
		srcRate = dstRate
	}
	if dstRate <= 0 || srcRate == dstRate {
		out := make([]float32, len(src))
		copy(out, src)
		return out
	}

	ratio := float64(srcRate) / float64(dstRate)
	if ratio == 0 {
		return nil
	}
	targetLen := int(math.Ceil(float64(len(src)) / ratio))
	if targetLen <= 0 {
		targetLen = 1
	}

	out := make([]float32, targetLen)
	for i := 0; i < targetLen; i++ {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := float32(srcPos - float64(idx))
		switch {
		case idx >= len(src)-1:
			out[i] = src[len(src)-1]
		default:
			val := src[idx]
			next := src[idx+1]
			out[i] = val + (next-val)*frac
		}
	}
	return out
}

func normalizeDefaults(o speech.Options) speech.Options {
	if strings.TrimSpace(o.Language) != "" {
		o.Language = strings.TrimSpace(o.Language)
		o.LanguageSet = true
	}
	if o.TranslateSet || o.Translate {
		o.TranslateSet = true
	}
	if o.ThreadsSet || o.Threads > 0 {
		o.ThreadsSet = true
	}
	if strings.TrimSpace(o.InitialPrompt) != "" {
		o.InitialPrompt = strings.TrimSpace(o.InitialPrompt)
		o.InitialPromptSet = true
	}
	if o.TemperatureSet {
		o.TemperatureSet = true
	}
	if o.TemperatureFloorSet {
		o.TemperatureFloorSet = true
	}
	return o
}
