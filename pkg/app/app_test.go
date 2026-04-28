package app

import (
	"context"
	"testing"
)

// TestStartWorkers_DisabledSkips proves the Worker.Enabled gate in Run() is the
// only protection against starting workers with empty GitHub config: startWorkers
// itself MUST error when AppID/PrivateKeyPath are zero. If startWorkers ever
// stops returning an error here, the gate becomes load-bearing-but-untested and
// fresh checkouts could crash on first boot.
func TestStartWorkers_DisabledSkips(t *testing.T) {
	a := &App{cfg: Config{}}
	if err := a.startWorkers(context.Background()); err == nil {
		t.Fatal("startWorkers with empty GitHub config must error; the Run() gate is the only protection")
	}
}

// TestStartWorkers_EnabledMissingKey verifies that startWorkers returns an error
// when Worker.Enabled is true but no private key is configured.
// This exercises the githubapp.NewApp failure path inside startWorkers.
func TestStartWorkers_EnabledMissingKey(t *testing.T) {
	a := &App{}
	a.cfg.Worker.Enabled = true
	// GitHub.AppID and PrivateKeyPath are both zero-values;
	// githubapp.NewApp will return "AppID required" or "private key required".

	err := a.startWorkers(context.Background())
	if err == nil {
		t.Fatal("expected an error from startWorkers with missing GitHub config, got nil")
	}
}
