package ctl

import "errors"

// Config holds all CLI flags and environment variables for reviewctl.
type Config struct {
	Key       string
	URL       string
	PublicURL string // browser-facing base URL for links in review output; falls back to URL
	Model     string
	Dir       string
	Verbose   bool

	// PR metadata (populated from environment).
	SourceBranch string
	TargetBranch string
	Commit       string
	Author       string
	MRTitle      string
	ExternalID   string

	// Claude session for --resume (reuses prompt cache).
	SessionID       string
	ContinueSession bool // use --continue instead of --resume
}

// Validate checks that required fields are set for the given subcommand.
func (c *Config) Validate(cmd string) error {
	if c.Key == "" {
		return errors.New("--key / $PROJECT_KEY is required")
	}
	if c.URL == "" {
		return errors.New("--url / $REVIEWSRV_URL is required")
	}
	return nil
}

// PublicBaseURL returns the browser-facing base URL for links shown to users,
// falling back to URL when PublicURL is not set.
func (c *Config) PublicBaseURL() string {
	if c.PublicURL != "" {
		return c.PublicURL
	}
	return c.URL
}
