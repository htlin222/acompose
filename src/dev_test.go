package main

// Tests for `acompose dev` (dev.go). Everything is hermetic: compose parsing
// goes through the production loadProject path, file fixtures live in temp
// dirs, and every runtime interaction hits the fakeContainer shim — no real
// `container` binary, no sleeps (devTick is called directly, so the debounce
// is exercised by call count, not wall time).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// fakeContainerLogging installs a fake `container` that appends every argv to
// a log file and succeeds; `extra` (shell) runs first, so tests can fail
// specific subcommands. Returns a func reading the log so far.
func fakeContainerLogging(t *testing.T, extra string) func() string {
	t.Helper()
	logf := filepath.Join(t.TempDir(), "argv.log")
	fakeContainer(t, extra+"\necho \"$@\" >> "+logf)
	return func() string {
		b, _ := os.ReadFile(logf)
		return string(b)
	}
}

// mkfile writes a file, creating parent dirs.
func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------- trigger resolution -------------------------------------------------

const devFixtureYAML = `services:
  api:
    build:
      context: .
    develop:
      watch:
        - path: go.mod
          action: rebuild
        - path: ./src
          action: sync
          target: /app/src
          ignore:
            - "*.tmp"
  web:
    image: nginx
    develop:
      watch:
        - path: ./html
          action: sync+restart
          target: /usr/share/nginx/html
  db:
    image: postgres
`

// TestResolveTriggersE2E parses develop.watch through the real compose-go
// loader and asserts the extracted triggers, including relative-path
// resolution against the project working dir (compose-go may additionally
// resolve symlinked components — macOS /var → /private/var — so both forms
// are accepted).
func TestResolveTriggersE2E(t *testing.T) {
	p := projectFromYAML(t, devFixtureYAML)
	triggers := resolveTriggers(p, toposort(p))
	if len(triggers) != 3 {
		t.Fatalf("got %d triggers, want 3: %+v", len(triggers), triggers)
	}

	wantPath := func(got, rel string) {
		t.Helper()
		raw := filepath.Join(p.WorkingDir, rel)
		resolved := raw
		if wd, err := filepath.EvalSymlinks(p.WorkingDir); err == nil {
			resolved = filepath.Join(wd, rel)
		}
		if got != raw && got != resolved {
			t.Errorf("path = %q, want %q (or symlink-resolved %q)", got, raw, resolved)
		}
	}

	// deterministic: sorted by service (api, api, web), Watch order within
	if triggers[0].service != "api" || triggers[0].action != types.WatchActionRebuild {
		t.Errorf("trigger[0] = %+v, want api/rebuild", triggers[0])
	}
	wantPath(triggers[0].path, "go.mod")
	if triggers[1].service != "api" || triggers[1].action != types.WatchActionSync ||
		triggers[1].target != "/app/src" || strings.Join(triggers[1].ignore, ",") != "*.tmp" {
		t.Errorf("trigger[1] = %+v, want api/sync→/app/src ignore *.tmp", triggers[1])
	}
	wantPath(triggers[1].path, "src")
	if triggers[2].service != "web" || triggers[2].action != types.WatchActionSyncRestart ||
		triggers[2].target != "/usr/share/nginx/html" {
		t.Errorf("trigger[2] = %+v, want web/sync+restart", triggers[2])
	}
	wantPath(triggers[2].path, "html")
}

