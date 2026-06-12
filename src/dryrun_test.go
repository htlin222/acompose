package main

// Dry-run end-to-end tests: runner{dry: true} turns every cmd* function into
// a pure printer (commands go to stdout, getIP returns "<cname-ip>"), so the
// full up/down/update/refresh flows can be asserted without Apple's
// `container` binary existing on the machine.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestCmdUpDryRun(t *testing.T) {
	p := loadUpFixture(t)

	t.Run("interpolation and naming resolved at load time", func(t *testing.T) {
		if got := p.Services["db"].Image; got != "postgres:16" {
			t.Errorf("db image = %q, want postgres:16 (.env interpolation)", got)
		}
		if got := cnameOf(p, "db"); got != "mydb" {
			t.Errorf("cnameOf(db) = %q, want container_name mydb", got)
		}
		if got := cnameOf(p, "app"); got != "proj-app" {
			t.Errorf("cnameOf(app) = %q, want proj-app", got)
		}
	})

	stdout, stderr := captureOutput(t, func() {
		cmdUp(p, runner{dry: true}, true, time.Second)
	})

	t.Run("network and volume creation", func(t *testing.T) {
		mustContain(t, stdout, "stdout",
			"container network create proj-net",
			"container volume create "+volName(p, "data"))
	})

	t.Run("topological start order db before app", func(t *testing.T) {
		mustContain(t, stdout, "stdout", "start order: db → app")
		mustOrder(t, stdout, "stdout",
			"run   db  (mydb)",
			"run   app  (proj-app)")
	})

	t.Run("db run command", func(t *testing.T) {
		mustContain(t, stdout, "stdout",
			"container run --detach --name mydb --network proj-net "+
				"--publish 5432:5432 --volume "+volName(p, "data")+":/var/lib/postgresql/data postgres:16")
	})

	t.Run("app run command publishes host-IP port and interpolated env", func(t *testing.T) {
		mustContain(t, stdout, "stdout",
			"--publish 127.0.0.1:8080:80",
			"--env GREETING=hello", // ${AC_TEST_GREETING:-hello} default
			"--workdir /srv",
			":/usr/share/nginx/html") // bind mount, source absolutized by compose-go
	})

	t.Run("DEP_HOST fallback env injected from dry-run placeholder IP", func(t *testing.T) {
		mustContain(t, stdout, "stdout", "--env DB_HOST=<mydb-ip>")
	})

	t.Run("build service gets a tagged build", func(t *testing.T) {
		mustContain(t, stdout, "stdout", "container build --tag proj-app")
	})

	t.Run("hosts wiring exec lines", func(t *testing.T) {
		mustContain(t, stdout, "stdout",
			"container exec mydb sh -c printf \"%s\\n\" '<mydb-ip>\tdb' >> /etc/hosts")
	})

	t.Run("warnUnsupported on stderr", func(t *testing.T) {
		mustContain(t, stderr, "stderr",
			"[db] exec-style healthcheck ignored",
			"[db] restart: 'always' not enforced",
			"[app] entrypoint: ignored",
			"[app] user: ignored")
		// deploy with only replicas: 1 matches what actually happens (one
		// container) — it must no longer warn
		mustNotContain(t, stderr, "stderr", "deploy:")
	})

	t.Run("summary", func(t *testing.T) {
		mustContain(t, stdout, "stdout", "stack up", "localhost:5432", "localhost:8080", "<mydb-ip>")
	})
}

func TestCmdUpDryRunNoPublish(t *testing.T) {
	p := loadUpFixture(t)
	stdout, _ := captureOutput(t, func() {
		cmdUp(p, runner{dry: true}, false, time.Second)
	})
	mustNotContain(t, stdout, "stdout", "--publish", "localhost:5432", "localhost:8080")
	mustContain(t, stdout, "stdout", "stack up")
}

