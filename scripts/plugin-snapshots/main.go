// Command plugin-snapshots generates and checks the two committed
// Everything Is a Plugin snapshot artifacts: every plugin's canonical
// identity (key, config path, cardinality, exported contracts) and every
// plugin's effective ConfigSpec default value. Both are regenerated and
// diffed at each merge to catch unreviewed drift.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultIdentityOutputPath = ".claude/migration/everything-plugin/identity-snapshot.json"
	defaultDefaultOutputPath  = ".claude/migration/everything-plugin/default-snapshot.json"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("plugin-snapshots", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	write := flags.Bool("write", false, "write the generated identity and default snapshots to their output paths")
	check := flags.Bool("check", false, "fail when either output snapshot differs from the generated snapshot")
	stdout := flags.Bool("stdout", false, "write both generated snapshots to stdout")
	identityOutput := flags.String("identity-output", defaultIdentityOutputPath, "checked identity snapshot path, relative to the repository root")
	defaultOutput := flags.String("default-output", defaultDefaultOutputPath, "checked default snapshot path, relative to the repository root")
	rootFlag := flags.String("root", "", "repository root containing go.work (normally auto-detected)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	modes := 0
	for _, enabled := range []bool{*write, *check, *stdout} {
		if enabled {
			modes++
		}
	}
	if modes != 1 {
		fmt.Fprintln(os.Stderr, "exactly one of -write, -check, or -stdout is required")
		return 2
	}

	root, err := resolveRepositoryRoot(*rootFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "locate repository: %v\n", err)
		return 1
	}

	descriptors, err := collectDescriptors()
	if err != nil {
		fmt.Fprintf(os.Stderr, "collect plugin descriptors: %v\n", err)
		return 1
	}

	identity := buildIdentitySnapshot(descriptors)
	generatedIdentity, err := marshalSnapshot(identity)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode identity snapshot: %v\n", err)
		return 1
	}

	defaults := buildDefaultSnapshot(descriptors)
	generatedDefaults, err := marshalSnapshot(defaults)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode default snapshot: %v\n", err)
		return 1
	}

	identityPath := resolveOutputPath(root, *identityOutput)
	defaultPath := resolveOutputPath(root, *defaultOutput)

	switch {
	case *stdout:
		fmt.Fprintln(os.Stdout, "=== identity-snapshot.json ===")
		if _, err := os.Stdout.Write(generatedIdentity); err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, "=== default-snapshot.json ===")
		if _, err := os.Stdout.Write(generatedDefaults); err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			return 1
		}
	case *write:
		if err := writeAtomically(identityPath, generatedIdentity); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", displayPath(root, identityPath), err)
			return 1
		}
		fmt.Printf("wrote %s (%d plugins)\n", displayPath(root, identityPath), len(identity.Plugins))
		if err := writeAtomically(defaultPath, generatedDefaults); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", displayPath(root, defaultPath), err)
			return 1
		}
		fmt.Printf("wrote %s (%d plugins)\n", displayPath(root, defaultPath), len(defaults.Plugins))
	case *check:
		identityOK := checkSnapshot(root, "identity", identityPath, generatedIdentity)
		defaultOK := checkSnapshot(root, "default", defaultPath, generatedDefaults)
		if !identityOK || !defaultOK {
			return 1
		}
		fmt.Printf("%s is current (%d plugins)\n", displayPath(root, identityPath), len(identity.Plugins))
		fmt.Printf("%s is current (%d plugins)\n", displayPath(root, defaultPath), len(defaults.Plugins))
	}
	return 0
}

// checkSnapshot reports whether the on-disk snapshot at path already matches
// generated, printing a diagnostic (checked/generated sha256 plus the first
// differing line) to stderr when it does not.
func checkSnapshot(root, label, path string, generated []byte) bool {
	checked, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read checked %s snapshot %s: %v\n", label, displayPath(root, path), err)
		return false
	}
	if !bytes.Equal(checked, generated) {
		wantHash := sha256.Sum256(checked)
		gotHash := sha256.Sum256(generated)
		line, want, got := firstDifferentLine(checked, generated)
		fmt.Fprintf(
			os.Stderr,
			"%s snapshot drifted: %s\nchecked sha256:   %x\ngenerated sha256: %x\nfirst difference at line %d:\n  checked:   %s\n  generated: %s\nregenerate with: go run ./scripts/plugin-snapshots -write\n",
			label, displayPath(root, path), wantHash, gotHash, line, want, got,
		)
		return false
	}
	return true
}

func resolveOutputPath(root, output string) string {
	if filepath.IsAbs(output) {
		return filepath.Clean(output)
	}
	return filepath.Join(root, filepath.FromSlash(output))
}

func resolveRepositoryRoot(explicit string) (string, error) {
	if explicit != "" {
		root, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(root, "go.work")); err != nil {
			return "", fmt.Errorf("%s does not contain go.work: %w", root, err)
		}
		return filepath.Clean(root), nil
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.work")); err == nil {
			return filepath.Clean(current), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no go.work found at or above the current directory")
		}
		current = parent
	}
}

func writeAtomically(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".plugin-snapshot-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func displayPath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func firstDifferentLine(left, right []byte) (line int, leftLine, rightLine string) {
	leftLines := strings.Split(string(left), "\n")
	rightLines := strings.Split(string(right), "\n")
	limit := min(len(rightLines), len(leftLines))
	for index := range limit {
		if leftLines[index] != rightLines[index] {
			return index + 1, leftLines[index], rightLines[index]
		}
	}
	if len(leftLines) != len(rightLines) {
		leftValue, rightValue := "<missing>", "<missing>"
		if limit < len(leftLines) {
			leftValue = leftLines[limit]
		}
		if limit < len(rightLines) {
			rightValue = rightLines[limit]
		}
		return limit + 1, leftValue, rightValue
	}
	return 0, "<none>", "<none>"
}
