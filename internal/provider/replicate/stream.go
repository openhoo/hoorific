package replicate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hoorific/internal/core"
)

// PredictionURLs is the URLs object returned by Replicate predictions.
type PredictionURLs struct {
	Web    string `json:"web,omitempty"`
	Get    string `json:"get,omitempty"`
	Stream string `json:"stream,omitempty"`
	Cancel string `json:"cancel,omitempty"`
}

// OpenStream follows only the stream URL returned by a prediction. Replicate's
// streaming guide (https://replicate.com/docs/topics/predictions/streaming)
// documents this as an SSE URL; there is no gateway-invented stream route.
func (c *Connector) OpenStream(ctx context.Context, conn core.Connection, streamURL string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := c.trustedReturnedURL(conn, streamURL)
	if err != nil {
		return nil, err
	}
	client, creds := c.DiscoveryClient(), c.CredentialSource()
	if client == nil || creds == nil {
		return nil, invalid("Replicate streaming requires explicitly injected HTTP client and credentials")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	lease, err := creds.Lease(ctx, conn)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, invalid("Replicate credential lease is unavailable")
	}
	authErr := lease.Authorize(ctx, req)
	closeErr := core.CloseCredentialLease(lease)
	if authErr != nil {
		return nil, authErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Never follow a returned URL redirect: a redirect could move credentials to
	// an untrusted origin. A shallow client copy keeps caller transport settings.
	safe := *client
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safe.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, core.GatewayError{Code: "upstream_outcome_unknown", HTTPStatus: resp.StatusCode, Message: "Replicate stream request failed", Origin: "upstream", Retryable: resp.StatusCode >= 500}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		resp.Body.Close()
		return nil, fmt.Errorf("Replicate stream returned non-SSE content type")
	}
	return resp.Body, nil
}

func (c *Connector) trustedReturnedURL(conn core.Connection, raw string) (*url.URL, error) {
	validated, err := ValidateStreamURL(conn, raw)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(validated)
	if err != nil {
		return nil, invalid("Replicate stream URL is invalid")
	}
	return u, nil
}
