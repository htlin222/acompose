package main

// Tests for `acompose menubar` (SwiftBar/xbar plugin output) and the
// `acompose start|stop SERVICE` subcommands it drives. renderMenubar is pure,
// so most assertions run against a hand-built uiState; the cmd-level paths
// run against the fakeContainer runtime.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"/with space/dir", "'/with space/dir'"},
		{"o'brien", `'o'\''brien'`},
		{"", "''"},
	}
	for _, tc := range cases {
		if got := shQuote(tc.in); got != tc.want {
			t.Errorf("shQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// menubarFixtureState is the hand-built state renderMenubar tests use: two
// running services with IPs (one with a duplicate published port), one
// stopped — a degraded 2/3 stack.
func menubarFixtureState() uiState {
	return uiState{
		Project: "myproj",
		Network: "myproj-net",
		Order:   []string{"db", "api", "worker"},
		Services: []svcState{
			{Name: "db", Cname: "myproj-db", State: "running", IP: "192.168.65.2",
				Ports: []portInfo{{Host: "5432", Target: 5432}}},
			{Name: "api", Cname: "myproj-api", State: "running", IP: "192.168.65.3",
				Ports: []portInfo{{Host: "8080", Target: 80}, {Host: "5432", Target: 5432}}},
			{Name: "worker", Cname: "myproj-worker", State: "stopped"},
		},
	}
}

const (
	menubarTestBin = "/Applications/My Tools/acompose"
	menubarTestDir = "/Users/me/my project"
)

func TestRenderMenubarDegraded(t *testing.T) {
	out := renderMenubar(menubarFixtureState(), menubarTestBin, menubarTestDir)
	lines := strings.Split(out, "\n")

	if lines[0] != "⛴ 2/3 | color=red" {
		t.Errorf("title = %q, want '⛴ 2/3 | color=red'", lines[0])
	}
	if lines[1] != "---" {
		t.Errorf("line after title = %q, want ---", lines[1])
	}
	mustContain(t, out, "menubar output",
		"myproj — 2/3 running | size=12",
		"🟢 db  192.168.65.2 | font=Menlo size=12",
		"🟢 api  192.168.65.3 | font=Menlo size=12",
		"🔴 worker  stopped | font=Menlo size=12")
	// no ANSI escapes ever — SwiftBar would render them as garbage
	mustNotContain(t, out, "menubar output", "\033")
}

func TestRenderMenubarActionLines(t *testing.T) {
	out := renderMenubar(menubarFixtureState(), menubarTestBin, menubarTestDir)

	// exact submenu action lines, including single-quoting of the space-y
	// workDir and binPath inside the /bin/sh -c payload
	mustContain(t, out, "menubar output",
		`-- stop | bash=/bin/sh param1=-c param2="cd '/Users/me/my project' && '/Applications/My Tools/acompose' stop db" terminal=false refresh=true`,
		`-- stop | bash=/bin/sh param1=-c param2="cd '/Users/me/my project' && '/Applications/My Tools/acompose' stop api" terminal=false refresh=true`,
		`-- start | bash=/bin/sh param1=-c param2="cd '/Users/me/my project' && '/Applications/My Tools/acompose' start worker" terminal=false refresh=true`)
	mustNotContain(t, out, "menubar output", "-- stop | bash=/bin/sh param1=-c param2=\"cd '/Users/me/my project' && '/Applications/My Tools/acompose' stop worker")

	// bottom whole-stack actions
	mustContain(t, out, "menubar output",
		`Start all (up) | bash=/bin/sh param1=-c param2="cd '/Users/me/my project' && '/Applications/My Tools/acompose' up" terminal=false refresh=true`,
		`Stop all (down) | bash=/bin/sh param1=-c param2="cd '/Users/me/my project' && '/Applications/My Tools/acompose' down" terminal=false refresh=true`,
		`Refresh /etc/hosts | bash=/bin/sh param1=-c param2="cd '/Users/me/my project' && '/Applications/My Tools/acompose' refresh" terminal=false refresh=true`)
}

func TestRenderMenubarPortsAndLinks(t *testing.T) {
	out := renderMenubar(menubarFixtureState(), menubarTestBin, menubarTestDir)

	mustContain(t, out, "menubar output",
		"Open dashboard (acompose ui) | href=http://127.0.0.1:4242")
	// deduped (5432 published by db AND api appears once, owned by the first
	// publisher) and numerically sorted (5432 before 8080)
	mustOrder(t, out, "menubar output",
		"localhost:5432 → db | href=http://localhost:5432",
		"localhost:8080 → api | href=http://localhost:8080")
	if n := strings.Count(out, "localhost:5432 →"); n != 1 {
		t.Errorf("port 5432 listed %d times, want 1 (deduped)", n)
	}
	mustNotContain(t, out, "menubar output", "localhost:5432 → api")
}

func TestRenderMenubarAllRunningTitle(t *testing.T) {
	st := menubarFixtureState()
	st.Services = st.Services[:2] // drop the stopped worker → 2/2
	out := renderMenubar(st, menubarTestBin, menubarTestDir)
	if first := strings.SplitN(out, "\n", 2)[0]; first != "⛴ 2/2" {
		t.Errorf("title = %q, want '⛴ 2/2' (no color when complete)", first)
	}
}

func TestRenderMenubarRunningWithoutIPShowsStateWord(t *testing.T) {
	st := uiState{Project: "p", Services: []svcState{{Name: "db", State: "running"}}}
	out := renderMenubar(st, "acompose", "/tmp")
	mustContain(t, out, "menubar output", "🟢 db  running | font=Menlo size=12")
}

func TestRenderMenubarMissingServiceDot(t *testing.T) {
	st := uiState{Project: "p", Services: []svcState{{Name: "ghost", State: "missing"}}}
	out := renderMenubar(st, "acompose", "/tmp")
	mustContain(t, out, "menubar output",
		"⚪ ghost  missing | font=Menlo size=12",
		`-- start | bash=/bin/sh param1=-c param2="cd '/tmp' && 'acompose' start ghost" terminal=false refresh=true`)
}

func TestRenderMenubarDeterministic(t *testing.T) {
	a := renderMenubar(menubarFixtureState(), menubarTestBin, menubarTestDir)
	b := renderMenubar(menubarFixtureState(), menubarTestBin, menubarTestDir)
	if a != b {
		t.Errorf("two renders differ:\n%s\n----\n%s", a, b)
	}
}

// runtime unreachable: ls fails → title ⛴ ?, one red item, exit code 0.
func TestMenubarRunUnreachable(t *testing.T) {
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n")
	fakeContainer(t, `echo "failed to connect to the system service"; exit 1`)
	var code int
	stdout, _ := captureOutput(t, func() { code = menubarRun(p) })
	if code != 0 {
		t.Errorf("menubarRun = %d, want 0 (a menu bar plugin must never hard-fail)", code)
	}
	mustContain(t, stdout, "stdout",
		"⛴ ?\n---\n",
		"container runtime unreachable — is the system service running? | color=red")
}

// full menubarRun against the fake runtime: one running service (with an IP
// from inspect), one stopped — both dots and the correct per-state actions.
func TestMenubarRunFull(t *testing.T) {
	p := projectFromYAML(t, `services:
  db:
    image: postgres
    ports:
      - "5432:5432"
  api:
    image: nginx
`)
	fakeContainer(t, `case "$1" in
  ls) echo 'ID IMAGE STATE'; echo 'proj-db postgres running'; echo 'proj-api nginx stopped' ;;
  inspect) echo '{"address":"192.168.64.7/24"}' ;;
esac
exit 0`)
	var code int
	stdout, _ := captureOutput(t, func() { code = menubarRun(p) })
	if code != 0 {
		t.Errorf("menubarRun = %d, want 0", code)
	}
	mustContain(t, stdout, "stdout",
		"⛴ 1/2 | color=red",
		"proj — 1/2 running | size=12",
		"🟢 db  192.168.64.7 | font=Menlo size=12",
		"🔴 api  stopped | font=Menlo size=12",
		"-- stop |",
		"-- start |",
		"' stop db\" terminal=false refresh=true",
		"' start api\" terminal=false refresh=true",
		"localhost:5432 → db | href=http://localhost:5432",
		"Open dashboard (acompose ui) | href=http://127.0.0.1:4242")
	mustNotContain(t, stdout, "stdout", "\033")
}

// ---------- acompose start|stop SERVICE ------------------------------------

// argLoggingFake installs a fake `container` that appends every invocation's
// arguments to a log file and returns the log path.
func argLoggingFake(t *testing.T, extra string) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls.log")
	fakeContainer(t, `echo "$@" >> "`+log+`"`+"\n"+extra)
	return log
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake container was never invoked: %v", err)
	}
	return string(b)
}

func TestStartStopRunStop(t *testing.T) {
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n    container_name: mydb\n")
	log := argLoggingFake(t, "exit 0")
	var code int
	stdout, _ := captureOutput(t, func() {
		code = startStopRun(p, runner{}, "stop", []string{"db"}, true)
	})
	if code != 0 {
		t.Errorf("stop = %d, want 0", code)
	}
	mustContain(t, readLog(t, log), "container calls", "stop mydb")
	mustContain(t, stdout, "stdout", "db stopped")
}

func TestStartStopRunStartStopped(t *testing.T) {
	// single-service project: ensureServiceRunning's `container start`
	// succeeds (stopped → started) and no rewiring runs (no peers)
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n")
	log := argLoggingFake(t, "exit 0")
	var code int
	stdout, _ := captureOutput(t, func() {
		code = startStopRun(p, runner{}, "start", []string{"db"}, true)
	})
	if code != 0 {
		t.Errorf("start = %d, want 0", code)
	}
	mustContain(t, readLog(t, log), "container calls", "start proj-db")
	mustContain(t, stdout, "stdout", "db started")
}

func TestStartStopRunUnknownService(t *testing.T) {
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n  api:\n    image: nginx\n")
	var code int
	_, stderr := captureOutput(t, func() {
		code = startStopRun(p, runner{}, "start", []string{"nope"}, true)
	})
	if code != 1 {
		t.Errorf("unknown service = %d, want 1", code)
	}
	mustContain(t, stderr, "stderr", "unknown service 'nope'", "api, db")
}

func TestStartStopRunMissingArg(t *testing.T) {
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n")
	for _, verb := range []string{"start", "stop"} {
		var code int
		_, stderr := captureOutput(t, func() {
			code = startStopRun(p, runner{}, verb, nil, true)
		})
		if code != 2 {
			t.Errorf("%s without arg = %d, want 2", verb, code)
		}
		mustContain(t, stderr, "stderr", "usage: acompose "+verb+" SERVICE")
	}
}

func TestStartStopRunStopFailure(t *testing.T) {
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n")
	fakeContainer(t, `echo "some hard failure"; exit 1`)
	var code int
	captureOutput(t, func() {
		code = startStopRun(p, runner{}, "stop", []string{"db"}, true)
	})
	if code != 1 {
		t.Errorf("failed stop = %d, want 1", code)
	}
}
