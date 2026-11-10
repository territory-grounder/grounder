package actuate

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	serviceProbeDial    = 3 * time.Second
	serviceProbeRequest = 6 * time.Second
)

// ProbeService checks one bound object at a tcp://, http:// or https:// address. A refused connection or, for http, a connected
// service that does not answer or returns a server error is down. An address the plane cannot reach at all is unknown,
// because that says nothing about the service.
func ProbeService(ctx context.Context, target string) ProbeResult {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return ProbeUnknown
	}
	dctx, cancel := context.WithTimeout(ctx, serviceProbeDial)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", u.Host)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return ProbeDown
		}
		return ProbeUnknown
	}
	_ = conn.Close()
	switch u.Scheme {
	case "tcp":
		return ProbeUp
	case "http", "https":
		return probeHTTP(ctx, target)
	}
	return ProbeUnknown
}

// isTLSHandshakeFailure reports a probe that could not agree TLS with the service. That is a wrong probe, not a fault.
func isTLSHandshakeFailure(err error) bool {
	var record tls.RecordHeaderError
	var alert tls.AlertError
	msg := err.Error()
	return errors.As(err, &record) || errors.As(err, &alert) || strings.Contains(msg, "tls: ") || strings.Contains(msg, "HTTP response to HTTPS client")
}

func probeHTTP(ctx context.Context, target string) ProbeResult {
	rctx, cancel := context.WithTimeout(ctx, serviceProbeRequest)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
	if err != nil {
		return ProbeUnknown
	}
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}, // #nosec G402 -- a liveness probe of a self-signed service, no secret is sent
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil || isTLSHandshakeFailure(err) {
			return ProbeUnknown
		}
		return ProbeDown
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusInternalServerError {
		return ProbeDown
	}
	return ProbeUp
}
