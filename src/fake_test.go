package main

// Non-dry-run tests against a FAKE `container` executable: a tiny /bin/sh
// script written into a temp dir that is prepended to PATH. The real Apple
// `container` binary is never executed (and need not exist — CI is Ubuntu);
// even on a Mac that has it, the fake shadows it.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeContainer installs a shell script named `container` at the front of
// PATH for the duration of the test.
func fakeContainer(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "container")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunnerRunReal(t *testing.T) {
	t.Run("success trims combined output", func(t *testing.T) {
		fakeContainer(t, `echo "  hello  "`)
		ok, out := runner{}.run([]string{"container", "anything"})
		if !ok || out != "hello" {
			t.Errorf("run = (%v, %q), want (true, hello)", ok, out)
		}
	})

	t.Run("tolerated failure is quiet", func(t *testing.T) {
		fakeContainer(t, `echo "network already exists"; exit 1`)
		var ok bool
		var out string
		_, stderr := captureOutput(t, func() {
			ok, out = runner{}.run([]string{"container", "network", "create", "x"}, "exist")
		})
		if ok || out != "network already exists" {
			t.Errorf("run = (%v, %q), want (false, network already exists)", ok, out)
		}
		mustNotContain(t, stderr, "stderr", "command failed")
	})

	t.Run("untolerated failure is loud", func(t *testing.T) {
		fakeContainer(t, `echo "boom"; exit 1`)
		var ok bool
		_, stderr := captureOutput(t, func() {
			ok, _ = runner{}.run([]string{"container", "nope"})
		})
		if ok {
			t.Error("run ok = true, want false")
		}
		mustContain(t, stderr, "stderr", "command failed: container nope", "boom")
	})
}

