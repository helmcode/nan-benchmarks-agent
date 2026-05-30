FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN VERSION=$(cat VERSION) && \
    CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w -X main.Version=${VERSION}" \
      -o /bench-agent ./cmd/bench-agent

# --- Runtime ---
# chromedp/headless-shell ships a minimal Chromium headless build that
# chromedp talks to over CDP. ~190 MB image vs ~700 MB for full Chrome.
FROM chromedp/headless-shell:latest

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates dumb-init && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd -r -g 65532 nonroot && \
    useradd -r -u 65532 -g nonroot -d /home/nonroot -m nonroot

COPY --from=builder /bench-agent /usr/local/bin/bench-agent
COPY templates /opt/bench-agent/templates

USER nonroot:nonroot
WORKDIR /home/nonroot

ENV BENCH_TEMPLATES_DIR=/opt/bench-agent/templates

# dumb-init reaps zombie chromium processes when the agent exits.
ENTRYPOINT ["dumb-init", "--", "bench-agent"]
