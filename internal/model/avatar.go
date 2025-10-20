package model

// Avatar represents a user's avatar, either as a remote URL (v3) or inline bytes (v4)
type Avatar struct {
	Username    string `json:"username"`
	URL         string `json:"url,omitempty"`
	ContentType string `json:"-"`
	Data        []byte `json:"-"`
}
