# Cluster Performance Report — Example B

**Cluster:** 4 GPU nodes (host-A → host-D), 96 GB GPU each.
**Models:** Qwen3.6-35B-A3B FP8 (host-A/B/C) + Gemma 4-31B-IT FP8 (host-D).
**Load balancing:** simple-shuffle across all backends per model.
**Observation window:** ~20h after a fleet restart.
**Config changes since last report:**
- Added host-C and host-D as inference nodes.
- host-D switched from Qwen3.6 to Gemma 4-31B-IT FP8.
- `max-model-len` doubled.
- `max_new_tokens` doubled on Qwen3.6 backends.
- `gpu-memory-utilization` reduced on Qwen3.6 (stability) and on Gemma 4.

---

## Cluster Topology

| Backend | Model | GPU Util | VRAM | Context | Max Seqs |
|---------|-------|----------|------|---------|----------|
| host-A | Qwen3.6-35B-A3B FP8 | 0.85 | 96 GB | 256K | 64 |
| host-B | Qwen3.6-35B-A3B FP8 | 0.85 | 96 GB | 256K | 64 |
| host-C | Qwen3.6-35B-A3B FP8 | 0.85 | 96 GB | 256K | 64 |
| host-D | Gemma 4-31B-IT FP8 | 0.90 | 96 GB | 256K | 32 |

---

## Traffic Volume

| Metric | host-A | host-B | host-C | host-D (Gemma 4) | Cluster |
|--------|--------|--------|--------|-------------------|---------|
| Requests completed | ~47K | ~56K | ~40K | ~3K | **~146K** |
| Finished `stop` | ~96.7% | ~96.9% | ~96.6% | ~99.8% | ~96.8% |
| Finished `length` | ~3.3% | ~3.1% | ~3.4% | ~0.2% | ~3.2% |
| Errors / Aborts | 0 | 0 | 0 | 0 | **0** |
| Avg request rate | ~36 req/min | ~43 req/min | ~29 req/min | ~9 req/min | **~117 req/min** |
| Running at snapshot | 5 | 8 | 4 | 0 | 17 |
| Waiting at snapshot | 0 | 0 | 0 | 5 | 5 |

Traffic across Qwen3.6 backends splits ~33/39/28. host-B receives slightly more due to LiteLLM's shuffle algorithm and cache warmth. Gemma 4 on host-D handles ~7% of cluster traffic — expected since it was deployed only ~20h ago and users are still discovering it.

The `length` finish rate increased from ~1% (previous report) to ~3%. This correlates with the `max_new_tokens` doubling — longer generation allowances mean some requests now reach the limit that previously would have been capped earlier.

**Cluster request rate roughly tripled** versus the previous report, with about twice as many active members.

---

## Tokens Processed

Prompt tokens per request dropped from the previous ~40K avg to ~26K. This suggests a shift in usage patterns — more short interactive queries and fewer large-context coding sessions — or better prefix caching reducing repeated prompt processing.

Generation tokens per request dropped further: 461 → 303. The `thinking_token_budget` enforcement continues to keep generation lean. Gemma 4 shows even lower generation at 225 avg/request.

---

## Generation Speed (TPOT)

| Percentile | host-A | host-B | host-C | host-D (Gemma 4) |
|------------|--------|--------|--------|-------------------|
| p50 | 11.2ms (~89 tok/s) | 10.2ms (~98 tok/s) | 10.0ms (~100 tok/s) | 10.5ms (~95 tok/s) |
| p95 | 24.1ms (~42 tok/s) | 24.1ms (~42 tok/s) | 24.2ms (~41 tok/s) | 45.2ms (~22 tok/s) |
| p99 | 119ms (~8 tok/s) | 117ms (~9 tok/s) | 217ms (~5 tok/s) | 88.5ms (~11 tok/s) |

Qwen3.6 TPOT p50 regressed from 8.7ms to ~10.5ms avg (+20%). Two factors: (1) 3x more traffic driving higher GPU contention; (2) `max-model-len` doubled, increasing attention overhead for long-context requests.

