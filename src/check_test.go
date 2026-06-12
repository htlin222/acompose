package main

// Tests for `acompose check`: the analyzeService classification tables (the
// single source of truth warnUnsupported also renders), countPresentFeatures,
// the project-level findings, and the full checkRun report end to end.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// featureOf renders a finding as "level/feature" for compact table asserts.
func featuresOf(fs []finding) []string {
	lvl := map[findingLevel]string{
		levelApproximated: "~",
		levelIgnored:      "!",
		levelBlocker:      "✗",
	}
	var got []string
	for _, f := range fs {
		got = append(got, lvl[f.level]+f.feature)
	}
	return got
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAnalyzeServiceTable(t *testing.T) {
	netRef := func(names ...string) map[string]*types.ServiceNetworkConfig {
		m := map[string]*types.ServiceNetworkConfig{}
		for _, n := range names {
			m[n] = nil
		}
		return m
	}
	cases := []struct {
		name string
		svc  types.ServiceConfig
		want []string // "level-symbol + feature", in order
	}{
		{
			name: "fully supported service is clean",
			svc: types.ServiceConfig{
				Image:         "nginx",
				ContainerName: "pinned",
				Ports:         []types.ServicePortConfig{{Published: "80", Target: 80}},
				Environment:   types.MappingWithEquals{"A": strp("1")},
				Volumes: []types.ServiceVolumeConfig{
					{Type: "volume", Source: "data", Target: "/d"},
					{Type: "bind", Source: "/host", Target: "/h"},
				},
				DependsOn:  types.DependsOnConfig{"db": {}},
				Command:    types.ShellCommand{"run"},
				WorkingDir: "/srv",
				Networks:   netRef("default"),
				Deploy:     limits(&types.Resource{NanoCPUs: 1.5, MemoryBytes: 536870912}),
			},
			want: nil,
		},
		{
			name: "exec healthcheck approximated",
			svc:  types.ServiceConfig{Image: "i", HealthCheck: &types.HealthCheckConfig{Test: []string{"CMD", "true"}}},
			want: []string{"~healthcheck"},
		},
		{
			name: "disabled healthcheck is silent",
			svc:  types.ServiceConfig{Image: "i", HealthCheck: &types.HealthCheckConfig{Disable: true}},
			want: nil,
		},
		{
			name: "restart approximated",
			svc:  types.ServiceConfig{Image: "i", Restart: "always"},
			want: []string{"~restart 'always'"},
		},
		{
			name: "deploy limits-only yields no finding",
			svc:  types.ServiceConfig{Image: "i", Deploy: limits(&types.Resource{NanoCPUs: 2})},
			want: nil,
		},
		{
			name: "deploy replicas 1 is silent",
			svc:  types.ServiceConfig{Image: "i", Deploy: &types.DeployConfig{Replicas: intp(1)}},
			want: nil,
		},
		{
			name: "deploy replicas 3 ignored",
			svc:  types.ServiceConfig{Image: "i", Deploy: &types.DeployConfig{Replicas: intp(3)}},
			want: []string{"!deploy.replicas"},
		},
		{
			name: "every unsupported deploy part, legacy order",
			svc: types.ServiceConfig{Image: "i", Deploy: &types.DeployConfig{
				Mode:           "global",
				Replicas:       intp(3),
				Labels:         types.Labels{"k": "v"},
				UpdateConfig:   &types.UpdateConfig{},
				RollbackConfig: &types.UpdateConfig{},
				Resources: types.Resources{
					Limits:       &types.Resource{Pids: 9},
					Reservations: &types.Resource{NanoCPUs: 1},
				},
				RestartPolicy: &types.RestartPolicy{Condition: "any"},
				Placement:     types.Placement{Constraints: []string{"node.role==manager"}},
				EndpointMode:  "vip",
			}},
			want: []string{
				"!deploy.mode", "!deploy.replicas", "!deploy.labels",
				"!deploy.update_config", "!deploy.rollback_config",
				"!deploy.resources.reservations", "!deploy.resources.limits.pids/devices",
				"!deploy.restart_policy", "!deploy.placement", "!deploy.endpoint_mode",
			},
		},
		{
			name: "secrets ignored",
			svc:  types.ServiceConfig{Image: "i", Secrets: []types.ServiceSecretConfig{{Source: "tls"}}},
			want: []string{"!secrets/configs"},
		},
		{
			name: "configs ignored",
			svc:  types.ServiceConfig{Image: "i", Configs: []types.ServiceConfigObjConfig{{Source: "cfg"}}},
			want: []string{"!secrets/configs"},
		},
		{
			name: "entrypoint ignored",
			svc:  types.ServiceConfig{Image: "i", Entrypoint: types.ShellCommand{"/entry.sh"}},
			want: []string{"!entrypoint"},
		},
		{
			name: "user ignored",
			svc:  types.ServiceConfig{Image: "i", User: "nobody"},
			want: []string{"!user"},
		},
		{
			name: "anonymous volume ignored, one finding per mount",
			svc: types.ServiceConfig{Image: "i", Volumes: []types.ServiceVolumeConfig{
				{Type: "volume", Source: "", Target: "/anon1"},
				{Type: "volume", Source: "data", Target: "/named"},
				{Type: "volume", Source: "", Target: "/anon2"},
			}},
			want: []string{"!volumes", "!volumes"},
		},
		{
			name: "custom networks ignored",
			svc:  types.ServiceConfig{Image: "i", Networks: netRef("default", "frontend")},
			want: []string{"!networks"},
		},
		{
			name: "default-only networks silent",
			svc:  types.ServiceConfig{Image: "i", Networks: netRef("default")},
			want: nil,
		},
		{
			name: "platform amd64 blocks",
			svc:  types.ServiceConfig{Image: "i", Platform: "linux/amd64"},
			want: []string{"✗platform linux/amd64"},
		},
		{
			name: "platform x86_64 blocks",
			svc:  types.ServiceConfig{Image: "i", Platform: "linux/x86_64"},
			want: []string{"✗platform linux/x86_64"},
		},
		{
			name: "platform arm64 is silent",
			svc:  types.ServiceConfig{Image: "i", Platform: "linux/arm64"},
			want: nil,
		},
		{
			name: "neither image nor build blocks",
			svc:  types.ServiceConfig{},
			want: []string{"✗image"},
		},
		{
			name: "build without image is fine",
			svc:  types.ServiceConfig{Build: &types.BuildConfig{Context: "."}},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := featuresOf(analyzeService("svc", tc.svc))
			if !sameStrings(got, tc.want) {
				t.Errorf("analyzeService = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnalyzeServiceDetails(t *testing.T) {
	t.Run("healthcheck wording for check vs up", func(t *testing.T) {
		fs := analyzeService("api", types.ServiceConfig{Image: "i",
			HealthCheck: &types.HealthCheckConfig{Test: []string{"CMD", "true"}}})
		if len(fs) != 1 {
			t.Fatalf("findings = %v", fs)
		}
		if fs[0].detail != "approximated by TCP polling on the first published port" {
			t.Errorf("detail = %q", fs[0].detail)
		}
		if fs[0].warnText != "exec-style healthcheck ignored — service_healthy is approximated by TCP polling" {
			t.Errorf("warnText = %q", fs[0].warnText)
		}
	})
	t.Run("restart embeds the policy", func(t *testing.T) {
		fs := analyzeService("api", types.ServiceConfig{Image: "i", Restart: "unless-stopped"})
		if len(fs) != 1 || fs[0].feature != "restart 'unless-stopped'" {
			t.Fatalf("findings = %v", fs)
		}
		if fs[0].warnText != "restart: 'unless-stopped' not enforced by the runtime — run 'acompose watch' to supervise" {
			t.Errorf("warnText = %q", fs[0].warnText)
		}
	})
	t.Run("point-of-use findings carry no up-time warn text", func(t *testing.T) {
		for _, svc := range []types.ServiceConfig{
			{}, // missing image/build — cmdUp fails hard itself
			{Image: "i", Volumes: []types.ServiceVolumeConfig{{Type: "volume", Target: "/anon"}}}, // runCmd warns per mount
		} {
			for _, f := range analyzeService("svc", svc) {
				if f.warnText != "" {
					t.Errorf("finding %q has warnText %q, want empty", f.feature, f.warnText)
				}
			}
		}
	})
}

func TestAnalyzeProject(t *testing.T) {
	t.Run("default-only network is silent", func(t *testing.T) {
		p := projectFromYAML(t, "services:\n  a:\n    image: nginx\n")
		if fs := analyzeProject(p); len(fs) != 0 {
			t.Errorf("findings = %v, want none", fs)
		}
	})
	t.Run("top-level custom networks yield one ignored finding", func(t *testing.T) {
		p := projectFromYAML(t, `services:
  a:
    image: nginx
    networks: [frontend, backend]
networks:
  frontend: {}
  backend: {}
`)
		fs := analyzeProject(p)
		if len(fs) != 1 || fs[0].level != levelIgnored || fs[0].feature != "networks" {
			t.Fatalf("findings = %v", fs)
		}
		mustContain(t, fs[0].detail, "project networks detail",
			"top-level networks (backend, frontend)", "one shared project network")
	})
	t.Run("external top-level volumes are not a finding", func(t *testing.T) {
		p := projectFromYAML(t, `services:
  a:
    image: nginx
    volumes:
      - ext:/data
volumes:
  ext:
    external: true
`)
		if fs := analyzeProject(p); len(fs) != 0 {
			t.Errorf("findings = %v, want none", fs)
		}
	})
}

func TestCountPresentFeatures(t *testing.T) {
	cases := []struct {
		name string
		svc  types.ServiceConfig
		want int
	}{
		{"empty service", types.ServiceConfig{}, 0},
		{"image only", types.ServiceConfig{Image: "i"}, 1},
		{"build only", types.ServiceConfig{Build: &types.BuildConfig{Context: "."}}, 1},
		{"ports", types.ServiceConfig{Ports: []types.ServicePortConfig{{Published: "80", Target: 80}}}, 1},
		{"environment", types.ServiceConfig{Environment: types.MappingWithEquals{"A": strp("1")}}, 1},
		{"named volume", types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{{Type: "volume", Source: "d", Target: "/d"}}}, 1},
		{"bind volume", types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{{Type: "bind", Source: "/h", Target: "/c"}}}, 1},
		{"two mounts count the volumes feature once",
			types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{
				{Type: "volume", Source: "d", Target: "/d"},
				{Type: "bind", Source: "/h", Target: "/c"},
			}}, 1},
		{"anonymous volume alone counts nothing",
			types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{{Type: "volume", Target: "/anon"}}}, 0},
		{"depends_on", types.ServiceConfig{DependsOn: types.DependsOnConfig{"db": {}}}, 1},
		{"command", types.ServiceConfig{Command: types.ShellCommand{"run"}}, 1},
		{"working_dir", types.ServiceConfig{WorkingDir: "/srv"}, 1},
		{"container_name", types.ServiceConfig{ContainerName: "pinned"}, 1},
		{"deploy limits", types.ServiceConfig{Deploy: limits(&types.Resource{NanoCPUs: 1})}, 1},
		{"deploy without limits counts nothing", types.ServiceConfig{Deploy: &types.DeployConfig{Replicas: intp(3)}}, 0},
		{"zero-valued limits count nothing", types.ServiceConfig{Deploy: limits(&types.Resource{})}, 0},
		{"everything at once", types.ServiceConfig{
			Image:         "i",
			Build:         &types.BuildConfig{Context: "."},
			Ports:         []types.ServicePortConfig{{Published: "80", Target: 80}},
			Environment:   types.MappingWithEquals{"A": strp("1")},
			Volumes:       []types.ServiceVolumeConfig{{Type: "bind", Source: "/h", Target: "/c"}},
			DependsOn:     types.DependsOnConfig{"db": {}},
			Command:       types.ShellCommand{"run"},
			WorkingDir:    "/srv",
			ContainerName: "pinned",
			Deploy:        limits(&types.Resource{MemoryBytes: mib}),
		}, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countPresentFeatures(tc.svc); got != tc.want {
				t.Errorf("countPresentFeatures = %d, want %d", got, tc.want)
			}
		})
	}
}

