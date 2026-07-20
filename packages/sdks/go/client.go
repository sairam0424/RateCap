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

// Allow is tier-1-only: it never establishes a tier-2 concurrency
// reservation, since it has no matching Release call to free one. Skipping
// tier 2 here (rather than leaking a slot per call) is what keeps Allow's
// original fire-and-forget contract intact now that tier 2 exists.
func (c *Client) Allow(ctx context.Context, key string) (allowed bool, retryAfterMs int64, err error) {
	reqURL := c.sidecarAddr + "/check?key=" + url.QueryEscape(key) + "&skip_reservations=true"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, 0, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, 0, nil
	}

	retryAfterMs = 0
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return false, retryAfterMs, nil
}

type reservation struct {
	key string
	tok string
}

type Ticket struct {
	Allowed      bool
	RetryAfterMs int64

	client       *Client
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ratecap: release failed with status %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) Acquire(ctx context.Context, key string) (*Ticket, error) {
	reqURL := c.sidecarAddr + "/check?key=" + url.QueryEscape(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reservations []reservation
	for i := 0; ; i++ {
		tok := resp.Header.Get(fmt.Sprintf("Concurrency-Token-%d", i))
		if tok == "" {
			break
		}
		key := resp.Header.Get(fmt.Sprintf("Concurrency-Key-%d", i))
		reservations = append(reservations, reservation{key: key, tok: tok})
	}

	if resp.StatusCode == http.StatusOK {
		return &Ticket{Allowed: true, client: c, reservations: reservations}, nil
	}

	var retryAfterMs int64
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return &Ticket{Allowed: false, RetryAfterMs: retryAfterMs, client: c, reservations: reservations}, nil
}
