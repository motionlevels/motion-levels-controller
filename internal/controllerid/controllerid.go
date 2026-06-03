package controllerid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const DefaultPath = ".motion-levels-controller-id"

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Resolve returns a stable controller UUID. An explicit override wins; otherwise
// the UUID is read from path or generated and stored if the file does not exist.
func Resolve(path string, override string) (string, error) {
	override = strings.TrimSpace(override)
	if override != "" {
		if err := Validate(override); err != nil {
			return "", fmt.Errorf("controller-id: %w", err)
		}
		return strings.ToLower(override), nil
	}

	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}

	existing, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(existing))
		if err := Validate(id); err != nil {
			return "", fmt.Errorf("controller-id-file %s: %w", path, err)
		}
		return strings.ToLower(id), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	id, err := NewUUID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(path)), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

func Validate(id string) error {
	id = strings.TrimSpace(id)
	if !uuidPattern.MatchString(id) {
		return fmt.Errorf("must be a UUID in 8-4-4-4-12 hex format")
	}
	return nil
}

func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4],
		b[4:6],
		b[6:8],
		b[8:10],
		b[10:16],
	), nil
}