// runner.run exits the process when the binary is missing entirely.
func TestRunnerRunBinaryMissingSubprocess(t *testing.T) {
	if os.Getenv("ACOMPOSE_TEST_NOBIN") == "1" {
		os.Setenv("PATH", os.Getenv("ACOMPOSE_TEST_EMPTY_DIR"))
		runner{}.run([]string{"container", "ls"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunnerRunBinaryMissingSubprocess$")
	cmd.Env = append(os.Environ(),
		"ACOMPOSE_TEST_NOBIN=1",
		"ACOMPOSE_TEST_EMPTY_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got err=%v, output:\n%s", err, out)
	}
	mustContain(t, string(out), "subprocess output", "`container` not found")
}

func TestGetIPReal(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{"json inspect output", `echo '{"networks":[{"address":"192.168.64.5/24","gateway":"192.168.64.1"}]}'`, "192.168.64.5"},
		{"non-json output falls back to first IP-looking token", `echo 'address: 192.168.64.6 state: running'`, "192.168.64.6"},
		{"empty output", `true`, ""},
		{"inspect failure", `echo "no such container"; exit 1`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeContainer(t, tc.script)
			var got string
			captureOutput(t, func() { got = getIP(runner{}, "mydb") }) // swallow tolerated-failure noise
			if got != tc.want {
				t.Errorf("getIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestImageDigestsReal(t *testing.T) {
	fakeContainer(t, `echo '[{"digest":"sha256:aa"},{"digest":"sha256:bb"},{"blobSum":"sha256:layer"}]'`)
	set, ok := imageDigests(runner{}, "nginx")
	if !ok || len(set) != 2 || !set["sha256:aa"] || !set["sha256:bb"] {
		t.Errorf("imageDigests = (%v, %v), want both manifest digests", set, ok)
	}
}

func TestWireHostsReal(t *testing.T) {
	pairs := [][2]string{{"db", "192.168.64.2"}}

	t.Run("empty pairs is a no-op", func(t *testing.T) {
		wireHosts(runner{}, "c", nil, "svc") // must not touch PATH at all
	})

	cases := []struct {
		name, svc, script, wantWarn string
	}{
		{"shell-less image", "noshell", `echo "failed to find target executable"; exit 1`, "image has no shell"},
		{"non-root image", "nonroot", `echo "permission denied"; exit 1`, "/etc/hosts not writable"},
		{"generic failure", "generic", `echo "kaboom"; exit 1`, "could not write /etc/hosts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delete(hostsWarned, tc.svc) // global dedupe map; reset for -count=2 reruns
			fakeContainer(t, tc.script)
			_, stderr := captureOutput(t, func() {
				wireHosts(runner{}, "c", pairs, tc.svc)
			})
			mustContain(t, stderr, "stderr", tc.wantWarn)

			// second failure for the same service is deduped
			_, stderr2 := captureOutput(t, func() {
				wireHosts(runner{}, "c", pairs, tc.svc)
			})
			mustNotContain(t, stderr2, "stderr", tc.wantWarn)
		})
	}

	t.Run("success warns nothing", func(t *testing.T) {
		delete(hostsWarned, "happy")
		fakeContainer(t, `true`)
		_, stderr := captureOutput(t, func() {
			wireHosts(runner{}, "c", pairs, "happy")
		})
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})
}

// Full cmdUp against the fake runtime: real runner paths, getIP, the
// service_healthy wait (timeout forced to ~0 so no wall time is spent),
// bidirectional hosts wiring, and the final `ls` liveness verification.
func TestCmdUpRealFakeRuntime(t *testing.T) {
	p := loadUpFixture(t)
	fakeContainer(t, `case "$1" in
  inspect) echo '{"address":"192.168.64.9/24"}' ;;
  ls) echo 'ID IMAGE STATE'; echo 'mydb postgres running'; echo 'proj-app nginx running' ;;
esac
exit 0`)
	stdout, stderr := captureOutput(t, func() {
		// negative timeout: the wait deadline is already in the past, so
		// waitTCP warns immediately without ever dialing the fake IP
		cmdUp(p, runner{}, true, -time.Second)
	})
	mustContain(t, stdout, "stdout",
		"waiting for db (service_healthy", // dep wait engaged…
		"build app",
		"stack up",
		"✓ running",
		"192.168.64.9")
	mustContain(t, stderr, "stderr", "db: no TCP answer") // …and timed out instantly
}

func TestCmdUpRealExistingContainerIsStarted(t *testing.T) {
	p := projectFromYAML(t, "services:\n  solo:\n    image: nginx\n")
	fakeContainer(t, `case "$1" in
  run) echo "container proj-solo already exists"; exit 1 ;;
  start) echo "started" ;;
  inspect) echo '{"address":"192.168.64.9/24"}' ;;
  ls) echo 'ID IMAGE STATE'; echo 'proj-solo nginx stopped' ;;
esac
exit 0`)
	stdout, stderr := captureOutput(t, func() {
		cmdUp(p, runner{}, true, time.Nanosecond)
	})
	mustContain(t, stdout, "stdout",
		"[solo] container exists — starting it",
		"✗ stopped") // the fake's ls says it didn't stay up
	mustContain(t, stderr, "stderr", "1 service(s) not running")
	mustNotContain(t, stdout, "stdout", "stack up")
}

func TestCmdUpRealHealthWaitWithoutPortWarns(t *testing.T) {
	p := projectFromYAML(t, `services:
  one:
    image: img1
  two:
    image: img2
    depends_on:
      one:
        condition: service_healthy
`)
	fakeContainer(t, `case "$1" in
  ls) echo 'ID IMAGE STATE'; echo 'proj-one img1 running'; echo 'proj-two img2 running' ;;
esac
exit 0`) // inspect prints nothing → no IP known for the dependency
	_, stderr := captureOutput(t, func() {
		cmdUp(p, runner{}, true, time.Nanosecond)
	})
	mustContain(t, stderr, "stderr",
		"cannot health-wait on 'one'",
		"could not determine IP")
}

func TestCmdUpdateReal(t *testing.T) {
	t.Run("unchanged digests stay current, build service recreated", func(t *testing.T) {
		p := loadUpFixture(t)
		fakeContainer(t, `if [ "$1" = image ] && [ "$2" = inspect ]; then echo '{"digest":"sha256:aaa"}'; exit 0; fi
if [ "$1" = inspect ]; then echo '{"address":"192.168.64.9/24"}'; fi
exit 0`)
		stdout, _ := captureOutput(t, func() {
			cmdUpdate(p, runner{}, true)
		})
		// non-dry run() executes silently, so only the info/okay lines show:
		// the pull happened but the digest didn't change → db stays current
		mustContain(t, stdout, "stdout",
			"already current: db",
			"recreate app",
			"updated: app")
		mustNotContain(t, stdout, "stdout", "recreate db")
	})

	t.Run("image missing locally counts as changed", func(t *testing.T) {
		p := projectFromYAML(t, "services:\n  app:\n    image: nginx:1\n")
		fakeContainer(t, `if [ "$1" = image ] && [ "$2" = inspect ]; then echo "no such image"; exit 1; fi
if [ "$1" = inspect ]; then echo '{"address":"192.168.64.9/24"}'; fi
exit 0`)
		stdout, _ := captureOutput(t, func() {
			cmdUpdate(p, runner{}, true)
		})
		mustContain(t, stdout, "stdout", "recreate app", "updated: app")
	})

	t.Run("failed pull leaves the service untouched", func(t *testing.T) {
		p := projectFromYAML(t, "services:\n  app:\n    image: nginx:1\n")
		fakeContainer(t, `if [ "$1" = image ] && [ "$2" = inspect ]; then echo '{"digest":"sha256:aaa"}'; exit 0; fi
if [ "$1" = image ] && [ "$2" = pull ]; then echo "registry unreachable"; exit 1; fi
if [ "$1" = inspect ]; then echo '{"address":"192.168.64.9/24"}'; fi
exit 0`)
		stdout, _ := captureOutput(t, func() {
			cmdUpdate(p, runner{}, true)
		})
		mustContain(t, stdout, "stdout", "everything already current")
		mustNotContain(t, stdout, "stdout", "recreate app")
	})
}

func TestCmdPs(t *testing.T) {
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n")
	fakeContainer(t, `echo 'ID IMAGE STATE'
echo 'proj-db postgres running'
echo 'other-x nginx running'`)
	stdout, _ := captureOutput(t, func() {
		cmdPs(p, runner{})
	})
	mustContain(t, stdout, "stdout", "ID IMAGE STATE", "proj-db postgres running")
	mustNotContain(t, stdout, "stdout", "other-x")
}

func TestPassthrough(t *testing.T) {
	fakeContainer(t, `echo "log line one"`)
	stdout, _ := captureOutput(t, func() {
		passthrough([]string{"container", "logs", "proj-db"})
	})
	mustContain(t, stdout, "stdout", "log line one")
}

func TestCollectState(t *testing.T) {
	p := projectFromYAML(t, `services:
  db:
    image: postgres
    ports:
      - "5432:5432"
  app:
    build:
      context: .
    depends_on:
      - db
  ghost:
    image: nginx
`)
	fakeContainer(t, `case "$1" in
  ls) echo 'ID IMAGE STATE'; echo 'proj-db postgres running'; echo 'proj-app x stopped' ;;
  inspect) echo '{"address":"192.168.64.4/24"}' ;;
esac
exit 0`)
	st := collectState(p)

	if st.Project != "proj" || st.Network != "proj-net" {
		t.Errorf("project/network = %q/%q", st.Project, st.Network)
	}
	wantOrder := []string{"db", "app", "ghost"}
	if fmt.Sprint(st.Order) != fmt.Sprint(wantOrder) {
		t.Errorf("order = %v, want %v", st.Order, wantOrder)
	}
	byName := map[string]svcState{}
	for _, s := range st.Services {
		byName[s.Name] = s
	}
	db := byName["db"]
	if db.State != "running" || db.IP != "192.168.64.4" {
		t.Errorf("db = %+v, want running with IP", db)
	}
	if len(db.Ports) != 1 || db.Ports[0].Host != "5432" || db.Ports[0].Target != 5432 {
		t.Errorf("db ports = %+v", db.Ports)
	}
	app := byName["app"]
	if app.State != "stopped" || app.IP != "" {
		t.Errorf("app = %+v, want stopped without IP", app)
	}
	if app.Image != "proj-app (built)" {
		t.Errorf("app image = %q, want 'proj-app (built)'", app.Image)
	}
	if strings.Join(app.Deps, ",") != "db" {
		t.Errorf("app deps = %v, want [db]", app.Deps)
	}
	if ghost := byName["ghost"]; ghost.State != "missing" {
		t.Errorf("ghost state = %q, want missing", ghost.State)
	}
}

// cmdWatch refuses --dry-run with exit code 2.
func TestCmdWatchDryRefusedSubprocess(t *testing.T) {
	if os.Getenv("ACOMPOSE_TEST_WATCH_DRY") == "1" {
		p := projectFromYAML(t, "services:\n  a:\n    image: x\n")
		cmdWatch(p, runner{dry: true}, time.Second)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCmdWatchDryRefusedSubprocess$")
	cmd.Env = append(os.Environ(), "ACOMPOSE_TEST_WATCH_DRY=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("want exit code 2, got err=%v, output:\n%s", err, out)
	}
	mustContain(t, string(out), "subprocess output", "watch needs a live runtime")
}

// loadProject's friendly "no compose file found" message exits 1.
func TestLoadProjectNoComposeFileSubprocess(t *testing.T) {
	if os.Getenv("ACOMPOSE_TEST_NOCOMPOSE") == "1" {
		loadProject(nil, "x") // cwd (set by the parent) is an empty dir
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestLoadProjectNoComposeFileSubprocess$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "ACOMPOSE_TEST_NOCOMPOSE=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got err=%v, output:\n%s", err, out)
	}
	mustContain(t, string(out), "subprocess output",
		"no compose file found",
		"acompose init")
}
