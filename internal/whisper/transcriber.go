package whisper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	openaiparam "github.com/openai/openai-go/v3/packages/param"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/pkg/util/silk"
)

// OpenAIConfig describes how to initialise an OpenAI-backed transcriber.
type OpenAIConfig struct {
	Model          string
	TranslateModel string
	APIKey         string
	BaseURL        string
	Organization   string
	ProxyURL       string
	RequestTimeout time.Duration
	DefaultOptions Options
}

// OpenAITranscriber uses OpenAI's REST API to perform speech-to-text tasks.
type OpenAITranscriber struct {
	client         *openai.Client
	model          openai.AudioModel
	translateModel openai.AudioModel
	defaultOptions Options
}

// NewOpenAITranscriber builds a new instance of the OpenAI transcription backend.
func NewOpenAITranscriber(cfg OpenAIConfig) (*OpenAITranscriber, error) {
	model := normalizeAudioModel(cfg.Model)
	translateModel := model
	if cfg.TranslateModel != "" {
		translateModel = normalizeAudioModel(cfg.TranslateModel)
	}

	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.Organization != "" {
		opts = append(opts, option.WithOrganization(cfg.Organization))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.ProxyURL != "" {
		client, err := buildHTTPClient(cfg.ProxyURL, cfg.RequestTimeout)
		if err != nil {
			return nil, err
		}
		opts = append(opts, option.WithHTTPClient(client))
	} else if cfg.RequestTimeout > 0 {
		opts = append(opts, option.WithRequestTimeout(cfg.RequestTimeout))
	}

	clientVal := openai.NewClient(opts...)
	client := &clientVal

	return &OpenAITranscriber{
		client:         client,
		model:          model,
		translateModel: translateModel,
		defaultOptions: cfg.DefaultOptions,
	}, nil
}

func buildHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	var baseTransport *http.Transport
	if ok {
		baseTransport = transport.Clone()
	} else {
		baseTransport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}

	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		baseTransport.Proxy = http.ProxyURL(parsed)
	}

	client := &http.Client{
		Transport: baseTransport,
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client, nil
}

// Close releases resources held by the transcriber. No-op for the OpenAI backend.
func (t *OpenAITranscriber) Close() {}

// ModelName returns the audio model identifier currently in use.
func (t *OpenAITranscriber) ModelName() string {
	return string(t.model)
}

// TranscribePCM converts PCM float32 samples into text via OpenAI's API.
func (t *OpenAITranscriber) TranscribePCM(ctx context.Context, samples []float32, sampleRate int, opts Options) (*Result, error) {
	merged := t.mergeOptions(opts)

	if len(samples) == 0 {
		return nil, nil
	}
	if sampleRate <= 0 {
		sampleRate = 24000
	}

	pcm := float32ToPCM16(samples)
	wav, err := encodePCM16AsWAV(pcm, sampleRate)
	if err != nil {
		return nil, err
	}

	return t.transcribeWAV(ctx, wav, sampleRate, len(pcm), merged)
}

// TranscribeSilk converts Silk-encoded payloads into text via OpenAI's API.
func (t *OpenAITranscriber) TranscribeSilk(ctx context.Context, silkData []byte, opts Options) (*Result, error) {
	merged := t.mergeOptions(opts)

	if len(silkData) == 0 {
		return nil, nil
	}

	samples, sampleRate, err := silk.Silk2PCM16(silkData)
	if err != nil {
		return nil, err
	}

	wav, err := encodePCM16AsWAV(samples, sampleRate)
	if err != nil {
		return nil, err
	}

	return t.transcribeWAV(ctx, wav, sampleRate, len(samples), merged)
}

func (t *OpenAITranscriber) transcribeWAV(ctx context.Context, wav []byte, sampleRate, sampleCount int, opts Options) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	duration := pcmDuration(sampleCount, sampleRate)

	if opts.TranslateSet && opts.Translate {
		return t.sendTranslation(ctx, wav, opts, duration)
	}

	return t.sendTranscription(ctx, wav, opts, duration)
}

func (t *OpenAITranscriber) sendTranscription(ctx context.Context, wav []byte, opts Options, fallbackDuration time.Duration) (*Result, error) {
	params := openai.AudioTranscriptionNewParams{
		File:                   openai.File(bytes.NewReader(wav), "audio.wav", "audio/wav"),
		Model:                  t.model,
		ResponseFormat:         openai.AudioResponseFormatVerboseJSON,
		TimestampGranularities: []string{"segment"},
	}

	if opts.LanguageSet {
		trimmed := strings.TrimSpace(opts.Language)
		if trimmed != "" && !strings.EqualFold(trimmed, "auto") {
			params.Language = openaiparam.NewOpt(trimmed)
		}
	}
	if opts.InitialPromptSet {
		prompt := strings.TrimSpace(opts.InitialPrompt)
		if prompt != "" {
			params.Prompt = openaiparam.NewOpt(prompt)
		}
	}
	if opts.TemperatureSet {
		params.Temperature = openaiparam.NewOpt(float64(opts.Temperature))
	}

	transcription, err := t.client.Audio.Transcriptions.New(ctx, params)
	if err != nil {
		return nil, err
	}

	return buildResultFromTranscription(transcription, opts, fallbackDuration)
}

