package whisper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	whis "github.com/ggerganov/whisper.cpp/bindings/go/pkg/whisper"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/pkg/util/silk"
)

// WhisperCPPConfig controls the on-device whisper.cpp backend.
type WhisperCPPConfig struct {
	ModelPath      string
	Threads        int
	DefaultOptions Options
}

// WhisperCPPTranscriber wraps a whisper.cpp model for local transcription.
type WhisperCPPTranscriber struct {
	model          whis.Model
	defaultOptions Options
	defaultThreads int
}

// NewWhisperCPPTranscriber loads a whisper.cpp model for on-device speech recognition.
func NewWhisperCPPTranscriber(cfg WhisperCPPConfig) (*WhisperCPPTranscriber, error) {
	modelPath := strings.TrimSpace(cfg.ModelPath)
	if modelPath == "" {
		return nil, fmt.Errorf("whisper.cpp model path is empty")
	}

	model, err := whis.New(modelPath)
	if err != nil {
		return nil, fmt.Errorf("load whisper.cpp model: %w", err)
	}

	return &WhisperCPPTranscriber{
		model:          model,
		defaultOptions: cfg.DefaultOptions,
		defaultThreads: cfg.Threads,
	}, nil
}

// Close releases resources held by the whisper.cpp model.
func (t *WhisperCPPTranscriber) Close() {
	if t.model != nil {
		if err := t.model.Close(); err != nil {
			log.Debug().Err(err).Msg("whispercpp: model close failed")
		}
		t.model = nil
	}
}

// TranscribePCM runs whisper.cpp against raw PCM samples.
func (t *WhisperCPPTranscriber) TranscribePCM(ctx context.Context, samples []float32, sampleRate int, opts Options) (*Result, error) {
	if t.model == nil {
		return nil, errors.New("whisper.cpp model not initialised")
	}
	merged := t.mergeOptions(opts)

	if len(samples) == 0 {
		return nil, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}

	if sampleRate <= 0 {
		sampleRate = int(whis.SampleRate)
	}

	processed := resampleIfNeeded(samples, sampleRate, int(whis.SampleRate))

	ctxInstance, err := t.model.NewContext()
	if err != nil {
		return nil, fmt.Errorf("create whisper.cpp context: %w", err)
	}

	threads := t.defaultThreads
	if merged.ThreadsSet && merged.Threads > 0 {
		threads = merged.Threads
	}
	if threads > 0 {
		ctxInstance.SetThreads(uint(threads))
	}

	lang := "auto"
	if merged.LanguageSet {
		trimmed := strings.TrimSpace(merged.Language)
		if trimmed != "" {
			lang = trimmed
		}
	}
	if err := ctxInstance.SetLanguage(lang); err != nil {
		log.Warn().Err(err).Str("language", lang).Msg("whispercpp: set language failed")
	}

	if merged.TranslateSet {
		ctxInstance.SetTranslate(merged.Translate)
	}
	if merged.InitialPromptSet {
		ctxInstance.SetInitialPrompt(merged.InitialPrompt)
	}
	if merged.TemperatureSet {
		ctxInstance.SetTemperature(merged.Temperature)
	}
	if merged.TemperatureFloorSet {
		ctxInstance.SetTemperatureFallback(merged.TemperatureFloor)
	}

	if err := ctxInstance.Process(processed, nil, nil, nil); err != nil {
		return nil, fmt.Errorf("whisper.cpp process pcm: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var (
		segments []Segment
		builder  strings.Builder
		lastEnd  time.Duration
	)

	for {
		seg, err := ctxInstance.NextSegment()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("whisper.cpp next segment: %w", err)
		}

		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}

		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(text)

		segments = append(segments, Segment{
			ID:    seg.Num,
			Start: seg.Start,
			End:   seg.End,
			Text:  text,
		})
		if seg.End > lastEnd {
			lastEnd = seg.End
		}
	}

	detected := strings.TrimSpace(ctxInstance.DetectedLanguage())
	if detected == "" {
		detected = fallbackLanguage(merged, merged.TranslateSet && merged.Translate)
	}

	return &Result{
		Text:     strings.TrimSpace(builder.String()),
		Language: detected,
		Duration: lastEnd,
		Segments: segments,
	}, nil
}

// TranscribeSilk decodes SILK payloads before invoking whisper.cpp.
func (t *WhisperCPPTranscriber) TranscribeSilk(ctx context.Context, silkData []byte, opts Options) (*Result, error) {
	if len(silkData) == 0 {
		return nil, nil
	}
	samples16, sampleRate, err := silk.Silk2PCM16(silkData)
	if err != nil {
		return nil, err
	}

	floatSamples := make([]float32, len(samples16))
	const scale = 1.0 / 32768.0
	for i, sample := range samples16 {
		floatSamples[i] = float32(float64(sample) * scale)
	}

	return t.TranscribePCM(ctx, floatSamples, sampleRate, opts)
}

func (t *WhisperCPPTranscriber) mergeOptions(overrides Options) Options {
	merged := t.defaultOptions

	if overrides.LanguageSet {
		merged.Language = overrides.Language
		merged.LanguageSet = true
	}
	if overrides.TranslateSet {
		merged.Translate = overrides.Translate
		merged.TranslateSet = true
	}
	if overrides.ThreadsSet {
		merged.Threads = overrides.Threads
		merged.ThreadsSet = true
	}
	if overrides.InitialPromptSet {
		merged.InitialPrompt = overrides.InitialPrompt
		merged.InitialPromptSet = true
	}
	if overrides.TemperatureSet {
		merged.Temperature = overrides.Temperature
		merged.TemperatureSet = true
	}
	if overrides.TemperatureFloorSet {
		merged.TemperatureFloor = overrides.TemperatureFloor
		merged.TemperatureFloorSet = true
	}

	return merged
}

func resampleIfNeeded(samples []float32, fromRate, toRate int) []float32 {
	if fromRate <= 0 {
		fromRate = toRate
	}
	if fromRate == toRate || len(samples) == 0 {
		dst := make([]float32, len(samples))
		copy(dst, samples)
		return dst
	}

	ratio := float64(fromRate) / float64(toRate)
	if ratio <= 0 {
		dst := make([]float32, len(samples))
		copy(dst, samples)
		return dst
	}

	outLen := int(math.Ceil(float64(len(samples)) / ratio))
	if outLen <= 0 {
		outLen = len(samples)
	}

	dst := make([]float32, outLen)
	for i := range dst {
		srcPos := float64(i) * ratio
		idx := int(math.Floor(srcPos))
		frac := srcPos - float64(idx)

		if idx >= len(samples)-1 {
			dst[i] = samples[len(samples)-1]
			continue
		}

		a := samples[idx]
		b := samples[idx+1]
		dst[i] = float32(float64(a)*(1-frac) + float64(b)*frac)
	}

	return dst
}
