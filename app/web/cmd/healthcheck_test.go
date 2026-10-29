package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// runHealthcheck executes the healthcheck command with args and returns its error.
func runHealthcheck(t *testing.T, args ...string) error {
	t.Helper()
	command := New(io.Discard, io.Discard)
	command.SetArgs(append([]string{"healthcheck"}, args...))
	return command.ExecuteContext(t.Context())
}

func TestHealthcheckCommandIsRegistered(t *testing.T) {
	cleanConfigEnv(t)
	command := New(io.Discard, io.Discard)
	found, _, err := command.Find([]string{"healthcheck"})
	if err != nil || found == nil || found.Name() != "healthcheck" {
		t.Fatalf("healthcheck command is not registered: %v %+v", err, found)
	}
}

// tlsPair builds the anonymous TLS field of configuration without repeating the
// mapstructure tags at every call site.
func tlsPair(cert, key string) struct {
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
} {
	return struct {
		CertFile string `mapstructure:"cert_file"`
		KeyFile  string `mapstructure:"key_file"`
	}{CertFile: cert, KeyFile: key}
}

func TestHealthcheckTargetDerivesFromListen(t *testing.T) {
	cases := []struct {
		name  string
		c     configuration
		want  string
		valid bool
	}{{
		name:  "loopback plain",
		c:     configuration{Listen: "127.0.0.1:8080"},
		want:  "http://127.0.0.1:8080/healthz",
		valid: true,
	}, {
		name:  "all interfaces with TLS become a loopback HTTPS probe",
		c:     configuration{Listen: "0.0.0.0:8443", TLS: tlsPair("/run/secrets/intern_tls_cert", "/run/secrets/intern_tls_key")},
		want:  "https://127.0.0.1:8443/healthz",
		valid: true,
	}, {
		name:  "IPv6 wildcard also probes the loopback",
		c:     configuration{Listen: "[::]:8443", TLS: tlsPair("/run/secrets/intern_tls_cert", "/run/secrets/intern_tls_key")},
		want:  "https://127.0.0.1:8443/healthz",
		valid: true,
	}, {
		name:  "a proxy-terminated listener stays plain",
		c:     configuration{Listen: "127.0.0.1:8080", TLSTerminatedByProxy: true, TLS: tlsPair("/run/secrets/intern_tls_cert", "")},
		want:  "http://127.0.0.1:8080/healthz",
		valid: true,
	}, {
		name:  "a named bind address is kept",
		c:     configuration{Listen: "192.0.2.5:8443", TLS: tlsPair("/run/secrets/intern_tls_cert", "/run/secrets/intern_tls_key")},
		want:  "https://192.0.2.5:8443/healthz",
		valid: true,
	}, {
		name:  "a portless listener cannot be probed",
		c:     configuration{Listen: "127.0.0.1"},
		valid: false,
	}}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint, err := healthcheckTarget("", testCase.c)
			if !testCase.valid {
				if err == nil {
					t.Fatalf("derived %v, want an error", endpoint)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if endpoint.String() != testCase.want {
				t.Fatalf("derived %q, want %q", endpoint, testCase.want)
			}
		})
	}
}

func TestHealthcheckTargetAcceptsExplicitURL(t *testing.T) {
	endpoint, err := healthcheckTarget("https://localhost:8443", configuration{})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.String() != "https://localhost:8443/healthz" {
		t.Fatalf("explicit URL without a path: %q", endpoint)
	}
	kept, err := healthcheckTarget("http://127.0.0.1:8080/readyz", configuration{})
	if err != nil {
		t.Fatal(err)
	}
	if kept.Path != "/readyz" {
		t.Fatalf("an explicit path must be kept: %q", kept)
	}
	for _, invalid := range []string{"ftp://127.0.0.1:1/healthz", "https:///healthz", "https://user@127.0.0.1:8443/healthz"} {
		if _, err = healthcheckTarget(invalid, configuration{}); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestHealthcheckIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.53", "::1"} {
		if !isLoopbackHost(host) {
			t.Fatalf("%q is loopback", host)
		}
	}
	for _, host := range []string{"", "*", "0.0.0.0", "::", "192.0.2.5", "intern.example", "[::1]", "localhost", "Localhost"} {
		if isLoopbackHost(host) {
			t.Fatalf("%q is not loopback", host)
		}
	}
}

