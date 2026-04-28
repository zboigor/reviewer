package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newTestPEM(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return pemBytes, key
}

func TestAppJWTContainsRequiredClaims(t *testing.T) {
	pemBytes, key := newTestPEM(t)

	a, err := NewApp(Config{AppID: 12345, PrivateKeyPEM: pemBytes})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	raw, err := a.appJWT(time.Now())
	if err != nil {
		t.Fatalf("appJWT: %v", err)
	}

	parsed, err := jwt.Parse(raw, func(_ *jwt.Token) (any, error) { return &key.PublicKey, nil })
	if err != nil || !parsed.Valid {
		t.Fatalf("verify: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["iss"].(string) != "12345" {
		t.Errorf("iss = %v, want 12345", claims["iss"])
	}
	if iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64)); exp-iat > 600 {
		t.Errorf("exp-iat = %d, want ≤ 600", exp-iat)
	}
}

func TestNewAppValidatesConfig(t *testing.T) {
	pemBytes, _ := newTestPEM(t)

	tests := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{
			name:    "missing AppID",
			cfg:     Config{PrivateKeyPEM: pemBytes},
			wantSub: "AppID",
		},
		{
			name:    "missing private key (no PEM, no path)",
			cfg:     Config{AppID: 1},
			wantSub: "private key",
		},
		{
			name:    "bad PEM bytes",
			cfg:     Config{AppID: 1, PrivateKeyPEM: []byte("not a pem")},
			wantSub: "parse private key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewApp(tc.cfg)
			if err == nil {
				t.Fatalf("NewApp: want error, got nil (app=%v)", a)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestInstallationTokenCachesAcrossCalls(t *testing.T) {
	pemBytes, _ := newTestPEM(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, `{"token":"tok-1","expires_at":%q}`, exp)
	}))
	defer srv.Close()

	a, err := NewApp(Config{AppID: 1, PrivateKeyPEM: pemBytes, APIBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	for i := 0; i < 2; i++ {
		tok, err := a.InstallationToken(context.Background(), 42)
		if err != nil {
			t.Fatalf("InstallationToken #%d: %v", i, err)
		}
		if tok != "tok-1" {
			t.Errorf("call %d token = %q, want tok-1", i, tok)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits = %d, want 1 (cache miss on second call)", got)
	}
}

func TestInstallationTokenDoesNotCacheOnError(t *testing.T) {
	pemBytes, _ := newTestPEM(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	a, err := NewApp(Config{AppID: 1, PrivateKeyPEM: pemBytes, APIBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := a.InstallationToken(context.Background(), 7); err == nil {
			t.Errorf("call %d: want error, got nil", i)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server hits = %d, want 2 (no negative caching)", got)
	}
}
