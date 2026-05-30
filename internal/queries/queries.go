// Package queries holds the PromQL queries the agent runs against VictoriaMetrics.
// Each Collect* function takes a vmclient, a window string ("7d", "30d") and an
// eval timestamp, then returns a typed result keyed by job label.
//
// The queries mirror the original Python prototype that produced the manual
// baseline reports, so future benchmark output remains comparable.
package queries

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/helmcode/nan-benchmarks-agent/internal/vmclient"
)

// Window is a PromQL range like "7d" or "30d".
type Window string

// Seconds returns the window length in seconds. Supports h/d/w suffixes.
func (w Window) Seconds() (float64, error) {
	s := string(w)
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid window %q", w)
	}
	unit := s[len(s)-1]
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q: %w", w, err)
	}
	switch unit {
	case 'h':
		return n * 3600, nil
	case 'd':
		return n * 86400, nil
	case 'w':
		return n * 7 * 86400, nil
	}
	return 0, fmt.Errorf("invalid window unit %q in %q", unit, w)
}

// Minutes returns the window length in whole minutes.
func (w Window) Minutes() (float64, error) {
	s, err := w.Seconds()
	return s / 60, err
}

// Traffic holds the per-job traffic counters for one window.
type Traffic struct {
	Success     map[string]float64 // total requests
	Stop        map[string]float64 // finished_reason="stop"
	Length      map[string]float64 // finished_reason="length"
	PromptTok   map[string]float64
	GenTok      map[string]float64
	Running     map[string]float64 // instant
	Waiting     map[string]float64 // instant
	PctWaiting  map[string]float64 // % of time waiting>0
	Preemptions map[string]float64
}

// Latency holds histogram_quantile percentiles per job for one metric.
type Latency struct {
	P50 map[string]float64
	P95 map[string]float64
	P99 map[string]float64
}

// Cache holds prefix cache hit rate and KV cache usage per job.
type Cache struct {
	HitRate   map[string]float64 // percent (0–100)
	KVAvg     map[string]float64 // percent (0–100)
	KVMax     map[string]float64 // percent (0–100)
}

// Hardware holds the NVIDIA-exporter metrics per job (the nvidia_gpu* job label).
type Hardware struct {
	GPUUtilAvg  map[string]float64 // %
	GPUTempAvg  map[string]float64 // °C
	GPUTempMax  map[string]float64 // °C
	PowerAvg    map[string]float64 // W
	PowerMax    map[string]float64 // W
	VRAMUsedAvg map[string]float64 // GB
	VRAMTotal   map[string]float64 // GB
	PwrCapPct   map[string]float64 // % of window time under SW power cap
}

// CollectTraffic runs the traffic-related queries for a window.
func CollectTraffic(ctx context.Context, c *vmclient.Client, w Window, at time.Time) (*Traffic, error) {
	mins, err := w.Minutes()
	if err != nil {
		return nil, err
	}

	type pair struct {
		key   string
		query string
	}
	queries := []pair{
		{"success", fmt.Sprintf(`sum by (job) (increase(vllm:request_success_total[%s]))`, w)},
		{"stop", fmt.Sprintf(`sum by (job) (increase(vllm:request_success_total{finished_reason="stop"}[%s]))`, w)},
		{"length", fmt.Sprintf(`sum by (job) (increase(vllm:request_success_total{finished_reason="length"}[%s]))`, w)},
		{"prompt", fmt.Sprintf(`sum by (job) (increase(vllm:prompt_tokens_total[%s]))`, w)},
		{"gen", fmt.Sprintf(`sum by (job) (increase(vllm:generation_tokens_total[%s]))`, w)},
		{"running", `vllm:num_requests_running`},
		{"waiting", `vllm:num_requests_waiting`},
		{"pct_wait", fmt.Sprintf(`(sum by (job) (sum_over_time((vllm:num_requests_waiting > bool 0)[%s:1m]))) / %g`, w, mins)},
		{"preempt", fmt.Sprintf(`sum by (job) (increase(vllm:num_preemption_total[%s]))`, w)},
	}

	out := &Traffic{}
	for _, p := range queries {
		samples, err := c.Instant(ctx, p.query, at)
		if err != nil {
			return nil, fmt.Errorf("traffic[%s]: %w", p.key, err)
		}
		m := vmclient.ByLabel(samples, "job")
		switch p.key {
		case "success":
			out.Success = m
		case "stop":
			out.Stop = m
		case "length":
			out.Length = m
		case "prompt":
			out.PromptTok = m
		case "gen":
			out.GenTok = m
		case "running":
			out.Running = m
		case "waiting":
			out.Waiting = m
		case "pct_wait":
			out.PctWaiting = m
		case "preempt":
			out.Preemptions = m
		}
	}
	return out, nil
}

