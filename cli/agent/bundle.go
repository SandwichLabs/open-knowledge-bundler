// Package agent implements `okb agent`: a self-contained chat agent over an OKF
// bundle. It pairs a local LLM and embedding model (via kronk/llama.cpp) with
// the bundle's DuckDB graph and browsable markdown concepts, orchestrated by the
// fantasy agent framework.
package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sandwich-labs/open-knowledge-bundler/cli/domain"
	"gopkg.in/yaml.v3"
)

// Bundle is a loaded OKF + skill bundle as produced by
// `okb bundle --skill`.
type Bundle struct {
	Dir    string              // bundle root directory
	Skill  string              // raw SKILL.md contents (system-prompt base), may be empty
	Config domain.DomainConfig // parsed domain.yaml
	DBPath string              // resolved path to the .duckdb inside the bundle
	Docs   []string            // bundle-relative (slash) paths of all *.md concept files
}

// LoadBundle reads and validates a bundle directory. dbOverride, when non-empty,
// replaces the database path resolved from domain.yaml.
func LoadBundle(dir, dbOverride string) (*Bundle, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("bundle %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle %q is not a directory", dir)
	}

	b := &Bundle{Dir: dir}

	// SKILL.md is expected but optional — the agent still works without it.
	if data, err := os.ReadFile(filepath.Join(dir, "SKILL.md")); err == nil {
		b.Skill = string(data)
	}

	// The domain config carries the embedding settings and database path. It is
	// copied into the bundle under its original basename (usually domain.yaml,
	// but e.g. ufo_domain.yaml when generated from a differently-named config),
	// so locate it rather than assuming a fixed name.
	cfgPath, err := findDomainConfig(dir)
	if err != nil {
		return nil, err
	}
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	if err := yaml.Unmarshal(cfgData, &b.Config); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", cfgPath, err)
	}

	// Resolve the database path.
	switch {
	case dbOverride != "":
		b.DBPath = dbOverride
	case b.Config.DatabasePath != "":
		b.DBPath = filepath.Join(dir, filepath.Base(b.Config.DatabasePath))
	default:
		return nil, fmt.Errorf("domain.yaml has no database_path; pass --db <path>")
	}
	if _, err := os.Stat(b.DBPath); err != nil {
		return nil, fmt.Errorf("bundle database %q not found: %w\nregenerate with `okb bundle`, or pass --db <path>", b.DBPath, err)
	}

	// Index markdown docs for the exploration tools.
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			if rel, err := filepath.Rel(dir, path); err == nil {
				b.Docs = append(b.Docs, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	sort.Strings(b.Docs)

	return b, nil
}

// findDomainConfig locates the domain config inside a bundle. It prefers
// domain.yaml, then any *.yaml that parses with a database_path or node
// definitions set (the markers of a domain config).
func findDomainConfig(dir string) (string, error) {
	preferred := filepath.Join(dir, "domain.yaml")
	if _, err := os.Stat(preferred); err == nil {
		return preferred, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading bundle dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var probe domain.DomainConfig
		if err := yaml.Unmarshal(data, &probe); err != nil {
			continue
		}
		if probe.DatabasePath != "" || len(probe.NodeDefinitions) > 0 {
			return full, nil
		}
	}
	return "", fmt.Errorf("no domain config (domain.yaml) found in bundle %q; regenerate with `okb bundle`", dir)
}

// ReadDoc reads a bundle-relative markdown path, confined to the bundle dir.
func (b *Bundle) ReadDoc(rel string) (string, error) {
	// Clean and confine: drop any leading separator and resolve ".." escapes.
	clean := filepath.Clean(string(filepath.Separator) + filepath.FromSlash(rel))
	clean = strings.TrimPrefix(clean, string(filepath.Separator))
	full := filepath.Join(b.Dir, clean)

	rp, err := filepath.Rel(b.Dir, full)
	if err != nil || rp == ".." || strings.HasPrefix(rp, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the bundle", rel)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("reading %q: %w", rel, err)
	}
	return string(data), nil
}

// Name returns a short bundle label for display (domain name or directory base).
func (b *Bundle) Name() string {
	if b.Config.DomainName != "" {
		return b.Config.DomainName
	}
	return filepath.Base(b.Dir)
}
