package githubapp

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Config struct {
	AppID          int64
	PrivateKeyPEM  []byte // takes precedence over PrivateKeyPath
	PrivateKeyPath string
	APIBaseURL     string // default https://api.github.com
}

type App struct {
	cfg     Config
	privKey *rsa.PrivateKey

	mu     sync.Mutex
	tokens map[int64]instToken // installationID -> token
	httpC  *http.Client
}

type instToken struct {
	token   string
	expires time.Time
}

func NewApp(cfg Config) (*App, error) {
	if cfg.AppID == 0 {
		return nil, errors.New("AppID required")
	}
	pem := cfg.PrivateKeyPEM
	if len(pem) == 0 && cfg.PrivateKeyPath != "" {
		b, err := os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		pem = b
	}
	if len(pem) == 0 {
		return nil, errors.New("private key required")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.github.com"
	}
	return &App{
		cfg:     cfg,
		privKey: key,
		tokens:  make(map[int64]instToken),
		httpC:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// appJWT signs a 9-minute App-level JWT (max 10 min per GitHub spec).
func (a *App) appJWT(now time.Time) (string, error) {
	claims := jwt.MapClaims{
		"iat": now.Add(-30 * time.Second).Unix(), // clock skew
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.cfg.AppID, 10),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.privKey)
}

// InstallationToken returns a cached or freshly minted token for the install.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	if t, ok := a.tokens[installationID]; ok && time.Until(t.expires) > 5*time.Minute {
		a.mu.Unlock()
		return t.token, nil
	}
	a.mu.Unlock()

	appJWT, err := a.appJWT(time.Now())
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.cfg.APIBaseURL, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.httpC.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("mint token HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}

	a.mu.Lock()
	a.tokens[installationID] = instToken{token: out.Token, expires: out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}
