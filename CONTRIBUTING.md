# Contributing to nan-benchmarks-agent

Thanks for your interest in contributing! We favor small, focused PRs and clear
intent over big bangs. This guide explains how to get set up and the workflow
we use.

## Quick Start

Prerequisites

- **Go 1.26+** (the build also pulls a chromedp-compatible Chromium at runtime;
  outside the container you just need a system Chromium for local PDF rendering)
- Git
- `kubectl` configured against a cluster running VictoriaMetrics, plus
  whatever credentials you need to port-forward it
- An OpenAI-compatible chat-completions endpoint and API key for the LLM
  narrative (any OpenAI-compatible provider works — point `NAN_API_URL` at it)

Setup

```bash
git clone https://github.com/<you>/nan-benchmarks-agent.git
cd nan-benchmarks-agent

go mod download

# Port-forward your VictoriaMetrics vmselect to localhost
kubectl -n <monitoring-namespace> port-forward svc/<vmselect-service> 18481:8481 &

export VM_URL=http://127.0.0.1:18481
export NAN_API_URL=https://your.api.example/v1
export NAN_API_KEY=...
export NAN_API_MODEL=deepseek-v4-flash

# Dry-run writes the PDF to disk instead of uploading it to Slack
go run ./cmd/bench-agent --mode=weekly --dry-run --out /tmp/report.pdf

# Add --html to also dump the intermediate HTML for inspection
go run ./cmd/bench-agent --mode=weekly --dry-run --html --out /tmp/report.pdf

# Use --skip-llm to bypass the narrative entirely when iterating on the
# template or queries
go run ./cmd/bench-agent --mode=weekly --dry-run --skip-llm --out /tmp/report.pdf
```

See `README.md` for the full list of environment variables and the deployment
overview.

## Development Workflow

1. **Create a feature branch**

   ```
   git checkout -b feat/<short-slug>
   ```

2. **Make changes and keep PRs small and focused**

   - Prefer a series of small PRs over one large one.
   - Update `README.md` when behavior or configuration changes.
   - If you touch the Helm chart, also update the corresponding files in
     [helmcode/nan-devops](https://github.com/helmcode/nan-devops) under
     `helm/nan-benchmarks-agent/`. The image tag there is rewritten
     automatically by the deploy workflow on every `VERSION` bump.

3. **Run checks locally before opening a PR**

   ```bash
   go build ./...            # must compile
   go vet ./...              # static checks (must pass)
   gofmt -l .                # formatting drift (must print nothing)
   go test ./...             # tests (must pass)
   ```

   To apply formatting fixes in place:

   ```bash
   gofmt -w .
   ```

4. **Commit using Conventional Commits**

   - `feat:` / `fix:` / `chore:` / `refactor:` / `docs:` / `perf:` / `test:` ...

   Example: `feat(render): paginate the data tables when more than 12 nodes`

5. **Open a Pull Request**

   - Describe the change, rationale, and testing steps.
   - Link related Issues.
   - Keep the PR title in Conventional Commit format.

## Testing

**Every new feature, fix, or refactor must ship with tests.** PRs that add
functionality without tests will not be merged.

- The project uses the standard `testing` package — no external test runner.
- Place new tests under the same package as the code they cover
  (`internal/queries/queries_test.go`, `internal/topology/topology_test.go`,
  ...).
- For pure logic (window math, label classification, hardware-job pairing,
  comparison-delta computation): table-driven tests with explicit fixtures.
  Cover positive cases, negative cases, and edge cases (empty input, missing
  labels, NaN/Inf values from PromQL).
- For HTTP clients (`vmclient`, `analysis`, `slack`): mock the network with
  `net/http/httptest`. Do not hit live VictoriaMetrics, the LLM endpoint or
  Slack in tests.
- For the report builder: feed canned `queries.Traffic` / `queries.Latency`
  / `queries.Cache` / `queries.Hardware` structs and assert on the
  `Aggregates` and on the `ComparisonRow` output.
- For the renderer: write a small fixture report, render to HTML (not PDF),
  and parse the HTML to assert on table contents — skip the chromedp step in
  CI, it is exercised end-to-end by the smoke job after deploy.

```bash
go test ./...                   # full suite
go test ./internal/queries -v   # one package, verbose
go test -run TestClassify ./... # filter by test name
go test -race ./...             # race detector (recommended)
```

## Code Style

- Follow the existing style in the codebase.
- `gofmt` is the formatter — `go vet` is the linter floor. CI runs both.
- Use the standard library wherever possible. Pull a third-party dependency
  only when it materially improves the project (eg. `chromedp` for headless
  Chromium, `goldmark` for GFM markdown, `slack-go` for Slack uploads). Pin
  the version in `go.mod`.
- Public identifiers carry a doc comment that starts with the identifier
  name. Internal helpers can skip the comment if the name is already clear.
- Never commit secrets, API keys, bot tokens or kubeconfigs. Use environment
  variables locally and the cluster's Secret store for deploys. The
  repository is **public** — assume anything checked in is world-readable
  forever.

## Issue Reports and Feature Requests

Use GitHub Issues. Include Go version, OS, steps to reproduce, relevant
logs (with secrets redacted), and the commit SHA the binary was built from.
