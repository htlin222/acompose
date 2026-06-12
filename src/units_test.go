package main

// Exhaustive tables for the pure helpers, plus the local-socket waitTCP tests.

import (
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

func TestExtractIPv4Table(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "gateway key skipped at top level",
			in:   map[string]any{"gateway": "192.168.65.1"},
			want: "",
		},
		{
			name: "gateway subtree skipped entirely",
			in: map[string]any{
				"a_gateway": map[string]any{"ip": "192.168.65.1"},
				"network":   map[string]any{"ipAddress": "192.168.65.7/24"},
			},
			want: "192.168.65.7",
		},
		{
			name: "dns subtree skipped",
			in: map[string]any{
				"dnsServers": map[string]any{"primary": "8.8.8.8"},
				"other":      "192.168.1.5",
			},
			want: "192.168.1.5",
		},
		{
			name: "nameserver list skipped next to an address",
			in: map[string]any{"config": map[string]any{
				"nameserver": []any{"10.0.0.53"},
				"ipAddress":  "10.1.2.3/16",
			}},
			want: "10.1.2.3",
		},
		{
			name: "address found deep in nested arrays",
			in:   map[string]any{"a": map[string]any{"b": []any{map[string]any{"ip_address": "172.16.0.4/12"}}}},
			want: "172.16.0.4",
		},
		{
			name: "address key preferred over earlier stray match",
			in: map[string]any{
				"aStray":  "10.9.9.9",
				"zwaddrs": map[string]any{"address": "10.0.0.2/24"},
			},
			want: "10.0.0.2",
		},
		{
			name: "CIDR suffix stripped",
			in:   map[string]any{"address": "192.168.64.9/24"},
			want: "192.168.64.9",
		},
		{
			name: "loopback and 0.0.0.0 excluded",
			in:   map[string]any{"address": "127.0.0.1", "bind": "0.0.0.0"},
			want: "",
		},
		{
			name: "fallback used when no address key exists",
			in:   map[string]any{"ip": "192.168.64.3/24"},
			want: "192.168.64.3",
		},
		{
			name: "deterministic across multiple addresses (sorted key walk)",
			in:   map[string]any{"zAddress": "10.0.0.2", "aAddress": "10.0.0.1"},
			want: "10.0.0.1",
		},
		{
			name: "bare string ip lands in fallback",
			in:   "10.0.0.9",
			want: "10.0.0.9",
		},
		{
			name: "non-IP string",
			in:   "hello world",
			want: "",
		},
		{
			name: "too many octets rejected",
			in:   map[string]any{"address": "1.2.3.4.5"},
			want: "",
		},
		{
			name: "empty map",
			in:   map[string]any{},
			want: "",
		},
		{
			name: "nil input",
			in:   nil,
			want: "",
		},
		{
			name: "numbers and bools ignored",
			in:   map[string]any{"address": 42.0, "up": true},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractIPv4(tc.in); got != tc.want {
				t.Errorf("extractIPv4 = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToposortTable(t *testing.T) {
	dep := func(names ...string) types.DependsOnConfig {
		d := types.DependsOnConfig{}
		for _, n := range names {
			d[n] = types.ServiceDependency{}
		}
		return d
	}
	cases := []struct {
		name     string
		services types.Services
		want     []string
	}{
		{
			name: "chain",
			services: types.Services{
				"web": {Name: "web", DependsOn: dep("api")},
				"api": {Name: "api", DependsOn: dep("db")},
				"db":  {Name: "db"},
			},
			want: []string{"db", "api", "web"},
		},
		{
			name: "diamond",
			services: types.Services{
				"top":   {Name: "top", DependsOn: dep("left", "right")},
				"left":  {Name: "left", DependsOn: dep("base")},
				"right": {Name: "right", DependsOn: dep("base")},
				"base":  {Name: "base"},
			},
			want: []string{"base", "left", "right", "top"},
		},
		{
			name: "independent services sorted alphabetically",
			services: types.Services{
				"zeta":  {Name: "zeta"},
				"alpha": {Name: "alpha"},
				"mid":   {Name: "mid"},
			},
			want: []string{"alpha", "mid", "zeta"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toposort(&types.Project{Name: "t", Services: tc.services})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("toposort = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("unknown dependency warns and is ignored", func(t *testing.T) {
		p := &types.Project{Name: "t", Services: types.Services{
			"web": {Name: "web", DependsOn: dep("ghost")},
		}}
		var got []string
		_, stderr := captureOutput(t, func() { got = toposort(p) })
		if !reflect.DeepEqual(got, []string{"web"}) {
			t.Errorf("toposort = %v, want [web]", got)
		}
		mustContain(t, stderr, "stderr", "depends on unknown service 'ghost'")
	})
}

func strp(s string) *string { return &s }

func TestRunCmdTable(t *testing.T) {
	p := &types.Project{Name: "proj", Volumes: types.Volumes{"data": {}}}
	cases := []struct {
		name    string
		svc     types.ServiceConfig
		extra   map[string]string
		publish bool
		want    []string
		notWant []string
	}{
		{
			name:    "port without HostIP",
			svc:     types.ServiceConfig{Ports: []types.ServicePortConfig{{Published: "8080", Target: 80}}},
			publish: true,
			want:    []string{"--publish 8080:80"},
		},
		{
			name:    "port with HostIP",
			svc:     types.ServiceConfig{Ports: []types.ServicePortConfig{{HostIP: "127.0.0.1", Published: "8080", Target: 80}}},
			publish: true,
			want:    []string{"--publish 127.0.0.1:8080:80"},
		},
		{
			name:    "unpublished port skipped",
			svc:     types.ServiceConfig{Ports: []types.ServicePortConfig{{Published: "", Target: 80}}},
			publish: true,
			notWant: []string{"--publish"},
		},
		{
			name:    "publish=false suppresses ports",
			svc:     types.ServiceConfig{Ports: []types.ServicePortConfig{{Published: "8080", Target: 80}}},
			publish: false,
			notWant: []string{"--publish"},
		},
		{
			name: "nil-valued env becomes K=",
			svc:  types.ServiceConfig{Environment: types.MappingWithEquals{"EMPTY": nil, "SET": strp("v")}},
			want: []string{"--env EMPTY= ", "--env SET=v"},
		},
		{
			name: "bind volume passed through",
			svc:  types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{{Type: "bind", Source: "/host/dir", Target: "/c"}}},
			want: []string{"--volume /host/dir:/c"},
		},
		{
			name: "named volume resolved via volName",
			svc:  types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{{Type: "volume", Source: "data", Target: "/d"}}},
			want: []string{"--volume proj-data:/d"},
		},
		{
			name:    "working_dir",
			svc:     types.ServiceConfig{WorkingDir: "/srv"},
			want:    []string{"--workdir /srv"},
			notWant: []string{"--volume", "--publish", "--env"},
		},
		{
			name: "command args appended after the image",
			svc:  types.ServiceConfig{Command: types.ShellCommand{"sh", "-c", "echo hi"}},
			want: []string{"img sh -c echo hi"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := strings.Join(runCmd(p, "proj-svc", "proj-net", "img", tc.svc, tc.extra, tc.publish), " ")
			mustContain(t, args+" ", "runCmd", tc.want...) // trailing space lets "K= " match end-of-arg
			mustNotContain(t, args, "runCmd", tc.notWant...)
		})
	}

	t.Run("extraEnv sorted and emitted after service env", func(t *testing.T) {
		svc := types.ServiceConfig{Environment: types.MappingWithEquals{"ZED": strp("z")}}
		extra := map[string]string{"B_HOST": "10.0.0.2", "A_HOST": "10.0.0.1"}
		args := strings.Join(runCmd(p, "c", "n", "img", svc, extra, false), " ")
		mustOrder(t, args, "runCmd env order",
			"--env ZED=z", "--env A_HOST=10.0.0.1", "--env B_HOST=10.0.0.2")
	})

	t.Run("anonymous volume skipped with a warning", func(t *testing.T) {
		svc := types.ServiceConfig{Volumes: []types.ServiceVolumeConfig{{Type: "volume", Source: "", Target: "/anon"}}}
		var args string
		_, stderr := captureOutput(t, func() {
			args = strings.Join(runCmd(p, "c", "n", "img", svc, nil, false), " ")
		})
		mustNotContain(t, args, "runCmd", "--volume")
		mustContain(t, stderr, "stderr", "anonymous volume on '/anon' is not supported")
	})
}

func TestBuildCmdTargetAndNilArg(t *testing.T) {
	v := "x"
	s := types.ServiceConfig{Build: &types.BuildConfig{
		Context: "ctx",
		Target:  "prod",
		Args:    types.MappingWithEquals{"NILARG": nil, "SET": &v},
	}}
	got := buildCmd("img", s, "")
	joined := strings.Join(got, " ")
	mustContain(t, joined, "buildCmd", "--target prod", "--build-arg SET=x")
	mustNotContain(t, joined, "buildCmd", "NILARG")
	if got[len(got)-1] != "ctx" {
		t.Errorf("context must be the final arg, got %v", got)
	}
}

func TestHostsInjectCmd(t *testing.T) {
	got := hostsInjectCmd("mydb", [][2]string{{"db", "10.0.0.2"}, {"app", "10.0.0.3"}})
	want := []string{
		"container", "exec", "mydb", "sh", "-c",
		"printf \"%s\\n\" '10.0.0.2\tdb' '10.0.0.3\tapp' >> /etc/hosts",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hostsInjectCmd:\n got %q\nwant %q", got, want)
	}
}

func TestEnvKeyTable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"db", "DB_HOST"},
		{"db-master", "DB_MASTER_HOST"},
		{"a.b.c", "A_B_C_HOST"},
		{"Mixed-Case.x", "MIXED_CASE_X_HOST"},
		// a leading digit gets an underscore prefix so the var name stays
		// POSIX-portable
		{"2cool", "_2COOL_HOST"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := envKey(tc.in); got != tc.want {
				t.Errorf("envKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNamedVolumes(t *testing.T) {
	p := &types.Project{
		Name: "np",
		Volumes: types.Volumes{
			"data":   {},                          // referenced by two services → once
			"logs":   {Name: "central-logs"},      // explicit name wins
			"ext":    {Name: "x", External: true}, // external → excluded
			"unused": {},                          // declared but never mounted → excluded
		},
		Services: types.Services{
			"a": {Name: "a", Volumes: []types.ServiceVolumeConfig{
				{Type: "volume", Source: "data", Target: "/d"},
				{Type: "volume", Source: "logs", Target: "/l"},
				{Type: "volume", Source: "ext", Target: "/e"},
				{Type: "bind", Source: "/host", Target: "/h"},
			}},
			"b": {Name: "b", Volumes: []types.ServiceVolumeConfig{
				{Type: "volume", Source: "data", Target: "/d2"},
				{Type: "volume", Source: "", Target: "/anon"}, // anonymous → excluded
			}},
		},
	}
	got := namedVolumes(p)
	want := []string{"central-logs", "np-data"} // sorted, deduped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("namedVolumes = %v, want %v", got, want)
	}
}

func TestListenUI(t *testing.T) {
	t.Run("free address binds as-is", func(t *testing.T) {
		ln, bound, err := listenUI("127.0.0.1:0", false)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if bound != "127.0.0.1:0" {
			t.Errorf("bound = %q", bound)
		}
	})
	t.Run("busy default walks to the next free port", func(t *testing.T) {
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer taken.Close()
		addr := taken.Addr().String()
		captureOutput(t, func() { // swallow the "was busy — using ..." narration
			ln, bound, err := listenUI(addr, false)
			if err != nil {
				t.Fatalf("expected fallback port, got %v", err)
			}
			defer ln.Close()
			if bound == addr {
				t.Error("must not report the busy address as bound")
			}
		})
	})
	t.Run("busy explicit address fails", func(t *testing.T) {
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer taken.Close()
		if ln, _, err := listenUI(taken.Addr().String(), true); err == nil {
			ln.Close()
			t.Error("explicit busy address must error, not fall back")
		}
	})
}

func TestProbeUI(t *testing.T) {
	t.Run("recognizes a dashboard", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer ln.Close()
		go func() {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/state", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"project":"demo"}`)
			})
			_ = http.Serve(ln, mux)
		}()
		proj, ok := probeUI(ln.Addr().String())
		if !ok || proj != "demo" {
			t.Errorf("probeUI = (%q, %v), want (demo, true)", proj, ok)
		}
	})
	t.Run("non-dashboard listener is not claimed", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		defer ln.Close()
		go func() { _ = http.Serve(ln, http.NotFoundHandler()) }()
		if _, ok := probeUI(ln.Addr().String()); ok {
			t.Error("404 server must not be recognized as a dashboard")
		}
	})
}