// CollectLatency returns histogram_quantile percentiles for the given bucket
// histogram (e.g. "vllm:time_to_first_token_seconds_bucket").
func CollectLatency(ctx context.Context, c *vmclient.Client, w Window, bucket string, at time.Time) (*Latency, error) {
	out := &Latency{}
	for _, p := range []float64{0.5, 0.95, 0.99} {
		q := fmt.Sprintf(`histogram_quantile(%g, sum by (le, job) (rate(%s[%s])))`, p, bucket, w)
		samples, err := c.Instant(ctx, q, at)
		if err != nil {
			return nil, fmt.Errorf("latency p%g: %w", p, err)
		}
		m := vmclient.ByLabel(samples, "job")
		switch p {
		case 0.5:
			out.P50 = m
		case 0.95:
			out.P95 = m
		case 0.99:
			out.P99 = m
		}
	}
	return out, nil
}

// CollectCache returns prefix cache hit rate and KV cache usage.
func CollectCache(ctx context.Context, c *vmclient.Client, w Window, at time.Time) (*Cache, error) {
	hitQ := fmt.Sprintf(`sum by (job) (increase(vllm:prefix_cache_hits_total[%s])) / sum by (job) (increase(vllm:prefix_cache_queries_total[%s])) * 100`, w, w)
	kvAvgQ := fmt.Sprintf(`avg_over_time(vllm:kv_cache_usage_perc[%s]) * 100`, w)
	kvMaxQ := fmt.Sprintf(`max_over_time(vllm:kv_cache_usage_perc[%s]) * 100`, w)

	hits, err := c.Instant(ctx, hitQ, at)
	if err != nil {
		return nil, fmt.Errorf("cache.hit: %w", err)
	}
	avg, err := c.Instant(ctx, kvAvgQ, at)
	if err != nil {
		return nil, fmt.Errorf("cache.kv_avg: %w", err)
	}
	mx, err := c.Instant(ctx, kvMaxQ, at)
	if err != nil {
		return nil, fmt.Errorf("cache.kv_max: %w", err)
	}
	return &Cache{
		HitRate: vmclient.ByLabel(hits, "job"),
		KVAvg:   vmclient.ByLabel(avg, "job"),
		KVMax:   vmclient.ByLabel(mx, "job"),
	}, nil
}

// CollectHardware queries the NVIDIA exporter (job=nvidia_gpu*).
func CollectHardware(ctx context.Context, c *vmclient.Client, w Window, at time.Time) (*Hardware, error) {
	secs, err := w.Seconds()
	if err != nil {
		return nil, err
	}

	type pair struct {
		key   string
		query string
	}
	queries := []pair{
		{"util_avg", fmt.Sprintf(`avg_over_time(nvidia_smi_utilization_gpu_ratio[%s]) * 100`, w)},
		{"temp_avg", fmt.Sprintf(`avg_over_time(nvidia_smi_temperature_gpu[%s])`, w)},
		{"temp_max", fmt.Sprintf(`max_over_time(nvidia_smi_temperature_gpu[%s])`, w)},
		{"power_avg", fmt.Sprintf(`avg_over_time(nvidia_smi_power_draw_watts[%s])`, w)},
		{"power_max", fmt.Sprintf(`max_over_time(nvidia_smi_power_draw_watts[%s])`, w)},
		{"vram_used", fmt.Sprintf(`avg_over_time(nvidia_smi_memory_used_bytes[%s]) / 1024 / 1024 / 1024`, w)},
		{"vram_total", `nvidia_smi_memory_total_bytes / 1024 / 1024 / 1024`},
		{"pwr_cap", fmt.Sprintf(`increase(nvidia_smi_clocks_event_reasons_counters_sw_power_cap_seconds[%s]) / %g * 100`, w, secs)},
	}

	out := &Hardware{}
	for _, p := range queries {
		samples, err := c.Instant(ctx, p.query, at)
		if err != nil {
			return nil, fmt.Errorf("hw[%s]: %w", p.key, err)
		}
		m := vmclient.ByLabel(samples, "job")
		switch p.key {
		case "util_avg":
			out.GPUUtilAvg = m
		case "temp_avg":
			out.GPUTempAvg = m
		case "temp_max":
			out.GPUTempMax = m
		case "power_avg":
			out.PowerAvg = m
		case "power_max":
			out.PowerMax = m
		case "vram_used":
			out.VRAMUsedAvg = m
		case "vram_total":
			out.VRAMTotal = m
		case "pwr_cap":
			out.PwrCapPct = m
		}
	}
	return out, nil
}

// ValidateWindow returns nil if the string is a valid PromQL range vector
// duration like "7d", "30d", "12h". Used for CLI flag validation.
func ValidateWindow(s string) error {
	w := Window(strings.TrimSpace(s))
	_, err := w.Seconds()
	return err
}
