// Package report orchestrates the metric collection for a benchmark window
// (current + previous period for comparison) and computes the aggregates the
// template renders.
package report

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/helmcode/nan-benchmarks-agent/internal/queries"
	"github.com/helmcode/nan-benchmarks-agent/internal/topology"
	"github.com/helmcode/nan-benchmarks-agent/internal/vmclient"
)

// Mode is the benchmark cadence.
type Mode string

const (
	ModeWeekly  Mode = "weekly"
	ModeMonthly Mode = "monthly"
)

// Window returns the PromQL range duration for this mode.
func (m Mode) Window() queries.Window {
	switch m {
	case ModeMonthly:
		return "30d"
	default:
		return "7d"
	}
}

// Report is the full dataset for one benchmark run.
type Report struct {
	Mode        Mode      `json:"mode"`
	Window      string    `json:"window"`
	GeneratedAt time.Time `json:"generated_at"`
	PeriodEnd   time.Time `json:"period_end"`
	PrevEnd     time.Time `json:"prev_end"`

	Topology *topology.Topology `json:"topology"`
	Current  *PeriodMetrics     `json:"current"`
	Previous *PeriodMetrics     `json:"previous"`
}

// PeriodMetrics holds everything for one window evaluated at one timestamp.
type PeriodMetrics struct {
	At         time.Time          `json:"at"`
	Traffic    *queries.Traffic   `json:"traffic"`
	TTFT       *queries.Latency   `json:"ttft"`
	TPOT       *queries.Latency   `json:"tpot"`
	E2E        *queries.Latency   `json:"e2e"`
	Cache      *queries.Cache     `json:"cache"`
	Hardware   *queries.Hardware  `json:"hardware"`
	LiteLLM    *queries.LiteLLM   `json:"litellm,omitempty"`
	Series     *queries.TimeSeries `json:"-"` // not in JSON — too large for the LLM prompt
	Aggregates Aggregates         `json:"aggregates"`
}

// Aggregates are summary stats computed across the discovered topology.
type Aggregates struct {
	QwenReqTotal     float64 `json:"qwen_req_total"`
	QwenReqPerMin    float64 `json:"qwen_req_per_min"`
	QwenPromptTokens float64 `json:"qwen_prompt_tokens"`
	QwenGenTokens    float64 `json:"qwen_gen_tokens"`
	QwenTTFTP50Avg   float64 `json:"qwen_ttft_p50_avg"` // seconds
	QwenTTFTP95Avg   float64 `json:"qwen_ttft_p95_avg"`
	QwenTTFTP99Avg   float64 `json:"qwen_ttft_p99_avg"`
	QwenTPOTP50Avg   float64 `json:"qwen_tpot_p50_avg"` // seconds
	QwenE2EP50Avg    float64 `json:"qwen_e2e_p50_avg"`  // seconds
	QwenE2EP99Avg    float64 `json:"qwen_e2e_p99_avg"`
	QwenHitRateAvg   float64 `json:"qwen_hit_rate_avg"`
	QwenKVAvg        float64 `json:"qwen_kv_avg"`
	QwenGPUUtilAvg   float64 `json:"qwen_gpu_util_avg"`
	QwenPwrCapAvg    float64 `json:"qwen_pwr_cap_avg"`
	PreemptionsTotal float64 `json:"preemptions_total"`
	GemmaReqTotal    float64 `json:"gemma_req_total"`
	EmbReqTotal      float64 `json:"emb_req_total"`
}

// Build collects everything for a given mode. `now` is typically time.Now()
// and previous-window metrics are evaluated at `now - window`.
func Build(ctx context.Context, c *vmclient.Client, mode Mode, now time.Time) (*Report, error) {
	w := mode.Window()
	secs, err := w.Seconds()
	if err != nil {
		return nil, err
	}
	prevAt := now.Add(-time.Duration(secs) * time.Second)

	top, err := topology.Discover(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("discover topology: %w", err)
	}

	cur, err := buildPeriod(ctx, c, w, now, top)
	if err != nil {
		return nil, fmt.Errorf("current period: %w", err)
	}
	prev, err := buildPeriod(ctx, c, w, prevAt, top)
	if err != nil {
		// Previous period is best-effort — for the very first run after a
		// retention reset there may be no historic data. We log and continue.
		prev = &PeriodMetrics{At: prevAt}
	}

	// Time series are only collected for the current window — the previous
	// window participates in the comparison table via instant aggregates.
	// LiteLLM time series are NOT collected here: their histograms carry
	// > 100k series and range queries exceed vmstorage's sample cap. The
	// proxy section renders single-point percentiles via the instant
	// CollectLiteLLM call inside buildPeriod.
	step := queries.RecommendedStep(w)
	start := now.Add(-time.Duration(secs) * time.Second)
	qwenSel := qwenSelector(top)
	if qwenSel != "" {
		if series, serr := queries.CollectVllmSeries(ctx, c, qwenSel, start, now, step); serr == nil {
			cur.Series = series
			slog.Info("time series collected",
				"req_points", len(series.ReqPerMinTotal),
				"ttft_p50_points", len(series.TTFTp50),
				"step", step.String())
		} else {
			slog.Warn("time series: vllm collection failed", "err", serr)
		}
	}

	return &Report{
		Mode:        mode,
		Window:      string(w),
		GeneratedAt: time.Now().UTC(),
		PeriodEnd:   now,
		PrevEnd:     prevAt,
		Topology:    top,
		Current:     cur,
		Previous:    prev,
	}, nil
}

