package main

// Feature tests for deploy.resources.limits translation: the pure formatters,
// the runCmd flag matrix, the new partial-support warnUnsupported behavior,
// compose-go end-to-end parsing, and the cmdUp dry-run output.

import (
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func intp(i int) *int { return &i }

func limits(l *types.Resource) *types.DeployConfig {
	return &types.DeployConfig{Resources: types.Resources{Limits: l}}
}

// Whole CPUs only — verified live that the runtime rejects fractions
// (`container run --cpus 1.5` → "The value '1.5' is invalid"). Round UP,
// never down, and never below 1.
func TestFormatCPUs(t *testing.T) {
	cases := []struct {
		in   float32
		want string
	}{
		{1, "1"},
		{2, "2"},
		{1.5, "2"},
		{0.5, "1"},
		{0.25, "1"},
		{0.1, "1"},
		{4, "4"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := formatCPUs(types.NanoCPUs(tc.in)); got != tc.want {
				t.Errorf("formatCPUs(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatMemory(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want string
	}{
		{"512M exact", 536870912, "512M"},
		{"1G becomes 1024M", 1073741824, "1024M"},
		{"exactly one MiB", 1048576, "1M"},
		{"sub-MiB rounds up to the minimum", 1, "1M"},
		{"one byte over a MiB rounds up", 1048577, "2M"},
		{"100 MiB", 100 * 1024 * 1024, "100M"},
		{"6G", 6 * 1024 * 1024 * 1024, "6144M"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatMemory(tc.in); got != tc.want {
				t.Errorf("formatMemory(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunCmdDeployLimits(t *testing.T) {
	p := &types.Project{Name: "proj"}
	cases := []struct {
		name    string
		svc     types.ServiceConfig
		want    []string
		notWant []string
	}{
		{
			name: "cpus only",
			svc:  types.ServiceConfig{Deploy: limits(&types.Resource{NanoCPUs: 1.5})},
			want: []string{"--cpus 2"}, notWant: []string{"--memory"},
		},
		{
			name: "fractional cpus",
			svc:  types.ServiceConfig{Deploy: limits(&types.Resource{NanoCPUs: 0.5})},
			want: []string{"--cpus 1"},
		},
		{
			name: "memory only 512M",
			svc:  types.ServiceConfig{Deploy: limits(&types.Resource{MemoryBytes: 536870912})},
			want: []string{"--memory 512M"}, notWant: []string{"--cpus"},
		},
		{
			name: "memory 1G emitted as 1024M",
			svc:  types.ServiceConfig{Deploy: limits(&types.Resource{MemoryBytes: 1073741824})},
			want: []string{"--memory 1024M"},
		},
		{
			name: "both flags",
			svc:  types.ServiceConfig{Deploy: limits(&types.Resource{NanoCPUs: 2, MemoryBytes: 268435456})},
			want: []string{"--cpus 2", "--memory 256M"},
		},
		{
			name:    "no deploy means no flags",
			svc:     types.ServiceConfig{},
			notWant: []string{"--cpus", "--memory"},
		},
		{
			name:    "deploy without limits means no flags",
			svc:     types.ServiceConfig{Deploy: &types.DeployConfig{Replicas: intp(3)}},
			notWant: []string{"--cpus", "--memory"},
		},
		{
			name:    "zero-valued limits emit nothing",
			svc:     types.ServiceConfig{Deploy: limits(&types.Resource{})},
			notWant: []string{"--cpus", "--memory"},
		},
		{
			name:    "reservations alone emit nothing",
			svc:     types.ServiceConfig{Deploy: &types.DeployConfig{Resources: types.Resources{Reservations: &types.Resource{NanoCPUs: 1}}}},
			notWant: []string{"--cpus", "--memory"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := strings.Join(runCmd(p, "c", "n", "img", tc.svc, nil, false), " ")
			mustContain(t, args, "runCmd", tc.want...)
			mustNotContain(t, args, "runCmd", tc.notWant...)
		})
	}

	t.Run("limit flags appear before the image", func(t *testing.T) {
		svc := types.ServiceConfig{Deploy: limits(&types.Resource{NanoCPUs: 1, MemoryBytes: 1048576})}
		args := strings.Join(runCmd(p, "c", "n", "img", svc, nil, false), " ")
		mustOrder(t, args, "runCmd flag order", "--cpus 1", "--memory 1M", " img")
	})

	t.Run("non-MiB-aligned memory rounds up with one warning", func(t *testing.T) {
		svc := types.ServiceConfig{Deploy: limits(&types.Resource{MemoryBytes: 1048577})}
		var args string
		_, stderr := captureOutput(t, func() {
			args = strings.Join(runCmd(p, "c", "n", "img", svc, nil, false), " ")
		})
		mustContain(t, args, "runCmd", "--memory 2M")
		mustContain(t, stderr, "stderr", "rounded up to 2M")
	})

	t.Run("MiB-aligned memory does not warn", func(t *testing.T) {
		svc := types.ServiceConfig{Deploy: limits(&types.Resource{MemoryBytes: 536870912})}
		_, stderr := captureOutput(t, func() {
			runCmd(p, "c", "n", "img", svc, nil, false)
		})
		mustNotContain(t, stderr, "stderr", "rounded up")
	})
}

func TestWarnUnsupportedDeploy(t *testing.T) {
	cases := []struct {
		name    string
		deploy  *types.DeployConfig
		want    []string
		notWant []string
	}{
		{
			name:    "nil deploy is silent",
			deploy:  nil,
			notWant: []string{"deploy"},
		},
		{
			name:    "limits-only is silent",
			deploy:  limits(&types.Resource{NanoCPUs: 1.5, MemoryBytes: 536870912}),
			notWant: []string{"deploy"},
		},
		{
			name:    "replicas 1 is silent (matches reality)",
			deploy:  &types.DeployConfig{Replicas: intp(1)},
			notWant: []string{"deploy"},
		},
		{
			name:   "replicas 3 warns",
			deploy: &types.DeployConfig{Replicas: intp(3)},
			want:   []string{"deploy: replicas ignored — only resources.limits (cpus, memory) are applied"},
		},
		{
			name:   "reservations warn",
			deploy: &types.DeployConfig{Resources: types.Resources{Reservations: &types.Resource{MemoryBytes: 1048576}}},
			want:   []string{"deploy: resources.reservations ignored"},
		},
		{
			name:   "update_config warns",
			deploy: &types.DeployConfig{UpdateConfig: &types.UpdateConfig{}},
			want:   []string{"deploy: update_config ignored"},
		},
		{
			name:   "rollback_config warns",
			deploy: &types.DeployConfig{RollbackConfig: &types.UpdateConfig{}},
			want:   []string{"deploy: rollback_config ignored"},
		},
		{
			name:   "restart_policy warns",
			deploy: &types.DeployConfig{RestartPolicy: &types.RestartPolicy{Condition: "any"}},
			want:   []string{"deploy: restart_policy ignored"},
		},
		{
			name:   "placement warns",
			deploy: &types.DeployConfig{Placement: types.Placement{Constraints: []string{"node.role==manager"}}},
			want:   []string{"deploy: placement ignored"},
		},
		{
			name:   "global mode warns",
			deploy: &types.DeployConfig{Mode: "global"},
			want:   []string{"deploy: mode ignored"},
		},
		{
			name:    "replicated mode alone is silent",
			deploy:  &types.DeployConfig{Mode: "replicated"},
			notWant: []string{"deploy"},
		},
		{
			name:   "pids limit warns",
			deploy: limits(&types.Resource{NanoCPUs: 1, Pids: 100}),
			want:   []string{"deploy: resources.limits.pids/devices ignored"},
		},
		{
			name: "multiple ignored parts listed together",
			deploy: &types.DeployConfig{
				Replicas:  intp(3),
				Resources: types.Resources{Reservations: &types.Resource{NanoCPUs: 1}},
			},
			want: []string{"deploy: replicas/resources.reservations ignored"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr := captureOutput(t, func() {
				warnUnsupported("svc", types.ServiceConfig{Deploy: tc.deploy})
			})
			mustContain(t, stderr, "stderr", tc.want...)
			mustNotContain(t, stderr, "stderr", tc.notWant...)
		})
	}
}

const deployLimitsYAML = `services:
  app:
    image: nginx
    deploy:
      resources:
        limits:
          cpus: "1.5"
          memory: 512M
`

// compose-go must parse deploy.resources.limits through the production
// loadProject path with no extra loader options.
func TestLoadProjectParsesDeployLimits(t *testing.T) {
	p := projectFromYAML(t, deployLimitsYAML)
	d := p.Services["app"].Deploy
	if d == nil || d.Resources.Limits == nil {
		t.Fatalf("deploy.resources.limits not parsed: %+v", d)
	}
	if got := d.Resources.Limits.NanoCPUs.Value(); got != 1.5 {
		t.Errorf("cpus = %v, want 1.5", got)
	}
	if got := int64(d.Resources.Limits.MemoryBytes); got != 536870912 {
		t.Errorf("memory = %d bytes, want 536870912 (512M)", got)
	}
}

// Dry-run e2e: the printed `container run` line carries the limit flags, and
// a limits-only deploy section produces no deploy warning at all.
func TestCmdUpDryRunDeployLimits(t *testing.T) {
	p := projectFromYAML(t, deployLimitsYAML)
	stdout, stderr := captureOutput(t, func() {
		cmdUp(p, runner{dry: true}, true, time.Second)
	})
	mustContain(t, stdout, "stdout", "--cpus 2", "--memory 512M")
	mustNotContain(t, stderr, "stderr", "deploy")
}

// A deploy section mixing limits with unsupported keys both applies the
// limits AND warns about the rest — nothing is silently dropped.
func TestCmdUpDryRunDeployMixed(t *testing.T) {
	p := projectFromYAML(t, `services:
  app:
    image: nginx
    deploy:
      replicas: 3
      resources:
        limits:
          cpus: "0.5"
        reservations:
          memory: 128M
`)
	stdout, stderr := captureOutput(t, func() {
		cmdUp(p, runner{dry: true}, true, time.Second)
	})
	mustContain(t, stdout, "stdout", "--cpus 1")
	mustContain(t, stderr, "stderr",
		"[app] deploy: replicas/resources.reservations ignored — only resources.limits (cpus, memory) are applied")
}
