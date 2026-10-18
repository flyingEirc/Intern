package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitListening(t *testing.T, address string, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if conn != nil {
			conn.Close()
		}
		if (err == nil) == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("listening=%v not reached: %v", want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestServerTimeoutsAndTLSFloor(t *testing.T) {
	srv := New(Config{Listen: "127.0.0.1:8080"}, http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 30*time.Second || srv.WriteTimeout != 60*time.Second || srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 1<<20 {
		t.Fatalf("server timeouts: %+v", srv)
	}
	if srv.TLSConfig == nil || srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("TLS floor is not TLS 1.2")
	}
}

func TestGracefulShutdownDrainsInFlightRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	engine := testEngine(t, readinessFunc(func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), io.Discard, nil)
	config := Config{Listen: freeAddress(t), ShutdownTimeout: 5 * time.Second}
	srv := New(config, engine)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, config) }()
	waitListening(t, config.Listen, true)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	validation := newContractClient(t, engine)
	validation.handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		forwardRequest(t, client, config.Listen, w, req)
	})
	requestDone := make(chan error, 1)
	go func() {
		// The request survives cancellation of the server's owner context.
		requestDone <- contractRequest(t.Context(), validation, "http://"+config.Listen+"/readyz")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter readiness handler")
	}
	cancel()
	waitListening(t, config.Listen, false)
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before request drained: %v", err)
	default:
	}
	close(release)
	awaitResult(t, requestDone, "in-flight request")
	awaitResult(t, done, "server shutdown")
}

func contractRequest(ctx context.Context, client *contractClient, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.roundTrip(req)
	if err != nil {
		return err
	}
	if response.Code != http.StatusOK {
		return errors.New("in-flight request was not completed")
	}
	return nil
}

func awaitResult(t *testing.T, result <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", operation)
	}
}

func forwardRequest(t *testing.T, client *http.Client, address string, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	request := req.Clone(req.Context())
	request.URL.Scheme = "http"
	request.URL.Host = address
	request.RequestURI = ""
	response, err := client.Do(request)
	if err != nil {
		t.Error(err)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		w.Header()[key] = values
	}
	w.WriteHeader(response.StatusCode)
	if _, err = io.Copy(w, response.Body); err != nil {
		t.Error(err)
	}
}

func TestStartupErrors(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := Config{Listen: listener.Addr().String(), ShutdownTimeout: time.Second}
	if err = Run(t.Context(), New(config, http.NotFoundHandler()), config); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("bind error = %v", err)
	}
	config.Listen = freeAddress(t)
	config.CertFile = "/nonexistent/cert.pem"
	config.KeyFile = "/nonexistent/key.pem"
	if err = Run(t.Context(), New(config, http.NotFoundHandler()), config); err == nil {
		t.Fatal("TLS startup error ignored")
	}
}
