// Command release-manifest generates and checks the committed release
// manifest: every module joined by go.work, the workspace modules each one
// imports, and the dependency-topology wave the module is tagged in.
//
// The manifest is the release-train checklist behind the service framework
// improvement plan's release order. Modules are tagged wave by wave -- core
// first, then the protocol-neutral integrations, then the Web-heavy and
// composition modules, then examples -- and a module may only gain a sibling
// requirement once every module it imports has a published tag. Architecture
// tests regenerate and compare the committed manifest, so neither a new
// module nor a new cross-module import can change the release order
// unnoticed.
//
// Usage:
//
//	go run ./scripts/release-manifest -stdout
//	go run ./scripts/release-manifest -write
//	go run ./scripts/release-manifest -check
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

const defaultManifestPath = "tests/architecture/testdata/release-manifest.json"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("release-manifest", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	write := flags.Bool("write", false, "write the generated release manifest to its checked-in path")
	check := flags.Bool("check", false, "fail when the checked-in release manifest differs from the generated one")
	stdout := flags.Bool("stdout", false, "write the generated release manifest to stdout")
	output := flags.String("output", defaultManifestPath, "checked release manifest path, relative to the repository root")
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

	generated, encoded, err := generate(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate release manifest: %v\n", err)
		return 1
	}
	summary := summarize(generated)

	path := resolveOutputPath(root, *output)
	switch {
	case *stdout:
		if _, err := os.Stdout.Write(encoded); err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			return 1
		}
	case *write:
		if err := writeAtomically(path, encoded); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", displayPath(root, path), err)
			return 1
		}
		fmt.Printf("wrote %s (%s)\n", displayPath(root, path), summary)
	case *check:
		if !checkManifest(root, path, encoded) {
			return 1
		}
		fmt.Printf("%s is current (%s)\n", displayPath(root, path), summary)
	}
	return 0
}

// summarize describes a manifest for terminal output: how many modules the
// release train tags and how many waves the topology needs.
func summarize(value manifest) string {
	released, waves := 0, 0
	for _, module := range value.Modules {
		if module.Release {
			released++
		}
		waves = max(waves, module.Wave+1)
	}
	return fmt.Sprintf("%d modules, %d released in %d waves", len(value.Modules), released, waves)
}

// checkManifest reports whether the checked-in manifest at path already
// matches generated, printing the checked and generated digests plus the
// first differing line when it does not.
func checkManifest(root, path string, generated []byte) bool {
	checked, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read checked release manifest %s: %v\n", displayPath(root, path), err)
		return false
	}
	if bytes.Equal(checked, generated) {
		return true
	}
	checkedHash := sha256.Sum256(checked)
	generatedHash := sha256.Sum256(generated)
	line, want, got := firstDifferentLine(checked, generated)
	fmt.Fprintf(
		os.Stderr,
		"release manifest drifted: %s\nchecked sha256:   %x\ngenerated sha256: %x\nfirst difference at line %d:\n  checked:   %s\n  generated: %s\nregenerate with: go run ./scripts/release-manifest -write\n",
		displayPath(root, path), checkedHash, generatedHash, line, want, got,
	)
	return false
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
	temporary, err := os.CreateTemp(filepath.Dir(path), ".release-manifest-*.tmp")
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
