// Package render turns a report + analysis markdown into a PDF. The HTML is
// produced by html/template; the PDF is produced by driving a local
// headless-shell Chromium via chromedp.PrintToPDF.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/helmcode/nan-benchmarks-agent/internal/queries"
	"github.com/helmcode/nan-benchmarks-agent/internal/report"
	"github.com/helmcode/nan-benchmarks-agent/internal/topology"
	"github.com/helmcode/nan-benchmarks-agent/internal/vmclient"
)

// mdRenderer is goldmark configured with the GitHub-flavoured extensions we
// rely on for the LLM narrative: tables (the LLM emits Capacity Planning as
// markdown tables), strikethrough, autolinks and task lists.
var mdRenderer = goldmark.New(
	goldmark.WithExtensions(
		extension.Table,
		extension.Strikethrough,
		extension.Linkify,
		extension.TaskList,
	),
)


// Options for rendering.
type Options struct {
	TemplateDir  string // dir containing report.html.tmpl
	AgentVersion string
	Model        string // model used to produce the narrative (for footer)
}

// PDF renders the full benchmark PDF. The analysis is the markdown narrative
// returned by the LLM; it is converted to HTML and dropped into the cover
// page.
func PDF(ctx context.Context, r *report.Report, analysisMD string, opt Options) ([]byte, error) {
	html, err := renderHTML(r, analysisMD, opt)
	if err != nil {
		return nil, fmt.Errorf("render html: %w", err)
	}
	return htmlToPDF(ctx, html)
}

// HTML is exposed for --dry-run / debugging — write the raw HTML to disk and
// open it locally before bothering with chromium.
func HTML(r *report.Report, analysisMD string, opt Options) ([]byte, error) {
	return renderHTML(r, analysisMD, opt)
}

func renderHTML(r *report.Report, analysisMD string, opt Options) ([]byte, error) {
	tmplPath := opt.TemplateDir + "/report.html.tmpl"
	tmpl, err := template.New("report.html.tmpl").Funcs(funcMap()).ParseFiles(tmplPath)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	// Chart.js sits next to the HTML template — loading it at runtime keeps
	// `go:embed` patterns happy without restructuring the repo, and the
	// Dockerfile already copies the whole templates/ directory.
	chartJSBytes, err := os.ReadFile(opt.TemplateDir + "/chart.umd.min.js")
	if err != nil {
		return nil, fmt.Errorf("read chart.umd.min.js: %w", err)
	}

	var analysisHTML bytes.Buffer
	if err := mdRenderer.Convert([]byte(analysisMD), &analysisHTML); err != nil {
		return nil, fmt.Errorf("markdown render: %w", err)
	}

	data, err := buildTemplateData(r, template.HTML(analysisHTML.String()), opt, string(chartJSBytes))
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	return buf.Bytes(), nil
}

