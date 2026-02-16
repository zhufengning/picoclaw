package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	content := `
# comment
PICOCLAW_TEST_ENV_A=alpha
export PICOCLAW_TEST_ENV_B = bravo
PICOCLAW_TEST_ENV_C="hello world"
PICOCLAW_TEST_ENV_D='single # keep'
PICOCLAW_TEST_ENV_E=value # inline comment
PICOCLAW_TEST_ENV_F="line1\nline2"
PICOCLAW_TEST_ENV_G="quoted with comment" # comment
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	if err := loadEnvFile(envPath); err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}

	tests := map[string]string{
		"PICOCLAW_TEST_ENV_A": "alpha",
		"PICOCLAW_TEST_ENV_B": "bravo",
		"PICOCLAW_TEST_ENV_C": "hello world",
		"PICOCLAW_TEST_ENV_D": "single # keep",
		"PICOCLAW_TEST_ENV_E": "value",
		"PICOCLAW_TEST_ENV_F": "line1\nline2",
		"PICOCLAW_TEST_ENV_G": "quoted with comment",
	}

	for k, want := range tests {
		got := os.Getenv(k)
		if got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLoadEnvFile_DoesNotOverrideExisting(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	if err := os.WriteFile(envPath, []byte("PICOCLAW_TEST_ENV_OVERRIDE=from_file\n"), 0644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	t.Setenv("PICOCLAW_TEST_ENV_OVERRIDE", "from_process")

	if err := loadEnvFile(envPath); err != nil {
		t.Fatalf("loadEnvFile() error = %v", err)
	}

	if got := os.Getenv("PICOCLAW_TEST_ENV_OVERRIDE"); got != "from_process" {
		t.Fatalf("PICOCLAW_TEST_ENV_OVERRIDE = %q, want %q", got, "from_process")
	}
}

func TestLoadEnvFile_InvalidLine(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	if err := os.WriteFile(envPath, []byte("bad_line_without_equal\n"), 0644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	if err := loadEnvFile(envPath); err == nil {
		t.Fatal("expected error for invalid .env line, got nil")
	}
}
