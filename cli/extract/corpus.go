package extract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadCorpus reads a corpus from a file or directory into a single text blob.
//   - A .json file is probed for a top-level {"context": "..."} (the
//     GraphRAG-Bench shape) or {"text": "..."}; otherwise its raw bytes are used.
//   - A directory is walked and all .txt/.md/.json files concatenated.
//   - Any other file is read as UTF-8 text.
func LoadCorpus(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return loadCorpusDir(path)
	}
	return loadCorpusFile(path)
}

func loadCorpusFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) == nil {
			for _, key := range []string{"context", "text", "corpus", "content"} {
				if v, ok := probe[key]; ok {
					var s string
					if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
						return s, nil
					}
				}
			}
		}
	}
	return string(raw), nil
}

func loadCorpusDir(dir string) (string, error) {
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".txt", ".md", ".json", ".text":
			s, err := loadCorpusFile(p)
			if err != nil {
				return err
			}
			b.WriteString(s)
			b.WriteString("\n\n")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no .txt/.md/.json files found under %s", dir)
	}
	return b.String(), nil
}
