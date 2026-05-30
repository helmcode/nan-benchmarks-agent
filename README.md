# nan-benchmarks-agent

Stateless Go agent that generates weekly and monthly performance reports for
an AI inference cluster running vLLM + LiteLLM. Designed to run as a Kubernetes
CronJob. Queries VictoriaMetrics in-cluster, asks an OpenAI-compatible chat
endpoint for an executive narrative, renders a PDF and uploads it to Slack.

## Pipeline

```
collect  → vmclient runs PromQL queries against your VictoriaMetrics vmselect
compute  → report builder aggregates per-backend and per-fleet, computes deltas
analyze  → analysis client posts metrics JSON to a chat-completions endpoint
render   → html/template + chromedp produce a styled PDF
publish  → slack uploader pushes the PDF to a target channel
```

The agent is **stateless**: each run recomputes the previous window from
VictoriaMetrics directly, no persistence layer.

## Modes

| Mode | Window | Comparison window |
|---|---|---|
| `--mode=weekly` | last 7 days | previous 7 days |
| `--mode=monthly` | last 30 days | previous 30 days |

## Required environment

| Variable | Purpose |
|---|---|
| `VM_URL` | Base URL of your VictoriaMetrics select endpoint (cluster-internal Service) |
| `NAN_API_KEY` | API key for the narrative LLM call |
| `NAN_API_URL` | OpenAI-compatible chat-completions base URL (must end in `/v1`) |
| `NAN_API_MODEL` | Model id used for the narrative, e.g. `deepseek-v4-flash` |
| `SLACK_BOT_TOKEN` | Slack bot token with `files:write` (only required when not `--dry-run`) |
| `SLACK_CHANNEL` | Channel ID (preferred) or name |
| `BENCH_TEMPLATES_DIR` | Path to the HTML templates (`templates/` by default) |

## Local run

```bash
# Port-forward your VictoriaMetrics vmselect to localhost
kubectl -n <monitoring-namespace> port-forward svc/<vmselect-service> 18481:8481 &

export VM_URL=http://127.0.0.1:18481
export NAN_API_KEY=...
export NAN_API_URL=https://your.api.example/v1
export NAN_API_MODEL=deepseek-v4-flash

# dry-run skips Slack upload and writes the PDF to disk
go run ./cmd/bench-agent --mode=weekly --dry-run --out /tmp/report.pdf

# add --html to also dump the intermediate HTML for inspection
go run ./cmd/bench-agent --mode=weekly --dry-run --html --out /tmp/report.pdf

# use --skip-llm to bypass the narrative entirely (debugging only)
go run ./cmd/bench-agent --mode=weekly --dry-run --skip-llm --out /tmp/report.pdf
```

## Deploy

The container image is published to `ghcr.io` by this repository's GitHub
Actions on every push that bumps `VERSION`. A separate Helm chart deploys it
as a CronJob — see the operator's devops repository for the chart and the
ArgoCD Application.

## How it discovers the cluster

The agent does **not** ship with hardcoded node names. On every run it
queries `vllm:num_requests_running` and reads the `job`, `node`, `model_name`
and `instance` labels from the live metric series to enumerate the live
backends. Each backend is classified as `qwen3.6`, `gemma4`, `embedding` or
`unknown` based on substring matching against `model_name`. The hardware-job
pairing is derived from the inference job's suffix (a job named
`vllm-<suffix>` is paired with `nvidia_gpu-<suffix>`).

Adding a new GPU backend requires no code change — once vmagent scrapes it,
the next benchmark picks it up automatically.

## Repository layout

```
cmd/bench-agent/         entry point with CLI flags
internal/vmclient/       VictoriaMetrics PromQL client
internal/queries/        the catalogue of metric queries
internal/topology/       live auto-discovery + family classification
internal/report/         dataset builder with previous-window comparison
internal/analysis/       chat-completions client + few-shot examples + prompt
internal/render/         Go templates + chromedp for PDF generation
internal/slack/          Slack files.uploadV2 wrapper
templates/               HTML/CSS for the PDF
```
