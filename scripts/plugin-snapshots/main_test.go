package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRepositoryRoot creates a temporary directory containing a minimal
// go.work stub so resolveRepositoryRoot accepts it as an explicit -root.
func newTestRepositoryRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.25.0\n"), 0o644); err != nil {
		t.Fatalf("write go.work stub: %v", err)
	}
	return root
}

func TestCLIWriteThenCheckSucceeds(t *testing.T) {
	root := newTestRepositoryRoot(t)

	if code := run([]string{
		"-root", root,
		"-write",
		"-identity-output", "identity-snapshot.json",
		"-default-output", "default-snapshot.json",
	}); code != 0 {
		t.Fatalf("-write exited %d, want 0", code)
	}

	identityPath := filepath.Join(root, "identity-snapshot.json")
	defaultPath := filepath.Join(root, "default-snapshot.json")
	for _, path := range []string{identityPath, defaultPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s to exist after -write: %v", path, err)
		}
	}

	if code := run([]string{
		"-root", root,
		"-check",
		"-identity-output", "identity-snapshot.json",
		"-default-output", "default-snapshot.json",
	}); code != 0 {
		t.Fatalf("-check exited %d, want 0 immediately after -write", code)
	}
}

func TestCLICheckFailsAfterMutation(t *testing.T) {
	root := newTestRepositoryRoot(t)

	if code := run([]string{
		"-root", root,
		"-write",
		"-identity-output", "identity-snapshot.json",
		"-default-output", "default-snapshot.json",
	}); code != 0 {
		t.Fatalf("-write exited %d, want 0", code)
	}

	identityPath := filepath.Join(root, "identity-snapshot.json")
	original, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read written identity snapshot: %v", err)
	}
	mutated := append(append([]byte(nil), original...), []byte("// drift\n")...)
	if err := os.WriteFile(identityPath, mutated, 0o644); err != nil {
		t.Fatalf("mutate identity snapshot: %v", err)
	}

	if code := run([]string{
		"-root", root,
		"-check",
		"-identity-output", "identity-snapshot.json",
		"-default-output", "default-snapshot.json",
	}); code == 0 {
		t.Fatal("-check exited 0 after the identity snapshot drifted, want non-zero")
	}
}

func TestCLIRequiresExactlyOneMode(t *testing.T) {
	root := newTestRepositoryRoot(t)

	if code := run([]string{"-root", root}); code == 0 {
		t.Fatal("run with no mode flag exited 0, want non-zero")
	}
	if code := run([]string{"-root", root, "-write", "-check"}); code == 0 {
		t.Fatal("run with two mode flags exited 0, want non-zero")
	}
}

func TestCLIStdoutPrintsBothSnapshots(t *testing.T) {
	root := newTestRepositoryRoot(t)

	stdout, restore := captureStdout(t)
	defer restore()

	if code := run([]string{"-root", root, "-stdout"}); code != 0 {
		t.Fatalf("-stdout exited %d, want 0", code)
	}

	output := stdout()
	if !strings.Contains(output, "=== identity-snapshot.json ===") {
		t.Errorf("stdout missing identity header, got:\n%s", output)
	}
	if !strings.Contains(output, "=== default-snapshot.json ===") {
		t.Errorf("stdout missing default header, got:\n%s", output)
	}
	if !strings.Contains(output, `"key": "session"`) {
		t.Errorf("stdout missing the session plugin entry, got:\n%s", output)
	}
}

// captureStdout redirects os.Stdout to a temporary file for the duration of
// the returned functions' use, so tests can assert on CLI output without
// risking a pipe-buffer deadlock for larger output.
func captureStdout(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	original := os.Stdout
	file, err := os.CreateTemp(t.TempDir(), "stdout-*.txt")
	if err != nil {
		t.Fatalf("create temp stdout file: %v", err)
	}
	os.Stdout = file
	restore = func() { os.Stdout = original }
	read = func() string {
		if err := file.Sync(); err != nil {
			t.Fatalf("sync captured stdout: %v", err)
		}
		data, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatalf("read captured stdout: %v", err)
		}
		return string(data)
	}
	return read, restore
}
