// Package wallapop uses the web app's private API: the official one is only for PRO sellers.
package wallapop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	DefaultBaseURL   = "https://api.wallapop.com"
	DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	// The web build number: the API ignores it, but a real request carries it.
	DefaultAppVersion = "826680"
)

var (
	// ErrUnauthorized means a human has to import a new session.
	ErrUnauthorized  = errors.New("wallapop: session rejected")
	ErrAccessExpired = errors.New("wallapop: access token expired")
)

type Client struct {
	BaseURL string
	// WebURL is where sessions are renewed.
	WebURL     string
	Scheme     SignScheme
	UserAgent  string
	AppVersion string
	// DeviceID is the session token's device_id claim, so it matches the browser.
	DeviceID string
	Session  TokenSource
	HTTP     *http.Client
}

func New(session TokenSource) *Client {
	return &Client{
		BaseURL:    DefaultBaseURL,
		WebURL:     DefaultWebURL,
		Scheme:     SchemeNone,
		UserAgent:  DefaultUserAgent,
		AppVersion: DefaultAppVersion,
		Session:    session,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
}

type apiError struct {
	Status int
	Path   string
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("wallapop: %s answered %d: %s", e.Path, e.Status, e.Body)
}

// deviceos is sent twice because the web app sends both spellings.
func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "es,en-US;q=0.9")
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Origin", "https://es.wallapop.com")
	req.Header.Set("Referer", "https://es.wallapop.com/")
	req.Header.Set("deviceos", "0")
	req.Header.Set("X-DeviceOS", "0")
	req.Header.Set("X-AppVersion", c.AppVersion)
	if c.DeviceID != "" {
		req.Header.Set("X-DeviceId", c.DeviceID)
	}
}

// do renews and retries once on a spent access token, like the web app's interceptor.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) (http.Header, error) {
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding body for %s: %w", path, err)
		}
	}

	header, err := c.attempt(ctx, method, path, query, encoded, out, true)
	if !errors.Is(err, ErrAccessExpired) {
		return header, err
	}
	if err := c.RenewSession(ctx); err != nil {
		return header, err
	}
	header, err = c.attempt(ctx, method, path, query, encoded, out, true)
	if errors.Is(err, ErrAccessExpired) {
		return header, fmt.Errorf("%w: rejected right after a renewal", ErrUnauthorized)
	}
	return header, err
}

// public sends no bearer on purpose, so watching searches is not tied to the account.
func (c *Client) public(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.attempt(ctx, "GET", path, query, nil, out, false)
	return err
}

func (c *Client) attempt(ctx context.Context, method, path string, query url.Values, body []byte, out any, auth bool) (http.Header, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}

	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, err
	}

	c.setCommonHeaders(req)
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.Session.AccessToken())
	} else {
		// The device id would still tie an anonymous call to the account.
		req.Header.Del("X-DeviceId")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Scheme != SchemeNone {
		ts := time.Now().UnixMilli()
		signature, err := Sign(c.Scheme, method, path, ts)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Signature", signature)
		req.Header.Set("Timestamp", strconv.FormatInt(ts, 10))
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return resp.Header, classify(resp.StatusCode, resp.Header, path, raw)
	case resp.StatusCode >= 400:
		return resp.Header, &apiError{Status: resp.StatusCode, Path: path, Body: snippet(raw)}
	}

	if out == nil || len(raw) == 0 {
		return resp.Header, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.Header, fmt.Errorf("decoding %s: %w (body: %s)", path, err, snippet(raw))
	}
	return resp.Header, nil
}

func snippet(b []byte) string {
	s := string(bytes.TrimSpace(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