func TestResolveTriggersValidation(t *testing.T) {
	t.Run("relative path joined to WorkingDir", func(t *testing.T) {
		p := &types.Project{Name: "proj", WorkingDir: "/wd", Services: types.Services{
			"a": {Name: "a", Develop: &types.DevelopConfig{Watch: []types.Trigger{
				{Path: "conf", Action: types.WatchActionRestart},
			}}},
		}}
		got := resolveTriggers(p, []string{"a"})
		if len(got) != 1 || got[0].path != "/wd/conf" {
			t.Errorf("= %+v, want one trigger with path /wd/conf", got)
		}
	})

	t.Run("unsupported action warned exactly once", func(t *testing.T) {
		p := &types.Project{Name: "proj", WorkingDir: "/wd", Services: types.Services{
			"a": {Name: "a", Develop: &types.DevelopConfig{Watch: []types.Trigger{
				{Path: "x", Action: types.WatchActionSyncExec, Target: "/t"},
				{Path: "y", Action: types.WatchActionSyncExec, Target: "/t"},
			}}},
		}}
		var got []resolvedTrigger
		_, stderr := captureOutput(t, func() { got = resolveTriggers(p, []string{"a"}) })
		if len(got) != 0 {
			t.Errorf("= %+v, want no triggers", got)
		}
		if n := strings.Count(stderr, "sync+exec"); n != 1 {
			t.Errorf("warned %d times, want once; stderr:\n%s", n, stderr)
		}
		mustContain(t, stderr, "stderr", "not supported", "rebuild, restart, sync, sync+restart")
	})

	t.Run("sync without target skipped with a warning", func(t *testing.T) {
		p := &types.Project{Name: "proj", WorkingDir: "/wd", Services: types.Services{
			"a": {Name: "a", Develop: &types.DevelopConfig{Watch: []types.Trigger{
				{Path: "x", Action: types.WatchActionSync},
			}}},
		}}
		var got []resolvedTrigger
		_, stderr := captureOutput(t, func() { got = resolveTriggers(p, []string{"a"}) })
		if len(got) != 0 {
			t.Errorf("= %+v, want no triggers", got)
		}
		mustContain(t, stderr, "stderr", "no target")
	})

	t.Run("services without develop are skipped", func(t *testing.T) {
		p := projectFromYAML(t, "services:\n  solo:\n    image: nginx\n")
		if got := resolveTriggers(p, []string{"solo"}); len(got) != 0 {
			t.Errorf("= %+v, want none", got)
		}
	})
}

// ---------- scanner -----------------------------------------------------------

func TestDevIgnored(t *testing.T) {
	cases := []struct {
		rel    string
		ignore []string
		want   bool
	}{
		{".git/config", nil, true},                // always-ignored segment
		{"x/.git/config", nil, true},              // built-in at any depth
		{"a/node_modules/x.js", nil, true},        // at any depth
		{"a/node_modules/b", nil, true},           // built-in exact segment, nested
		{".DS_Store", nil, true},                  //
		{"main.go", nil, false},                   //
		{".github/workflows/ci.yml", nil, false},  // ".git" must not swallow ".github"
		{"deploy.gitlab-ci.yml", nil, false},      // nor a ".git" substring mid-name
		{"app.git.bak", nil, false},               //
		{"node_modules_backup/f", nil, false},     // built-ins are exact segments only
		{"foo.tmp", []string{"*.tmp"}, true},      // glob on base name
		{"sub/foo.tmp", []string{"*.tmp"}, true},  // glob on a segment
		{"build/out.js", []string{"build"}, true}, // segment match
		{"mycache/f", []string{"cache"}, true},    // substring of rel path
		{"src/app.py", []string{"*.tmp"}, false},  //
		{"gitlog.txt", []string{}, false},         // ".git" is no substring here
		{"tmp/x", []string{"*.tmp"}, false},       // *.tmp does not match "tmp"/"x"
	}
	for _, tc := range cases {
		if got := devIgnored(tc.rel, tc.ignore); got != tc.want {
			t.Errorf("devIgnored(%q, %v) = %v, want %v", tc.rel, tc.ignore, got, tc.want)
		}
	}
}

func TestScanTree(t *testing.T) {
	t.Run("directory scan with ignores", func(t *testing.T) {
		dir := t.TempDir()
		mkfile(t, filepath.Join(dir, "a.txt"), "a")
		mkfile(t, filepath.Join(dir, "sub", "b.txt"), "b")
		mkfile(t, filepath.Join(dir, ".git", "config"), "x") // always ignored
		mkfile(t, filepath.Join(dir, "node_modules", "m.js"), "x")
		mkfile(t, filepath.Join(dir, "c.tmp"), "x") // custom ignore

		scan, err := scanTree(dir, []string{"*.tmp"})
		if err != nil {
			t.Fatal(err)
		}
		if len(scan) != 2 {
			t.Errorf("scan = %v, want exactly a.txt and sub/b.txt", scan)
		}
		for _, want := range []string{filepath.Join(dir, "a.txt"), filepath.Join(dir, "sub", "b.txt")} {
			if _, ok := scan[want]; !ok {
				t.Errorf("scan missing %s", want)
			}
		}
	})

	t.Run("single-file root", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "go.mod")
		mkfile(t, f, "module x")
		scan, err := scanTree(f, nil)
		if err != nil || len(scan) != 1 {
			t.Fatalf("scan = (%v, %v), want exactly the file", scan, err)
		}
		if fp := scan[f]; fp.size != int64(len("module x")) {
			t.Errorf("fingerprint = %+v, want size %d", fp, len("module x"))
		}
	})

	t.Run("missing root is an empty scan, not an error", func(t *testing.T) {
		scan, err := scanTree(filepath.Join(t.TempDir(), "nope"), nil)
		if err != nil || len(scan) != 0 {
			t.Errorf("= (%v, %v), want empty and nil", scan, err)
		}
	})
}

