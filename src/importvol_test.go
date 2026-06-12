package main

// import-volumes tests — fully hermetic: argv construction is pure, execution
// goes through either the stubbed runPipeline hook or fake `docker` and
// `container` binaries on a temp PATH (extending the fakeContainer pattern).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeBins installs shell scripts under the given names at the front of PATH
// for the duration of the test (the multi-binary sibling of fakeContainer)
// and returns the directory holding them.
func fakeBins(t *testing.T, bins map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range bins {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// stubPipeline swaps runPipeline for a recording stub and restores it after
// the test. Each invocation's argv pair is appended to the returned slice.
func stubPipeline(t *testing.T, n int64, err error) *[][2][]string {
	t.Helper()
	calls := &[][2][]string{}
	orig := runPipeline
	runPipeline = func(src, dst []string) (int64, error) {
		*calls = append(*calls, [2][]string{src, dst})
		return n, err
	}
	t.Cleanup(func() { runPipeline = orig })
	return calls
}

const importFixtureYAML = `services:
  a:
    image: x
    volumes:
      - data:/d
      - cache:/c
volumes:
  data:
  cache:
`

func TestImportPipelineArgv(t *testing.T) {
	src, dst := importPipeline("proj-data", "proj-data")
	wantSrc := []string{"docker", "run", "--rm", "-v", "proj-data:/from", "alpine", "tar", "-C", "/from", "-cf", "-", "."}
	wantDst := []string{"container", "run", "--rm", "--volume", "proj-data:/to", "alpine", "tar", "-C", "/to", "-xf", "-"}
	if fmt.Sprint(src) != fmt.Sprint(wantSrc) {
		t.Errorf("src argv = %v, want %v", src, wantSrc)
	}
	if fmt.Sprint(dst) != fmt.Sprint(wantDst) {
		t.Errorf("dst argv = %v, want %v", dst, wantDst)
	}
}

func TestNamedVolumeKeys(t *testing.T) {
	p := projectFromYAML(t, `services:
  a:
    image: x
    volumes:
      - data:/d
      - ext:/e
      - ./bind:/b
volumes:
  data:
  ext:
    external: true
`)
	got := namedVolumeKeys(p)
	if fmt.Sprint(got) != fmt.Sprint([]string{"data"}) {
		t.Errorf("namedVolumeKeys = %v, want [data] (external and bind mounts excluded)", got)
	}
}

func TestResolveImportKeys(t *testing.T) {
	p := projectFromYAML(t, importFixtureYAML)

	t.Run("no request means every named volume, sorted", func(t *testing.T) {
		keys, err := resolveImportKeys(p, nil)
		if err != nil || fmt.Sprint(keys) != fmt.Sprint([]string{"cache", "data"}) {
			t.Errorf("resolveImportKeys = (%v, %v), want ([cache data], nil)", keys, err)
		}
	})

	t.Run("requested keys are sorted and deduplicated", func(t *testing.T) {
		keys, err := resolveImportKeys(p, []string{"data", "cache", "data"})
		if err != nil || fmt.Sprint(keys) != fmt.Sprint([]string{"cache", "data"}) {
			t.Errorf("resolveImportKeys = (%v, %v), want ([cache data], nil)", keys, err)
		}
	})

	t.Run("unknown key fails listing the valid ones", func(t *testing.T) {
		_, err := resolveImportKeys(p, []string{"nope"})
		if err == nil {
			t.Fatal("want error for unknown key")
		}
		mustContain(t, err.Error(), "error", "unknown volume 'nope'", "valid keys: cache, data")
	})
}

func TestImportVolumesUnknownKeyExit1(t *testing.T) {
	p := projectFromYAML(t, importFixtureYAML)
	var code int
	_, stderr := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, []string{"nope"})
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	mustContain(t, stderr, "stderr", "unknown volume 'nope'", "valid keys: cache, data")
}

// Dry-run prints, per volume and sorted, the volume create line plus both
// pipeline command lines — the transcript of what a real run performs (the
// emptiness probe is replaced by a note: it reads state a dry run cannot
// have) — and executes nothing: runPipeline is booby-trapped and PATH holds
// no docker at all.
func TestImportVolumesDryRun(t *testing.T) {
	p := projectFromYAML(t, importFixtureYAML)
	stubPipeline(t, 0, errors.New("dry-run must not execute"))
	runPipeline = func(src, dst []string) (int64, error) {
		t.Error("runPipeline called during --dry-run")
		return 0, nil
	}
	t.Setenv("PATH", t.TempDir()) // no docker, no container — dry needs neither

	var code int
	stdout, _ := captureOutput(t, func() {
		code = importVolumesRun(p, runner{dry: true}, nil)
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	cacheVol, dataVol := volName(p, "cache"), volName(p, "data")
	mustOrder(t, stdout, "stdout",
		"$ container volume create "+cacheVol,
		"$ docker run --rm -v "+cacheVol+":/from alpine tar -C /from -cf - . | container run --rm --volume "+cacheVol+":/to alpine tar -C /to -xf -",
		"$ container volume create "+dataVol,
		"$ docker run --rm -v "+dataVol+":/from alpine tar -C /from -cf - . | container run --rm --volume "+dataVol+":/to alpine tar -C /to -xf -",
		"(target emptiness is checked on a real run)")
}

func TestImportVolumesDockerMissingExit1(t *testing.T) {
	p := projectFromYAML(t, importFixtureYAML)
	t.Setenv("PATH", t.TempDir()) // empty dir: no docker anywhere on PATH
	var code int
	_, stderr := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, nil)
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	mustContain(t, stderr, "stderr",
		"docker CLI not found — import-volumes copies FROM Docker/OrbStack, which must still be installed")
}

func TestImportVolumesSourceMissingSkipped(t *testing.T) {
	p := projectFromYAML(t, "services:\n  a:\n    image: x\n    volumes:\n      - data:/d\nvolumes:\n  data:\n")
	fakeBins(t, map[string]string{
		"docker":    `if [ "$1" = volume ]; then echo "Error: no such volume" >&2; exit 1; fi; exit 0`,
		"container": `exit 0`,
	})
	calls := stubPipeline(t, 0, nil)

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, nil)
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a skip is not a failure)", code)
	}
	mustContain(t, stderr, "stderr", "[data] no docker volume named '"+volName(p, "data")+"' — skipped")
	mustContain(t, stdout, "stdout", "imported 0 volume(s), skipped 1")
	if len(*calls) != 0 {
		t.Errorf("runPipeline called %d times, want 0", len(*calls))
	}
}

