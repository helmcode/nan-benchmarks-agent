// Package vmclient is a thin HTTP client for VictoriaMetrics' /select/0/prometheus
// PromQL endpoint. It returns the raw vector samples — interpretation lives in
// the queries package.
package vmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Sample is one entry in a PromQL vector result.
type Sample struct {
	Metric map[string]string
	Value  float64
}

// Client talks to a VictoriaMetrics cluster's vmselect.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client with a 30 s default timeout per request.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type vmResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any             `json:"value"`  // instant only
			Values [][2]any           `json:"values"` // range only
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Point is one (timestamp, value) pair in a range query response.
type Point struct {
	T time.Time
	V float64
}

// Series is one labelled stream of points returned by a range query.
type Series struct {
	Metric map[string]string
	Points []Point
}

// Instant runs a PromQL query at a single timestamp. `at` is the eval moment;
// pass time.Time{} to use the server's now.
func (c *Client) Instant(ctx context.Context, query string, at time.Time) ([]Sample, error) {
	u := fmt.Sprintf("%s/select/0/prometheus/api/v1/query", c.BaseURL)
	v := url.Values{"query": []string{query}}
	if !at.IsZero() {
		v.Set("time", fmt.Sprintf("%d", at.Unix()))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vm query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vm query: status %d: %s", resp.StatusCode, body)
	}

	var r vmResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("vm query decode: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("vm query failed (%s): %s", r.ErrorType, r.Error)
	}

	out := make([]Sample, 0, len(r.Data.Result))
	for _, s := range r.Data.Result {
		// VM returns [timestamp, "stringFloat"].
		raw, ok := s.Value[1].(string)
		if !ok {
			continue
		}
		f, err := parseFloat(raw)
		if err != nil {
			continue
		}
		out = append(out, Sample{Metric: s.Metric, Value: f})
	}
	return out, nil
}

// Range runs a PromQL query over a time window, returning one Series per
// label combination. `step` is the resolution; pick it based on the window
// (eg. 1h for 7d windows, 6h for 30d windows) so the result stays under a
// few hundred points per series — Chart.js renders that cleanly and the
// payload stays small.
func (c *Client) Range(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	u := fmt.Sprintf("%s/select/0/prometheus/api/v1/query_range", c.BaseURL)
	v := url.Values{
		"query": []string{query},
		"start": []string{fmt.Sprintf("%d", start.Unix())},
		"end":   []string{fmt.Sprintf("%d", end.Unix())},
		"step":  []string{fmt.Sprintf("%ds", int(step.Seconds()))},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vm range: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vm range: status %d: %s", resp.StatusCode, body)
	}

	var r vmResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("vm range decode: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("vm range failed (%s): %s", r.ErrorType, r.Error)
	}

	out := make([]Series, 0, len(r.Data.Result))
	for _, s := range r.Data.Result {
		series := Series{Metric: s.Metric, Points: make([]Point, 0, len(s.Values))}
		for _, pair := range s.Values {
			ts, ok := pair[0].(float64)
			if !ok {
				continue
			}
			raw, ok := pair[1].(string)
			if !ok {
				continue
			}
			val, err := parseFloat(raw)
			if err != nil {
				continue
			}
			series.Points = append(series.Points, Point{
				T: time.Unix(int64(ts), 0).UTC(),
				V: val,
			})
		}
		out = append(out, series)
	}
	return out, nil
}

// ByLabel groups instant results into a map keyed by the given label.
// Samples missing the label are dropped silently.
func ByLabel(samples []Sample, label string) map[string]float64 {
	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		v, ok := s.Metric[label]
		if !ok {
			continue
		}
		out[v] = s.Value
	}
	return out
}

// First returns the single sample's value, or an error if the result was empty
// or contained more than one series.
func First(samples []Sample) (float64, error) {
	switch len(samples) {
	case 0:
		return 0, errors.New("empty result")
	case 1:
		return samples[0].Value, nil
	default:
		return 0, fmt.Errorf("expected 1 sample, got %d", len(samples))
	}
}

func parseFloat(s string) (float64, error) {
	// Handles "NaN", "+Inf", regular floats.
	switch s {
	case "NaN":
		return 0, errors.New("nan")
	case "+Inf", "Inf":
		return 0, errors.New("+inf")
	case "-Inf":
		return 0, errors.New("-inf")
	}
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	if err != nil {
		return 0, err
	}
	return f, nil
}