func TestCmdDownDryRun(t *testing.T) {
	p := loadUpFixture(t)

	t.Run("reverse order, volumes kept by default", func(t *testing.T) {
		stdout, _ := captureOutput(t, func() {
			cmdDown(p, runner{dry: true}, false)
		})
		mustOrder(t, stdout, "stdout",
			"container stop proj-app",
			"container delete proj-app",
			"  removed app",
			"container stop mydb",
			"container delete mydb",
			"  removed db",
			"container network delete proj-net")
		mustContain(t, stdout, "stdout",
			"named volumes kept ("+volName(p, "data")+")",
			"down")
		mustNotContain(t, stdout, "stdout", "volume delete")
	})

	t.Run("down -v removes named volumes", func(t *testing.T) {
		stdout, _ := captureOutput(t, func() {
			cmdDown(p, runner{dry: true}, true)
		})
		mustContain(t, stdout, "stdout",
			"container volume delete "+volName(p, "data"),
			"  removed volume "+volName(p, "data"))
		mustNotContain(t, stdout, "stdout", "named volumes kept")
	})
}

func TestCmdUpdateDryRun(t *testing.T) {
	p := loadUpFixture(t)
	stdout, _ := captureOutput(t, func() {
		cmdUpdate(p, runner{dry: true}, true)
	})

	t.Run("image service pulled and recreated", func(t *testing.T) {
		mustOrder(t, stdout, "stdout",
			"container image pull postgres:16",
			"recreate db  (mydb)",
			"container stop mydb",
			"container delete mydb",
			"container run --detach --name mydb")
	})

	t.Run("build service rebuilt and recreated", func(t *testing.T) {
		mustOrder(t, stdout, "stdout",
			"build app",
			"container build --tag proj-app",
			"recreate app  (proj-app)",
			"container stop proj-app",
			"container delete proj-app",
			"container run --detach --name proj-app")
	})

	t.Run("everything reported as updated", func(t *testing.T) {
		mustContain(t, stdout, "stdout", "updated: db, app")
		mustNotContain(t, stdout, "stdout", "already current")
	})
}

func TestCmdRefreshDryRun(t *testing.T) {
	p := loadUpFixture(t)
	stdout, _ := captureOutput(t, func() {
		cmdRefresh(p, runner{dry: true})
	})
	// rewireAll: scrub stale lines, then re-inject the full pair set into both
	mustContain(t, stdout, "stdout",
		"container exec mydb sh -c grep -vE '\\s(db|app)$' /etc/hosts",
		"container exec proj-app sh -c grep -vE",
		"'<mydb-ip>\tdb' '<proj-app-ip>\tapp' >> /etc/hosts",
		"refreshed")
}

func TestCmdRefreshSingleServiceIsNoop(t *testing.T) {
	p := projectFromYAML(t, "services:\n  solo:\n    image: nginx\n")
	stdout, _ := captureOutput(t, func() {
		cmdRefresh(p, runner{dry: true})
	})
	// one service has no peers — rewireAll must not exec anything
	mustNotContain(t, stdout, "stdout", "container exec")
	mustContain(t, stdout, "stdout", "refreshed")
}

