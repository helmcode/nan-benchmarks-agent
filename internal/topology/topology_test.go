package topology

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		in   string
		want Family
	}{
		{"/models/qwen3.6-35b-a3b-fp8", FamilyQwen},
		{"qwen3.6-base", FamilyQwen},
		{"/models/gemma-4-26b-a4b-it-fp8", FamilyGemma},
		{"qwen3-embedding", FamilyEmbedding},
		{"text-embedding-large", FamilyEmbedding},
		{"random-model-name", FamilyUnknown},
		{"", FamilyUnknown},
	}
	for _, c := range cases {
		if got := classify(c.in); got != c.want {
			t.Errorf("classify(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNvidiaJobFor(t *testing.T) {
	// The historical convention: bare "vllm" → "nvidia_gpu", everything else
	// gets the suffix appended after stripping "embedding-" if present.
	cases := []struct {
		in, want string
	}{
		{"vllm", "nvidia_gpu"},
		{"vllm-eu002", "nvidia_gpu-eu002"},
		{"vllm-eu009", "nvidia_gpu-eu009"},
		{"vllm-embedding-eu005", "nvidia_gpu-eu005"},
	}
	for _, c := range cases {
		if got := nvidiaJobFor(c.in); got != c.want {
			t.Errorf("nvidiaJobFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