func (t *OpenAITranscriber) sendTranslation(ctx context.Context, wav []byte, opts Options, fallbackDuration time.Duration) (*Result, error) {
	params := openai.AudioTranslationNewParams{
		File:  openai.File(bytes.NewReader(wav), "audio.wav", "audio/wav"),
		Model: t.translateModel,
	}

	if opts.InitialPromptSet {
		prompt := strings.TrimSpace(opts.InitialPrompt)
		if prompt != "" {
			params.Prompt = openaiparam.NewOpt(prompt)
		}
	}
	if opts.TemperatureSet {
		params.Temperature = openaiparam.NewOpt(float64(opts.Temperature))
	}

	translation, err := t.client.Audio.Translations.New(ctx, params)
	if err != nil {
		return nil, err
	}

	return &Result{
		Text:     strings.TrimSpace(translation.Text),
		Language: fallbackLanguage(opts, true),
		Duration: fallbackDuration,
	}, nil
}

func (t *OpenAITranscriber) mergeOptions(overrides Options) Options {
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

func buildResultFromTranscription(tr *openai.Transcription, opts Options, fallbackDuration time.Duration) (*Result, error) {
	if tr == nil {
		return nil, errors.New("openai transcription response is empty")
	}

	res := &Result{
		Text:     strings.TrimSpace(tr.Text),
		Duration: fallbackDuration,
	}

	raw := tr.RawJSON()
	if raw != "" {
		var payload verboseTranscription
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			log.Debug().Err(err).Msg("failed to parse verbose transcription payload")
		} else {
			if payload.Duration > 0 {
				res.Duration = secondsToDuration(payload.Duration)
			}
			if text := strings.TrimSpace(payload.Text); text != "" {
				res.Text = text
			}
			if lang := strings.TrimSpace(payload.Language); lang != "" {
				res.Language = lang
			}
			if len(payload.Segments) > 0 {
				segments := make([]Segment, 0, len(payload.Segments))
				for _, seg := range payload.Segments {
					segments = append(segments, Segment{
						ID:    seg.ID,
						Start: secondsToDuration(seg.Start),
						End:   secondsToDuration(seg.End),
						Text:  strings.TrimSpace(seg.Text),
					})
				}
				res.Segments = segments
			}
		}
	}

	if res.Duration == 0 {
		res.Duration = fallbackDuration
	}
	if res.Language == "" {
		res.Language = fallbackLanguage(opts, false)
	}

	return res, nil
}

func normalizeAudioModel(name string) openai.AudioModel {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return openai.AudioModelWhisper1
	}

	lower := strings.ToLower(trimmed)
	if strings.HasSuffix(lower, ".bin") || strings.ContainsAny(trimmed, "\\/") {
		log.Warn().Str("model", trimmed).Msg("ignoring local whisper model path; using OpenAI whisper-1")
		return openai.AudioModelWhisper1
	}

	return openai.AudioModel(trimmed)
}

func fallbackLanguage(opts Options, translated bool) string {
	if translated {
		return "en"
	}
	if opts.LanguageSet {
		trimmed := strings.TrimSpace(opts.Language)
		if trimmed != "" && !strings.EqualFold(trimmed, "auto") {
			return trimmed
		}
	}
	return ""
}

type verboseTranscription struct {
	Text     string                        `json:"text"`
	Language string                        `json:"language"`
	Duration float64                       `json:"duration"`
	Segments []verboseTranscriptionSegment `json:"segments"`
}

type verboseTranscriptionSegment struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

func float32ToPCM16(src []float32) []int16 {
	if len(src) == 0 {
		return nil
	}
	dst := make([]int16, len(src))
	for i, sample := range src {
		v := float64(sample)
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		dst[i] = int16(math.Round(v * 32767))
	}
	return dst
}

func writePCM16AsWAVToWriter(w io.Writer, samples []int16, sampleRate int) error {
	if sampleRate <= 0 {
		sampleRate = 24000
	}

	dataSize := len(samples) * 2
	riffSize := 36 + dataSize
	byteRate := sampleRate * 2
	blockAlign := 2

	header := make([]byte, 44)
	copy(header[0:], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:], uint32(riffSize))
	copy(header[8:], []byte("WAVEfmt "))
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:], uint32(dataSize))

	if _, err := w.Write(header); err != nil {
		return err
	}

	payload := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(payload[i*2:], uint16(sample))
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}

	return nil
}

func encodePCM16AsWAV(samples []int16, sampleRate int) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	if err := writePCM16AsWAVToWriter(buf, samples, sampleRate); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func pcmDuration(sampleCount int, sampleRate int) time.Duration {
	if sampleRate <= 0 || sampleCount <= 0 {
		return 0
	}
	seconds := float64(sampleCount) / float64(sampleRate)
	return secondsToDuration(seconds)
}

func secondsToDuration(seconds float64) time.Duration {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