func TestEnsureServiceRunning(t *testing.T) {
	p := projectFromYAML(t, `
services:
  web:
    image: nginx
    depends_on: [db]
  db:
    image: postgres
`)
	t.Run("stopped container is started", func(t *testing.T) {
		fakeContainer(t, `case "$1" in start) exit 0;; esac; echo "{}"; exit 0`)
		ok, detail := ensureServiceRunning(p, runner{}, "web", true)
		if !ok || detail != "started" {
			t.Errorf("= (%v, %q), want (true, started)", ok, detail)
		}
	})
	t.Run("missing container is recreated", func(t *testing.T) {
		fakeContainer(t, `case "$1" in
start) echo "container not found"; exit 1;;
run) exit 0;;
*) echo "{}"; exit 0;;
esac`)
		ok, detail := ensureServiceRunning(p, runner{}, "web", true)
		if !ok || detail != "recreated" {
			t.Errorf("= (%v, %q), want (true, recreated)", ok, detail)
		}
	})
	t.Run("recreate failure surfaces the runtime message", func(t *testing.T) {
		fakeContainer(t, `case "$1" in
start) echo "container not found"; exit 1;;
run) echo "Address already in use"; exit 1;;
*) echo "{}"; exit 0;;
esac`)
		ok, detail := ensureServiceRunning(p, runner{}, "web", true)
		if ok || !strings.Contains(detail, "Address already in use") {
			t.Errorf("= (%v, %q), want failure mentioning the address error", ok, detail)
		}
	})
	t.Run("unknown service refused", func(t *testing.T) {
		ok, _ := ensureServiceRunning(p, runner{}, "ghost", true)
		if ok {
			t.Error("unknown service must not be ok")
		}
	})
}

