package app

import (
	"errors"
	"testing"

	"hoorific/internal/core"
)

func TestJoinConnectionURLPreservesPrefixEscapedResourceAndQuery(t *testing.T) {
	tests := []struct {
		name, base, path, want string
	}{
		{
			name: "https prefix and escaped resource",
			base: "https://api.example.test/root/",
			path: "/v1/jobs/a%2Fb/cancel?wait=5",
			want: "https://api.example.test/root/v1/jobs/a%2Fb/cancel?wait=5",
		},
		{
			name: "http fixture prefix",
			base: "http://127.0.0.1:43123/root",
			path: "v1/jobs/job-1/cancel",
			want: "http://127.0.0.1:43123/root/v1/jobs/job-1/cancel",
		},
		{
			name: "escaped base prefix",
			base: "https://api.example.test/root%2Ftenant",
			path: "v1/jobs/job-1",
			want: "https://api.example.test/root%2Ftenant/v1/jobs/job-1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := joinConnectionURL(test.base, test.path)
			if err != nil {
				t.Fatalf("joinConnectionURL() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("joinConnectionURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestJoinConnectionURLRejectsUnsafeEndpointAuthorityAndTraversal(t *testing.T) {
	for _, path := range []string{
		"//attacker.example/v1/jobs/job-1",
		"https://attacker.example/v1/jobs/job-1",
		"v1/jobs/../other",
		"v1/jobs/%2e%2e/other",
		"v1/./jobs/job-1",
	} {
		t.Run(path, func(t *testing.T) {
			_, err := joinConnectionURL("https://api.example.test/root", path)
			if err == nil {
				t.Fatal("joinConnectionURL() unexpectedly accepted unsafe endpoint")
			}
			var gatewayErr core.GatewayError
			if !errors.As(err, &gatewayErr) || gatewayErr.Code != "unsupported_operation" || gatewayErr.HTTPStatus != 400 {
				t.Fatalf("joinConnectionURL() error = %v, want unsupported_operation/400", err)
			}
		})
	}
}