// qwenSelector builds a PromQL label selector that matches every job in the
// Qwen3.6 family. We anchor on the live model_name labels so the selector
// stays accurate even after model renames; if no qwen backend is discovered
// we return an empty string and skip time-series collection.
func qwenSelector(top *topology.Topology) string {
	models := map[string]struct{}{}
	for _, n := range top.ByFamily(topology.FamilyQwen) {
		if n.Model != "" {
			models[n.Model] = struct{}{}
		}
	}
	if len(models) == 0 {
		return ""
	}
	// Build a regex alternation of the literal model_name values so the
	// PromQL `model_name=~"..."` selector tracks whatever the cluster
	// is actually serving today.
	parts := make([]string, 0, len(models))
	for m := range models {
		parts = append(parts, regexpQuoteForPromQL(m))
	}
	return `model_name=~"` + strings.Join(parts, "|") + `"`
}

// regexpQuoteForPromQL escapes regex metacharacters so a literal
// model_name string is matched verbatim by Prometheus's RE2 engine.
//
// The string we build here ends up double-escaped on purpose: PromQL
// parses the double-quoted body of `=~"..."` as a Go-style string
// literal first (where `\\` reduces to `\`) and then hands the result
// to RE2. So to match a literal "." we need to emit `\\.` (two source
// backslashes + dot), which PromQL string-decodes to `\.` and RE2
// then reads as "literal dot".
func regexpQuoteForPromQL(s string) string {
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

func buildPeriod(ctx context.Context, c *vmclient.Client, w queries.Window, at time.Time, top *topology.Topology) (*PeriodMetrics, error) {
	traffic, err := queries.CollectTraffic(ctx, c, w, at)
	if err != nil {
		return nil, err
	}
	ttft, err := queries.CollectLatency(ctx, c, w, "vllm:time_to_first_token_seconds_bucket", at)
	if err != nil {
		return nil, err
	}
	tpot, err := queries.CollectLatency(ctx, c, w, "vllm:inter_token_latency_seconds_bucket", at)
	if err != nil {
		return nil, err
	}
	e2e, err := queries.CollectLatency(ctx, c, w, "vllm:e2e_request_latency_seconds_bucket", at)
	if err != nil {
		return nil, err
	}
	cache, err := queries.CollectCache(ctx, c, w, at)
	if err != nil {
		return nil, err
	}
	hw, err := queries.CollectHardware(ctx, c, w, at)
	if err != nil {
		return nil, err
	}
	llm, err := queries.CollectLiteLLM(ctx, c, w, at)
	if err != nil {
		// LiteLLM metrics are best-effort — the proxy may not be scraped
		// during a partial outage. Carry on rather than failing the whole
		// report.
		llm = &queries.LiteLLM{}
	}

	pm := &PeriodMetrics{
		At:       at,
		Traffic:  traffic,
		TTFT:     ttft,
		TPOT:     tpot,
		E2E:      e2e,
		Cache:    cache,
		Hardware: hw,
		LiteLLM:  llm,
	}
	pm.Aggregates = aggregate(top, pm, w)
	return pm, nil
}

func aggregate(top *topology.Topology, pm *PeriodMetrics, w queries.Window) Aggregates {
	var a Aggregates

	qwen := top.ByFamily(topology.FamilyQwen)
	gemma := top.ByFamily(topology.FamilyGemma)
	emb := top.ByFamily(topology.FamilyEmbedding)

	mins, _ := w.Minutes()

	for _, n := range qwen {
		a.QwenReqTotal += pm.Traffic.Success[n.Job]
		a.QwenPromptTokens += pm.Traffic.PromptTok[n.Job]
		a.QwenGenTokens += pm.Traffic.GenTok[n.Job]
		a.PreemptionsTotal += pm.Traffic.Preemptions[n.Job]
	}
	if mins > 0 {
		a.QwenReqPerMin = a.QwenReqTotal / mins
	}
	a.QwenTTFTP50Avg = avgPerJob(pm.TTFT.P50, qwen)
	a.QwenTTFTP95Avg = avgPerJob(pm.TTFT.P95, qwen)
	a.QwenTTFTP99Avg = avgPerJob(pm.TTFT.P99, qwen)
	a.QwenTPOTP50Avg = avgPerJob(pm.TPOT.P50, qwen)
	a.QwenE2EP50Avg = avgPerJob(pm.E2E.P50, qwen)
	a.QwenE2EP99Avg = avgPerJob(pm.E2E.P99, qwen)
	a.QwenHitRateAvg = avgPerJob(pm.Cache.HitRate, qwen)
	a.QwenKVAvg = avgPerJob(pm.Cache.KVAvg, qwen)
	a.QwenGPUUtilAvg = avgByNvidia(pm.Hardware.GPUUtilAvg, qwen)
	a.QwenPwrCapAvg = avgByNvidia(pm.Hardware.PwrCapPct, qwen)

	for _, n := range gemma {
		a.GemmaReqTotal += pm.Traffic.Success[n.Job]
		a.PreemptionsTotal += pm.Traffic.Preemptions[n.Job]
	}
	for _, n := range emb {
		a.EmbReqTotal += pm.Traffic.Success[n.Job]
	}
	return a
}

func avgPerJob(m map[string]float64, nodes []topology.Node) float64 {
	if len(nodes) == 0 {
		return 0
	}
	var sum float64
	var n int
	for _, node := range nodes {
		v, ok := m[node.Job]
		if !ok {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func avgByNvidia(m map[string]float64, nodes []topology.Node) float64 {
	if len(nodes) == 0 {
		return 0
	}
	var sum float64
	var n int
	for _, node := range nodes {
		v, ok := m[node.NvidiaJob]
		if !ok {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}
