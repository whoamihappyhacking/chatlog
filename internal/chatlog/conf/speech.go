package conf

import (
	"strings"

	"github.com/sjzar/chatlog/internal/whisper"
)

// OpenAISettings captures provider-specific fields retained for backwards compatibility.
type OpenAISettings struct {
	APIKey       string `mapstructure:"api_key" json:"api_key"`
	BaseURL      string `mapstructure:"base_url" json:"base_url"`
	Organization string `mapstructure:"organization" json:"organization"`
	Project      string `mapstructure:"project" json:"project"`
	Proxy        string `mapstructure:"proxy" json:"proxy"`
}

// SpeechConfig controls optional speech-to-text features.
type SpeechConfig struct {
	Enabled               bool           `mapstructure:"enabled" json:"enabled"`
	Provider              string         `mapstructure:"provider" json:"provider"`
	Model                 string         `mapstructure:"model" json:"model"`
	TranslateModel        string         `mapstructure:"translate_model" json:"translate_model"`
	Threads               int            `mapstructure:"threads" json:"threads"`
	Language              string         `mapstructure:"language" json:"language"`
	Translate             *bool          `mapstructure:"translate" json:"translate"`
	InitialPrompt         string         `mapstructure:"initial_prompt" json:"initial_prompt"`
	Temperature           *float64       `mapstructure:"temperature" json:"temperature"`
	TemperatureFallback   *float64       `mapstructure:"temperature_fallback" json:"temperature_fallback"`
	APIKey                string         `mapstructure:"api_key" json:"api_key"`
	BaseURL               string         `mapstructure:"base_url" json:"base_url"`
	Organization          string         `mapstructure:"organization" json:"organization"`
	Project               string         `mapstructure:"project" json:"project"`
	Proxy                 string         `mapstructure:"proxy" json:"proxy"`
	RequestTimeoutSeconds int            `mapstructure:"request_timeout_seconds" json:"request_timeout_seconds"`
	OpenAI                OpenAISettings `mapstructure:"openai" json:"openai"`
}

// Normalize hydrates legacy OpenAI fields into the flattened structure and applies defaults.
func (c *SpeechConfig) Normalize() {
	if c == nil {
		return
	}
	if c.Provider == "" {
		c.Provider = "openai"
	}

	if c.APIKey == "" {
		c.APIKey = c.OpenAI.APIKey
	}
	if c.BaseURL == "" {
		c.BaseURL = c.OpenAI.BaseURL
	}
	if c.Organization == "" {
		c.Organization = c.OpenAI.Organization
	}
	if c.Project == "" {
		c.Project = c.OpenAI.Project
	}
	if c.Proxy == "" {
		c.Proxy = c.OpenAI.Proxy
	}

	c.APIKey = strings.TrimSpace(c.APIKey)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.Organization = strings.TrimSpace(c.Organization)
	c.Project = strings.TrimSpace(c.Project)
	c.Proxy = strings.TrimSpace(c.Proxy)
}

// PrepareForSave syncs the flattened OpenAI fields back into the legacy nested structure.
func (c *SpeechConfig) PrepareForSave() {
	if c == nil {
		return
	}
	if c.Provider == "" {
		c.Provider = "openai"
	}

	c.OpenAI.APIKey = c.APIKey
	c.OpenAI.BaseURL = c.BaseURL
	c.OpenAI.Organization = c.Organization
	c.OpenAI.Project = c.Project
	c.OpenAI.Proxy = c.Proxy
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