func TestImportVolumesTargetHasDataRefused(t *testing.T) {
	p := projectFromYAML(t, "services:\n  a:\n    image: x\n    volumes:\n      - data:/d\nvolumes:\n  data:\n")
	fakeBins(t, map[string]string{
		"docker":    `exit 0`, // volume inspect ok, ps prints nothing
		"container": `if [ "$1" = run ]; then echo "pgdata"; fi; exit 0`,
	})
	calls := stubPipeline(t, 0, nil)

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, nil)
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (refusal counts as skipped)", code)
	}
	mustContain(t, stderr, "stderr",
		"[data] target volume already has data — delete it first: container volume delete "+volName(p, "data"))
	mustContain(t, stdout, "stdout", "imported 0 volume(s), skipped 1")
	if len(*calls) != 0 {
		t.Errorf("runPipeline called %d times, want 0", len(*calls))
	}
}

// A FAILED emptiness probe proves nothing about the target volume — it must
// never fall through into an overwrite. Warn + skip, quietly.
func TestImportVolumesProbeFailureSkips(t *testing.T) {
	p := projectFromYAML(t, "services:\n  a:\n    image: x\n    volumes:\n      - data:/d\nvolumes:\n  data:\n")
	fakeBins(t, map[string]string{
		"docker": `exit 0`, // volume inspect ok, ps prints nothing
		"container": `if [ "$1" = run ]; then echo "apiserver is not running" >&2; exit 1; fi
exit 0`, // volume create ok, the emptiness probe FAILS
	})
	calls := stubPipeline(t, 0, nil)

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, nil)
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a skip is not a failure)", code)
	}
	mustContain(t, stderr, "stderr",
		"[data] cannot verify the target volume is empty — skipped (is the system service running?)")
	mustNotContain(t, stderr, "stderr", "command failed") // tolerated, not loud
	mustContain(t, stdout, "stdout", "imported 0 volume(s), skipped 1")
	if len(*calls) != 0 {
		t.Errorf("runPipeline called %d times after a failed probe, want 0", len(*calls))
	}
}

func TestImportVolumesRunningContainerWarnsButProceeds(t *testing.T) {
	p := projectFromYAML(t, "services:\n  a:\n    image: x\n    volumes:\n      - data:/d\nvolumes:\n  data:\n")
	fakeBins(t, map[string]string{
		"docker":    `if [ "$1" = ps ]; then echo "my-postgres"; fi; exit 0`,
		"container": `exit 0`, // volume create ok, emptiness probe prints nothing
	})
	calls := stubPipeline(t, 4096, nil)

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, nil)
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	mustContain(t, stderr, "stderr",
		"[data] docker container 'my-postgres' is using this volume — stop it first for a consistent copy")
	mustContain(t, stdout, "stdout", "[data] imported (4.0 KiB)", "imported 1 volume(s), skipped 0")
	if len(*calls) != 1 {
		t.Errorf("runPipeline called %d times, want 1", len(*calls))
	}
}

