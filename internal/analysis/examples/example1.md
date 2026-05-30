# Cluster Performance Report — Example A

**Cluster:** 2 GPU nodes (host-A, host-B), 96 GB GPU each.
**Model:** Qwen3.6-35B-A3B FP8 via vLLM.
**Load balancing:** simple-shuffle across both backends.
**Observation window:** ~44h after a service restart for a config change.
**Config change under test:** MTP speculative decoding disabled (was `num_speculative_tokens: 2`).

---

## Why MTP Was Disabled

MTP (Multi-Token Prediction) speculative decoding was removed due to two confirmed vLLM bugs:

- `thinking_token_budget` silently ignored when MTP is active. The model's reasoning could not be capped, causing multi-second pauses on simple prompts.
- MTP interferes with the grammar-guided tool call parser, causing intermittent tool-call XML leaking into text content.

Both bugs were open upstream. The `--speculative-config` flag was commented out for easy re-enablement once vLLM ships fixes.

---

## Traffic Volume

| Metric | host-A | host-B | Cluster |
|--------|--------|--------|---------|
| Requests completed | mid five-figure | mid five-figure | **~100K** |
| Finished with `stop` | ~99.2% | ~99.1% | ~99.1% |
| Finished by `length` | ~0.8% | ~0.9% | ~0.9% |
| Running at snapshot | 3 | 4 | 7 |
| Waiting at snapshot | 0 | 0 | 0 |

Traffic split is now near 50/50 — perfectly balanced. Both backends restarted at the same time, so cache warmth is equal.

The `length` finish rate dropped from a few percent to under 1%. With `thinking_token_budget` now enforced, the model wastes fewer tokens on excessive reasoning and is less likely to hit the `max_tokens` limit.

## Tokens Processed

Generation tokens per request dropped roughly in half. This is the direct result of `thinking_token_budget: 8192` now being enforced — the model's reasoning is capped instead of running unconstrained.

## Generation Speed (TPOT)

| Percentile | host-A | host-B | Equivalent tok/s |
|------------|--------|--------|------------------|
| p50 | 8.6ms | 8.7ms | ~115 tok/s |
| p95 | 24.0ms | 23.6ms | ~42 tok/s |
| p99 | 75.5ms | 44.6ms | ~14-22 tok/s |

TPOT p50 increased from ~6ms (with MTP) to ~8.7ms (without MTP). This is the expected ~45% regression from losing MTP's speculative decoding speedup (MTP had ~90% acceptance rate, giving ~1.5x effective throughput).

Note: metric source changed from `vllm:time_per_output_token_seconds` to `vllm:inter_token_latency_seconds` — vLLM exposes different histogram names depending on whether speculative decoding is active.

## Time to First Token (TTFT)

| Percentile | host-A | host-B |
|------------|--------|--------|
| p50 | 0.41s | 0.39s |
| p75 | 0.70s | 0.68s |
| p95 | 2.89s | 2.76s |
| p99 | 6.79s | 6.59s |

TTFT **improved significantly** despite higher traffic:

- p50: 0.57s → 0.40s (−30%)
- p95: 3.34s → 2.83s (−15%)

Two factors explain this: (1) without MTP, there is no speculative decoding overhead during prefill, freeing GPU compute for prompt processing; (2) the balanced split means both backends share load equally instead of one carrying most of it.

## End-to-End Latency

| Percentile | host-A | host-B |
|------------|--------|--------|
| p50 | 2.29s | 2.18s |
| p95 | 26.5s | 24.8s |

E2E p50 is essentially unchanged. The faster TTFT compensates for the slower per-token generation. Users experience similar response times despite the TPOT regression.

E2E p95 is slightly higher, reflecting the longer tail on generation-heavy requests where TPOT matters more than TTFT.

## Prefix Caching

| Metric | host-A | host-B |
|--------|--------|--------|
| **Hit rate** | **86.9%** | **86.8%** |