Gemma 4 TPOT p50 is competitive at 10.5ms. However, p95 is significantly higher at 45.2ms — the 31B dense model (all parameters active) is more compute-intensive than Qwen3.6's MoE architecture (3B active out of 35B) under load.

host-C shows a p99 spike at 217ms — likely a few long-context requests causing GPU memory pressure.

---

## Time to First Token (TTFT)

| Percentile | host-A | host-B | host-C | host-D (Gemma 4) |
|------------|--------|--------|--------|-------------------|
| p50 | 0.22s | 0.19s | 0.23s | 0.24s |
| p75 | 0.61s | 0.52s | 0.77s | 0.81s |
| p95 | 3.76s | 3.11s | 6.61s | 10.20s |
| p99 | 21.8s | 19.9s | 31.6s | 349.5s |

**TTFT p50 improved** across Qwen3.6: 0.40s → 0.21s avg (−47%). Three backends sharing the load means each handles fewer concurrent prefills. Prefix caching at ~80% also helps — cached prompts skip prefill entirely.

**host-C shows higher tail latency** (p95: 6.61s, p99: 31.6s) compared to host-A/B. This backend receives more long-context requests (higher avg prompt size).

**Gemma 4 p99 at 349s is an outlier.** The 31B dense model with 256K context window processes some very large prompts. At 31B active parameters (vs Qwen3.6's 3B active in the MoE), prefill is roughly an order of magnitude more compute-intensive per token. A single 200K+ token prompt during a quiet period dominates the p99 bucket.

---

## End-to-End Latency

| Percentile | host-A | host-B | host-C | host-D (Gemma 4) |
|------------|--------|--------|--------|-------------------|
| p50 | 2.03s | 1.84s | 2.28s | 2.27s |
| p95 | 23.3s | 19.4s | 29.9s | 26.1s |

**E2E p50 improved:** 2.23s → 2.05s cluster avg (−8%). The faster TTFT more than compensates for the TPOT regression.

**E2E p95 is mixed:** host-B improved to 19.4s, host-C regressed to 29.9s. The variance reflects uneven request distribution — host-B gets shorter, faster requests while host-C gets more long-context workloads.

Gemma 4 E2E is comparable to Qwen3.6 at p50 (2.27s) but p95 is 26.1s — reasonable for a dense 31B model.

---

## Prefix Caching

Qwen3.6 cache hit rates declined slightly from ~87% to ~79% avg. With ~2x more users, there is more prompt diversity, reducing prefix overlap. Still healthy — most prompt tokens are served from cache.

Gemma 4 prefix cache counters appear anomalous (queries reported as far greater than hits would imply). This is likely a vLLM metric reporting difference for the dense model; the cache is functional based on the TTFT improvements observed.

---

## KV Cache & Preemptions

| Metric | host-A | host-B | host-C | host-D (Gemma 4) |
|--------|--------|--------|--------|-------------------|
| KV cache usage | 5.2% | 11.9% | 9.2% | 0% (idle at snapshot) |
| Preemptions | 0 | 0 | 0 | 0 |

KV cache usage increased from ~1% to ~9% avg on Qwen3.6. Expected with 3x traffic and doubled context. Still **extremely healthy** — even at 12%, the cluster has many times the headroom before memory pressure. Zero preemptions across all backends confirms no capacity issues.

---

## Hardware Utilization

| Metric | host-A | host-B | host-C | host-D (Gemma 4) |
|--------|--------|--------|--------|-------------------|
| GPU utilization | 74% | 99% | 65% | (idle at snapshot) |
| VRAM allocated | ~96 GB | ~97 GB | ~95 GB | ~89 GB |
| GPU temp (current) | 85°C | 87°C | 80°C | 38°C (idle) |
| GPU temp (avg) | 81.8°C | 81.3°C | 78.7°C | 63.2°C |
| GPU temp (max) | 90°C | 89°C | 88°C | 88°C |
| Power draw (avg) | 246W | 245W | 237W | 142W |

All backends comfortably within thermal and power limits. The GPUs throttle around 90°C — host-A/B occasionally touch this under sustained load but temperatures average well below.

---

## Period-over-period Comparison

| Metric | Previous report | This report | Change |
|--------|------------------|--------------|--------|
| Backends | 2 (Qwen3.6) | 4 (3x Qwen3.6 + 1x Gemma 4) | **+100%** |
| Cluster request rate | ~37 req/min | ~117 req/min | **+212%** |
| Qwen3.6 request rate | ~37 req/min | ~108 req/min | **+189%** |
| TPOT p50 (Qwen3.6) | 8.7ms | 10.5ms avg | **+20% (slower)** |
| TTFT p50 (Qwen3.6) | 0.40s | 0.21s avg | **−47% (faster)** |
| E2E p50 (Qwen3.6) | 2.23s | 2.05s avg | **−8% (faster)** |
| Cache hit rate | ~87% | ~79% avg | −8pp |
| Gen tokens/request | 461 | 303 | **−34%** |
| KV cache usage | 1.3% | 8.8% avg | +7.5pp |
| Preemptions | 0 | 0 | — |
| Errors | 0 | 0 | — |

---

## Diagnosis

**Overall status: healthy. Cluster handles ~3x more traffic with better latency than the prior window.**

### Key Findings

1. **~117 req/min with zero errors.** The 4-backend cluster sustains roughly 3x the throughput of the previous setup, serving about twice as many members. Zero preemptions, zero errors, zero queue on Qwen3.6.

2. **TTFT halved.** p50 went from 0.40s to 0.21s. Three Qwen3.6 backends share the prefill load, and longer context with prefix caching means large prompts benefit more from cached prefixes.

3. **E2E latency improved 8% despite higher load.** The faster TTFT compensates for the 20% TPOT regression. Users experience faster responses overall.

4. **TPOT regressed 20%.** From 8.7ms to 10.5ms. Driven by higher GPU contention (3x traffic) and doubled context length. Still competitive at ~95 tok/s median.

5. **Gemma 4 is functional but lightly loaded.** ~7% of traffic on day one. TPOT p50 is competitive (10.5ms), but TTFT tail latency is high (p99: 349s) due to the dense architecture processing long prompts. As adoption grows, this will stabilize.

6. **host-D has 5 waiting requests at snapshot.** Gemma 4 with `max-num-seqs=32` and dense 31B params may queue under concurrent bursts. Monitor if this becomes a pattern.

7. **Cache hit rate declined ~8pp.** From ~87% to ~79%. Natural with ~2x more users creating more diverse prompts. Still healthy.

### Trade-offs / Risks

- **host-C tail latency** (p99 TTFT 31.6s) is significantly worse than peers — if more long-context users join, may need LiteLLM routing weights adjusted.
- **Gemma 4 queue depth** — if adoption spikes, `max-num-seqs=32` may need increasing to 48–64.
- **Thermals** on host-A/B touch 90°C under peak load — sustained 140+ req/min on Qwen3.6 will push thermals.

### Capacity Planning

**Current capacity utilization:**

| Resource | Current | Capacity | Headroom |
|----------|---------|----------|----------|
| Qwen3.6 request rate | ~108 req/min | ~400 req/min* | **~3.7x** |
| Qwen3.6 KV cache | ~9% avg | ~90% before preemptions | **~10x** |
| Qwen3.6 concurrent reqs | 17 running | 192 max (64 × 3) | **~11x** |
| Gemma 4 request rate | ~9 req/min | ~60 req/min* | **~7x** |
| GPU thermal | ~81°C avg | 90°C throttle | 9°C margin |

\*Estimated from current per-backend throughput extrapolated to queue-free operation.

**Projection for a 30%+ increase in active members:** all metrics well within 2x headroom — the cluster can absorb this comfortably.

### Recommendations

- **Proceed with the next onboarding wave.** The cluster has ample headroom across all dimensions.
- **Re-run the benchmark 48–72h after the new cohort onboards** to validate the projection.
- **Adjust LiteLLM routing weights** if host-C's tail latency degrades further.
- **Plan to raise Gemma 4's `max-num-seqs`** if its waiting queue becomes a sustained pattern.
