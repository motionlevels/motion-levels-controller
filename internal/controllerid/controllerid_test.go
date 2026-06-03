package controllerid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveGeneratesAndPersistsControllerID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller-id")

	first, err := Resolve(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(first); err != nil {
		t.Fatal(err)
	}

	second, err := Resolve(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second resolve = %q, want %q", second, first)
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stored)) != first {
		t.Fatalf("stored controller id = %q, want %q", strings.TrimSpace(string(stored)), first)
	}
}

func TestResolveUsesValidOverrideWithoutWritingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller-id")
	id := "01234567-89ab-4def-8123-456789abcdef"

	got, err := Resolve(path, id)
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("override id = %q, want %q", got, id)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("override should not write id file, stat err=%v", err)
	}
}

func TestResolveRejectsInvalidExistingID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller-id")
	if err := os.WriteFile(path, []byte("not-a-uuid\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(path, ""); err == nil {
		t.Fatal("expected invalid stored id error")
	}
}
