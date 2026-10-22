package google

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application"
	"github.com/golang-jwt/jwt/v5"
)

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestExchangePreservesContextCause(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		kind   application.ErrorKind
		reason string
	}{
		{context.Canceled, application.Canceled, "request_cancelled"},
		{context.DeadlineExceeded, application.DeadlineExceeded, "deadline_exceeded"},
		{errors.New("private transport details"), application.Unavailable, "login_unavailable"},
	} {
		o := NewWithEndpoints(t.Context(), "client", "secret", "https://callback.test", "version", "https://issuer.test", "https://auth.test", "https://token.test", "https://keys.test")
		o.client.Transport = failingTransport{fmt.Errorf("transport: %w", tc.cause)}
		_, err := o.Exchange(t.Context(), "code", "verifier", nil)
		failure := application.Failure(err)
		if failure.Kind != tc.kind || failure.Reason != tc.reason || !errors.Is(err, tc.cause) {
			t.Errorf("%v: got %#v", tc.cause, failure)
		}
	}
}

func TestJWKSDeadlineSurvivesOIDCTextWrapping(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"access_token": "test", "token_type": "Bearer", "id_token": token})
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	token, err = jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": server.URL, "aud": "client", "sub": "account", "exp": time.Now().Add(time.Hour).Unix()}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	o := NewWithEndpoints(t.Context(), "client", "secret", "https://callback.test", "version", server.URL, server.URL+"/authorize", server.URL+"/token", server.URL+"/keys")
	o.client.Timeout = 100 * time.Millisecond
	_, err = o.Exchange(t.Context(), "code", "verifier", nil)
	failure := application.Failure(err)
	if failure.Kind != application.DeadlineExceeded || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("JWKS timeout lost: %#v", failure)
	}
	if t.Context().Err() != nil {
		t.Fatal("test must exercise a child timeout with a live caller")
	}
}