func TestDiffScans(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "sub", "b.txt")
	mkfile(t, a, "one")
	mkfile(t, b, "two")
	base, err := scanTree(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no changes", func(t *testing.T) {
		again, _ := scanTree(dir, nil)
		if d := diffScans(base, again); len(d) != 0 {
			t.Errorf("diff = %v, want empty", d)
		}
	})

	t.Run("modify (same size, mtime bump) + add + delete, sorted", func(t *testing.T) {
		mkfile(t, a, "ONE") // same size — fingerprint must catch the mtime
		if err := os.Chtimes(a, time.Now(), time.Now().Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		c := filepath.Join(dir, "c.txt")
		mkfile(t, c, "new")
		if err := os.Remove(b); err != nil {
			t.Fatal(err)
		}
		got, _ := scanTree(dir, nil)
		d := diffScans(base, got)
		want := []string{a, c, b} // a.txt, c.txt, sub/b.txt — sorted
		if len(d) != 3 || d[0] != want[0] || d[1] != want[1] || d[2] != want[2] {
			t.Errorf("diff = %v, want %v", d, want)
		}
	})
}

// ---------- decision -------------------------------------------------------------

func devTestTriggers() []resolvedTrigger {
	return []resolvedTrigger{
		{service: "api", action: types.WatchActionRebuild, path: "/proj/go.mod"},
		{service: "api", action: types.WatchActionSync, path: "/proj/src", target: "/app/src", ignore: []string{"*.tmp"}},
		{service: "db", action: types.WatchActionRestart, path: "/proj/db.conf"},
		{service: "web", action: types.WatchActionSyncRestart, path: "/proj/html", target: "/usr/share/nginx/html"},
	}
}

