package whisper

import (
	"context"
	"time"
)

// Options configures a transcription request.
type Options struct {
	Language            string  // "auto" to let the model detect language
	LanguageSet         bool    // true when Language should override defaults
	Translate           bool    // translate non-English speech into English
	TranslateSet        bool    // true when Translate should override defaults
	Threads             int     // number of threads used by the backend (<=0 uses default)
	ThreadsSet          bool    // true when Threads should override defaults
	InitialPrompt       string  // optional priming prompt
	InitialPromptSet    bool    // true when InitialPrompt should override defaults
	Temperature         float32 // sampling temperature
	TemperatureSet      bool    // true when Temperature should override defaults
	TemperatureFloor    float32 // optional fallback temperature when decoding stalls
	TemperatureFloorSet bool    // true when TemperatureFloor should override defaults
}

// Segment represents a portion of transcribed text with timestamps.
type Segment struct {
	ID    int           `json:"id"`
	Start time.Duration `json:"start"`
	End   time.Duration `json:"end"`
	Text  string        `json:"text"`
}

// Result holds the transcription outcome returned by a backend.
type Result struct {
	Text     string        `json:"text"`
	Language string        `json:"language"`
	Duration time.Duration `json:"duration"`
	Segments []Segment     `json:"segments"`
}

// Transcriber describes a component capable of converting speech into text.
type Transcriber interface {
	Close()
	TranscribePCM(ctx context.Context, samples []float32, sampleRate int, opts Options) (*Result, error)
	TranscribeSilk(ctx context.Context, silkData []byte, opts Options) (*Result, error)
}
