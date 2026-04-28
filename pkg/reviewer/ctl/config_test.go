package ctl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		cmd     string
		wantErr bool
	}{
		{"empty key", Config{URL: "http://x"}, "review", true},
		{"empty url", Config{Key: "k"}, "review", true},
		{"valid review", Config{Key: "k", URL: "http://x"}, "review", false},
		{"valid upload", Config{Key: "k", URL: "http://x"}, "upload", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate(tt.cmd)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
