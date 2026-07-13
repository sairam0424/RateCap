package ratecap

import (
	"context"
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

func (c *Client) Allow(ctx context.Context, key string) (allowed bool, retryAfterMs int64, err error) {
	reqURL := c.sidecarAddr + "/check?key=" + url.QueryEscape(key)

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

type Ticket struct {
	Allowed      bool
	RetryAfterMs int64

	client *Client
	key    string
	tok    string
}

func (t *Ticket) Release(ctx context.Context) error {
	if t.tok == "" {
		return nil
	}

	params := url.Values{}
	params.Set("key", t.key)
	params.Set("token", t.tok)
	reqURL := t.client.sidecarAddr + "/release?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := t.client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

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

	concurrencyTok := resp.Header.Get("Concurrency-Token")

	if resp.StatusCode == http.StatusOK {
		return &Ticket{Allowed: true, client: c, key: key, tok: concurrencyTok}, nil
	}

	var retryAfterMs int64
	if v := resp.Header.Get("Retry-After-Ms"); v != "" {
		retryAfterMs, _ = strconv.ParseInt(v, 10, 64)
	}
	return &Ticket{Allowed: false, RetryAfterMs: retryAfterMs, client: c, key: key, tok: concurrencyTok}, nil
}