// htmlToPDF serves the html via a localhost HTTP listener, navigates a
// headless-shell Chromium to it, and prints to PDF.
func htmlToPDF(ctx context.Context, htmlBytes []byte) ([]byte, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(htmlBytes)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	url := "http://" + ln.Addr().String() + "/"

	// chromedp config: in the production image we run as the `nonroot` user
	// inside chromedp/headless-shell, which already has the right flags baked
	// in. When running locally outside Docker, ExecPath defaults to the
	// system Chromium.
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.Flag("headless", true),
		chromedp.Flag("hide-scrollbars", true),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer cancel()

	tabCtx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	timeoutCtx, cancel := context.WithTimeout(tabCtx, 90*time.Second)
	defer cancel()

	var pdf []byte
	err = chromedp.Run(timeoutCtx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
		// Poll the page-side sentinel set by the chart-init script. Without
		// it Chrome would PrintToPDF before Chart.js had a chance to paint
		// any of the line charts.
		chromedp.ActionFunc(func(ctx context.Context) error {
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				var ready bool
				if err := chromedp.Evaluate(`Boolean(window.__chartsReady)`, &ready).Do(ctx); err == nil && ready {
					return nil
				}
				time.Sleep(150 * time.Millisecond)
			}
			return errors.New("charts did not signal __chartsReady within 30s")
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			out, _, err := page.PrintToPDF().
				WithPrintBackground(true).
				WithPreferCSSPageSize(true).
				Do(ctx)
			if err != nil {
				return err
			}
			pdf = out
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("chromedp: %w", err)
	}
	if len(pdf) == 0 {
		return nil, errors.New("chromedp produced empty pdf")
	}
	return pdf, nil
}

// ── Template data ──

type templateData struct {
	Title             string
	ModeUpper         string
	Window            string
	WindowMinutes     float64
	GeneratedAtPretty string
	PeriodEndPretty   string
	PrevEndPretty     string
	AgentVersion      string
	Model             string

	Topology        *topology.Topology
	QwenNodes       []topology.Node
	GemmaNodes      []topology.Node
	EmbNodes        []topology.Node
	AllVllmNodes    []topology.Node // qwen + gemma + emb (everything with vllm metrics)
	GenerationNodes []topology.Node // qwen + gemma (have TPOT/E2E — embeddings don't)
	QwenCount       int
	GemmaCount      int
	EmbCount        int

	Current  *report.PeriodMetrics
	Previous *report.PeriodMetrics

	AnalysisHTML template.HTML

	HasComparison bool
	Comparisons   []ComparisonRow

	// LiteLLM counters formatted for the table.
	LiteLLM             *queries.LiteLLM
	HasLiteLLM          bool
	FallbackRows        []FallbackRow
	LiteLLMCacheHitPct  float64
	ProviderRows        []ProviderRow
	HasProviders        bool
	ProviderChartLabels template.JS // shared X-axis (timestamps)
	ProviderSeriesJSON  template.JS // []ProviderSeries marshalled

	// Chart payload — embedded into a single <script> as JSON. The template
	// reads it with `JSON.parse` so we never have to template-escape inside
	// JavaScript string literals.
	ChartJS   template.JS   // inlined chart.umd.min.js
	ChartData template.JS   // JSON marshalled chartBundle, ready to inline
	HasCharts bool
}

// FallbackRow is one line in the LiteLLM fallbacks-per-deployment table.
type FallbackRow struct {
	Deployment string
	Fallbacks  string
	Cooldowns  string
}

// ProviderRow is one line in the "external providers" table. Strings are
// pre-formatted so the template only handles layout.
type ProviderRow struct {
	Provider    string
	Model       string
	Requests    string
	ErrorRate   string
	ErrorClass  string  // delta-good / delta-flat / delta-bad
	InputTok    string
	OutputTok   string
	CachedTok   string
	LatencyP50  string
	LatencyP95  string
	IsSlowest   bool
	IsErrorest  bool
}

// ProviderSeries is one labelled line in the provider time chart.
type ProviderSeries struct {
	Label string  // "deepinfra · DeepSeek-V4-Flash"
	Color string  // "#b095ff"
	Data  []float64
}

// chartSeries is what the page-side JS reads to draw one line. Labels are
// short timestamps formatted for the X axis ("Mon 14", "06:00", ...).
type chartSeries struct {
	Labels []string  `json:"labels"`
	Data   []float64 `json:"data"`
}

// chartBundle is the JSON payload the template hands over to Chart.js. Every
// field is one canvas in the PDF. nil-valued fields are rendered as empty
// charts so the layout stays stable.
type chartBundle struct {
	ReqPerMin          chartSeries `json:"reqPerMin"`
	TTFTp50            chartSeries `json:"ttftP50"`
	TTFTp95            chartSeries `json:"ttftP95"`
	TTFTp99            chartSeries `json:"ttftP99"`
	TPOTp50            chartSeries `json:"tpotP50"`
	TPOTp95            chartSeries `json:"tpotP95"`
	E2Ep50             chartSeries `json:"e2eP50"`
	E2Ep95             chartSeries `json:"e2eP95"`
	PrefixHitPct       chartSeries `json:"prefixHitPct"`
	KVAvgPct           chartSeries `json:"kvAvgPct"`
	GPUUtilPct         chartSeries `json:"gpuUtilPct"`
	PwrCapPct          chartSeries `json:"pwrCapPct"`
	LiteLLMOverheadP50 chartSeries `json:"litellmOverheadP50"`
	LiteLLMOverheadP95 chartSeries `json:"litellmOverheadP95"`
	LiteLLMApiLatP50   chartSeries `json:"litellmApiLatP50"`
	LiteLLMApiLatP95   chartSeries `json:"litellmApiLatP95"`
}

// ComparisonRow is one line in the "vs previous window" table.
type ComparisonRow struct {
	Label       string
	Current     string
	Previous    string
	DeltaPretty string
	DeltaClass  string // delta-good | delta-bad | delta-flat
}

func buildTemplateData(r *report.Report, analysisHTML template.HTML, opt Options, chartJS string) (*templateData, error) {
	mins, err := parseWindowMinutes(r.Window)
	if err != nil {
		return nil, err
	}

	qwen := r.Topology.ByFamily(topology.FamilyQwen)
	gemma := r.Topology.ByFamily(topology.FamilyGemma)
	emb := r.Topology.ByFamily(topology.FamilyEmbedding)

	d := &templateData{
		Title:             fmt.Sprintf("NaN Cluster — %s benchmark", r.Mode),
		ModeUpper:         strings.ToUpper(string(r.Mode)),
		Window:            r.Window,
		WindowMinutes:     mins,
		GeneratedAtPretty: r.GeneratedAt.UTC().Format("2006-01-02 15:04"),
		PeriodEndPretty:   r.PeriodEnd.UTC().Format("2006-01-02 15:04"),
		PrevEndPretty:     r.PrevEnd.UTC().Format("2006-01-02 15:04"),
		AgentVersion:      opt.AgentVersion,
		Model:             opt.Model,
		Topology:          r.Topology,
		QwenNodes:         qwen,
		GemmaNodes:        gemma,
		EmbNodes:          emb,
		AllVllmNodes:      append(append(append([]topology.Node{}, qwen...), gemma...), emb...),
		GenerationNodes:   append(append([]topology.Node{}, qwen...), gemma...),
		QwenCount:         len(qwen),
		GemmaCount:        len(gemma),
		EmbCount:          len(emb),
		Current:           r.Current,
		Previous:          r.Previous,
		AnalysisHTML:      analysisHTML,
	}

	if r.Previous != nil && r.Previous.Traffic != nil {
		d.HasComparison = true
		d.Comparisons = comparisonRows(r.Current.Aggregates, r.Previous.Aggregates)
	}

	// LiteLLM counters + per-deployment breakdown for the table.
	if r.Current.LiteLLM != nil {
		d.LiteLLM = r.Current.LiteLLM
		d.HasLiteLLM = r.Current.LiteLLM.SuccessRequests > 0 ||
			r.Current.LiteLLM.SuccessfulFallbacks > 0 ||
			r.Current.LiteLLM.CacheHits > 0
		d.FallbackRows = fallbackRows(r.Current.LiteLLM)
		if hits, miss := r.Current.LiteLLM.CacheHits, r.Current.LiteLLM.CacheMisses; hits+miss > 0 {
			d.LiteLLMCacheHitPct = hits / (hits + miss) * 100
		}
		d.ProviderRows = providerRows(r.Current.LiteLLM.Providers)
		d.HasProviders = len(d.ProviderRows) > 0

		if r.Current.Series != nil && len(r.Current.Series.ProviderReqPerMin) > 0 {
			labels, seriesJSON, err := buildProviderChart(r.Current.Series)
			if err == nil {
				d.ProviderChartLabels = template.JS(labels)
				d.ProviderSeriesJSON = template.JS(seriesJSON)
			}
		}
	}

	// Chart bundle.
	d.ChartJS = template.JS(chartJS) //nolint:gosec  // bundled Chart.js from CDN, embed-only
	bundle := buildChartBundle(r.Current)
	bts, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("chart bundle marshal: %w", err)
	}
	d.ChartData = template.JS(bts)
	d.HasCharts = r.Current.Series != nil

	return d, nil
}