// checkFixtureYAML hits all three verdicts: db is clean, api has an
// approximated healthcheck plus an ignored user, legacy has an x86 blocker.
// Present supported features: db 4 (image/ports/env/volumes) + api 4
// (build/ports/command/depends_on) + legacy 3 (image/working_dir/env) = 11;
// with 3 findings (1 approximated) that is 12/14.
const checkFixtureYAML = `services:
  db:
    image: postgres:16
    ports: ["5432:5432"]
    environment:
      POSTGRES_PASSWORD: devpass
    volumes:
      - data:/var/lib/postgresql/data
  api:
    build:
      context: .
    ports: ["8080:8080"]
    command: ["./api", "--serve"]
    depends_on: [db]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080"]
    user: nobody
  legacy:
    image: oldco/payments:1.0
    platform: linux/amd64
    working_dir: /srv
    environment:
      MODE: legacy
volumes:
  data:
`

const checkFixtureReport = `:: acompose check — proj  (3 services)

  db
    ✓ translates cleanly
  api
    ~ healthcheck: approximated by TCP polling on the first published port
    ! user: ignored — runs as the image's default user
  legacy
    ✗ platform linux/amd64: x86 images are not seamless on this runtime — may fail to run

summary: 1 clean · 1 approximated · 1 with blockers
12/14 compose features in this file translate cleanly or approximated
`

