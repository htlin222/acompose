package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestExtractIPv4PrefersAddressOverGateway(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "gateway and address as siblings",
			in: map[string]any{"networks": []any{map[string]any{
				"gateway": "192.168.65.1",
				"address": "192.168.65.2/24",
			}}},
			want: "192.168.65.2",
		},
		{
			name: "gateway nested before address in a different subtree",
			in: map[string]any{
				"a_gateway": map[string]any{"ip": "192.168.65.1"},
				"network":   map[string]any{"ipAddress": "192.168.65.7/24"},
			},
			want: "192.168.65.7",
		},
		{
			name: "loopback excluded",
			in:   map[string]any{"address": "127.0.0.1", "bind": "0.0.0.0"},
			want: "",
		},
		{
			name: "fallback when no address key",
			in:   map[string]any{"ip": "192.168.64.3/24"},
			want: "192.168.64.3",
		},
	}
	for _, tc := range cases {
		if got := extractIPv4(tc.in); got != tc.want {
			t.Errorf("%s: extractIPv4 = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestToposort(t *testing.T) {
	p := &types.Project{
		Name: "t",
		Services: types.Services{
			"web": {Name: "web", DependsOn: types.DependsOnConfig{"api": {}}},
			"api": {Name: "api", DependsOn: types.DependsOnConfig{"db": {}}},
			"db":  {Name: "db"},
		},
	}
	got := toposort(p)
	want := []string{"db", "api", "web"}
	if len(got) != len(want) {
		t.Fatalf("toposort = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("toposort = %v, want %v", got, want)
		}
	}
}

func TestEnvKey(t *testing.T) {
	if got := envKey("db-master"); got != "DB_MASTER_HOST" {
		t.Errorf("envKey(db-master) = %q, want DB_MASTER_HOST", got)
	}
}

func TestRunCmdNamedVolume(t *testing.T) {
	p := &types.Project{
		Name: "proj",
		Volumes: types.Volumes{
			"data": {},
			"ext":  {Name: "customname", External: true},
		},
	}
	s := types.ServiceConfig{
		Volumes: []types.ServiceVolumeConfig{
			{Type: "volume", Source: "data", Target: "/target"},
			{Type: "volume", Source: "ext", Target: "/ext"},
		},
	}
	args := strings.Join(runCmd(p, "proj-svc", "proj-net", "img", s, nil, false), " ")
	if !strings.Contains(args, "--volume proj-data:/target") {
		t.Errorf("default-named volume missing project prefix: %s", args)
	}
	if !strings.Contains(args, "--volume customname:/ext") {
		t.Errorf("explicit volume name not used: %s", args)
	}
}

func TestBuildCmd(t *testing.T) {
	v1, v2 := "1", "2"
	s := types.ServiceConfig{Build: &types.BuildConfig{
		Context:    ".",
		Dockerfile: "Dockerfile", // the default — must NOT emit --file
		Args:       types.MappingWithEquals{"ZED": &v2, "ALPHA": &v1},
	}}
	got := strings.Join(buildCmd("img", s, ""), " ")
	if strings.Contains(got, "--file") {
		t.Errorf("default Dockerfile should not emit --file: %s", got)
	}
	a, z := strings.Index(got, "ALPHA=1"), strings.Index(got, "ZED=2")
	if a < 0 || z < 0 || a > z {
		t.Errorf("build args missing or not sorted: %s", got)
	}

	s.Build.Dockerfile = "Dockerfile.dev"
	got = strings.Join(buildCmd("img", s, ""), " ")
	if !strings.Contains(got, "--file Dockerfile.dev") {
		t.Errorf("non-default Dockerfile should emit --file: %s", got)
	}
}

func TestVolName(t *testing.T) {
	p := &types.Project{
		Name: "proj",
		Volumes: types.Volumes{
			"data":  {},
			"named": {Name: "explicit"},
		},
	}
	if got := volName(p, "data"); got != "proj-data" {
		t.Errorf("volName(data) = %q, want proj-data", got)
	}
	if got := volName(p, "named"); got != "explicit" {
		t.Errorf("volName(named) = %q, want explicit", got)
	}
}

// parseIntervalArg guards the --interval flag: Sscanf-style leniency used to
// accept garbage and 0/negative seconds, turning watch/dev into busy loops.
func TestParseIntervalArg(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false},
		{"10", 10, false},
		{"3600", 3600, false},
		{"0", 0, true},   // a zero interval is a busy loop
		{"-5", 0, true},  // so is a negative one
		{"abc", 0, true}, // not a number at all
		{"", 0, true},    //
		{"5x", 0, true},  // Sscanf would have happily parsed the 5
		{"2.5", 0, true}, // whole seconds only
	}
	for _, tc := range cases {
		t.Run("in="+tc.in, func(t *testing.T) {
			got, err := parseIntervalArg(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseIntervalArg(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("parseIntervalArg(%q) = %d, want %d", tc.in, got, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "--interval") {
				t.Errorf("error %q must name the flag", err)
			}
		})
	}
}

// printUsage must list every dispatched subcommand — a help screen that
// omits a command is how a feature becomes undiscoverable. (help itself
// goes to stdout with exit 0; the wrong-invocation path is usage()→stderr.)
func TestPrintUsageListsSubcommands(t *testing.T) {
	var b strings.Builder
	printUsage(&b)
	out := b.String()
	for _, cmd := range []string{
		"up", "down", "refresh", "ps", "build", "start", "stop", "watch",
		"dev", "update", "stats", "ui", "menubar", "logs", "exec", "check",
		"dns", "import-volumes", "init", "doctor", "version", "help",
	} {
		if !strings.Contains(out, cmd) {
			t.Errorf("usage screen does not mention %q", cmd)
		}
	}
}

// The `acompose init` template must stay loadable by compose-go — this is
// the first thing a brand-new user runs.
func TestDemoComposeParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(path, []byte(demoCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	p := loadProject([]string{path}, "demo")
	svc, ok := p.Services["hello"]
	if !ok {
		t.Fatalf("demo template has no 'hello' service; got %v", p.ServiceNames())
	}
	if svc.Image != "traefik/whoami" {
		t.Errorf("hello image = %q", svc.Image)
	}
	if len(svc.Ports) != 1 || svc.Ports[0].Published != "8080" || svc.Ports[0].Target != 80 {
		t.Errorf("hello ports = %+v, want 8080:80", svc.Ports)
	}
}
