// Package render turns a report + analysis markdown into a PDF. The HTML is
// produced by html/template; the PDF is produced by driving a local
// headless-shell Chromium via chromedp.PrintToPDF.
package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/helmcode/nan-benchmarks-agent/internal/report"
	"github.com/helmcode/nan-benchmarks-agent/internal/topology"
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

	var analysisHTML bytes.Buffer
	if err := mdRenderer.Convert([]byte(analysisMD), &analysisHTML); err != nil {
		return nil, fmt.Errorf("markdown render: %w", err)
	}

	data, err := buildTemplateData(r, template.HTML(analysisHTML.String()), opt)
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
}

// ComparisonRow is one line in the "vs previous window" table.
type ComparisonRow struct {
	Label       string
	Current     string
	Previous    string
	DeltaPretty string
	DeltaClass  string // delta-good | delta-bad | delta-flat
}

func buildTemplateData(r *report.Report, analysisHTML template.HTML, opt Options) (*templateData, error) {
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
	return d, nil
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