func TestCheckRunReport(t *testing.T) {
	p := projectFromYAML(t, checkFixtureYAML)
	var blockers int
	stdout, stderr := captureOutput(t, func() {
		blockers = checkRun(p)
	})
	if stdout != checkFixtureReport {
		t.Errorf("report differs:\n got: %q\nwant: %q", stdout, checkFixtureReport)
	}
	if stderr != "" {
		t.Errorf("check must not write to stderr, got %q", stderr)
	}
	if blockers != 1 {
		t.Errorf("blockers = %d, want 1", blockers)
	}
}

func TestCheckRunDeterministic(t *testing.T) {
	p := projectFromYAML(t, checkFixtureYAML)
	first, _ := captureOutput(t, func() { checkRun(p) })
	second, _ := captureOutput(t, func() { checkRun(p) })
	if first != second {
		t.Errorf("two runs differ:\n%q\nvs\n%q", first, second)
	}
}

func TestCheckRunCleanProject(t *testing.T) {
	p := projectFromYAML(t, "services:\n  solo:\n    image: nginx\n    ports: [\"80:80\"]\n")
	var blockers int
	stdout, _ := captureOutput(t, func() { blockers = checkRun(p) })
	if blockers != 0 {
		t.Errorf("blockers = %d, want 0", blockers)
	}
	mustContain(t, stdout, "stdout",
		"(1 services)",
		"✓ translates cleanly",
		"summary: 1 clean · 0 approximated · 0 with blockers",
		"2/2 compose features in this file translate cleanly or approximated")
}

