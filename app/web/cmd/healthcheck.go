package cmd

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// healthcheckMaxBodyBytes bounds the probe response. /healthz answers one small
// status object, so anything larger is not the health endpoint and must not be
// read into memory by a container healthcheck that runs every few seconds.
const healthcheckMaxBodyBytes = 1 << 14

// healthcheckPath is the endpoint the deployment healthcheck probes. It never
// touches the session store, so it answers as soon as the listener is up.
const healthcheckPath = "/healthz"

// healthcheckCommand is the container healthcheck entry point: the image ships
// no curl, so the binary probes its own listener. Exit status is the only output
// a container runtime consumes.
func (a *application) healthcheckCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe this instance's own /healthz and exit non-zero when it does not answer",
		Long: "Probe the local health endpoint with the same INTERN_LISTEN and TLS settings as serve. " +
			"Certificate verification is skipped only for a loopback target, which is a container probing " +
			"itself with its own deployment certificate; any other target is verified normally. " +
			"The probe never sends credentials and reads at most 16 KiB.",
		Args: cobra.NoArgs,
		RunE: a.healthcheck,
	}
	command.Flags().String("url", "", "Health URL to probe (default: derived from --listen and the TLS settings)")
	command.Flags().Duration("timeout", 3*time.Second, "Deadline for the whole probe (must be positive)")
	return command
}

func (a *application) healthcheck(command *cobra.Command, _ []string) error {
	c, err := a.config.load()
	if err != nil {
		return err
	}
	raw, err := command.Flags().GetString("url")
	if err != nil {
		return err
	}
	timeout, err := command.Flags().GetDuration("timeout")
	if err != nil {
		return err
	}
	target, err := healthcheckTarget(raw, c)
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return errors.New("healthcheck timeout must be positive")
	}
	return probeHealth(command.Context(), target, timeout)
}

// probeHealth issues one GET and accepts only a 200 whose body is the health
// status object. A transport error, a wrong status, an unreadable body or an
// over-sized body are all failures: an unhealthy container must not look alive.
func probeHealth(ctx context.Context, endpoint *url.URL, timeout time.Duration) error {
	client := &http.Client{
		Timeout:   timeout,
		Transport: healthcheckTransport(endpoint),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("healthcheck: cannot reach %s: %w", redactedHost(endpoint), unwrapURLError(err))
	}
	defer func() { _ = response.Body.Close() }() //nolint:errcheck // best-effort close; the status and body already decide the outcome
	body, err := io.ReadAll(io.LimitReader(response.Body, healthcheckMaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("healthcheck: cannot read the response from %s: %w", redactedHost(endpoint), err)
	}
	if len(body) > healthcheckMaxBodyBytes {
		return fmt.Errorf("healthcheck: %s answered more than %d bytes, which is not the health endpoint",
			redactedHost(endpoint), healthcheckMaxBodyBytes)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s answered %d", redactedHost(endpoint), response.StatusCode)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err = json.Unmarshal(body, &status); err != nil {
		return fmt.Errorf("healthcheck: %s answered a body that is not the health status object: %w",
			redactedHost(endpoint), err)
	}
	if status.Status != "ok" {
		return fmt.Errorf("healthcheck: %s answered status %q", redactedHost(endpoint), status.Status)
	}
	return nil
}

// healthcheckTransport verifies the certificate for every target except a
// loopback one. The loopback exception is what makes an in-container healthcheck
// possible with a deployment certificate whose name is the host's own: the probe
// never leaves the container's network namespace, so it can authenticate nothing
// but the process it is meant to keep alive. Any other target keeps the shared
// default transport, with system roots and certificate verification intact.
func healthcheckTransport(endpoint *url.URL) http.RoundTripper {
	if endpoint.Scheme != "https" || !isLoopbackHost(endpoint.Hostname()) {
		return http.DefaultTransport
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, // #nosec G402 -- loopback-only: a container probing its own listener.
	}
	return transport
}

// healthcheckTarget resolves the URL to probe. An explicit --url wins; otherwise
// the listener configuration decides the scheme and the port, and an
// unspecified bind address is probed through the loopback interface.
func healthcheckTarget(explicit string, c configuration) (*url.URL, error) {
	if explicit == "" {
		return healthcheckFromListen(c)
	}
	endpoint, err := url.Parse(explicit)
	if err != nil {
		return nil, fmt.Errorf("invalid --url: %w", err)
	}
	if err = validHealthURL(endpoint); err != nil {
		return nil, err
	}
	if endpoint.Path == "" {
		endpoint.Path = healthcheckPath
	}
	return endpoint, nil
}

// healthcheckFromListen derives the probe URL from the listener configuration:
// the scheme follows the TLS settings, and an unspecified bind address is probed
// through the loopback interface, because the probe runs in the same network
// namespace as the listener it checks.
func healthcheckFromListen(c configuration) (*url.URL, error) {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return nil, fmt.Errorf("cannot derive a health URL from listen %q: %w", c.Listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || strings.HasPrefix(host, "*") {
		host = "127.0.0.1"
	}
	scheme := "http"
	if c.TLS.CertFile != "" && !c.TLSTerminatedByProxy {
		scheme = "https"
	}
	return url.Parse(scheme + "://" + net.JoinHostPort(host, port) + healthcheckPath)
}

func validHealthURL(endpoint *url.URL) error {
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return fmt.Errorf("health URL scheme %q is not http or https", endpoint.Scheme)
	}
	if endpoint.Host == "" {
		return errors.New("health URL must carry a host and port")
	}
	if endpoint.User != nil {
		return errors.New("the health URL never carries credentials")
	}
	return nil
}

// isLoopbackHost accepts only literal loopback addresses. A name such as
// "localhost" is resolved by the host's resolver configuration and could point
// elsewhere, so it never earns the certificate-verification exception.
func isLoopbackHost(host string) bool {
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// redactedHost reports scheme and host only. A probe URL's path or query never
// needs to reach a log or an exit message.
func redactedHost(endpoint *url.URL) string {
	return endpoint.Scheme + "://" + endpoint.Host
}

// unwrapURLError keeps the real cause of a failed request; net/http hides it
// behind one generic message.
func unwrapURLError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}
