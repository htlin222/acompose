package main

// `acompose import-volumes` — migrate named-volume DATA from Docker/OrbStack.
//
// The biggest cost of switching runtimes is data: your postgres volume lives
// in Docker (or OrbStack, which speaks the docker CLI). This command copies
// named volumes across: the docker side streams a tar of the volume, the
// Apple container side untars it into the (created-if-needed) named volume.
//
// Naming: docker compose calls volumes `<project>_<key>`; acompose's
// volName(p, key) resolves the same compose key, so the docker-side source
// name defaults to the exact same string as the target name.
//
// Style follows dns.go/doctor.go: a pure importVolumesRun(...) int with a
// thin cmdImportVolumes wrapper, pure argv construction (importPipeline) and
// an injectable execution hook (runPipeline) so tests never need docker.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// importPipeline builds the two argv slices of the copy pipeline: the docker
// side tars the source volume to stdout, the container side untars stdin into
// the target volume. Pure — the one place the pipeline flags live.
func importPipeline(source, target string) (src, dst []string) {
	src = []string{"docker", "run", "--rm", "-v", source + ":/from", "alpine", "tar", "-C", "/from", "-cf", "-", "."}
	dst = ctr("run", "--rm", "--volume", target+":/to", "alpine", "tar", "-C", "/to", "-xf", "-")
	return src, dst
}

// countingWriter counts the bytes streamed through the pipe so the success
// line can report a best-effort size.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// stderrTail returns the last non-empty stderr line (the part worth showing),
// falling back to the exec error itself when the side wrote nothing.
func stderrTail(stderr string, err error) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}

// runPipeline executes src | dst with stderr collected per side, returning
// the byte count that flowed through the pipe. A var so tests can stub the
// execution while still asserting argv construction via importPipeline.
//
// dst.Stdin is a REAL *os.File (an os.Pipe read end), never a bare
// io.Reader: with a Reader, dst.Wait() also waits for exec's internal
// stdin-copying goroutine, which can block forever in Read when src hangs
// after dst has already died. With an *os.File the child inherits the fd
// directly and Wait() only reaps the process; we run the counting copy
// src.StdoutPipe → write end ourselves.
var runPipeline = func(srcArgv, dstArgv []string) (int64, error) {
	src := exec.Command(srcArgv[0], srcArgv[1:]...)
	dst := exec.Command(dstArgv[0], dstArgv[1:]...)
	var srcErrBuf, dstErrBuf bytes.Buffer
	src.Stderr, dst.Stderr = &srcErrBuf, &dstErrBuf

	stdout, err := src.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("docker side failed to start: %v", err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("container side failed to start: %v", err)
	}
	dst.Stdin = pr

	if err := src.Start(); err != nil {
		pr.Close()
		pw.Close()
		return 0, fmt.Errorf("docker side failed to start: %v", err)
	}
	if err := dst.Start(); err != nil {
		pr.Close()
		pw.Close()
		_ = src.Process.Kill()
		_ = src.Wait()
		return 0, fmt.Errorf("container side failed to start: %v", err)
	}
	counter := &countingWriter{}
	copyDone := make(chan struct{})
	go func() {
		// the counting copy: closes the write end when it returns (EOF or
		// error) so dst's stdin sees EOF on the normal path, and closes
		// stdout so a src still streaming after dst died gets EPIPE instead
		// of blocking forever into an undrained pipe.
		_, _ = io.Copy(pw, io.TeeReader(stdout, counter))
		pw.Close()
		stdout.Close()
		close(copyDone)
	}()
	// Wait for the consumer first. If dst dies EARLY, closing our read end
	// right after gives the copy goroutine EPIPE on its next write — it can
	// never block forever — and src is then reaped normally by src.Wait.
	dstErr := dst.Wait()
	pr.Close()
	srcErr := src.Wait()
	<-copyDone // counter is quiescent from here on
	// Root-cause attribution: a src that died of a broken pipe was killed BY
	// the dst failure, so the container side is the story to tell.
	if srcErr != nil && !strings.Contains(srcErr.Error(), "broken pipe") {
		return counter.n, fmt.Errorf("docker side failed: %s", stderrTail(srcErrBuf.String(), srcErr))
	}
	if dstErr != nil {
		return counter.n, fmt.Errorf("container side failed: %s", stderrTail(dstErrBuf.String(), dstErr))
	}
	if srcErr != nil {
		return counter.n, fmt.Errorf("docker side failed: %s", stderrTail(srcErrBuf.String(), srcErr))
	}
	return counter.n, nil
}