// TestHealthcheckTransportSkipsVerifyOnlyForLoopback is the locked rule: the
// probe never disables certificate verification for a target that is not this
// container's own loopback address.
func TestHealthcheckTransportSkipsVerifyOnlyForLoopback(t *testing.T) {
	loopback, err := healthcheckTarget("https://127.0.0.1:8443/healthz", configuration{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := healthcheckTransport(loopback).(*http.Transport)
	if !ok {
		t.Fatal("a loopback probe must use an owned transport")
	}
	if config := transport.TLSClientConfig; config == nil || !config.InsecureSkipVerify {
		t.Fatalf("a loopback probe must accept the deployment's own certificate: %+v", config)
	}
	for _, target := range []string{"https://192.0.2.5:8443/healthz", "https://intern.example:8443/healthz", "http://127.0.0.1:8080/healthz"} {
		endpoint, parseErr := healthcheckTarget(target, configuration{})
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if got := healthcheckTransport(endpoint); got != http.DefaultTransport {
			t.Fatalf("%s did not keep the default verifying transport", target)
		}
	}
}

func TestHealthcheckSucceedsAgainstRealListeners(t *testing.T) {
	cleanConfigEnv(t)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	}))
	defer plain.Close()
	t.Setenv("INTERN_LISTEN", strings.TrimPrefix(plain.URL, "http://"))
	if err := runHealthcheck(t); err != nil {
		t.Fatalf("plain loopback probe: %v", err)
	}
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer secure.Close()
	// A self-signed certificate on the loopback interface is the container's own
	// deployment certificate: the probe must still succeed.
	if err := runHealthcheck(t, "--url", secure.URL); err != nil {
		t.Fatalf("TLS loopback probe: %v", err)
	}
}

func TestHealthcheckRefuses(t *testing.T) {
	cleanConfigEnv(t)
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{{
		name: "unavailable", status: http.StatusServiceUnavailable, body: `{"status":"ok"}`, want: "answered 503",
	}, {
		name: "not the health object", status: http.StatusOK, body: "ready", want: "not the health status object",
	}, {
		name: "wrong status field", status: http.StatusOK, body: `{"status":"degraded"}`, want: `status "degraded"`,
	}, {
		name: "oversized body", status: http.StatusOK,
		body: `{"status":"ok","padding":"` + strings.Repeat("x", healthcheckMaxBodyBytes) + `"}`,
		want: "more than 16384 bytes",
	}}
	for _, testCase := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(testCase.status)
			_, _ = w.Write([]byte(testCase.body))
		}))
		err := runHealthcheck(t, "--url", server.URL, "--timeout", "5s")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: err = %v, want it to contain %q", testCase.name, err, testCase.want)
		}
		if strings.Contains(err.Error(), "\n") {
			t.Fatalf("%s: the failure message must stay one line: %q", testCase.name, err)
		}
	}
}

func TestHealthcheckTimeoutAndUnreachable(t *testing.T) {
	cleanConfigEnv(t)
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	start := time.Now()
	err := runHealthcheck(t, "--url", slow.URL, "--timeout", "50ms")
	if err == nil {
		t.Fatal("a stalled health endpoint answered")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the probe outlived its timeout: %s", elapsed)
	}
	// An address nothing listens on is a failure, never a silent success.
	if err = runHealthcheck(t, "--url", "http://127.0.0.1:1/healthz", "--timeout", "2s"); err == nil {
		t.Fatal("an unreachable listener passed")
	}
}

func TestHealthcheckRejectsBadArgumentsAndConfiguration(t *testing.T) {
	cleanConfigEnv(t)
	if err := runHealthcheck(t, "--url", "http://127.0.0.1:1/healthz", "--timeout", "0s"); err == nil ||
		!strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("zero timeout: %v", err)
	}
	if err := runHealthcheck(t, "--url", "://nope"); err == nil {
		t.Fatal("an unparseable URL passed")
	}
	t.Setenv("INTERN_LISTEN", "not-an-address")
	if err := runHealthcheck(t); err == nil {
		t.Fatal("an invalid listener configuration passed")
	}
	if err := runHealthcheck(t, "extra-argument"); err == nil {
		t.Fatal("healthcheck accepted a positional argument")
	}
}