func TestPlanActions(t *testing.T) {
	triggers := devTestTriggers()
	cases := []struct {
		name    string
		changed []string
		want    []devAction
	}{
		{"single synced file", []string{"/proj/src/a.py"},
			[]devAction{{kind: "sync", service: "api", paths: []string{"/proj/src/a.py"}, dests: []string{"/app/src/a.py"}}}},
		{"nested sync path keeps the relative subtree", []string{"/proj/src/pkg/deep/m.py"},
			[]devAction{{kind: "sync", service: "api", paths: []string{"/proj/src/pkg/deep/m.py"}, dests: []string{"/app/src/pkg/deep/m.py"}}}},
		{"rebuild beats sync for the same service", []string{"/proj/src/a.py", "/proj/go.mod"},
			[]devAction{{kind: "rebuild", service: "api", paths: []string{"/proj/go.mod", "/proj/src/a.py"}}}},
		{"multiple synced files batched into one action", []string{"/proj/src/b.py", "/proj/src/a.py"},
			[]devAction{{kind: "sync", service: "api", paths: []string{"/proj/src/a.py", "/proj/src/b.py"}, dests: []string{"/app/src/a.py", "/app/src/b.py"}}}},
		{"file trigger maps to the target itself", []string{"/proj/db.conf"},
			[]devAction{{kind: "restart", service: "db", paths: []string{"/proj/db.conf"}}}},
		{"sync+restart trigger", []string{"/proj/html/index.html"},
			[]devAction{{kind: "sync+restart", service: "web", paths: []string{"/proj/html/index.html"}, dests: []string{"/usr/share/nginx/html/index.html"}}}},
		{"two services changed → two actions sorted by service", []string{"/proj/html/i.html", "/proj/src/a.py"},
			[]devAction{
				{kind: "sync", service: "api", paths: []string{"/proj/src/a.py"}, dests: []string{"/app/src/a.py"}},
				{kind: "sync+restart", service: "web", paths: []string{"/proj/html/i.html"}, dests: []string{"/usr/share/nginx/html/i.html"}}}},
		{"trigger ignore filters inside planActions too", []string{"/proj/src/x.tmp"}, nil},
		{"unrelated path plans nothing", []string{"/elsewhere/file"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planActions(tc.changed, triggers)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d actions %+v, want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				g, w := got[i], tc.want[i]
				if g.kind != w.kind || g.service != w.service ||
					strings.Join(g.paths, "|") != strings.Join(w.paths, "|") ||
					strings.Join(g.dests, "|") != strings.Join(w.dests, "|") {
					t.Errorf("action[%d] = %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

func TestSyncDest(t *testing.T) {
	dirTrig := resolvedTrigger{path: "/proj/src", target: "/app/src"}
	if got := syncDest(dirTrig, "/proj/src/a/b.py"); got != "/app/src/a/b.py" {
		t.Errorf("dir trigger dest = %q", got)
	}
	fileTrig := resolvedTrigger{path: "/proj/app.conf", target: "/etc/app.conf"}
	if got := syncDest(fileTrig, "/proj/app.conf"); got != "/etc/app.conf" {
		t.Errorf("file trigger dest = %q", got)
	}
}

// ---------- devSetup -----------------------------------------------------------

func TestDevSetup(t *testing.T) {
	t.Run("dry-run refused with code 2", func(t *testing.T) {
		p := projectFromYAML(t, devFixtureYAML)
		var code int
		_, stderr := captureOutput(t, func() { _, code = devSetup(p, runner{dry: true}, nil) })
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		mustContain(t, stderr, "stderr", "dev needs a live runtime")
	})

	t.Run("unknown SERVICE arg refused", func(t *testing.T) {
		p := projectFromYAML(t, devFixtureYAML)
		var code int
		_, stderr := captureOutput(t, func() { _, code = devSetup(p, runner{}, []string{"ghost"}) })
		if code != 1 {
			t.Errorf("code = %d, want 1", code)
		}
		mustContain(t, stderr, "stderr", "unknown service 'ghost'", "api", "db", "web")
	})

	t.Run("no develop.watch anywhere fails with example snippet", func(t *testing.T) {
		p := projectFromYAML(t, "services:\n  solo:\n    image: nginx\n")
		var code int
		_, stderr := captureOutput(t, func() { _, code = devSetup(p, runner{}, nil) })
		if code != 1 {
			t.Errorf("code = %d, want 1", code)
		}
		mustContain(t, stderr, "stderr",
			"no develop.watch triggers found",
			"develop:", "watch:", "action: sync", "action: rebuild", "target: /app/src")
	})

	t.Run("SERVICE filter narrows the trigger set", func(t *testing.T) {
		p := projectFromYAML(t, devFixtureYAML)
		triggers, code := devSetup(p, runner{}, []string{"web"})
		if code != 0 || len(triggers) != 1 || triggers[0].service != "web" {
			t.Errorf("= (%+v, %d), want one web trigger, code 0", triggers, code)
		}
	})

	t.Run("happy path resolves all triggers", func(t *testing.T) {
		p := projectFromYAML(t, devFixtureYAML)
		triggers, code := devSetup(p, runner{}, nil)
		if code != 0 || len(triggers) != 3 {
			t.Errorf("= (%d triggers, code %d), want 3 and 0", len(triggers), code)
		}
	})
}

func TestDevNarrateStartup(t *testing.T) {
	p := projectFromYAML(t, devFixtureYAML)
	triggers, _ := devSetup(p, runner{}, nil)
	stdout, _ := captureOutput(t, func() { devNarrateStartup(p, triggers, time.Second) })
	mustContain(t, stdout, "stdout",
		"dev mode: 3 trigger(s)",
		"poll every 1s, Ctrl-C to quit",
		"[api]", "rebuild",
		"sync → /app/src",
		"[web]", "sync+restart → /usr/share/nginx/html")
}

// ---------- devTick against the fake runtime ------------------------------------

// devTickFixture loads a single-service project with the given develop.watch
// YAML fragment and returns project + resolved triggers + initialized state.
func devTickFixture(t *testing.T, yaml string) (*types.Project, []resolvedTrigger, *devState) {
	t.Helper()
	p := projectFromYAML(t, yaml)
	triggers, code := devSetup(p, runner{}, nil)
	if code != 0 {
		t.Fatalf("devSetup code = %d", code)
	}
	var st *devState
	captureOutput(t, func() { st = newDevState(triggers) })
	return p, triggers, st
}

const devSyncYAML = `services:
  api:
    image: pyapp
    develop:
      watch:
        - path: ./src
          action: sync
          target: /app/src
`

func TestDevTickSync(t *testing.T) {
	p, triggers, st := devTickFixture(t, devSyncYAML)
	log := fakeContainerLogging(t, "")

	mkfile(t, filepath.Join(triggers[0].path, "app.py"), "print(1)")

	// debounce: the tick that discovers the change waits one quiet tick
	var acts []devAction
	captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if acts != nil {
		t.Fatalf("first tick acted immediately: %+v", acts)
	}
	stdout, _ := captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 || acts[0].kind != "sync" {
		t.Fatalf("second tick = %+v, want one sync action", acts)
	}
	mustContain(t, log(), "argv log",
		"cp "+filepath.Join(triggers[0].path, "app.py")+" proj-api:/app/src/app.py")
	mustContain(t, stdout, "stdout", "[api] src/app.py changed → sync", "synced 1 file(s)")

	// quiet ticks afterwards do nothing
	captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if acts != nil {
		t.Errorf("quiet tick = %+v, want nil", acts)
	}
}

func TestDevTickContinuousWritesActAfterTwoTicks(t *testing.T) {
	p, triggers, st := devTickFixture(t, devSyncYAML)
	fakeContainerLogging(t, "")

	mkfile(t, filepath.Join(triggers[0].path, "a.py"), "1")
	var acts []devAction
	captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if acts != nil {
		t.Fatalf("tick 1 acted: %+v", acts)
	}
	mkfile(t, filepath.Join(triggers[0].path, "b.py"), "2") // still writing…
	captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 || len(acts[0].paths) != 2 {
		t.Fatalf("tick 2 = %+v, want one action covering both files", acts)
	}
}

func TestDevTickRebuild(t *testing.T) {
	p, triggers, st := devTickFixture(t, `services:
  api:
    build:
      context: .
    develop:
      watch:
        - path: go.mod
          action: rebuild
`)
	log := fakeContainerLogging(t, "")

	mkfile(t, triggers[0].path, "module x") // the watched file appears
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	var acts []devAction
	stdout, _ := captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 || acts[0].kind != "rebuild" {
		t.Fatalf("= %+v, want one rebuild", acts)
	}
	mustOrder(t, log(), "argv log",
		"build --tag proj-api",
		"stop proj-api",
		"delete proj-api",
		"run --detach --name proj-api --network proj-net")
	mustContain(t, stdout, "stdout", "go.mod changed → rebuild", "[api] rebuilt and recreated")
}

func TestDevTickRestart(t *testing.T) {
	p, triggers, st := devTickFixture(t, `services:
  api:
    image: pyapp
    develop:
      watch:
        - path: app.conf
          action: restart
`)
	log := fakeContainerLogging(t, "")

	mkfile(t, triggers[0].path, "k=v")
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	var acts []devAction
	stdout, _ := captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 || acts[0].kind != "restart" {
		t.Fatalf("= %+v, want one restart", acts)
	}
	mustOrder(t, log(), "argv log", "stop proj-api", "start proj-api")
	mustNotContain(t, log(), "argv log", "build", "delete")
	mustContain(t, stdout, "stdout", "[api] restarted")
}

func TestDevTickSyncRestart(t *testing.T) {
	p, triggers, st := devTickFixture(t, `services:
  web:
    image: nginx
    develop:
      watch:
        - path: ./html
          action: sync+restart
          target: /usr/share/nginx/html
`)
	log := fakeContainerLogging(t, "")

	mkfile(t, filepath.Join(triggers[0].path, "index.html"), "<h1>")
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	stdout, _ := captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	mustOrder(t, log(), "argv log",
		"cp "+filepath.Join(triggers[0].path, "index.html")+" proj-web:/usr/share/nginx/html/index.html",
		"stop proj-web",
		"start proj-web")
	mustContain(t, stdout, "stdout", "[web] restarted")
}

// A broken build warns, leaves the running container alone, and the loop
// survives — the defining failure mode of a dev loop.
func TestDevTickBuildFailureSurvivable(t *testing.T) {
	p, triggers, st := devTickFixture(t, `services:
  api:
    build:
      context: .
    develop:
      watch:
        - path: go.mod
          action: rebuild
`)
	log := fakeContainerLogging(t, `case "$1" in build) echo "syntax error"; exit 1 ;; esac`)

	mkfile(t, triggers[0].path, "module broken")
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	var acts []devAction
	_, stderr := captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 {
		t.Fatalf("= %+v, want the action still reported", acts)
	}
	mustContain(t, stderr, "stderr", "[api] build failed — keeping the current container running")
	mustNotContain(t, log(), "argv log", "stop", "delete")

	// the loop keeps going: a later fix triggers a clean tick again
	mkfile(t, triggers[0].path, "module fixed and longer")
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 {
		t.Errorf("post-failure tick = %+v, want one action", acts)
	}
}

func TestDevTickSyncFailureWarnsAndContinues(t *testing.T) {
	p, triggers, st := devTickFixture(t, devSyncYAML)
	fakeContainerLogging(t, `case "$1" in cp) echo "container not running"; exit 1 ;; esac`)

	mkfile(t, filepath.Join(triggers[0].path, "a.py"), "1")
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	var acts []devAction
	_, stderr := captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 {
		t.Fatalf("= %+v, want the action reported despite the cp failure", acts)
	}
	mustContain(t, stderr, "stderr", "sync of src/a.py failed")
}

// A trigger whose path cannot be scanned at startup is UNINITIALIZED, not
// baselined as empty: the first later successful scan is adopted silently
// (no spurious mass rebuild of every pre-existing file), and the error state
// warns exactly once — on the transition, not every tick.
func TestDevStateUninitializedTriggerAdoptsBaseline(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based scan failure cannot be provoked as root")
	}
	p := projectFromYAML(t, devSyncYAML)
	locked := filepath.Join(t.TempDir(), "locked")
	watched := filepath.Join(locked, "src")
	mkfile(t, filepath.Join(watched, "a.py"), "print(1)") // pre-exists the recovery
	if err := os.Chmod(locked, 0o000); err != nil {       // stat(watched) → EACCES
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	triggers := []resolvedTrigger{
		{service: "api", action: types.WatchActionSync, path: watched, target: "/app/src"},
	}
	log := fakeContainerLogging(t, "")

	var st *devState
	_, stderr := captureOutput(t, func() { st = newDevState(triggers) })
	mustContain(t, stderr, "stderr", "cannot scan")

	// repeated failure must not warn again — only the ok→error transition does
	var acts []devAction
	for i := 0; i < 2; i++ {
		_, stderr = captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
		if acts != nil {
			t.Fatalf("failing tick %d produced actions %+v, want none", i, acts)
		}
		mustNotContain(t, stderr, "stderr", "cannot scan")
	}

	// recovery: the first successful scan is ADOPTED as the baseline —
	// the pre-existing a.py is state, not a change
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // adoption tick + would-be debounce ticks
		captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
		if acts != nil {
			t.Fatalf("post-recovery tick %d produced actions %+v, want none", i, acts)
		}
	}
	mustNotContain(t, log(), "argv log", "cp ")

	// a REAL change against the adopted baseline is still detected
	mkfile(t, filepath.Join(watched, "b.py"), "print(2)")
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	captureOutput(t, func() { acts = devTick(p, runner{}, triggers, st, true) })
	if len(acts) != 1 || acts[0].kind != types.WatchActionSync ||
		len(acts[0].paths) != 1 || filepath.Base(acts[0].paths[0]) != "b.py" {
		t.Fatalf("post-adoption change = %+v, want one sync of b.py only", acts)
	}
}

// A host-side delete under a sync trigger cannot be mirrored by `container
// cp` — it warns instead of running a doomed cp.
func TestDevTickSyncDeletedFileWarns(t *testing.T) {
	p := projectFromYAML(t, devSyncYAML)
	triggers, _ := devSetup(p, runner{}, nil)
	doomed := filepath.Join(triggers[0].path, "gone.py")
	mkfile(t, doomed, "x") // exists at baseline…
	var st *devState
	captureOutput(t, func() { st = newDevState(triggers) })
	log := fakeContainerLogging(t, "")

	if err := os.Remove(doomed); err != nil { // …then deleted
		t.Fatal(err)
	}
	captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	_, stderr := captureOutput(t, func() { devTick(p, runner{}, triggers, st, true) })
	mustContain(t, stderr, "stderr", "gone.py was deleted", "cp cannot remove")
	mustNotContain(t, log(), "argv log", "cp ")
}
