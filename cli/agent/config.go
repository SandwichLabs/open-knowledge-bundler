package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// tierPreset is a seeded model-size option.
type tierPreset struct {
	Name   string
	Source string // kronk model source (provider/repo:quant)
	Desc   string
}

// defaultTiers are the seeded Gemma 4 presets. They point at the un-gated
// unsloth GGUF mirrors with the Q4_K_M quant (good size/quality balance, and
// the full quant range is published for every size — unlike the ggml-org
// mirrors which only ship Q8_0/bf16). Users can edit ~/.config/okb/config.yaml
// to pin other repos/quants, or the official google/*-qat repos (gated, need
// KRONK_HF_TOKEN).
var defaultTiers = []tierPreset{
	{"small", "unsloth/gemma-4-E2B-it-GGUF:Q4_K_M", "Gemma 4 E2B (2.3B eff, 128k) — fastest, lightest"},
	{"medium", "unsloth/gemma-4-E4B-it-GGUF:Q4_K_M", "Gemma 4 E4B (4.5B eff, 128k) — balanced (default)"},
	{"large", "unsloth/gemma-4-12B-it-GGUF:Q4_K_M", "Gemma 4 12B (256k) — stronger, more RAM/GPU"},
	{"xl", "unsloth/gemma-4-31B-it-GGUF:Q4_K_M", "Gemma 4 31B (256k) — largest dense, GPU recommended"},
	{"moe", "unsloth/gemma-4-26B-A4B-it-GGUF:Q4_K_M", "Gemma 4 26B-A4B MoE (4B active, 256k) — efficient large"},
}

// defaultEmbedSource is an un-gated EmbeddingGemma GGUF (768-dim native,
// Matryoshka-reducible) matching the dimension used by `okb` bundles. The
// unsloth mirror resolves cleanly through kronk's quant-tag selector (the
// ggml-org qat repo name does not resolve).
const defaultEmbedSource = "unsloth/embeddinggemma-300m-GGUF:Q8_0"

const defaultTier = "medium"

// defaultProcessor is the llama.cpp backend kronk installs/loads. Empty means
// auto-detect (CUDA → ROCm → Vulkan → CPU). We default to "vulkan" because the
// auto-detector prefers ROCm when rocminfo is present, but Vulkan is the
// reliable, high-performance path on AMD APUs (e.g. Strix Halo). Override in
// config (processor: "") to restore auto-detection, or set "cpu"/"cuda"/"rocm".
const defaultProcessor = "vulkan"

// Inference backend identifiers (the inference.provider config value).
const (
	ProviderKronk  = "kronk"  // in-process llama.cpp via kronk (default)
	ProviderOpenAI = "openai" // external OpenAI-compatible endpoint (llama-server, vLLM, Ollama)
)

// defaultInferenceEndpoint is the OpenAI-compatible base URL used when the
// provider is external and no endpoint is configured. A llama.cpp llama-server
// serves the OpenAI API under /v1.
const defaultInferenceEndpoint = "http://localhost:8080/v1"

// Inference selects the LLM backend used for GENERATION (okb extract and okb
// agent). It does not affect embeddings, which always run in-process (or via the
// domain endpoint), pinned to the bundle's index dimension.
//
//	provider: kronk   -> in-process llama.cpp (default; current behaviour)
//	provider: openai  -> external OpenAI-compatible chat endpoint
type Inference struct {
	Provider string // "kronk" | "openai"
	ModelID  string // model name sent to an external server (provider=openai)
	Endpoint string // base URL for provider=openai, e.g. http://localhost:8080/v1
	APIKey   string // optional bearer token (a local llama-server ignores it)
}

// IsExternal reports whether generation should route to the external
// OpenAI-compatible endpoint rather than in-process kronk.
func (i Inference) IsExternal() bool {
	switch strings.ToLower(strings.TrimSpace(i.Provider)) {
	case ProviderOpenAI, "external", "openaicompat", "llama-server", "llamacpp", "vllm", "ollama":
		return true
	default:
		return false
	}
}

// BaseURL returns the endpoint normalized to an OpenAI-style base that already
// includes a version path (clients append /chat/completions). A bare host:port
// gets /v1 appended; an endpoint that already targets /v1 (or similar) is left
// as-is.
func (i Inference) BaseURL() string {
	u := strings.TrimRight(strings.TrimSpace(i.Endpoint), "/")
	if u == "" {
		u = strings.TrimRight(defaultInferenceEndpoint, "/")
	}
	if !strings.Contains(u, "/v1") {
		u += "/v1"
	}
	return u
}

// AuthKey returns the API key, or a harmless placeholder so OpenAI-style clients
// that require a non-empty key work against a local server that ignores auth.
func (i Inference) AuthKey() string {
	if strings.TrimSpace(i.APIKey) != "" {
		return i.APIKey
	}
	return "sk-no-auth"
}

// Config is the persisted, machine-wide user config (~/.config/okb/config.yaml).
type Config struct {
	Tier        string            // selected tier name
	Models      map[string]string // tier name -> kronk model source
	EmbedSource string            // kronk model source for embeddings
	Processor   string            // llama.cpp backend (cpu|cuda|rocm|vulkan|"" for auto)
	Inference   Inference         // generation backend (kronk or external)
	v           *viper.Viper
	path        string
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "okb", "config.yaml"), nil
}

