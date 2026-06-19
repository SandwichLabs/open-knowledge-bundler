package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDocConfinement(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "catalog"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog", "index.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret outside the bundle that must never be readable through ReadDoc.
	parent := filepath.Dir(dir)
	secret := filepath.Join(parent, "secret.txt")
	_ = os.WriteFile(secret, []byte("top secret"), 0o644)
	defer os.Remove(secret)

	b := &Bundle{Dir: dir}

	if got, err := b.ReadDoc("catalog/index.md"); err != nil || got != "hello" {
		t.Errorf("ReadDoc legit failed: got %q err %v", got, err)
	}

	for _, esc := range []string{"../secret.txt", "../../etc/passwd", "/etc/passwd"} {
		if _, err := b.ReadDoc(esc); err == nil {
			t.Errorf("ReadDoc should reject escape %q", esc)
		}
	}
}

func TestLoadBundle(t *testing.T) {
	dir := t.TempDir()
	// Minimal bundle: domain.yaml + a (dummy) db file + a doc.
	yaml := "domain_name: testdomain\nembedding_dim: 768\ndatabase_path: graph.duckdb\n"
	if err := os.WriteFile(filepath.Join(dir, "domain.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graph.duckdb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("skill notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "catalog"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog", "index.md"), []byte("# index"), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := LoadBundle(dir, "")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if b.Name() != "testdomain" {
		t.Errorf("Name = %q, want testdomain", b.Name())
	}
	if b.Config.EmbeddingDim != 768 {
		t.Errorf("EmbeddingDim = %d, want 768", b.Config.EmbeddingDim)
	}
	if filepath.Base(b.DBPath) != "graph.duckdb" {
		t.Errorf("DBPath = %q", b.DBPath)
	}
	if b.Skill != "skill notes" {
		t.Errorf("Skill = %q", b.Skill)
	}
	if len(b.Docs) == 0 {
		t.Error("expected indexed docs")
	}

	// Missing database should error helpfully.
	dir2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir2, "domain.yaml"), []byte(yaml), 0o644)
	if _, err := LoadBundle(dir2, ""); err == nil {
		t.Error("expected error when database is missing")
	}
}
