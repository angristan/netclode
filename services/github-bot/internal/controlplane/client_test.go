package controlplane

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
)

func TestParseSdkType(t *testing.T) {
	tests := []struct {
		input string
		want  pb.SdkType
	}{
		{"claude", pb.SdkType_SDK_TYPE_CLAUDE},
		{"Claude", pb.SdkType_SDK_TYPE_CLAUDE},
		{"CLAUDE", pb.SdkType_SDK_TYPE_CLAUDE},
		{"opencode", pb.SdkType_SDK_TYPE_OPENCODE},
		{"copilot", pb.SdkType_SDK_TYPE_COPILOT},
		{"codex", pb.SdkType_SDK_TYPE_CODEX},
		{"unknown", pb.SdkType_SDK_TYPE_CLAUDE},
		{"", pb.SdkType_SDK_TYPE_CLAUDE},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ParseSdkType(tt.input); got != tt.want {
				t.Errorf("ParseSdkType(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBearerFileRoundTripper(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("projected-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	transport := &bearerFileRoundTripper{
		tokenPath: tokenPath,
		next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer projected-token" {
				t.Fatalf("Authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		}),
	}
	req, err := http.NewRequest(http.MethodPost, "http://control-plane/rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("transport mutated the caller's request")
	}
}
