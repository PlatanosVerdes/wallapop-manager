// Package wallapop talks to the private API the Wallapop web app uses. The official
// Connect API is only open to PRO sellers with a registered OAuth client, so a personal
// account has to go the same way the browser does.
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
	// Web build number sent by the app. Cosmetic as far as the API cares, but it is part
	// of what a real request looks like.
	DefaultAppVersion = "826680"
)

var (
	// ErrUnauthorized means the session is finished: a human has to import a new one.
	ErrUnauthorized = errors.New("wallapop: session rejected")
	// ErrAccessExpired is the routine case the client fixes by itself.
	ErrAccessExpired = errors.New("wallapop: access token expired")
)

type Client struct {
	BaseURL string
	// WebURL is the site itself, which is where sessions are renewed.
	WebURL     string
	Scheme     SignScheme
	UserAgent  string
	AppVersion string
	// DeviceID goes out as x-deviceid. It is the device_id claim of the session token, so
	// it matches the browser the session was born in.
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

// setCommonHeaders sends the same set a real browser call carries. deviceos is duplicated
// because the web app sends both spellings.
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

// do renews the session and retries once when the access token turns out to be spent,
// which is the same thing the web app's interceptor does.
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
		// Renewed and still rejected: the session is not coming back on its own.
		return header, fmt.Errorf("%w: rejected right after a renewal", ErrUnauthorized)
	}
	return header, err
}

// public calls an endpoint that needs no session, and deliberately sends no bearer: the
// catalogue search works anonymously, so watching it is not traffic tied to the account.
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
		// The device id is the browser the session was born in; an anonymous call that
		// carried it would still be traceable back to the account.
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