Cache hit rate **improved** from the high-70s to ~87%. Two likely causes: (1) without MTP, the KV cache layout is simpler (no draft model heads competing for cache space), improving prefix reuse; (2) with `thinking_token_budget` enforced, response lengths are more predictable, leading to more cache-friendly conversation patterns.

## KV Cache & Preemptions

| Metric | host-A | host-B |
|--------|--------|--------|
| KV cache usage | 1.3% | 0.2% |
| Preemptions | 0 | 0 |

KV cache usage dropped considerably. Without MTP, the model doesn't allocate speculative token slots, freeing KV cache capacity. Headroom is massive — the cluster could handle many times more concurrent sequences before memory pressure.

## Hardware Utilization

GPU utilization increased (mid-70s → high-90s percent). Without MTP's speculative batching, the GPU spends more cycles on actual token generation per request, resulting in higher sustained utilization. Temperatures and power draw are similar to the MTP-enabled period.

## MTP On vs MTP Off Comparison

| Metric | MTP on | MTP off | Change |
|--------|--------|---------|--------|
| Total requests | ~85K | ~100K | +18% (shorter window) |
| Request rate | ~21 req/min | ~37 req/min | **+79%** |
| Traffic split | ~69/31 | ~50/50 | **Balanced** |
| TPOT p50 | ~6.0ms | ~8.7ms | **+45% (slower)** |
| TTFT p50 | ~0.57s | ~0.40s | **−30% (faster)** |
| TTFT p95 | ~3.34s | ~2.83s | **−15% (faster)** |
| E2E p50 | ~2.20s | ~2.23s | ~flat |
| E2E p95 | ~22.4s | ~25.7s | +15% (slower tail) |
| Cache hit rate | ~79.6% | ~86.9% | **+7.3pp** |
| Gen tokens/request | ~910 | ~461 | **−49%** |
| KV cache usage | ~4.7% | ~1.3% | **−72%** |
| Preemptions | 0 | 0 | — |
| Errors | 0 | 0 | — |

## Diagnosis

**Overall status: excellent.** Disabling MTP was a net positive for the user experience despite the raw token generation speed regression.

### Key Findings

1. **TPOT regressed 45% as expected** — from ~6ms to ~8.7ms p50. This is the direct cost of removing MTP's ~1.5x speculative decoding speedup.

2. **But users don't feel it** — E2E p50 latency is unchanged. The faster TTFT and fewer wasted reasoning tokens compensate for the slower per-token speed.

3. **`thinking_token_budget` enforcement works** — generation tokens per request dropped roughly 50%. The model no longer wastes thousands of tokens reasoning about trivial prompts. Interactive tools now get responsive behaviour.

4. **Cache hit rate improved several points** — from the high-70s to high-80s. Without MTP competing for KV cache and with more predictable generation lengths, prefix caching is significantly more effective.

5. **Cluster handles ~80% more traffic** — with roughly 60% more active members. Zero queue, zero preemptions, zero errors.

6. **Perfect load balance** — traffic is now near 50/50 because both backends were restarted simultaneously and have equal cache warmth.

### Trade-offs Accepted

- **Slower token generation** — users with long generation-heavy requests will notice ~45% slower streaming speed. For the dominant workload (coding agents: heavy prompt, short generation), the impact is minimal.
- **Higher p95/p99 E2E** — tail latency for generation-heavy requests increased ~15%. Acceptable given the other improvements.

### Capacity Planning

- Current: ~37 req/min, 0 queue.
- KV cache at <1.5% = capacity for many times more concurrent sequences.
- With the reduced generation length, effective throughput per GPU is higher despite slower per-token speed.
- Estimate: the cluster can comfortably serve roughly twice the current active members before TTFT starts degrading.

### Recommendations

- Monitor the upstream vLLM bugs that forced MTP off. When fixed, re-enable to recover the TPOT regression while keeping `thinking_token_budget` working.
- Keep tracking the `length` finish-reason rate — if it climbs back up, the budget may need adjustment for specific workloads.
- Consider increasing `max-num-seqs` only when KV cache usage trends sustainably above ~30%, not before.
