// Package hostprobe hosts scriptable host-capability probes
// for the Dependencies rows in docs/TODO.md. The probes are
// pure-stdlib Go (no third-party deps) and shell out to the
// host tooling (clang, llvm-objdump) via absolute argv, never
// `sh -c`. The package is a verifier — the production binary
// never imports it. Probes are exercised from `*_test.go`
// files so a missing host tool gracefully `t.Skip`s instead
// of breaking the gate.
package hostprobe

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ClangNotFoundError signals that the scriptable probe could
// not run because clang is not on PATH. The caller (a test)
// uses errors.Is to detect this and t.Skip — a host that
// lacks the eBPF toolchain must not fail the build, just
// surface the dep as missing in TODO row 10a.
type ClangNotFoundError struct {
	// Path is the absolute path that was LookPath'd.
	// Empty when the binary is on PATH but the caller
	// specified an absolute path that didn't resolve.
	Path string
	// Err wraps the underlying exec.LookPath error (if any).
	Err error
}

func (e *ClangNotFoundError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf(
			"hostprobe: clang not found at %q (%v); install via TODO row 10a (sudo apt install clang libbpf-dev llvm)",
			e.Path, e.Err)
	}
	return fmt.Sprintf(
		"hostprobe: clang not found on PATH (%v); install via TODO row 10a (sudo apt install clang libbpf-dev llvm)",
		e.Err)
}

// Unwrap exposes the underlying LookPath error so callers
// can errors.Is(err, exec.ErrNotFound) if they wish.
func (e *ClangNotFoundError) Unwrap() error { return e.Err }

// bpfSource is a minimal eBPF C source for the compile probe.
// The body is empty-by-design: the *point* of the probe is
// that clang accepts the file end-to-end against the host's
// BTF and emits a `.o` with a `.BTF` section. Anything more
// elaborate would couple the probe to eBPF runtime semantics
// that change between kernel versions; the empty program is
// BTF-only and stable forever.
const bpfSource = `/* hostprobe: minimal CO-RE eBPF source. */
#include "vmlinux.h"
SEC("license")
char _license[] = "GPL";
`

// ProbeResult captures the artefacts of a successful CO-RE
// compile probe. Used by tests for table-driven assertions
// (the BTF section must be present; the exit codes must be
// zero; the objdump output must contain ".BTF").
type ProbeResult struct {
	// ObjectPath is the absolute path of the compiled `.o`
	// written to a temp file. Owned by the caller — pass
	// to Cleanup() to remove.
	ObjectPath string
	// ClangVersion is the trimmed first line of
	// `clang --version`.
	ClangVersion string
	// ObjDumpBTFLine is the trimmed line from
	// `llvm-objdump -h <obj>` whose first column is `.BTF`
	// (or its absence, in which case the probe failed and
	// the caller surfaced a typed error).
	ObjDumpBTFLine string
}

// COReProbe runs the scriptable CO-RE compile probe for
// TODO row 10b. It (1) compiles a minimal eBPF C source
// with `clang -target bpf ... -c -o <obj>` using the host's
// `/sys/kernel/btf/vmlinux` as the CO-RE relocations source,
// then (2) runs `llvm-objdump -h <obj>` and asserts a `.BTF`
// section line is present. Returns a typed error if either
// host tool is missing or any step fails.
//
// The probe is invoked from a `*_test.go` so the absence of
// clang on this host does not break the gate — the test
// `t.Skip`s on ClangNotFoundError and TODO row 10a remains
// the operator-gated install action.
func COReProbe() (*ProbeResult, error) {
	clangPath, lookErr := exec.LookPath("clang")
	if lookErr != nil {
		return nil, &ClangNotFoundError{Err: lookErr}
	}
	objdumpPath, lookErr := exec.LookPath("llvm-objdump")
	if lookErr != nil {
		return nil, &ClangNotFoundError{
			Path: "llvm-objdump",
			Err:  lookErr,
		}
	}

	// Write the C source to a per-call temp file so two
	// probes do not race on the same path under
	// `-race -count=N`. `os.CreateTemp` is stdlib-only.
	srcFile, err := writeTempFile("hostprobe-bpf-*.c", bpfSource)
	if err != nil {
		return nil, fmt.Errorf("hostprobe: write source: %w", err)
	}
	defer func() { _ = os.Remove(srcFile) }()

	objFile, err := writeTempFile("hostprobe-bpf-*.o", "")
	if err != nil {
		return nil, fmt.Errorf("hostprobe: write obj: %w", err)
	}

	// Step 1: `clang -target bpf -c -I /usr/include ...
	// -o <obj>` against the host's BTF. The absolute argv
	// is required by the project's shell-out policy
	// (ADR 0004 / 0006: never `sh -c`).
	clangArgs := []string{
		clangPath,
		"-target", "bpf",
		"-c", srcFile,
		"-I", "/usr/include",
		"-o", objFile,
	}
	if runErr := exec.Command(clangArgs[0], clangArgs[1:]...).Run(); runErr != nil {
		_ = os.Remove(objFile)
		return nil, fmt.Errorf(
			"hostprobe: clang -target bpf -c failed: %w", runErr)
	}

	// Step 2: `llvm-objdump -h <obj>` to confirm a `.BTF`
	// section landed in the object. This is the canonical
	// verifier probe — a CO-RE compile that produces no
	// `.BTF` section is a non-portable BPF object.
	out, dumpErr := runCaptured(objdumpPath, "-h", objFile)
	if dumpErr != nil {
		_ = os.Remove(objFile)
		return nil, fmt.Errorf(
			"hostprobe: llvm-objdump -h failed: %w", dumpErr)
	}

	var btfLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ".BTF") {
			btfLine = strings.TrimSpace(line)
			break
		}
	}
	if btfLine == "" {
		_ = os.Remove(objFile)
		return nil, errors.New(
			"hostprobe: CO-RE compile produced no .BTF section " +
				"(host BTF / vmlinux.h mismatch?)")
	}

	// `clang --version` for the caller's diagnostic.
	verOut, verErr := runCaptured(clangPath, "--version")
	var ver string
	if verErr == nil {
		ver = firstLine(verOut)
	}

	return &ProbeResult{
		ObjectPath:     objFile,
		ClangVersion:   ver,
		ObjDumpBTFLine: btfLine,
	}, nil
}

// firstLine trims and returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// runCaptured runs argv[0] argv[1:]... and returns the
// combined stdout+stderr as a string. `exec.Command` is
// invoked with absolute argv (the caller passes an
// exec.LookPath-resolved path).
func runCaptured(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

// writeTempFile creates a per-call temp file with the given
// glob pattern, writes content into it, closes it, and
// returns the absolute path. stdlib-only.
func writeTempFile(pattern, content string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if content != "" {
		if _, err := f.WriteString(content); err != nil {
			return "", err
		}
	}
	return f.Name(), nil
}