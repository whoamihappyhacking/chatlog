package whisper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sjzar/chatlog/pkg/util/silk"
)

// WebServiceConfig controls the behaviour of the HTTP Whisper webservice backend.
type WebServiceConfig struct {
	BaseURL        string
	OutputFormat   string
	WordTimestamps bool
	VADFilter      bool
	Encode         *bool
	Diarize        bool
	MinSpeakers    int
	MaxSpeakers    int
	RequestTimeout time.Duration
	DefaultOptions Options
}

// WebServiceTranscriber implements transcription against a HTTP whisper-asr-webservice instance.
type WebServiceTranscriber struct {
	client  *http.Client
	cfg     WebServiceConfig
	baseURL string
}

// Close releases resources held by the webservice transcriber. No-op for HTTP client.
func (t *WebServiceTranscriber) Close() {}

// NewWebServiceTranscriber constructs a transcriber for whisper-asr-webservice.
func NewWebServiceTranscriber(cfg WebServiceConfig) (*WebServiceTranscriber, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("webservice base URL cannot be empty")
	}

	httpClient := &http.Client{}
	if cfg.RequestTimeout > 0 {
		httpClient.Timeout = cfg.RequestTimeout
	}

	if cfg.OutputFormat == "" {
		cfg.OutputFormat = "json"
	}

	return &WebServiceTranscriber{
		client:  httpClient,
		cfg:     cfg,
		baseURL: baseURL,
	}, nil
}

// TranscribePCM handles PCM16 input.
func (t *WebServiceTranscriber) TranscribePCM(ctx context.Context, samples []float32, sampleRate int, opts Options) (*Result, error) {
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

	duration := pcmDuration(len(pcm), sampleRate)
	return t.transcribeWAV(ctx, wav, duration, merged)
}

// TranscribeSilk handles SILK input by converting it into PCM before forwarding to PCM handler.
func (t *WebServiceTranscriber) TranscribeSilk(ctx context.Context, silkData []byte, opts Options) (*Result, error) {
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

	duration := pcmDuration(len(samples), sampleRate)
	return t.transcribeWAV(ctx, wav, duration, merged)
}

func (t *WebServiceTranscriber) transcribeWAV(ctx context.Context, wav []byte, fallbackDuration time.Duration, opts Options) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	requestURL, err := t.buildRequestURL(opts)
	if err != nil {
		return nil, err
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	fileWriter, err := writer.CreateFormFile("audio_file", "audio.wav")
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(fileWriter, bytes.NewReader(wav)); err != nil {
		return nil, err
	}

	if t.cfg.Encode != nil {
		if err := writer.WriteField("encode", strconv.FormatBool(*t.cfg.Encode)); err != nil {
			return nil, err
		}
	}
	if t.cfg.Diarize {
		if err := writer.WriteField("diarize", "true"); err != nil {
			return nil, err
		}
		if t.cfg.MinSpeakers > 0 {
			if err := writer.WriteField("min_speakers", strconv.Itoa(t.cfg.MinSpeakers)); err != nil {
				return nil, err
			}
		}
		if t.cfg.MaxSpeakers > 0 {
			if err := writer.WriteField("max_speakers", strconv.Itoa(t.cfg.MaxSpeakers)); err != nil {
				return nil, err
			}
		}
	}

	if err = writer.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(raw) == 0 {
			return nil, fmt.Errorf("webservice returned status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("webservice error (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if t.cfg.OutputFormat != "json" {
		// Only JSON can be mapped into the common Result structure for now.
		return &Result{Text: string(raw), Duration: fallbackDuration}, nil
	}

	var payload webServiceResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode webservice response: %w", err)
	}

	result := &Result{
		Text:     strings.TrimSpace(payload.Text),
		Language: strings.TrimSpace(payload.Language),
	}

	var maxSegmentEnd float64
	for _, seg := range payload.Segments {
		segment := Segment{
			Start: time.Duration(seg.Start * float64(time.Second)),
			End:   time.Duration(seg.End * float64(time.Second)),
			Text:  seg.Text,
		}
		result.Segments = append(result.Segments, segment)
		if seg.End > maxSegmentEnd {
			maxSegmentEnd = seg.End
		}
	}

	if maxSegmentEnd > 0 {
		result.Duration = time.Duration(maxSegmentEnd * float64(time.Second))
	} else {
		result.Duration = fallbackDuration
	}

	if result.Duration == 0 {
		result.Duration = fallbackDuration
	}

	translated := opts.TranslateSet && opts.Translate
	if result.Language == "" {
		result.Language = fallbackLanguage(opts, translated)
	}

	result.Text = strings.TrimSpace(result.Text)

	return result, nil
}

func (t *WebServiceTranscriber) buildRequestURL(opts Options) (string, error) {
	if t.baseURL == "" {
		return "", fmt.Errorf("webservice base URL is empty")
	}

	target, err := url.Parse(t.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	trimmedPath := strings.TrimSuffix(target.Path, "/")
	target.Path = trimmedPath + "/asr"

	query := target.Query()
	query.Set("output", t.cfg.OutputFormat)

	if opts.TranslateSet {
		if opts.Translate {
			query.Set("task", "translate")
		} else {
			query.Set("task", "transcribe")
		}
	}
	if opts.LanguageSet && opts.Language != "" {
		query.Set("language", opts.Language)
	}
	if t.cfg.WordTimestamps {
		query.Set("word_timestamps", "true")
	}
	if t.cfg.VADFilter {
		query.Set("vad_filter", "true")
	}

	target.RawQuery = query.Encode()
	return target.String(), nil
}

func (t *WebServiceTranscriber) mergeOptions(overrides Options) Options {
	merged := t.cfg.DefaultOptions

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

// webServiceResponse models the JSON payload returned by whisper-asr-webservice.
type webServiceResponse struct {
	Text     string              `json:"text"`
	Segments []webServiceSegment `json:"segments"`
	Language string              `json:"language"`
}

type webServiceSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}