func TestLsLineRunning(t *testing.T) {
	lsOut := "ID         IMAGE      STATE\n" +
		"proj-db    postgres   running\n" +
		"proj-app   nginx      stopped\n"
	cases := []struct {
		name  string
		lsOut string
		cname string
		want  bool
	}{
		{"running container", lsOut, "proj-db", true},
		{"stopped container", lsOut, "proj-app", false},
		{"missing container", lsOut, "proj-ghost", false},
		// matching is by exact ID column, so "proj-app" must NOT claim the
		// "proj-app2" line (this was a substring-collision false positive)
		{"no collision with a longer name", "proj-app2  nginx  running\n", "proj-app", false},
		{"exact name still matches", "proj-app2  nginx  stopped\nproj-app  nginx  running\n", "proj-app", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lsLineRunning(tc.lsOut, tc.cname); got != tc.want {
				t.Errorf("lsLineRunning(%q) = %v, want %v", tc.cname, got, tc.want)
			}
		})
	}
}

func TestWarnUnsupportedSecretsAndPlatform(t *testing.T) {
	// the healthcheck/restart/deploy/entrypoint/user branches are exercised by
	// the cmdUp dry-run e2e; these two only trip on less common compose files
	s := types.ServiceConfig{
		Platform: "linux/amd64",
		Secrets:  []types.ServiceSecretConfig{{Source: "tls-cert"}},
	}
	_, stderr := captureOutput(t, func() {
		warnUnsupported("svc", s)
	})
	mustContain(t, stderr, "stderr",
		"[svc] secrets/configs ignored",
		"x86 images are NOT seamless")
}

