package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPostgresDSNPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"database":{"host":"10.1.2.3","port":6000,"user":"u","password":"p","dbname":"d","sslmode":"require"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)

	// no env DSN → built from config file fields
	t.Setenv("ARTEX_PG_DSN", "")
	got, _, err := PostgresDSN()
	want := "postgres://u:p@10.1.2.3:6000/d?sslmode=require"
	if err != nil || got != want {
		t.Fatalf("from file: got %q err %v want %q", got, err, want)
	}

	// env wins over config file
	t.Setenv("ARTEX_PG_DSN", "postgres://envwins/x")
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://envwins/x" {
		t.Fatalf("env should win, got %q err %v", got, err)
	}

	// no env, no file → error (no built-in default)
	t.Setenv("ARTEX_PG_DSN", "")
	t.Setenv("ARTEX_CONFIG", filepath.Join(dir, "nope.json"))
	if got, _, err := PostgresDSN(); err == nil {
		t.Fatalf("missing config should error, got %q", got)
	}

	// full dsn in config file is used verbatim
	os.WriteFile(cfgPath, []byte(`{"database":{"dsn":"postgres://full/dsn"}}`), 0o644)
	t.Setenv("ARTEX_CONFIG", cfgPath)
	if got, _, err := PostgresDSN(); err != nil || got != "postgres://full/dsn" {
		t.Fatalf("file dsn verbatim, got %q err %v", got, err)
	}
}

func TestKeyDirOverride(t *testing.T) {
	t.Setenv("ARTEX_KEY_DIR", "/var/lib/artex/keys")
	if got := KeyDir(); got != "/var/lib/artex/keys" {
		t.Fatalf("KeyDir() = %q, quer /var/lib/artex/keys", got)
	}
}

func TestKeyDirFallsBackToBaseDir(t *testing.T) {
	t.Setenv("ARTEX_KEY_DIR", "")
	if got, want := KeyDir(), BaseDir(); got != want {
		t.Fatalf("KeyDir() = %q, quer BaseDir() = %q", got, want)
	}
}

func TestKeyDirIgnoresBlank(t *testing.T) {
	t.Setenv("ARTEX_KEY_DIR", "   ")
	if got, want := KeyDir(), BaseDir(); got != want {
		t.Fatalf("KeyDir() com espaço em branco = %q, quer %q", got, want)
	}
}
