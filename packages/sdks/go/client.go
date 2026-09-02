package ratecap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type Client struct {
	sidecarAddr string
	httpClient  *http.Client
}

func NewClient(sidecarAddr string) *Client {
	return &Client{sidecarAddr: sidecarAddr, httpClient: http.DefaultClient}
}

type Priority int

const (
	Sheddable Priority = iota
	Critical
)

func (p Priority) headerValue() string {
	if p == Critical {
		return "critical"
	}
	return "sheddable"
}

type checkOptions struct {
	cost        int
	hasCost     bool
	priority    Priority
	hasPriority bool
	route       string
	hasRoute    bool
}

type CheckOption func(*checkOptions)

func WithCost(cost int) CheckOption {
	return func(o *checkOptions) { o.cost, o.hasCost = cost, true }
}

func WithPriority(p Priority) CheckOption {
	return func(o *checkOptions) { o.priority, o.hasPriority = p, true }
}

func WithRoute(route string) CheckOption {
	return func(o *checkOptions) { o.route, o.hasRoute = route, true }
}

func applyCheckOptions(opts []CheckOption) checkOptions {
	var o checkOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func (o checkOptions) applyToRequest(req *http.Request, query url.Values) {
	if o.hasCost {
		query.Set("cost", strconv.Itoa(o.cost))
	}
	if o.hasPriority {
		req.Header.Set("x-ratecap-priority", o.priority.headerValue())
	}
	if o.hasRoute {
		req.Header.Set("x-ratecap-route", o.route)
	}
}

// Allow is tier-1-only: it never establishes a tier-2 concurrency
// reservation, since it has no matching Release call to free one. Skipping
// tier 2 here (rather than leaking a slot per call) is what keeps Allow's
// original fire-and-forget contract intact now that tier 2 exists.
func (c *Client) Allow(ctx context.Context, key string, opts ...CheckOption) (allowed bool, retryAfterMs int64, rateLimitReset int64, rateLimitLimit int64, rateLimitRemaining int64, err error) {
	query := url.Values{"key": {key}, "skip_reservations": {"true"}}
	options := applyCheckOptions(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarAddr+"/check", nil)
	if err != nil {
		return false, 0, 0, 0, 0, err
	}
	options.applyToRequest(req, query)
	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, 0, 0, 0, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is never read further here; a Close error carries no new information beyond the already-inspected status code/headers

	if resp.StatusCode == http.StatusOK {
		return true, 0, 0, 0, 0, nil
	}

	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("RateLimit-Reset"); v != "" {
		rateLimitReset, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("RateLimit-Limit"); v != "" {
		rateLimitLimit, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("RateLimit-Remaining"); v != "" {
		rateLimitRemaining, _ = strconv.ParseInt(v, 10, 64)
	}
	return false, retryAfterMs, rateLimitReset, rateLimitLimit, rateLimitRemaining, nil
}

type reservation struct {
	key string
	tok string
}

type Ticket struct {
	Allowed            bool
	RetryAfterMs       int64
	RateLimitReset     int64
	RateLimitLimit     int64
	RateLimitRemaining int64

	client       *Client
	key          string
	reservations []reservation
}

// Release is best-effort with no retry, releasing every reservation the
// Ticket holds (a single Acquire can produce more than one — e.g. tier 2's
// per-user slot and tier 3's global slot): a non-nil error is a signal for
// the caller to log, not something to retry or otherwise act on — the
// design spec's Lua reaper (max_request_duration_ms) is the actual
// mechanism that frees a slot after a lost or failed Release, not a
// fallback for one, for every reservation individually.
func (t *Ticket) Release(ctx context.Context) error {
	var errs []error
	for _, r := range t.reservations {
		if err := t.releaseOne(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *Ticket) releaseOne(ctx context.Context, r reservation) error {
	reqURL := t.client.sidecarAddr + "/release"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-RateCap-Concurrency-Key", r.key)
	req.Header.Set("X-RateCap-Concurrency-Token", r.tok)

	resp, err := t.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is never read further here; a Close error carries no new information beyond the already-inspected status code/headers

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ratecap: release failed with status %d", resp.StatusCode)
	}

	return nil
}

// Refund reserves-upfront-refund-unused: a caller that estimated a higher
// cost than it actually used (e.g. an LLM call whose real token count came
// in under the max_tokens estimate) calls this with the difference. It is
// independent of Release — a Ticket from a plain Acquire with no reservations
// can still be refunded, and Refund never touches t.reservations.
func (t *Ticket) Refund(ctx context.Context, refundAmount int) error {
	reqURL := t.client.sidecarAddr + "/release"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-RateCap-Refund-Key", t.key)
	req.Header.Set("X-RateCap-Refund-Amount", strconv.Itoa(refundAmount))

	resp, err := t.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is never read further here; a Close error carries no new information beyond the already-inspected status code/headers

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ratecap: refund failed with status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Acquire(ctx context.Context, key string, opts ...CheckOption) (*Ticket, error) {
	query := url.Values{"key": {key}}
	options := applyCheckOptions(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sidecarAddr+"/check", nil)
	if err != nil {
		return nil, err
	}
	options.applyToRequest(req, query)
	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body is never read further here; a Close error carries no new information beyond the already-inspected status code/headers

	var reservations []reservation
	for i := 0; ; i++ {
		tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i))
		if tok == "" {
			break
		}
		resKey := resp.Header.Get(fmt.Sprintf("Concurrency-Key-%d", i))
		reservations = append(reservations, reservation{key: resKey, tok: tok})
	}

	if resp.StatusCode == http.StatusOK {
		return &Ticket{Allowed: true, client: c, key: key, reservations: reservations}, nil
	}

	var retryAfterMs, rateLimitReset, rateLimitLimit, rateLimitRemaining int64
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("RateLimit-Reset"); v != "" {
		rateLimitReset, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("RateLimit-Limit"); v != "" {
		rateLimitLimit, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := resp.Header.Get("RateLimit-Remaining"); v != "" {
		rateLimitRemaining, _ = strconv.ParseInt(v, 10, 64)
	}
	return &Ticket{Allowed: false, RetryAfterMs: retryAfterMs, RateLimitReset: rateLimitReset, RateLimitLimit: rateLimitLimit, RateLimitRemaining: rateLimitRemaining, client: c, key: key, reservations: reservations}, nil
}
