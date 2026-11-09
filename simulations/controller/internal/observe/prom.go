// Package observe measures the system around a simulation: Prometheus range
// queries and controller-run probes, summarised per window (baseline, fault,
// recovery, after) and exportable as raw series.
package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prom is a minimal Prometheus HTTP API client.
type Prom struct {
	URL  string
	HTTP *http.Client
}

// Point is one sample.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Series is one result series.
type Series struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// QueryRange runs /api/v1/query_range.
func (p *Prom) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	if p == nil || p.URL == "" {
		return nil, fmt.Errorf("no Prometheus configured (--prometheus-url)")
	}
	v := url.Values{}
	v.Set("query", q)
	v.Set("start", strconv.FormatFloat(float64(start.UnixMilli())/1000, 'f', 3, 64))
	v.Set("end", strconv.FormatFloat(float64(end.UnixMilli())/1000, 'f', 3, 64))
	v.Set("step", strconv.FormatFloat(step.Seconds(), 'f', 3, 64))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/api/v1/query_range", strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := p.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("prometheus: %s: %w", resp.Status, err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", body.Error)
	}
	out := make([]Series, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s := Series{Labels: r.Metric}
		for _, pair := range r.Values {
			ts, _ := pair[0].(float64)
			str, _ := pair[1].(string)
			f, err := strconv.ParseFloat(str, 64)
			if err != nil {
				continue
			}
			s.Points = append(s.Points, Point{T: time.UnixMilli(int64(ts * 1000)), V: f})
		}
		out = append(out, s)
	}
	return out, nil
}
