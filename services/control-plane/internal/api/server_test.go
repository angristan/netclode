package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeWorkloadVerifier struct {
	err            error
	token          string
	audience       string
	serviceAccount string
}

func (f *fakeWorkloadVerifier) VerifyWorkloadToken(_ context.Context, token, audience, serviceAccount string) (string, error) {
	f.token = token
	f.audience = audience
	f.serviceAccount = serviceAccount
	return "test-pod", f.err
}

func TestRequireWorkloadAuth(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		verifyErr  error
		wantStatus int
		wantNext   bool
	}{
		{name: "missing token", wantStatus: http.StatusUnauthorized},
		{name: "malformed token", header: "Basic abc", wantStatus: http.StatusUnauthorized},
		{name: "rejected identity", header: "Bearer abc", verifyErr: errors.New("wrong service account"), wantStatus: http.StatusUnauthorized},
		{name: "valid identity", header: "Bearer abc", wantStatus: http.StatusNoContent, wantNext: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := &fakeWorkloadVerifier{err: tt.verifyErr}
			nextCalled := false
			handler := requireWorkloadAuth(verifier, "netclode-client", "github-bot", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodPost, "/rpc", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)

			if resp.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.Code, tt.wantStatus)
			}
			if nextCalled != tt.wantNext {
				t.Fatalf("next called = %v, want %v", nextCalled, tt.wantNext)
			}
			if tt.wantNext {
				if verifier.token != "abc" || verifier.audience != "netclode-client" || verifier.serviceAccount != "github-bot" {
					t.Fatalf("unexpected verification input: %#v", verifier)
				}
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	if token, ok := bearerToken("Bearer abc"); !ok || token != "abc" {
		t.Fatalf("valid bearer token rejected: token=%q ok=%v", token, ok)
	}
	for _, value := range []string{"", "Bearer", "Bearer ", "Bearer a b", "Basic abc"} {
		if _, ok := bearerToken(value); ok {
			t.Fatalf("invalid bearer value accepted: %q", value)
		}
	}
}