// humanBytes renders a byte count for the success line: 512 → "512 B",
// 1536 → "1.5 KiB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// namedVolumeKeys returns the sorted COMPOSE KEYS of every non-external named
// volume referenced by a service mount — the key-level twin of namedVolumes
// (which returns runtime names), because import-volumes addresses volumes by
// their compose key.
func namedVolumeKeys(p *types.Project) []string {
	set := map[string]bool{}
	for _, svc := range p.Services {
		for _, m := range svc.Volumes {
			if m.Type != "volume" || m.Source == "" {
				continue
			}
			if cfg, ok := p.Volumes[m.Source]; ok && bool(cfg.External) {
				continue
			}
			set[m.Source] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveImportKeys validates the requested keys against the project (every
// key must be a declared, service-referenced named volume) and returns them
// sorted and deduplicated; no request means all of them.
func resolveImportKeys(p *types.Project, requested []string) ([]string, error) {
	valid := namedVolumeKeys(p)
	if len(requested) == 0 {
		return valid, nil
	}
	validSet := map[string]bool{}
	for _, k := range valid {
		validSet[k] = true
	}
	set := map[string]bool{}
	for _, k := range requested {
		if !validSet[k] {
			return nil, fmt.Errorf("unknown volume '%s' — valid keys: %s", k, strings.Join(valid, ", "))
		}
		set[k] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// dockerOut runs a docker CLI query (NOT through ctr/runner — this is the
// side we are migrating away from) and returns its trimmed stdout.
func dockerOut(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// importVolumesRun migrates each requested named volume from the docker side
// and returns the process exit code: 0 when no hard failures (skips are ok),
// 1 on any pipeline or environment failure.
func importVolumesRun(p *types.Project, r runner, requested []string) int {
	keys, err := resolveImportKeys(p, requested)
	if err != nil {
		fail("%v", err)
		return 1
	}
	if len(keys) == 0 {
		info("no named volumes in %s%s%s — nothing to import", bold, p.Name, reset)
		return 0
	}

	if r.dry {
		// show what a real run performs: the volume create plus both sides of
		// every pipeline; needs no docker installed. The emptiness probe is
		// skipped by design — it exists to READ the target's state, which a
		// dry run cannot do — so a note stands in for it.
		for _, key := range keys {
			target := volName(p, key)
			r.run(ctr("volume", "create", target), "exist")
			src, dst := importPipeline(target, target)
			fmt.Printf("  %s$%s %s | %s\n", dim, reset, strings.Join(src, " "), strings.Join(dst, " "))
		}
		fmt.Printf("  %s(target emptiness is checked on a real run)%s\n", dim, reset)
		return 0
	}

	if _, err := exec.LookPath("docker"); err != nil {
		fail("docker CLI not found — import-volumes copies FROM Docker/OrbStack, which must still be installed")
		return 1
	}

	imported, skipped, failures := 0, 0, 0
	for _, key := range keys {
		target := volName(p, key)
		source := target // docker compose names volumes the same way

		if _, err := dockerOut("volume", "inspect", source); err != nil {
			warn("[%s] no docker volume named '%s' — skipped", key, source)
			skipped++
			continue
		}

		// a still-running docker container can change the data mid-copy —
		// warn, but proceed: it is the user's call.
		if names, err := dockerOut("ps", "--filter", "volume="+source, "--format", "{{.Names}}"); err == nil && names != "" {
			first := strings.Fields(names)[0]
			warn("[%s] docker container '%s' is using this volume — stop it first for a consistent copy", key, first)
		}

		r.run(ctr("volume", "create", target), "exist")

		// refuse to untar over existing data — cheap emptiness probe. A probe
		// that FAILS proves nothing about the target, so it must never fall
		// through into an overwrite: warn and skip. The failure itself is
		// tolerated (kept quiet) — the warning is the story.
		probe := ctr("run", "--rm", "--volume", target+":/to", "alpine", "sh", "-c", "ls -A /to | grep -v lost+found | head -1")
		ok, out := r.run(probe, "error", "not", "failed", "unable", "cannot", "stopped")
		if !ok {
			warn("[%s] cannot verify the target volume is empty — skipped (is the system service running?)", key)
			skipped++
			continue
		}
		if strings.TrimSpace(out) != "" {
			warn("[%s] target volume already has data — delete it first: container volume delete %s", key, target)
			skipped++
			continue
		}

		src, dst := importPipeline(source, target)
		n, err := runPipeline(src, dst)
		if err != nil {
			fail("[%s] %v", key, err)
			failures++
			continue
		}
		if n > 0 {
			okay("[%s] imported (%s)", key, humanBytes(n))
		} else {
			okay("[%s] imported", key)
		}
		imported++
	}

	fmt.Printf("imported %d volume(s), skipped %d\n", imported, skipped)
	if failures > 0 {
		return 1
	}
	return 0
}

func cmdImportVolumes(p *types.Project, r runner, keys []string) {
	if code := importVolumesRun(p, r, keys); code != 0 {
		os.Exit(code)
	}
}
