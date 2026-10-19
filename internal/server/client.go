package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/timfewi/ichnora/internal/contract"
)

// Client calls the service over its Unix socket. It never falls back to
// opening the database or to a network service.
type Client struct {
	Socket string
	// Role optionally narrows the socket's roles (comma-separated).
	Role    string
	Timeout time.Duration
}

const maxResponse = 2 << 20

// Call sends one operation and returns the raw, already bounded response.
func (c Client) Call(ctx context.Context, op string, req any) ([]byte, *contract.Error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, contract.Errf(contract.CodeInvalid, "request cannot be encoded")
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	unavailable := contract.Errf(contract.CodeUnavailable, "the Ichnora service socket is unavailable; no memory was read or written")
	if c.Socket == "" {
		return nil, unavailable
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		},
		DisableKeepAlives: true,
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ichnora/v1/"+op, bytes.NewReader(body))
	if err != nil {
		return nil, contract.Errf(contract.CodeInvalid, "invalid operation")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.Role != "" {
		httpReq.Header.Set(RoleHeader, c.Role)
	}
	resp, err := (&http.Client{Transport: transport}).Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, contract.Errf(contract.CodeUnavailable, "the Ichnora service did not answer in time; the outcome is unknown")
		}
		return nil, unavailable
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, unavailable
	}
	var env contract.Response
	if json.Unmarshal(out, &env) != nil {
		return nil, contract.Errf(contract.CodeInternal, "the service returned an invalid response")
	}
	if !env.OK {
		if env.Error == nil {
			env.Error = contract.Errf(contract.CodeInternal, "unknown service error")
		}
		return out, env.Error
	}
	return out, nil
}
