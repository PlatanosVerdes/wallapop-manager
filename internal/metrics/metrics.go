// Package metrics pushes state to the Pushgateway; Grafana decides what is an alert.
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Gauge struct {
	Name  string
	Help  string
	Value float64
}

type Pusher struct {
	// Empty URL pushes nothing.
	URL    string
	Job    string
	Client *http.Client
}

func New(url, job string) *Pusher {
	return &Pusher{URL: strings.TrimSuffix(url, "/"), Job: job, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *Pusher) Enabled() bool { return p != nil && p.URL != "" }

// Push replaces the job's whole group, so nothing stale is left behind.
func (p *Pusher) Push(ctx context.Context, gauges []Gauge) error {
	if !p.Enabled() {
		return nil
	}

	var body bytes.Buffer
	for _, g := range gauges {
		fmt.Fprintf(&body, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.Name, g.Help, g.Name, g.Name, g.Value)
	}

	target := fmt.Sprintf("%s/metrics/job/%s", p.URL, p.Job)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("pushing metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pushgateway answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
