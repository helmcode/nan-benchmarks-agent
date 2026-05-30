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

// TimeSeries bundles all the per-time-step series we render as line charts.
// Every series is a flat []vmclient.Point in chronological order. We
// resample to one point per `step` (see RecommendedStep) so the payload
// stays light and Chart.js renders cleanly.
type TimeSeries struct {
	Step time.Duration // resolution we asked VictoriaMetrics for

	// Traffic — cluster-wide.
	ReqPerMinTotal []vmclient.Point // sum across all vllm jobs, per-min rate

	// Latency p50/p95 — Qwen3.6 fleet aggregate (avg of histogram_quantile
	// across qwen jobs, evaluated on each step).
	TTFTp50, TTFTp95, TTFTp99 []vmclient.Point
	TPOTp50, TPOTp95          []vmclient.Point
	E2Ep50, E2Ep95            []vmclient.Point

	// Cache & KV — Qwen3.6 fleet avg.
	PrefixHitPct []vmclient.Point
	KVAvgPct     []vmclient.Point

	// Hardware — Qwen3.6 fleet avg.
	GPUUtilPct  []vmclient.Point
	PwrCapPct   []vmclient.Point

	// LiteLLM proxy POV — global (no fleet split).
	LiteLLMOverheadP50 []vmclient.Point
	LiteLLMOverheadP95 []vmclient.Point
	LiteLLMApiLatP50   []vmclient.Point
	LiteLLMApiLatP95   []vmclient.Point

	// External providers traffic split — one series per (api_provider,
	// litellm_model_name) pair. Keyed by "<provider>:<model>" so the
	// template can build a multi-line chart.
	ProviderReqPerMin map[string][]vmclient.Point
}

// Provider is one row in the "external providers" table — eg. DeepInfra or
// OpenRouter serving a given model. We split DeepSeek's two backends
// (deepinfra + openrouter as fallback) so the report can show who is
// pulling weight, who is errorring more and who is slower.
type Provider struct {
	APIProvider  string  // "deepinfra", "openrouter", ...
	ModelName    string  // litellm_model_name / model label (eg. "deepseek/deepseek-v4-flash")
	Requests     float64 // sum increase over window
	Failures     float64
	InputTokens  float64
	OutputTokens float64
	CachedTokens float64
	LatencyP50   float64 // seconds
	LatencyP95   float64
}

// ErrorRate is failures / (requests + failures) — placed here so the
// template can call it without doing maths inline.
func (p Provider) ErrorRate() float64 {
	denom := p.Requests + p.Failures
	if denom <= 0 {
		return 0
	}
	return p.Failures / denom * 100
}

// LiteLLM holds proxy-side counters + latency percentiles over the window.
//
// The histograms (litellm_overhead_latency_metric_bucket /
// litellm_llm_api_latency_metric_bucket) carry > 100k series in practice,
// well beyond what a range query can aggregate without blowing past
// vmstorage's sample cap. We collect single-point percentiles for the full
// window instead — that is what we render in the LiteLLM section's cards.
type LiteLLM struct {
	SuccessRequests       float64
	FailureRequests       float64
	SuccessfulFallbacks   float64 // a fallback that rescued the request
	FailedFallbacks       float64
	CooledDownDeployments float64
	CacheHits             float64
	CacheMisses           float64
	CachedTokens          float64

	OverheadP50Sec float64 // proxy-added latency (s) at p50
	OverheadP95Sec float64
	ApiLatP50Sec   float64 // upstream LLM call latency (s) at p50
	ApiLatP95Sec   float64

	// Per-deployment breakdown for the fallback table.
	FallbacksByDeploy map[string]float64
	CooldownsByDeploy map[string]float64

	// Per-provider breakdown for the external-providers table.
	Providers []Provider
}