// An empty project must short-circuit to "nothing to check" — not a blank
// per-service report ending in a nonsensical "0/0 compose features" ratio.
func TestCheckRunEmptyProject(t *testing.T) {
	p := projectFromYAML(t, "services: {}\n")
	var blockers int
	stdout, stderr := captureOutput(t, func() { blockers = checkRun(p) })
	if blockers != 0 {
		t.Errorf("blockers = %d, want 0", blockers)
	}
	mustContain(t, stdout, "stdout",
		":: acompose check — proj  (0 services)",
		"nothing to check")
	mustNotContain(t, stdout, "stdout", "0/0", "compose features", "summary:")
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestCheckRunProjectPseudoEntry(t *testing.T) {
	p := projectFromYAML(t, `services:
  a:
    image: nginx
    networks: [frontend]
networks:
  frontend: {}
`)
	var blockers int
	stdout, _ := captureOutput(t, func() { blockers = checkRun(p) })
	if blockers != 0 {
		t.Errorf("blockers = %d, want 0 — flattened networks are not blockers", blockers)
	}
	mustOrder(t, stdout, "stdout",
		"  a\n",
		"! networks: custom networks — acompose puts every service on one shared project network",
		"  project\n",
		"! networks: top-level networks (frontend) — acompose flattens everything onto one shared project network")
	// service finding + project finding over a base of 1 (image)
	mustContain(t, stdout, "stdout",
		"summary: 0 clean · 1 approximated · 0 with blockers",
		"1/3 compose features in this file translate cleanly or approximated")
}

// cmdCheck must exit 1 on a blocker (the suite-wide "found problems" code;
// 2 is reserved for usage/refusals) — same re-exec pattern as the other
// os.Exit paths.
func TestCmdCheckBlockerExitSubprocess(t *testing.T) {
	if os.Getenv("ACOMPOSE_TEST_CHECK_BLOCKER") == "1" {
		p := loadProject([]string{"docker-compose.yml"}, "blk")
		cmdCheck(p) // must os.Exit(1) before this returns
		return
	}
	dir := t.TempDir()
	yaml := "services:\n  old:\n    image: oldapp\n    platform: linux/amd64\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCmdCheckBlockerExitSubprocess$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ACOMPOSE_TEST_CHECK_BLOCKER=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got err=%v, output:\n%s", err, out)
	}
	mustContain(t, string(out), "subprocess output", "1 with blockers")
}

func TestCmdCheckNoBlockerReturns(t *testing.T) {
	p := projectFromYAML(t, "services:\n  solo:\n    image: nginx\n")
	stdout, _ := captureOutput(t, func() { cmdCheck(p) }) // returning at all proves no os.Exit
	mustContain(t, stdout, "stdout", "translates cleanly")
}

// warnUnsupported is now a renderer over analyzeService — the legacy texts
// asserted by dryrun_test/deploy_test still hold (whole suite), and the
// point-of-use findings must NOT leak in as duplicate warnings.
func TestWarnUnsupportedFromFindings(t *testing.T) {
	t.Run("custom networks warn during up", func(t *testing.T) {
		p := projectFromYAML(t, `services:
  a:
    image: nginx
    networks: [frontend, backend]
networks:
  frontend: {}
  backend: {}
`)
		_, stderr := captureOutput(t, func() {
			warnUnsupported("a", p.Services["a"])
		})
		mustContain(t, stderr, "stderr",
			"[a] custom networks (backend, frontend) ignored — every service joins one shared project network")
	})
	t.Run("anonymous volume and missing image are silent here", func(t *testing.T) {
		svc := types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{{Type: "volume", Target: "/anon"}}}
		_, stderr := captureOutput(t, func() {
			warnUnsupported("a", svc)
		})
		if stderr != "" {
			t.Errorf("stderr = %q, want empty (runCmd and cmdUp own these warnings)", stderr)
		}
	})
}
