package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"time"
)

type Config struct {
	Listen          string
	CertFile        string
	KeyFile         string
	ShutdownTimeout time.Duration
}

func New(config Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: config.Listen, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 1 << 20,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// Run drains in-flight requests on cancellation before returning to close the store.
func Run(ctx context.Context, srv *http.Server, config Config) error {
	done := make(chan error, 1)
	go func() {
		if config.CertFile != "" {
			done <- srv.ListenAndServeTLS(config.CertFile, config.KeyFile)
			return
		}
		done <- srv.ListenAndServe()
	}()
	select {
	case err := <-done:
		return serverError(err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return errors.Join(err, srv.Close(), serverError(<-done))
		}
		return serverError(<-done)
	}
}

func serverError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