// providerRows turns the raw Provider list into pre-formatted rows. The
// row whose p95 latency is the worst gets IsSlowest=true so the template
// can flag it visually; same for the highest error rate.
func providerRows(in []queries.Provider) []ProviderRow {
	if len(in) == 0 {
		return nil
	}
	var slowestIdx, errorestIdx int
	var slowestVal, errorestVal float64
	for i, p := range in {
		if p.LatencyP95 > slowestVal {
			slowestVal = p.LatencyP95
			slowestIdx = i
		}
		er := p.ErrorRate()
		if er > errorestVal {
			errorestVal = er
			errorestIdx = i
		}
	}

	out := make([]ProviderRow, len(in))
	for i, p := range in {
		er := p.ErrorRate()
		class := "delta-flat"
		switch {
		case er == 0:
			class = "delta-good"
		case er >= 5:
			class = "delta-bad"
		case er >= 1:
			class = "delta-flat"
		default:
			class = "delta-good"
		}
		out[i] = ProviderRow{
			Provider:   p.APIProvider,
			Model:      p.ModelName,
			Requests:   humanIntStr(p.Requests),
			ErrorRate:  fmt.Sprintf("%.2f%%", er),
			ErrorClass: class,
			InputTok:   humanIntStr(p.InputTokens),
			OutputTok:  humanIntStr(p.OutputTokens),
			CachedTok:  humanIntStr(p.CachedTokens),
			LatencyP50: fmt.Sprintf("%.2fs", p.LatencyP50),
			LatencyP95: fmt.Sprintf("%.2fs", p.LatencyP95),
			IsSlowest:  i == slowestIdx && slowestVal > 0 && len(in) > 1,
			IsErrorest: i == errorestIdx && errorestVal > 0 && len(in) > 1,
		}
	}
	return out
}