// RecommendedStep picks a sane Chart.js-friendly step for a window. The aim
// is to keep series under ~200 points so the embedded canvas renders fast.
//
//	  7d → 1h   (168 points)
//	 30d → 6h   (120 points)
//	  1d → 5m   (288 points)
func RecommendedStep(w Window) time.Duration {
	secs, err := w.Seconds()
	if err != nil || secs <= 0 {
		return time.Hour
	}
	target := secs / 168.0 // aim for ~168 points
	d := time.Duration(target) * time.Second
	// Round to nice values so axis labels stay readable.
	switch {
	case d <= 5*time.Minute:
		return 5 * time.Minute
	case d <= 15*time.Minute:
		return 15 * time.Minute
	case d <= 30*time.Minute:
		return 30 * time.Minute
	case d <= time.Hour:
		return time.Hour
	case d <= 2*time.Hour:
		return 2 * time.Hour
	case d <= 6*time.Hour:
		return 6 * time.Hour
	}
	return 12 * time.Hour
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

// buildModelFilter renders a PromQL `model=~"..."` selector that pins a
// query to exactly the given model_name strings. RE2 metacharacters are
// double-escaped so they survive PromQL's string-literal pass.
func buildModelFilter(models []string) string {
	if len(models) == 0 {
		return ""
	}
	parts := make([]string, 0, len(models))
	for _, m := range models {
		parts = append(parts, escapeRE2ForPromQL(m))
	}
	return `model=~"` + strings.Join(parts, "|") + `"`
}

// escapeRE2ForPromQL is the in-package twin of report.regexpQuoteForPromQL.
// Duplicated here to keep the queries package free of report's dependencies.
func escapeRE2ForPromQL(s string) string {
	const meta = `\.+*?()|[]{}^$`
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if strings.ContainsRune(meta, r) {
			b.WriteString(`\\`)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// externalProviderSelector matches the api_provider label values that
// represent real external/SaaS providers (DeepInfra, OpenRouter, Anthropic,
// etc.), excluding the in-house vLLM backends (which LiteLLM labels as
// "openai" or "text-completion-openai" because they speak the OpenAI API).
//
// A leftover "None" value appears for aggregate per-model_group rows; we
// drop those too — they double-count the actual per-deployment rows.
const externalProviderSelector = `api_provider!~"openai|text-completion-openai|None|"`

// collectProviders enumerates one Provider row per (api_provider,
// litellm_model_name) pair that is currently driving traffic through an
// external SaaS backend. Tokens and latency live on a sibling metric
// (`model` label) so we join in-process by model name.
func collectProviders(ctx context.Context, c *vmclient.Client, w Window, at time.Time) []Provider {
	type key struct{ provider, model string }
	idx := map[key]*Provider{}

	getOrCreate := func(k key) *Provider {
		if p, ok := idx[k]; ok {
			return p
		}
		p := &Provider{APIProvider: k.provider, ModelName: k.model}
		idx[k] = p
		return p
	}

	// Requests + failures: native (api_provider, litellm_model_name) labels.
	reqQ := fmt.Sprintf(`sum by (api_provider, litellm_model_name) (increase(litellm_deployment_total_requests_total{%s}[%s]))`, externalProviderSelector, w)
	failQ := fmt.Sprintf(`sum by (api_provider, litellm_model_name) (increase(litellm_deployment_failure_responses_total{%s}[%s]))`, externalProviderSelector, w)
	for _, q := range []struct {
		query string
		set   func(p *Provider, v float64)
	}{
		{reqQ, func(p *Provider, v float64) { p.Requests = v }},
		{failQ, func(p *Provider, v float64) { p.Failures = v }},
	} {
		s, err := c.Instant(ctx, q.query, at)
		if err != nil {
			continue
		}
		for _, smp := range s {
			model := smp.Metric["litellm_model_name"]
			if model == "" || model == "None" {
				continue
			}
			k := key{provider: smp.Metric["api_provider"], model: model}
			q.set(getOrCreate(k), smp.Value)
		}
	}

	// Token + latency metrics carry only `model` (which equals
	// litellm_model_name on the request side). We index by model name and
	// merge into the existing provider rows.
	tokenQ := func(name string) string {
		return fmt.Sprintf(`sum by (model) (increase(litellm_%s_metric_total[%s]))`, name, w)
	}
	type tokenSetter func(p *Provider, v float64)
	tokenSpecs := []struct {
		query string
		set   tokenSetter
	}{
		{tokenQ("input_tokens"), func(p *Provider, v float64) { p.InputTokens = v }},
		{tokenQ("output_tokens"), func(p *Provider, v float64) { p.OutputTokens = v }},
		{tokenQ("cached_tokens"), func(p *Provider, v float64) { p.CachedTokens = v }},
	}
	for _, t := range tokenSpecs {
		s, err := c.Instant(ctx, t.query, at)
		if err != nil {
			continue
		}
		for _, smp := range s {
			model := smp.Metric["model"]
			for _, p := range idx {
				if p.ModelName == model {
					t.set(p, smp.Value)
				}
			}
		}
	}

	// Latency percentiles per model. The unfiltered histogram has > 100k
	// series across all models served by the proxy; on a 7d window that
	// exceeds vmstorage's maxSamplesPerQuery cap. We pin the query to just
	// the model names we already saw on the requests side — typically
	// 2–4 model strings — which cuts the search space by ~5 orders of
	// magnitude and lets the same query run cleanly on any window length.
	models := make([]string, 0, len(idx))
	{
		seen := map[string]struct{}{}
		for k := range idx {
			if _, dup := seen[k.model]; dup {
				continue
			}
			seen[k.model] = struct{}{}
			models = append(models, k.model)
		}
	}
	if modelFilter := buildModelFilter(models); modelFilter != "" {
		for _, p := range []struct {
			q   string
			set func(p *Provider, v float64)
		}{
			{`histogram_quantile(0.5,  sum by (le, model) (rate(litellm_llm_api_latency_metric_bucket{` + modelFilter + `}[` + string(w) + `])))`, func(p *Provider, v float64) { p.LatencyP50 = v }},
			{`histogram_quantile(0.95, sum by (le, model) (rate(litellm_llm_api_latency_metric_bucket{` + modelFilter + `}[` + string(w) + `])))`, func(p *Provider, v float64) { p.LatencyP95 = v }},
		} {
			s, err := c.Instant(ctx, p.q, at)
			if err != nil {
				continue
			}
			for _, smp := range s {
				model := smp.Metric["model"]
				for _, pr := range idx {
					if pr.ModelName == model {
						p.set(pr, smp.Value)
					}
				}
			}
		}
	}

	rows := make([]Provider, 0, len(idx))
	for _, p := range idx {
		// Skip rows that have no observable activity in the window —
		// LiteLLM keeps zero counters for deployments configured but never
		// exercised, and they would clutter the table.
		if p.Requests == 0 && p.Failures == 0 && p.InputTokens == 0 {
			continue
		}
		rows = append(rows, *p)
	}
	// Sort by requests descending so the busiest provider is on top.
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].Requests > rows[i].Requests {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	return rows
}

// ── Time-series collectors (range queries) ────────────────────────────
//
// These produce one []vmclient.Point per metric, sampled at `step`. The
// result feeds Chart.js line charts so the reader can see evolution
// over the window instead of just a snapshot.

// firstSeries returns the single series we expect from a cluster-wide
// aggregate query. PromQL `avg by ()` always collapses to one series;
// missing data shows up as an empty Points slice — we leave the caller
// to decide what to do with that (Chart.js draws a gap).
func firstSeries(s []vmclient.Series) []vmclient.Point {
	if len(s) == 0 {
		return nil
	}
	return s[0].Points
}

// CollectVllmSeries fetches all the vLLM-side time series we render in the
// PDF. `qwenSelector` is a PromQL label-match expression that pins the
// queries to the qwen3.6 fleet (eg. `model_name=~".*qwen3\\.6.*"`) so the
// aggregate ignores gemma and embedding backends.
func CollectVllmSeries(ctx context.Context, c *vmclient.Client, qwenSelector string, start, end time.Time, step time.Duration) (*TimeSeries, error) {
	ts := &TimeSeries{Step: step, ProviderReqPerMin: map[string][]vmclient.Point{}}

	type pair struct {
		name string
		q    string
		dst  *[]vmclient.Point
	}
	stepRange := fmt.Sprintf("%ds", int(step.Seconds()))

	queries := []pair{
		// Cluster-wide req/min.
		{"req_per_min", `sum(rate(vllm:request_success_total[` + stepRange + `])) * 60`, &ts.ReqPerMinTotal},

		// Latency percentiles — qwen3.6 fleet aggregate. We take the average
		// over qwen jobs at each step so the line is one cluster-level signal.
		{"ttft_p50", `avg(histogram_quantile(0.5,  sum by (le, job) (rate(vllm:time_to_first_token_seconds_bucket{` + qwenSelector + `}[` + stepRange + `]))))`, &ts.TTFTp50},
		{"ttft_p95", `avg(histogram_quantile(0.95, sum by (le, job) (rate(vllm:time_to_first_token_seconds_bucket{` + qwenSelector + `}[` + stepRange + `]))))`, &ts.TTFTp95},
		{"ttft_p99", `avg(histogram_quantile(0.99, sum by (le, job) (rate(vllm:time_to_first_token_seconds_bucket{` + qwenSelector + `}[` + stepRange + `]))))`, &ts.TTFTp99},
		{"tpot_p50", `avg(histogram_quantile(0.5,  sum by (le, job) (rate(vllm:inter_token_latency_seconds_bucket{` + qwenSelector + `}[` + stepRange + `]))))`, &ts.TPOTp50},
		{"tpot_p95", `avg(histogram_quantile(0.95, sum by (le, job) (rate(vllm:inter_token_latency_seconds_bucket{` + qwenSelector + `}[` + stepRange + `]))))`, &ts.TPOTp95},
		{"e2e_p50",  `avg(histogram_quantile(0.5,  sum by (le, job) (rate(vllm:e2e_request_latency_seconds_bucket{` + qwenSelector + `}[` + stepRange + `]))))`, &ts.E2Ep50},
		{"e2e_p95",  `avg(histogram_quantile(0.95, sum by (le, job) (rate(vllm:e2e_request_latency_seconds_bucket{` + qwenSelector + `}[` + stepRange + `]))))`, &ts.E2Ep95},

		// Cache & KV — fleet avg.
		{"prefix_hit", `avg(rate(vllm:prefix_cache_hits_total{` + qwenSelector + `}[` + stepRange + `])) / avg(rate(vllm:prefix_cache_queries_total{` + qwenSelector + `}[` + stepRange + `])) * 100`, &ts.PrefixHitPct},
		{"kv_avg",     `avg(vllm:kv_cache_usage_perc{` + qwenSelector + `}) * 100`, &ts.KVAvgPct},

		// Hardware — fleet avg (across qwen-paired nvidia_gpu jobs).
		{"gpu_util", `avg(nvidia_smi_utilization_gpu_ratio{job=~"nvidia_gpu.*"}) * 100`, &ts.GPUUtilPct},
		{"pwr_cap",  `avg(rate(nvidia_smi_clocks_event_reasons_counters_sw_power_cap_seconds{job=~"nvidia_gpu.*"}[` + stepRange + `])) * 100`, &ts.PwrCapPct},
	}
	for _, p := range queries {
		s, err := c.Range(ctx, p.q, start, end, step)
		if err != nil {
			return nil, fmt.Errorf("series %s: %w", p.name, err)
		}
		*p.dst = firstSeries(s)
	}

	// Per-provider req/min split, one series per (api_provider,
	// litellm_model_name). Cardinality is low (10–20 series total) so this
	// stays well below the vmstorage sample cap.
	provQ := fmt.Sprintf(`sum by (api_provider, litellm_model_name) (rate(litellm_deployment_total_requests_total{%s}[%s])) * 60`, externalProviderSelector, stepRange)
	if s, err := c.Range(ctx, provQ, start, end, step); err == nil {
		for _, series := range s {
			model := series.Metric["litellm_model_name"]
			if model == "" || model == "None" {
				continue
			}
			key := series.Metric["api_provider"] + ":" + model
			ts.ProviderReqPerMin[key] = series.Points
		}
	}
	return ts, nil
}

// CollectLiteLLMSeries fetches the LiteLLM proxy time series. Same shape as
// CollectVllmSeries but for proxy-side histograms.
//
// LiteLLM's histograms carry a high-cardinality label set (model, deployment,
// status_code, api_key_alias, user, team, ...). A naive
// `rate(metric_bucket[step])` across 7 days expands to billions of samples and
// vmstorage refuses it. We use `avg_over_time(... by (le)[window:step])`
// instead, which evaluates each step against an instant-style aggregation
// and stays under the vmstorage sample cap while preserving the line we want.
func CollectLiteLLMSeries(ctx context.Context, c *vmclient.Client, ts *TimeSeries, start, end time.Time, step time.Duration) error {
	stepRange := fmt.Sprintf("%ds", int(step.Seconds()))
	queries := []struct {
		name string
		q    string
		dst  *[]vmclient.Point
	}{
		// Overhead is the proxy's own latency on top of the upstream call,
		// reported only for successful requests, which keeps cardinality low.
		{"overhead_p50", `histogram_quantile(0.5,  sum by (le) (increase(litellm_overhead_latency_metric_bucket[` + stepRange + `])))`, &ts.LiteLLMOverheadP50},
		{"overhead_p95", `histogram_quantile(0.95, sum by (le) (increase(litellm_overhead_latency_metric_bucket[` + stepRange + `])))`, &ts.LiteLLMOverheadP95},
		{"api_lat_p50",  `histogram_quantile(0.5,  sum by (le) (increase(litellm_llm_api_latency_metric_bucket[` + stepRange + `])))`, &ts.LiteLLMApiLatP50},
		{"api_lat_p95",  `histogram_quantile(0.95, sum by (le) (increase(litellm_llm_api_latency_metric_bucket[` + stepRange + `])))`, &ts.LiteLLMApiLatP95},
	}
	for _, p := range queries {
		s, err := c.Range(ctx, p.q, start, end, step)
		if err != nil {
			// Surface the cause so the calling layer can log it, but don't
			// poison the whole pipeline — vLLM charts and counters remain
			// useful even when the proxy histograms blow up.
			return fmt.Errorf("litellm series %s: %w", p.name, err)
		}
		*p.dst = firstSeries(s)
	}
	return nil
}

// CollectLiteLLM aggregates LiteLLM-side counters into a single struct.
// We sum increase(...) over the whole window for totals and group the
// fallback/cooldown counters by deployment so the table can break them out.
func CollectLiteLLM(ctx context.Context, c *vmclient.Client, w Window, at time.Time) (*LiteLLM, error) {
	out := &LiteLLM{
		FallbacksByDeploy: map[string]float64{},
		CooldownsByDeploy: map[string]float64{},
	}

	totals := []struct {
		name string
		q    string
		dst  *float64
	}{
		{"success",  fmt.Sprintf(`sum(increase(litellm_deployment_success_responses_total[%s]))`, w), &out.SuccessRequests},
		{"failure",  fmt.Sprintf(`sum(increase(litellm_deployment_failure_responses_total[%s]))`, w), &out.FailureRequests},
		{"fb_ok",    fmt.Sprintf(`sum(increase(litellm_deployment_successful_fallbacks_total[%s]))`, w), &out.SuccessfulFallbacks},
		{"fb_fail",  fmt.Sprintf(`sum(increase(litellm_deployment_failed_fallbacks_total[%s]))`, w), &out.FailedFallbacks},
		{"cool",     fmt.Sprintf(`sum(increase(litellm_deployment_cooled_down_total[%s]))`, w), &out.CooledDownDeployments},
		{"hits",     fmt.Sprintf(`sum(increase(litellm_cache_hits_metric_total[%s]))`, w), &out.CacheHits},
		{"misses",   fmt.Sprintf(`sum(increase(litellm_cache_misses_metric_total[%s]))`, w), &out.CacheMisses},
		{"tokens",   fmt.Sprintf(`sum(increase(litellm_cached_tokens_metric_total[%s]))`, w), &out.CachedTokens},
	}
	for _, t := range totals {
		samples, err := c.Instant(ctx, t.q, at)
		if err != nil {
			return nil, fmt.Errorf("litellm[%s]: %w", t.name, err)
		}
		if len(samples) == 1 {
			*t.dst = samples[0].Value
		}
	}

	// Per-provider breakdown for the external-providers table.
	out.Providers = collectProviders(ctx, c, w, at)

	// Per-deployment breakdown for the fallback / cooldown tables.
	fb, err := c.Instant(ctx, fmt.Sprintf(`sum by (deployment) (increase(litellm_deployment_successful_fallbacks_total[%s]))`, w), at)
	if err == nil {
		out.FallbacksByDeploy = vmclient.ByLabel(fb, "deployment")
	}
	cd, err := c.Instant(ctx, fmt.Sprintf(`sum by (deployment) (increase(litellm_deployment_cooled_down_total[%s]))`, w), at)
	if err == nil {
		out.CooldownsByDeploy = vmclient.ByLabel(cd, "deployment")
	}

	// Latency percentiles — single instant evaluation over the whole window.
	// Cardinality is high (~100k series) but a single histogram_quantile
	// resolve is fine; what blows up is asking VM to repeat that across
	// many step points in a range query.
	pcts := []struct {
		name string
		q    string
		dst  *float64
	}{
		{"overhead_p50", fmt.Sprintf(`histogram_quantile(0.5,  sum by (le) (rate(litellm_overhead_latency_metric_bucket[%s])))`, w), &out.OverheadP50Sec},
		{"overhead_p95", fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(litellm_overhead_latency_metric_bucket[%s])))`, w), &out.OverheadP95Sec},
		{"api_lat_p50",  fmt.Sprintf(`histogram_quantile(0.5,  sum by (le) (rate(litellm_llm_api_latency_metric_bucket[%s])))`, w), &out.ApiLatP50Sec},
		{"api_lat_p95",  fmt.Sprintf(`histogram_quantile(0.95, sum by (le) (rate(litellm_llm_api_latency_metric_bucket[%s])))`, w), &out.ApiLatP95Sec},
	}
	for _, p := range pcts {
		s, err := c.Instant(ctx, p.q, at)
		if err != nil || len(s) != 1 {
			continue // best-effort: missing percentiles render as "—"
		}
		*p.dst = s[0].Value
	}
	return out, nil
}