func TestSameDigests(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]bool
		want bool
	}{
		{"equal sets", map[string]bool{"sha256:a": true, "sha256:b": true}, map[string]bool{"sha256:a": true, "sha256:b": true}, true},
		{"different size", map[string]bool{"sha256:a": true}, map[string]bool{"sha256:a": true, "sha256:b": true}, false},
		{"same size different content", map[string]bool{"sha256:a": true, "sha256:b": true}, map[string]bool{"sha256:a": true, "sha256:c": true}, false},
		{"both empty", map[string]bool{}, map[string]bool{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameDigests(tc.a, tc.b); got != tc.want {
				t.Errorf("sameDigests = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDigestKeyRE(t *testing.T) {
	// realistic `container image inspect` shape: manifest/config digests live
	// under "digest" keys, layer hashes appear under other keys and as bare
	// strings — only the former may match.
	inspect := `{
	  "name": "postgres:16",
	  "index": {
	    "manifests": [
	      {"digest": "sha256:0a1b2c", "platform": {"architecture": "arm64"}},
	      {"digest" : "sha256:0A1B2C3D", "platform": {"architecture": "amd64"}}
	    ]
	  },
	  "layers": [
	    {"mediaType": "tar+gzip", "blobSum": "sha256:deadbeef"},
	    "sha256:cafebabe"
	  ],
	  "config": {"digest":"sha256:fff"}
	}`
	var got []string
	for _, m := range digestKeyRE.FindAllStringSubmatch(inspect, -1) {
		got = append(got, m[1])
	}
	want := []string{"sha256:0a1b2c", "sha256:0A1B2C3D", "sha256:fff"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("digestKeyRE matches = %v, want %v", got, want)
	}
}

func TestVolNameUnknownKeyFallsBackToProjectPrefix(t *testing.T) {
	p := &types.Project{Name: "proj"}
	if got := volName(p, "nowhere"); got != "proj-nowhere" {
		t.Errorf("volName(undeclared) = %q, want proj-nowhere", got)
	}
}

func TestWaitTCP(t *testing.T) {
	t.Run("accepting listener succeeds quickly", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		port := uint32(l.Addr().(*net.TCPAddr).Port)
		start := time.Now()
		stdout, stderr := captureOutput(t, func() {
			waitTCP("127.0.0.1", port, 3*time.Second, "db")
		})
		mustContain(t, stdout, "stdout", "db is accepting connections")
		mustNotContain(t, stderr, "stderr", "no TCP answer")
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("success path took %s, expected near-instant", elapsed)
		}
	})

	t.Run("closed port warns after the timeout", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := uint32(l.Addr().(*net.TCPAddr).Port)
		l.Close() // nothing listens here anymore — dials are refused instantly
		_, stderr := captureOutput(t, func() {
			waitTCP("127.0.0.1", port, 1*time.Second, "db")
		})
		mustContain(t, stderr, "stderr", "db: no TCP answer")
	})
}