// Success path: stubbed runPipeline records each argv pair; both volumes go
// through in sorted key order with the exact importPipeline argv.
func TestImportVolumesSuccess(t *testing.T) {
	p := projectFromYAML(t, importFixtureYAML)
	fakeBins(t, map[string]string{
		"docker":    `exit 0`,
		"container": `exit 0`,
	})
	calls := stubPipeline(t, 2048, nil)

	var code int
	stdout, _ := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, nil)
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	mustOrder(t, stdout, "stdout",
		"[cache] imported (2.0 KiB)",
		"[data] imported (2.0 KiB)",
		"imported 2 volume(s), skipped 0")
	if len(*calls) != 2 {
		t.Fatalf("runPipeline called %d times, want 2", len(*calls))
	}
	for i, key := range []string{"cache", "data"} { // sorted key order
		wantSrc, wantDst := importPipeline(volName(p, key), volName(p, key))
		if fmt.Sprint((*calls)[i][0]) != fmt.Sprint(wantSrc) || fmt.Sprint((*calls)[i][1]) != fmt.Sprint(wantDst) {
			t.Errorf("call %d argv = %v, want (%v, %v)", i, (*calls)[i], wantSrc, wantDst)
		}
	}
}

func TestImportVolumesPipelineFailureExit1(t *testing.T) {
	p := projectFromYAML(t, "services:\n  a:\n    image: x\n    volumes:\n      - data:/d\nvolumes:\n  data:\n")
	fakeBins(t, map[string]string{
		"docker":    `exit 0`,
		"container": `exit 0`,
	})
	stubPipeline(t, 0, errors.New("docker side failed: boom"))

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = importVolumesRun(p, runner{}, nil)
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1 on pipeline failure", code)
	}
	mustContain(t, stderr, "stderr", "[data] docker side failed: boom")
	mustContain(t, stdout, "stdout", "imported 0 volume(s), skipped 0")
}

// Two runs over the same two-volume project always process in sorted key
// order — the dry-run transcript is byte-identical.
func TestImportVolumesDeterministicOrder(t *testing.T) {
	p := projectFromYAML(t, importFixtureYAML)
	first, _ := captureOutput(t, func() { importVolumesRun(p, runner{dry: true}, nil) })
	second, _ := captureOutput(t, func() { importVolumesRun(p, runner{dry: true}, nil) })
	if first != second {
		t.Errorf("dry-run output not deterministic:\n--- first\n%s--- second\n%s", first, second)
	}
	mustOrder(t, first, "stdout", volName(p, "cache")+":/from", volName(p, "data")+":/from")
}

// The REAL runPipeline against fake docker/container binaries: success
// counts the streamed bytes; each side's failure is attributed with its
// stderr tail.
func TestRunPipelineReal(t *testing.T) {
	argvFor := func() ([]string, []string) { return importPipeline("proj-data", "proj-data") }

	t.Run("success streams and counts bytes", func(t *testing.T) {
		fakeBins(t, map[string]string{
			"docker":    `printf '12345678'`,
			"container": `cat > /dev/null; exit 0`,
		})
		src, dst := argvFor()
		n, err := runPipeline(src, dst)
		if err != nil || n != 8 {
			t.Errorf("runPipeline = (%d, %v), want (8, nil)", n, err)
		}
	})

	t.Run("docker side failure is attributed with stderr tail", func(t *testing.T) {
		fakeBins(t, map[string]string{
			"docker":    `echo "no such volume" >&2; exit 1`,
			"container": `cat > /dev/null; exit 0`,
		})
		src, dst := argvFor()
		_, err := runPipeline(src, dst)
		if err == nil {
			t.Fatal("want error")
		}
		mustContain(t, err.Error(), "error", "docker side failed", "no such volume")
	})

	t.Run("container side failure is attributed with stderr tail", func(t *testing.T) {
		fakeBins(t, map[string]string{
			"docker":    `printf 'x'`,
			"container": `echo "untar exploded" >&2; exit 1`,
		})
		src, dst := argvFor()
		_, err := runPipeline(src, dst)
		if err == nil {
			t.Fatal("want error")
		}
		mustContain(t, err.Error(), "error", "container side failed", "untar exploded")
	})
}

// dst dying immediately while src is still slow must neither deadlock nor
// hang in exec's stdin copier (the io.Reader-Stdin failure mode): dst.Wait
// returns at once, src is still reaped when it exits, and the failure is
// attributed to the container side. Timing stays generous to avoid flakes —
// the src fake sleeps 2s, so anything past ~3s means a hang.
func TestRunPipelineDstDiesEarlySrcSlow(t *testing.T) {
	fakeBins(t, map[string]string{
		"docker":    `sleep 2; echo x`,
		"container": `echo "untar exploded" >&2; exit 1`,
	})
	src, dst := importPipeline("proj-data", "proj-data")
	start := time.Now()
	_, err := runPipeline(src, dst)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want error")
	}
	mustContain(t, err.Error(), "error", "container side failed", "untar exploded")
	if elapsed > 3*time.Second {
		t.Errorf("runPipeline took %v, want under ~3s (src must be reaped, not waited on forever)", elapsed)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{2048, "2.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{5 << 30, "5.0 GiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestStderrTail(t *testing.T) {
	if got := stderrTail("first\nlast line\n", errors.New("exit 1")); got != "last line" {
		t.Errorf("stderrTail = %q, want last line", got)
	}
	if got := stderrTail("", errors.New("exit 1")); got != "exit 1" {
		t.Errorf("stderrTail on empty stderr = %q, want the error text", got)
	}
}
