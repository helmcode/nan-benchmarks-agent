// nan-bench-agent — generates a weekly or monthly performance report for the
// NaN cluster and uploads it to Slack. See README.md for the full pipeline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/helmcode/nan-benchmarks-agent/internal/analysis"
	"github.com/helmcode/nan-benchmarks-agent/internal/render"
	"github.com/helmcode/nan-benchmarks-agent/internal/report"
	"github.com/helmcode/nan-benchmarks-agent/internal/slack"
	"github.com/helmcode/nan-benchmarks-agent/internal/vmclient"
)

// Version is injected at build time via -ldflags="-X main.Version=...".
var Version = "dev"

func main() {
	mode := flag.String("mode", "weekly", "benchmark cadence: weekly | monthly")
	dryRun := flag.Bool("dry-run", false, "skip Slack upload, write PDF (and HTML if --html) to --out")
	dumpHTML := flag.Bool("html", false, "also write the intermediate HTML next to the PDF (--dry-run only)")
	skipLLM := flag.Bool("skip-llm", false, "skip the LLM narrative call, use a placeholder (debugging only)")
	out := flag.String("out", "/tmp/bench.pdf", "output path when --dry-run")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	m := report.Mode(*mode)
	if m != report.ModeWeekly && m != report.ModeMonthly {
		fail("invalid --mode %q (must be 'weekly' or 'monthly')", *mode)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, m, *dryRun, *dumpHTML, *skipLLM, *out); err != nil {
		slog.Error("run failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, mode report.Mode, dryRun, dumpHTML, skipLLM bool, out string) error {
	vmURL := os.Getenv("VM_URL")
	apiKey := os.Getenv("NAN_API_KEY")
	apiURL := os.Getenv("NAN_API_URL")
	apiModel := os.Getenv("NAN_API_MODEL")
	tmplDir := envOr("BENCH_TEMPLATES_DIR", "templates")

	if vmURL == "" {
		return fmt.Errorf("VM_URL is required (the in-cluster VictoriaMetrics vmselect endpoint)")
	}
	if !skipLLM {
		if apiKey == "" {
			return fmt.Errorf("NAN_API_KEY is required (or use --skip-llm for debugging)")
		}
		if apiURL == "" {
			return fmt.Errorf("NAN_API_URL is required (OpenAI-compatible chat completions endpoint)")
		}
		if apiModel == "" {
			return fmt.Errorf("NAN_API_MODEL is required (model id, e.g. deepseek-v4-flash)")
		}
	}

	slog.Info("starting",
		"version", Version, "mode", mode, "window", mode.Window(),
		"vm_url", vmURL, "api_model", apiModel)

	// 1) collect
	vm := vmclient.New(vmURL)
	rep, err := report.Build(ctx, vm, mode, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	slog.Info("collected",
		"nodes", len(rep.Topology.Nodes),
		"qwen_req", rep.Current.Aggregates.QwenReqTotal,
		"qwen_req_per_min", rep.Current.Aggregates.QwenReqPerMin)

	// 2) analyze
	var narrative string
	if skipLLM {
		narrative = "## Executive Summary\n\n_LLM narrative skipped (`--skip-llm`)._ See the data tables on the following pages.\n\n" +
			"## Key Findings\n\n_Skipped._\n\n## Trade-offs / Risks\n\n_Skipped._\n\n" +
			"## Capacity Planning\n\n_Skipped._\n\n## Recommendations\n\n_Skipped._\n"
		slog.Info("narrative skipped (--skip-llm)")
	} else {
		llm := analysis.New(apiURL, apiKey, apiModel)
		var err error
		narrative, err = llm.Analyze(ctx, rep)
		if err != nil {
			return fmt.Errorf("analyze: %w", err)
		}
		slog.Info("narrative produced", "chars", len(narrative))
	}

	// 3) render
	pdf, err := render.PDF(ctx, rep, narrative, render.Options{
		TemplateDir:  tmplDir,
		AgentVersion: Version,
		Model:        apiModel,
	})
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	slog.Info("pdf rendered", "bytes", len(pdf))

	// 4) publish
	if dryRun {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, pdf, 0o644); err != nil {
			return err
		}
		slog.Info("dry-run: pdf written", "path", out)
		if dumpHTML {
			html, err := render.HTML(rep, narrative, render.Options{
				TemplateDir: tmplDir, AgentVersion: Version, Model: apiModel,
			})
			if err != nil {
				return err
			}
			htmlPath := out + ".html"
			if err := os.WriteFile(htmlPath, html, 0o644); err != nil {
				return err
			}
			slog.Info("dry-run: html written", "path", htmlPath)
		}
		return nil
	}

	channel := os.Getenv("SLACK_CHANNEL")
	token := os.Getenv("SLACK_BOT_TOKEN")
	if channel == "" || token == "" {
		return fmt.Errorf("SLACK_CHANNEL and SLACK_BOT_TOKEN are required when not --dry-run")
	}
	uploader := slack.New(token, channel)
	modeES := map[report.Mode]string{
		report.ModeWeekly:  "semanal",
		report.ModeMonthly: "mensual",
	}[mode]
	if modeES == "" {
		modeES = string(mode)
	}
	filename := fmt.Sprintf("nan-bench-%s-%s.pdf", mode, time.Now().UTC().Format("2006-01-02"))
	title := fmt.Sprintf("Benchmark %s del cluster NaN — %s", modeES, time.Now().UTC().Format("2006-01-02"))
	comment := fmt.Sprintf(
		":bar_chart: Benchmark %s del cluster NaN — ventana %s, %d nodos, "+
			"%.0f req/min en qwen3.6, TTFT p50 %.2fs.",
		modeES, mode.Window(), len(rep.Topology.Nodes),
		rep.Current.Aggregates.QwenReqPerMin,
		rep.Current.Aggregates.QwenTTFTP50Avg,
	)
	if err := uploader.Upload(ctx, pdf, filename, title, comment); err != nil {
		return fmt.Errorf("slack upload: %w", err)
	}
	slog.Info("uploaded to slack", "channel", channel, "filename", filename)
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