// LoadConfig loads (seeding defaults and prompting on first use) the user
// config. When reconfigure is true the model picker is always shown. When
// interactive is false the picker is skipped (defaults are seeded/saved),
// which suits non-interactive use such as --ask.
func LoadConfig(reconfigure, interactive bool) (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, fmt.Errorf("locating config dir: %w", err)
	}

	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("yaml")

	models := make(map[string]string, len(defaultTiers))
	for _, t := range defaultTiers {
		models[t.Name] = t.Source
	}
	v.SetDefault("models", models)
	v.SetDefault("embed_source", defaultEmbedSource)
	v.SetDefault("tier", defaultTier)
	v.SetDefault("processor", defaultProcessor)
	v.SetDefault("inference.provider", ProviderKronk)
	v.SetDefault("inference.model_id", "")
	v.SetDefault("inference.endpoint", defaultInferenceEndpoint)
	v.SetDefault("inference.api_key", "")

	exists := false
	if _, err := os.Stat(path); err == nil {
		exists = true
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("reading config %s: %w", path, err)
		}
	}

	c := &Config{v: v, path: path}
	c.reload()

	switch {
	case interactive && (!exists || reconfigure):
		if err := c.runPicker(); err != nil {
			return nil, err
		}
		if err := c.save(); err != nil {
			return nil, fmt.Errorf("saving config: %w", err)
		}
		fmt.Printf("Saved model choice (%s) to %s\n\n", c.Tier, c.path)
	case !exists:
		// Seed defaults without prompting (non-interactive first run).
		if err := c.save(); err != nil {
			return nil, fmt.Errorf("saving config: %w", err)
		}
	}
	return c, nil
}

func (c *Config) reload() {
	c.Tier = c.v.GetString("tier")
	c.Models = c.v.GetStringMapString("models")
	c.EmbedSource = c.v.GetString("embed_source")
	c.Processor = c.v.GetString("processor")
	c.Inference = Inference{
		Provider: c.v.GetString("inference.provider"),
		ModelID:  c.v.GetString("inference.model_id"),
		Endpoint: c.v.GetString("inference.endpoint"),
		APIKey:   c.v.GetString("inference.api_key"),
	}
	if strings.TrimSpace(c.Inference.Provider) == "" {
		c.Inference.Provider = ProviderKronk
	}
	// Backfill any tiers missing from a hand-edited config.
	if c.Models == nil {
		c.Models = map[string]string{}
	}
	for _, t := range defaultTiers {
		if _, ok := c.Models[t.Name]; !ok {
			c.Models[t.Name] = t.Source
		}
	}
}

// SetTier overrides the active tier (e.g. from a --tier flag), without saving.
func (c *Config) SetTier(tier string) error {
	if _, ok := c.Models[tier]; !ok {
		return fmt.Errorf("unknown tier %q (known: %s)", tier, strings.Join(c.tierNames(), ", "))
	}
	c.Tier = tier
	return nil
}

func (c *Config) tierNames() []string {
	names := make([]string, len(defaultTiers))
	for i, t := range defaultTiers {
		names[i] = t.Name
	}
	return names
}

// LLMSource returns the model source for the active tier. If the tier value is
// not a known tier name it is treated as a literal source string (so a
// --model flag can pass an arbitrary GGUF source).
func (c *Config) LLMSource() string {
	if s, ok := c.Models[c.Tier]; ok && s != "" {
		return s
	}
	return c.Tier
}

func (c *Config) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	c.v.Set("tier", c.Tier)
	c.v.Set("models", c.Models)
	c.v.Set("embed_source", c.EmbedSource)
	c.v.Set("processor", c.Processor)
	c.v.Set("inference.provider", c.Inference.Provider)
	c.v.Set("inference.model_id", c.Inference.ModelID)
	c.v.Set("inference.endpoint", c.Inference.Endpoint)
	c.v.Set("inference.api_key", c.Inference.APIKey)
	return c.v.WriteConfigAs(c.path)
}

// runPicker prompts the user on stdin to choose a model tier. It runs before
// the TUI starts, so plain terminal I/O is fine.
func (c *Config) runPicker() error {
	fmt.Println("okb agent — choose a local model size (downloaded once via kronk):")
	fmt.Println()
	for i, t := range defaultTiers {
		marker := "  "
		if t.Name == c.Tier {
			marker = "→ "
		}
		fmt.Printf("  %s%d) %-7s %s\n", marker, i+1, t.Name, t.Desc)
	}
	fmt.Println()
	fmt.Printf("Selection [1-%d] (Enter for %s): ", len(defaultTiers), c.Tier)

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return nil // keep current/default
	}
	for i, t := range defaultTiers {
		if line == fmt.Sprint(i+1) || strings.EqualFold(line, t.Name) {
			c.Tier = t.Name
			return nil
		}
	}
	return fmt.Errorf("invalid selection %q", line)
}
