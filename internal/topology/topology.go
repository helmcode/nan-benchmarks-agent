// Package topology discovers the live cluster shape directly from
// VictoriaMetrics. We classify each vllm job into one of three families
// (qwen, gemma, embedding) based on its model_name label, and we map each
// inference job to its companion nvidia-exporter job.
//
// Auto-discovery means adding a new GPU node requires no code change — the
// next benchmark will pick it up automatically as long as vmagent is
// scraping it.
package topology

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/helmcode/nan-benchmarks-agent/internal/vmclient"
)

// Family is the role a node plays in the cluster.
type Family string

const (
	FamilyQwen      Family = "qwen3.6"
	FamilyGemma     Family = "gemma4"
	FamilyEmbedding Family = "embedding"
	FamilyUnknown   Family = "unknown"
)

// Node is one inference backend.
type Node struct {
	Job       string // vmagent job label, e.g. "vllm" or "vllm-<suffix>"
	NvidiaJob string // hardware metrics job label, paired by suffix
	Hostname  string // host label from the metric, e.g. the node's short name
	Model     string // model_name label as exposed by vLLM
	Instance  string // host:port the vLLM engine listens on
	Family    Family
}

// Topology is the snapshot of all currently-up vLLM backends.
type Topology struct {
	Nodes []Node
}

// Discover queries VM for live vllm jobs and one sample metric to extract labels.
func Discover(ctx context.Context, c *vmclient.Client) (*Topology, error) {
	// Use vllm:num_requests_running because it always carries the full set of
	// useful labels (job, node, model_name, instance) even when the gauge is 0.
	samples, err := c.Instant(ctx, `vllm:num_requests_running`, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("topology discover: %w", err)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("topology discover: no vllm:num_requests_running series")
	}

	// One series per (job, instance, ...) — collapse to unique jobs.
	seen := make(map[string]Node, len(samples))
	for _, s := range samples {
		job := s.Metric["job"]
		if job == "" || !strings.HasPrefix(job, "vllm") {
			continue
		}
		if _, ok := seen[job]; ok {
			continue
		}
		n := Node{
			Job:      job,
			Hostname: s.Metric["node"],
			Model:    s.Metric["model_name"],
			Instance: s.Metric["instance"],
		}
		n.Family = classify(n.Model)
		n.NvidiaJob = nvidiaJobFor(job)
		seen[job] = n
	}

	t := &Topology{Nodes: make([]Node, 0, len(seen))}
	for _, n := range seen {
		t.Nodes = append(t.Nodes, n)
	}
	sort.Slice(t.Nodes, func(i, j int) bool { return t.Nodes[i].Job < t.Nodes[j].Job })
	return t, nil
}

// ByFamily returns all nodes in the given family, in deterministic order.
func (t *Topology) ByFamily(f Family) []Node {
	var out []Node
	for _, n := range t.Nodes {
		if n.Family == f {
			out = append(out, n)
		}
	}
	return out
}

// classify uses the model_name label to bucket a backend.
func classify(model string) Family {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "qwen3.6"), strings.Contains(m, "qwen3-6"):
		return FamilyQwen
	case strings.Contains(m, "gemma"):
		return FamilyGemma
	case strings.Contains(m, "embedding"), strings.Contains(m, "embed"):
		return FamilyEmbedding
	}
	return FamilyUnknown
}

// nvidiaJobFor maps a vllm job label to its sibling nvidia_gpu job. The
// historical convention is:
//
//	vllm                  → nvidia_gpu          (legacy: first host, no suffix)
//	vllm-<suffix>         → nvidia_gpu-<suffix>
//	vllm-embedding-<sfx>  → nvidia_gpu-<sfx>    (drop the "embedding-" infix)
func nvidiaJobFor(job string) string {
	if job == "vllm" {
		return "nvidia_gpu"
	}
	suffix := strings.TrimPrefix(job, "vllm-")
	suffix = strings.TrimPrefix(suffix, "embedding-")
	return "nvidia_gpu-" + suffix
}
