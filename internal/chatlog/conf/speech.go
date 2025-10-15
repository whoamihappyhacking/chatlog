package conf

import "github.com/sjzar/chatlog/internal/whisper"

// SpeechConfig controls optional speech-to-text features.
type SpeechConfig struct {
	Enabled               bool     `mapstructure:"enabled" json:"enabled"`
	Model                 string   `mapstructure:"model" json:"model"`
	TranslateModel        string   `mapstructure:"translate_model" json:"translate_model"`
	Threads               int      `mapstructure:"threads" json:"threads"`
	Language              string   `mapstructure:"language" json:"language"`
	Translate             *bool    `mapstructure:"translate" json:"translate"`
	InitialPrompt         string   `mapstructure:"initial_prompt" json:"initial_prompt"`
	Temperature           *float64 `mapstructure:"temperature" json:"temperature"`
	TemperatureFallback   *float64 `mapstructure:"temperature_fallback" json:"temperature_fallback"`
	APIKey                string   `mapstructure:"api_key" json:"api_key"`
	BaseURL               string   `mapstructure:"base_url" json:"base_url"`
	Organization          string   `mapstructure:"organization" json:"organization"`
	RequestTimeoutSeconds int      `mapstructure:"request_timeout_seconds" json:"request_timeout_seconds"`
}

// ToOptions converts the speech config into runtime options for a transcription backend.
func (c *SpeechConfig) ToOptions() whisper.Options {
	var opts whisper.Options

	if c == nil {
		return opts
	}

	if c.Language != "" {
		opts.Language = c.Language
		opts.LanguageSet = true
	}
	if c.Translate != nil {
		opts.Translate = *c.Translate
		opts.TranslateSet = true
	}
	if c.Threads > 0 {
		opts.Threads = c.Threads
		opts.ThreadsSet = true
	}
	if c.InitialPrompt != "" {
		opts.InitialPrompt = c.InitialPrompt
		opts.InitialPromptSet = true
	}
	if c.Temperature != nil {
		opts.Temperature = float32(*c.Temperature)
		opts.TemperatureSet = true
	}
	if c.TemperatureFallback != nil {
		opts.TemperatureFloor = float32(*c.TemperatureFallback)
		opts.TemperatureFloorSet = true
	}

	return opts
}