// humanIntStr is funcMap's "humanInt" but callable from Go-land (we use it
// to pre-format ProviderRow strings outside of the template).
func humanIntStr(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.2fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fK", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

// buildProviderChart turns the per-provider point streams into a Chart.js-
// friendly (labels, series) pair. Labels come from the busiest series so
// the X axis covers the full window even if a fallback only fired briefly.
func buildProviderChart(ts *queries.TimeSeries) (labelsJSON, seriesJSON string, err error) {
	if ts == nil || len(ts.ProviderReqPerMin) == 0 {
		return "[]", "[]", nil
	}
	// Pick the busiest series for the shared X axis so gaps don't shift
	// the timeline.
	var primaryKey string
	var primaryLen int
	for k, pts := range ts.ProviderReqPerMin {
		if len(pts) > primaryLen {
			primaryLen = len(pts)
			primaryKey = k
		}
	}
	labels := make([]string, 0, primaryLen)
	for _, p := range ts.ProviderReqPerMin[primaryKey] {
		labels = append(labels, chartLabel(p.T, ts.Step))
	}

	// Stable ordering: sort keys alphabetically so the legend doesn't
	// shuffle between runs.
	keys := make([]string, 0, len(ts.ProviderReqPerMin))
	for k := range ts.ProviderReqPerMin {
		keys = append(keys, k)
	}
	sortStrings(keys)

	palette := []string{"#b095ff", "#ffaf60", "#5ad9d9", "#5ad97a", "#ff7e85", "#a0c4ff"}
	series := make([]ProviderSeries, 0, len(keys))
	for i, k := range keys {
		// key format is "<provider>:<model>" — render as "provider · short(model)"
		provider, model := splitKey(k)
		label := provider + " · " + shortModel(model)
		data := alignToLabels(ts.ProviderReqPerMin[k], len(labels))
		series = append(series, ProviderSeries{
			Label: label,
			Color: palette[i%len(palette)],
			Data:  data,
		})
	}

	lbytes, err := json.Marshal(labels)
	if err != nil {
		return "", "", err
	}
	sbytes, err := json.Marshal(series)
	if err != nil {
		return "", "", err
	}
	return string(lbytes), string(sbytes), nil
}

func splitKey(k string) (provider, model string) {
	if i := strings.Index(k, ":"); i >= 0 {
		return k[:i], k[i+1:]
	}
	return "", k
}

// shortModel strips the namespace prefix so legend labels stay compact:
// "deepseek-ai/DeepSeek-V4-Flash" → "DeepSeek-V4-Flash".
func shortModel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

// alignToLabels right-pads or truncates a point stream so it lines up with
// the shared X-axis label count. Missing points become NaN so Chart.js
// renders a gap; we encode them as 0 here because Chart.js' `spanGaps:
// true` with explicit nulls would require a JSON null which is uglier to
// emit; the visual is OK either way.
func alignToLabels(pts []vmclient.Point, n int) []float64 {
	out := make([]float64, n)
	for i := 0; i < n && i < len(pts); i++ {
		v := pts[i].V
		if math.IsNaN(v) || math.IsInf(v, 0) {
			out[i] = 0
		} else {
			out[i] = v
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// fallbackRows merges the fallback-success and cooldown maps into one
// deployment-keyed table. Deployments with zero of both are dropped.
func fallbackRows(l *queries.LiteLLM) []FallbackRow {
	keys := map[string]struct{}{}
	for k := range l.FallbacksByDeploy {
		keys[k] = struct{}{}
	}
	for k := range l.CooldownsByDeploy {
		keys[k] = struct{}{}
	}
	rows := make([]FallbackRow, 0, len(keys))
	for k := range keys {
		fb := l.FallbacksByDeploy[k]
		cd := l.CooldownsByDeploy[k]
		if fb == 0 && cd == 0 {
			continue
		}
		rows = append(rows, FallbackRow{
			Deployment: k,
			Fallbacks:  fmt.Sprintf("%.0f", fb),
			Cooldowns:  fmt.Sprintf("%.0f", cd),
		})
	}
	return rows
}

// chartLabel picks a short label for one timestamp depending on how wide the
// window is. For a 7-day weekly we emit weekday + day-of-month + hour ("Mon
// 14 09:00"); for a 30-day monthly only the date ("Aug 14").
func chartLabel(t time.Time, step time.Duration) string {
	if step >= 6*time.Hour {
		return t.Format("Jan 02")
	}
	return t.Format("Mon 02 15:04")
}

// pointsToSeries converts vmclient.Points into the Chart.js-friendly
// (labels, data) pair. NaN/Inf are dropped silently — Chart.js draws a gap.
func pointsToSeries(pts []vmclient.Point, step time.Duration) chartSeries {
	// Return non-nil slices even when empty so the JSON the template hands to
	// Chart.js is `{"labels":[],"data":[]}` rather than `null` — the latter
	// makes the chart-init script throw and breaks PDF rendering.
	if len(pts) == 0 {
		return chartSeries{Labels: []string{}, Data: []float64{}}
	}
	labels := make([]string, 0, len(pts))
	values := make([]float64, 0, len(pts))
	for _, p := range pts {
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
			continue
		}
		labels = append(labels, chartLabel(p.T, step))
		values = append(values, p.V)
	}
	return chartSeries{Labels: labels, Data: values}
}

// pointsToSeriesMs is like pointsToSeries but multiplies values by 1000 —
// convenient for histograms that return seconds (TPOT) when we want
// milliseconds on the Y axis.
func pointsToSeriesMs(pts []vmclient.Point, step time.Duration) chartSeries {
	s := pointsToSeries(pts, step)
	for i := range s.Data {
		s.Data[i] *= 1000
	}
	return s
}

func buildChartBundle(pm *report.PeriodMetrics) chartBundle {
	if pm == nil || pm.Series == nil {
		return emptyChartBundle()
	}
	ts := pm.Series
	step := ts.Step
	return chartBundle{
		ReqPerMin:          pointsToSeries(ts.ReqPerMinTotal, step),
		TTFTp50:            pointsToSeries(ts.TTFTp50, step),
		TTFTp95:            pointsToSeries(ts.TTFTp95, step),
		TTFTp99:            pointsToSeries(ts.TTFTp99, step),
		TPOTp50:            pointsToSeriesMs(ts.TPOTp50, step),
		TPOTp95:            pointsToSeriesMs(ts.TPOTp95, step),
		E2Ep50:             pointsToSeries(ts.E2Ep50, step),
		E2Ep95:             pointsToSeries(ts.E2Ep95, step),
		PrefixHitPct:       pointsToSeries(ts.PrefixHitPct, step),
		KVAvgPct:           pointsToSeries(ts.KVAvgPct, step),
		GPUUtilPct:         pointsToSeries(ts.GPUUtilPct, step),
		PwrCapPct:          pointsToSeries(ts.PwrCapPct, step),
		LiteLLMOverheadP50: pointsToSeriesMs(ts.LiteLLMOverheadP50, step),
		LiteLLMOverheadP95: pointsToSeriesMs(ts.LiteLLMOverheadP95, step),
		LiteLLMApiLatP50:   pointsToSeries(ts.LiteLLMApiLatP50, step),
		LiteLLMApiLatP95:   pointsToSeries(ts.LiteLLMApiLatP95, step),
	}
}

// emptyChartBundle is returned when the time-series collection failed (e.g.
// a transient vmselect blip). Every field is a zero-length but non-nil
// chartSeries so the template's Chart.js init script doesn't trip over nulls.
func emptyChartBundle() chartBundle {
	empty := chartSeries{Labels: []string{}, Data: []float64{}}
	return chartBundle{
		ReqPerMin:          empty,
		TTFTp50:            empty,
		TTFTp95:            empty,
		TTFTp99:            empty,
		TPOTp50:            empty,
		TPOTp95:            empty,
		E2Ep50:             empty,
		E2Ep95:             empty,
		PrefixHitPct:       empty,
		KVAvgPct:           empty,
		GPUUtilPct:         empty,
		PwrCapPct:          empty,
		LiteLLMOverheadP50: empty,
		LiteLLMOverheadP95: empty,
		LiteLLMApiLatP50:   empty,
		LiteLLMApiLatP95:   empty,
	}
}

func comparisonRows(cur, prev report.Aggregates) []ComparisonRow {
	type spec struct {
		label       string
		cur, prev   float64
		fmt         string
		lowerBetter bool
	}
	specs := []spec{
		{"Solicitudes Qwen3.6", cur.QwenReqTotal, prev.QwenReqTotal, "%.0f", false},
		{"Qwen3.6 req/min", cur.QwenReqPerMin, prev.QwenReqPerMin, "%.1f", false},
		{"Tokens de prompt", cur.QwenPromptTokens, prev.QwenPromptTokens, "%.0f", false},
		{"Tokens generados", cur.QwenGenTokens, prev.QwenGenTokens, "%.0f", false},
		{"TTFT p50 (s)", cur.QwenTTFTP50Avg, prev.QwenTTFTP50Avg, "%.2f", true},
		{"TTFT p99 (s)", cur.QwenTTFTP99Avg, prev.QwenTTFTP99Avg, "%.2f", true},
		{"TPOT p50 (ms)", cur.QwenTPOTP50Avg * 1000, prev.QwenTPOTP50Avg * 1000, "%.1f", true},
		{"E2E p50 (s)", cur.QwenE2EP50Avg, prev.QwenE2EP50Avg, "%.2f", true},
		{"E2E p99 (s)", cur.QwenE2EP99Avg, prev.QwenE2EP99Avg, "%.2f", true},
		{"Prefix hit %", cur.QwenHitRateAvg, prev.QwenHitRateAvg, "%.1f", false},
		{"KV cache media %", cur.QwenKVAvg, prev.QwenKVAvg, "%.2f", false},
		{"GPU util %", cur.QwenGPUUtilAvg, prev.QwenGPUUtilAvg, "%.1f", false},
		{"SW power-cap %", cur.QwenPwrCapAvg, prev.QwenPwrCapAvg, "%.1f", true},
		{"Preemptions", cur.PreemptionsTotal, prev.PreemptionsTotal, "%.0f", true},
		{"Solicitudes Gemma", cur.GemmaReqTotal, prev.GemmaReqTotal, "%.0f", false},
		{"Solicitudes Embedding", cur.EmbReqTotal, prev.EmbReqTotal, "%.0f", false},
	}

	rows := make([]ComparisonRow, 0, len(specs))
	for _, s := range specs {
		var delta string
		class := "delta-flat"
		if s.prev == 0 {
			delta = "—"
		} else {
			pct := (s.cur - s.prev) / s.prev * 100
			delta = fmt.Sprintf("%+.1f%%", pct)
			improved := pct < 0
			if !s.lowerBetter {
				improved = pct > 0
			}
			switch {
			case math.Abs(pct) < 2:
				class = "delta-flat"
			case improved:
				class = "delta-good"
			default:
				class = "delta-bad"
			}
		}
		rows = append(rows, ComparisonRow{
			Label:       s.label,
			Current:     fmt.Sprintf(s.fmt, s.cur),
			Previous:    fmt.Sprintf(s.fmt, s.prev),
			DeltaPretty: delta,
			DeltaClass:  class,
		})
	}
	return rows
}

func parseWindowMinutes(w string) (float64, error) {
	if len(w) < 2 {
		return 0, fmt.Errorf("invalid window %q", w)
	}
	var n float64
	if _, err := fmt.Sscanf(w[:len(w)-1], "%g", &n); err != nil {
		return 0, err
	}
	switch w[len(w)-1] {
	case 'h':
		return n * 60, nil
	case 'd':
		return n * 24 * 60, nil
	case 'w':
		return n * 7 * 24 * 60, nil
	}
	return 0, fmt.Errorf("invalid window unit in %q", w)
}

// ── Template helpers ──

func funcMap() template.FuncMap {
	return template.FuncMap{
		"humanInt": func(v float64) string {
			switch {
			case v >= 1e9:
				return fmt.Sprintf("%.2fB", v/1e9)
			case v >= 1e6:
				return fmt.Sprintf("%.1fM", v/1e6)
			case v >= 1e3:
				return fmt.Sprintf("%.1fK", v/1e3)
			}
			return fmt.Sprintf("%.0f", v)
		},
		"divFloat": func(a, b float64) float64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"pctOf": func(a, b float64) float64 {
			if b == 0 {
				return 0
			}
			return a / b * 100
		},
		"tpotMs": func(v float64) string {
			if v <= 0 {
				return "—"
			}
			return fmt.Sprintf("%.1fms (~%d tok/s)", v*1000, int(1/v))
		},
		"mul": func(a, b float64) float64 { return a * b },
		"familyClass": func(f topology.Family) string {
			switch f {
			case topology.FamilyQwen:
				return "qwen"
			case topology.FamilyGemma:
				return "gemma"
			case topology.FamilyEmbedding:
				return "embed"
			}
			return ""
		},
	}
}
