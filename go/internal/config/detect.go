package config

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultBedrockRegion is used when a bedrock provider omits its region.
const DefaultBedrockRegion = "us-east-1"

// BedrockBaseURL is the OpenAI-compatible base URL for a Bedrock region.
func BedrockBaseURL(region string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		region = DefaultBedrockRegion
	}
	return "https://bedrock-runtime." + region + ".amazonaws.com/v1"
}

// LocalCandidate is a locally-reachable provider surfaced by discovery. It is
// NOT written to config — the operator chooses whether to add it.
type LocalCandidate struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	Note    string `json:"note"`
}

// localHosts are the addresses probed for local backends. host.docker.internal
// matters when the gateway runs in a container and the backend on the host.
func localHosts() []string {
	cands := []string{"127.0.0.1", "host.docker.internal", "localhost"}
	seen := map[string]bool{}
	var out []string
	for _, h := range cands {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// openaiCompatProbe is a well-known local OpenAI-compatible server.
type openaiCompatProbe struct {
	Port int
	Name string
}

// Well-known local OpenAI-compatible servers (extensible).
var openaiCompatProbes = []openaiCompatProbe{
	{8080, "localai"},
	{8790, "localai"},
	{1234, "lmstudio"},
	{8000, "vllm"},
	{1337, "jan"},
	{5000, "textgen"},
	{11435, "llamacpp"},
}

func probeOK(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// DiscoverLocalProviders probes the usual local hosts/ports for Ollama and
// OpenAI-compatible servers and returns reachable candidates. It does NOT
// mutate config — callers surface these for the operator to add.
func DiscoverLocalProviders() []LocalCandidate {
	type hit struct{ id, typ, baseURL, note string }
	var (
		mu     sync.Mutex
		hits   []hit
		wg     sync.WaitGroup
		to     = 700 * time.Millisecond
		record = func(id, typ, baseURL, note string) {
			mu.Lock()
			hits = append(hits, hit{id, typ, baseURL, note})
			mu.Unlock()
		}
	)

	for _, h := range localHosts() {
		host := h
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := fmt.Sprintf("http://%s:11434", host)
			if probeOK(base+"/api/tags", to) {
				record("ollama", "ollama", base, "Ollama (native) @ "+base)
			}
		}()
		for _, p := range openaiCompatProbes {
			probe := p
			wg.Add(1)
			go func() {
				defer wg.Done()
				base := fmt.Sprintf("http://%s:%d", host, probe.Port)
				if probeOK(base+"/v1/models", to) {
					record(probe.Name, "openai_compatible", base+"/v1", "OpenAI-compatible @ "+base)
				}
			}()
		}
	}
	wg.Wait()

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].id != hits[j].id {
			return hits[i].id < hits[j].id
		}
		return hits[i].baseURL < hits[j].baseURL
	})
	seenURL := map[string]bool{}
	seenID := map[string]bool{}
	var out []LocalCandidate
	for _, h := range hits {
		if seenURL[h.baseURL] {
			continue
		}
		seenURL[h.baseURL] = true
		id := h.id
		for n := 2; seenID[id]; n++ {
			id = fmt.Sprintf("%s%d", h.id, n)
		}
		seenID[id] = true
		out = append(out, LocalCandidate{ID: id, Type: h.typ, BaseURL: h.baseURL, Note: h.note})
	}
	return out
}

// AutodetectProviders is the OPT-IN silent path: add every discovered local
// provider not already configured. Off by default (surface-don't-hardwire);
// used at startup only when LLMGW_AUTODISCOVER_LOCAL is truthy. Returns ids added.
func AutodetectProviders(persist bool) []string {
	candidates := DiscoverLocalProviders()
	if len(candidates) == 0 {
		return nil
	}
	var added []string
	Update(func(s *Settings) {
		configured := map[string]bool{}
		for _, pc := range s.Providers {
			if pc.BaseURL != "" {
				configured[strings.TrimRight(pc.BaseURL, "/")] = true
			}
		}
		for _, c := range candidates {
			if _, exists := s.Providers[c.ID]; exists {
				continue
			}
			if configured[strings.TrimRight(c.BaseURL, "/")] {
				continue
			}
			s.Providers[c.ID] = &ProviderConfig{Type: c.Type, BaseURL: c.BaseURL}
			added = append(added, c.ID)
		}
	})
	if len(added) > 0 && persist {
		_ = Save()
	}
	return added
}