func TestCmdInitHappyPath(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	stdout, _ := captureOutput(t, cmdInit)
	mustContain(t, stdout, "stdout", "wrote docker-compose.yml")
	got, err := os.ReadFile(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != demoCompose {
		t.Errorf("written file differs from demoCompose template")
	}
}

// cmdInit refuses to overwrite an existing compose file via os.Exit(1), so
// the refusal path runs in a re-exec'd copy of the test binary.
func TestCmdInitRefusesOverwriteSubprocess(t *testing.T) {
	if os.Getenv("ACOMPOSE_TEST_INIT_REFUSE") == "1" {
		cmdInit() // cwd (set by the parent) already holds a compose file
		return
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCmdInitRefusesOverwriteSubprocess$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ACOMPOSE_TEST_INIT_REFUSE=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got err=%v, output:\n%s", err, out)
	}
	mustContain(t, string(out), "subprocess output", "refusing to overwrite")
}

// toposort exits the process on a dependency cycle — same re-exec pattern.
func TestToposortCycleSubprocess(t *testing.T) {
	if os.Getenv("ACOMPOSE_TEST_CYCLE") == "1" {
		p := &types.Project{Name: "c", Services: types.Services{
			"a": {Name: "a", DependsOn: types.DependsOnConfig{"b": {}}},
			"b": {Name: "b", DependsOn: types.DependsOnConfig{"a": {}}},
		}}
		toposort(p)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestToposortCycleSubprocess$")
	cmd.Env = append(os.Environ(), "ACOMPOSE_TEST_CYCLE=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got err=%v, output:\n%s", err, out)
	}
	mustContain(t, string(out), "subprocess output", "circular depends_on detected")
}

func TestLoadProjectEnvFile(t *testing.T) {
	p := projectFromFiles(t, "envf", map[string]string{
		"docker-compose.yml": "services:\n  app:\n    image: nginx\n    env_file:\n      - service.env\n",
		"service.env":        "FOO=bar\n",
	})
	v, ok := p.Services["app"].Environment["FOO"]
	if !ok || v == nil || *v != "bar" {
		t.Errorf("env_file FOO = %v, want bar", v)
	}
}

func TestLoadProjectDotEnvInterpolation(t *testing.T) {
	t.Run(".env value wins over the default", func(t *testing.T) {
		p := projectFromFiles(t, "dot", map[string]string{
			"docker-compose.yml": "services:\n  app:\n    image: nginx:${AC_UNIT_TAG:-1.25}\n",
			".env":               "AC_UNIT_TAG=1.27\n",
		})
		if got := p.Services["app"].Image; got != "nginx:1.27" {
			t.Errorf("image = %q, want nginx:1.27", got)
		}
	})
	t.Run("default used when the variable is unset", func(t *testing.T) {
		p := projectFromYAML(t, "services:\n  app:\n    image: nginx:${AC_UNIT_UNSET_TAG:-1.25}\n")
		if got := p.Services["app"].Image; got != "nginx:1.25" {
			t.Errorf("image = %q, want nginx:1.25", got)
		}
	})
}

func TestLoadProjectOverrideAutoMerge(t *testing.T) {
	dir := t.TempDir()
	base := "services:\n  app:\n    image: nginx:1.0\n    ports:\n      - \"8080:80\"\n"
	override := "services:\n  app:\n    image: nginx:2.0\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.override.yml"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	p := loadProject(nil, "ovr") // no --file: default discovery picks up both
	app, ok := p.Services["app"]
	if !ok {
		t.Fatalf("no app service; got %v", p.ServiceNames())
	}
	if app.Image != "nginx:2.0" {
		t.Errorf("image = %q, want override's nginx:2.0", app.Image)
	}
	if len(app.Ports) != 1 || app.Ports[0].Published != "8080" {
		t.Errorf("ports = %+v, want base's 8080:80 preserved", app.Ports)
	}
}

func TestLoadProjectContainerName(t *testing.T) {
	p := projectFromYAML(t, "services:\n  db:\n    image: postgres\n    container_name: pinned\n  web:\n    image: nginx\n")
	if got := cnameOf(p, "db"); got != "pinned" {
		t.Errorf("cnameOf(db) = %q, want pinned", got)
	}
	if got := cnameOf(p, "web"); got != "proj-web" {
		t.Errorf("cnameOf(web) = %q, want proj-web", got)
	}
}

func TestRunnerRunDry(t *testing.T) {
	var ok bool
	var out string
	stdout, stderr := captureOutput(t, func() {
		ok, out = runner{dry: true}.run([]string{"container", "ls", "--all"})
	})
	if !ok || out != "" {
		t.Errorf("dry run = (%v, %q), want (true, \"\")", ok, out)
	}
	if stdout != "  $ container ls --all\n" {
		t.Errorf("dry run stdout = %q", stdout)
	}
	if stderr != "" {
		t.Errorf("dry run stderr = %q, want empty", stderr)
	}
}

func TestGetIPDry(t *testing.T) {
	if got := getIP(runner{dry: true}, "mydb"); got != "<mydb-ip>" {
		t.Errorf("getIP dry = %q, want <mydb-ip>", got)
	}
}

func TestImageDigestsDry(t *testing.T) {
	var set map[string]bool
	var ok bool
	captureOutput(t, func() {
		set, ok = imageDigests(runner{dry: true}, "nginx:latest")
	})
	// dry run returns (true, "") from run: empty output yields no digests
	if ok {
		t.Errorf("imageDigests dry ok = true, want false (no digests in empty output)")
	}
	if len(set) != 0 {
		t.Errorf("imageDigests dry set = %v, want empty", set)
	}
}
