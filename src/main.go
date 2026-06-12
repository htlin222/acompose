// acompose — run an existing docker-compose.yml on Apple's `container` CLI.
//
// The parsing layer is the official compose-spec/compose-go (the same library
// Docker Compose uses), which gives us the full spec for free: ${VAR} and $$
// interpolation, .env, env_file, override-file merging, port ranges, long
// syntax, profiles, extends — the entire class of bugs the Python prototype
// had to fix by hand simply cannot happen here.
//
// What this program adds on top is the actual "flip":
//   - topological start order from depends_on
//   - condition: service_healthy approximated by TCP polling (the platform
//     cannot run exec-style healthchecks)
//   - every container gets its real IP wired into every peer's /etc/hosts,
//     immediately and bidirectionally, so service-name DNS (db:5432) works
//     in unmodified app code; <SERVICE>_HOST env vars are injected as the
//     fallback for shell-less (distroless/scratch) images
//   - loud, specific warnings for everything the platform cannot honour
//
// All `container` subcommand construction lives in cmds.go-style helpers at
// the bottom — if your CLI version renamed a flag, fix it in one place.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
)

// ---------- pretty output ----------------------------------------------------

var isTTY = func() bool { fi, _ := os.Stdout.Stat(); return fi.Mode()&os.ModeCharDevice != 0 }()

func c(code string) string {
	if isTTY {
		return code
	}
	return ""
}

var (
	bold, dim   = c("\033[1m"), c("\033[2m")
	green, cyan = c("\033[32m"), c("\033[36m")
	yellow, red = c("\033[33m"), c("\033[31m")
	reset       = c("\033[0m")
)

func info(f string, a ...any) { fmt.Printf(cyan+"::"+reset+" "+f+"\n", a...) }
func okay(f string, a ...any) { fmt.Printf(green+"\u2713"+reset+" "+f+"\n", a...) }
func warn(f string, a ...any) { fmt.Fprintf(os.Stderr, yellow+"!"+reset+" "+f+"\n", a...) }
func fail(f string, a ...any) { fmt.Fprintf(os.Stderr, red+"\u2717"+reset+" "+f+"\n", a...) }

// ---------- runner: nothing fails silently ------------------------------------

type runner struct{ dry bool }

func (r runner) run(args []string, opts ...string) (bool, string) {
	printable := strings.Join(args, " ")
	if r.dry {
		fmt.Printf("  %s$%s %s\n", dim, reset, printable)
		return true, ""
	}
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			fail("`container` not found — needs macOS with Apple's container CLI")
			os.Exit(1)
		}
		msg := strings.TrimSpace(string(out))
		for _, tolerate := range opts { // e.g. "exist" when re-creating the network
			if tolerate != "" && strings.Contains(strings.ToLower(msg), tolerate) {
				return false, msg
			}
		}
		fail("command failed: %s", printable)
		if lines := strings.Split(msg, "\n"); len(lines) > 0 && lines[len(lines)-1] != "" {
			fmt.Fprintf(os.Stderr, "  %s%s%s\n", dim, lines[len(lines)-1], reset)
		}
		return false, msg
	}
	return true, strings.TrimSpace(string(out))
}

// ---------- compose loading (all spec handling delegated to compose-go) -------

func loadProject(files []string, name string) *types.Project {
	var opts []cli.ProjectOptionsFn
	dir := "."
	if len(files) > 0 {
		dir = filepath.Dir(files[0])
	}
	if len(files) == 0 {
		// auto-discovers compose.yml & friends AND their override files
		opts = append(opts, cli.WithDefaultConfigPath)
	}
	// WorkingDirectory must be set BEFORE WithDotEnv, which reads .env from it
	opts = append(opts, cli.WithWorkingDirectory(dir), cli.WithOsEnv, cli.WithEnvFiles(), cli.WithDotEnv, cli.WithInterpolation(true))
	if name != "" {
		opts = append(opts, cli.WithName(name))
	}
	po, err := cli.NewProjectOptions(files, opts...)
	if err != nil {
		fail("%v", err)
		os.Exit(1)
	}
	project, err := po.LoadProject(context.Background())
	if err != nil {
		// compose-go's bare "no configuration file provided: not found" is
		// the #1 first-run stumble — say what we looked for and what to do.
		if strings.Contains(err.Error(), "no configuration file provided") {
			abs, _ := filepath.Abs(dir)
			fail("no compose file found in %s", abs)
			fmt.Fprintf(os.Stderr, "  %slooked for compose.yaml / compose.yml / docker-compose.yml / docker-compose.yaml%s\n", dim, reset)
			fmt.Fprintf(os.Stderr, "  %srun acompose inside your project directory, or point it at one: acompose up --file path/to/docker-compose.yml%s\n", dim, reset)
			fmt.Fprintf(os.Stderr, "  %sno project yet? scaffold a demo stack: acompose init%s\n", dim, reset)
			os.Exit(1)
		}
		fail("%v", err)
		os.Exit(1)
	}
	return project
}

// ---------- dependency ordering -------------------------------------------------

func toposort(p *types.Project) []string {
	order := []string{}
	done, temp := map[string]bool{}, map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if done[n] {
			return
		}
		if temp[n] {
			fail("circular depends_on detected at '%s'", n)
			os.Exit(1)
		}
		temp[n] = true
		svc := p.Services[n]
		deps := make([]string, 0, len(svc.DependsOn))
		for d := range svc.DependsOn {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		for _, d := range deps {
			if _, ok := p.Services[d]; ok {
				visit(d)
			} else {
				warn("'%s' depends on unknown service '%s', ignoring", n, d)
			}
		}
		temp[n] = false
		done[n] = true
		order = append(order, n)
	}
	names := make([]string, 0, len(p.Services))
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		visit(n)
	}
	return order
}

// ---------- platform-gap warnings -------------------------------------------------

// warnUnsupported renders analyzeService's findings (check.go — the single
// source of truth for the per-service platform-gap analysis) as `up`-time
// warnings. The deploy.* findings collapse into today's single aggregated
// line; findings with no warnText are warned/enforced elsewhere in the up
// path (runCmd warns per anonymous mount, cmdUp fails on a missing image).
func warnUnsupported(name string, s types.ServiceConfig) {
	var deployParts []string
	for _, f := range analyzeService(name, s) {
		if strings.HasPrefix(f.feature, "deploy.") {
			deployParts = append(deployParts, strings.TrimPrefix(f.feature, "deploy."))
			continue
		}
		if f.warnText == "" {
			continue
		}
		warn("[%s] %s", name, f.warnText)
	}
	if len(deployParts) > 0 {
		warn("[%s] deploy: %s ignored — only resources.limits (cpus, memory) are applied", name, strings.Join(deployParts, "/"))
	}
}

// ---------- container command construction (the one place flags live) -------------

func ctr(args ...string) []string { return append([]string{"container"}, args...) }

func buildCmd(image string, s types.ServiceConfig, projDir string) []string {
	b := s.Build
	cmd := ctr("build", "--tag", image)
	if b.Dockerfile != "" && b.Dockerfile != "Dockerfile" {
		cmd = append(cmd, "--file", b.Dockerfile)
	}
	keys := make([]string, 0, len(b.Args))
	for k := range b.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := b.Args[k]; v != nil {
			cmd = append(cmd, "--build-arg", k+"="+*v)
		}
	}
	if b.Target != "" {
		cmd = append(cmd, "--target", b.Target)
	}
	return append(cmd, b.Context)
}

// volName resolves a compose volume key to the runtime volume name:
// an explicit `name:` wins, otherwise it is prefixed with the project name.
func volName(p *types.Project, src string) string {
	if v, ok := p.Volumes[src]; ok && v.Name != "" {
		return v.Name
	}
	return p.Name + "-" + src
}

// namedVolumes returns the sorted runtime names of every non-external named
// volume referenced by a service mount (the set acompose itself manages).
func namedVolumes(p *types.Project) []string {
	set := map[string]bool{}
	for _, svc := range p.Services {
		for _, m := range svc.Volumes {
			if m.Type != "volume" || m.Source == "" {
				continue
			}
			if cfg, ok := p.Volumes[m.Source]; ok && bool(cfg.External) {
				continue
			}
			set[volName(p, m.Source)] = true
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func runCmd(p *types.Project, cname, network, image string, s types.ServiceConfig, extraEnv map[string]string, publish bool) []string {
	cmd := ctr("run", "--detach", "--name", cname, "--network", network)
	if publish {
		for _, p := range s.Ports {
			if p.Published == "" {
				continue
			}
			spec := fmt.Sprintf("%s:%d", p.Published, p.Target)
			if p.HostIP != "" {
				spec = p.HostIP + ":" + spec
			}
			cmd = append(cmd, "--publish", spec)
		}
	}
	keys := make([]string, 0, len(s.Environment))
	for k := range s.Environment {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := ""
		if s.Environment[k] != nil {
			v = *s.Environment[k]
		}
		cmd = append(cmd, "--env", k+"="+v)
	}
	ekeys := make([]string, 0, len(extraEnv))
	for k := range extraEnv {
		ekeys = append(ekeys, k)
	}
	sort.Strings(ekeys)
	for _, k := range ekeys {
		cmd = append(cmd, "--env", k+"="+extraEnv[k])
	}
	for _, v := range s.Volumes {
		if v.Type == "volume" {
			if v.Source == "" {
				warn("anonymous volume on '%s' is not supported — name it in the compose file; skipping this mount", v.Target)
				continue
			}
			cmd = append(cmd, "--volume", volName(p, v.Source)+":"+v.Target)
			continue
		}
		cmd = append(cmd, "--volume", v.Source+":"+v.Target)
	}
	if s.WorkingDir != "" {
		cmd = append(cmd, "--workdir", s.WorkingDir)
	}
	// deploy.resources.limits → real VM-level flags (`container run -c/-m`)
	if s.Deploy != nil && s.Deploy.Resources.Limits != nil {
		l := s.Deploy.Resources.Limits
		if l.NanoCPUs > 0 {
			cpus := formatCPUs(l.NanoCPUs)
			if frac := float64(l.NanoCPUs.Value()); frac != float64(int(frac)) {
				warn("cpus %s rounded up to %s — VM allocation is whole CPUs (verified: the runtime rejects fractions)", strconv.FormatFloat(frac, 'f', -1, 32), cpus)
			}
			cmd = append(cmd, "--cpus", cpus)
		}
		if l.MemoryBytes > 0 {
			mem := formatMemory(int64(l.MemoryBytes))
			if int64(l.MemoryBytes)%mib != 0 {
				warn("memory limit %d bytes rounded up to %s — the platform's granularity is 1MiB", int64(l.MemoryBytes), mem)
			}
			cmd = append(cmd, "--memory", mem)
		}
	}
	cmd = append(cmd, image)
	return append(cmd, s.Command...)
}

// formatCPUs renders a compose cpus limit as a WHOLE cpu count, rounding up —
// verified live: `container run --cpus 1.5` is rejected ("Number of CPUs"),
// because the limit is VM allocation, not a cgroups share. Never round down:
// the service must not get less than it asked for.
func formatCPUs(n types.NanoCPUs) string {
	v := float64(n.Value())
	whole := int(v)
	if v != float64(whole) {
		whole++
	}
	if whole < 1 {
		whole = 1
	}
	return strconv.Itoa(whole)
}

const mib = 1024 * 1024

// formatMemory converts a byte count to the M-suffixed form `container run
// --memory` documents as its granularity (1MiByte): 536870912 → "512M",
// 1073741824 → "1024M". Non-whole-MiB values round UP so the service never
// gets less than the compose file asked for.
func formatMemory(b int64) string {
	m := b / mib
	if b%mib != 0 {
		m++
	}
	if m < 1 {
		m = 1
	}
	return fmt.Sprintf("%dM", m)
}

func hostsInjectCmd(cname string, pairs [][2]string) []string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = fmt.Sprintf("'%s\t%s'", p[1], p[0]) // ip<TAB>name
	}
	script := fmt.Sprintf(`printf "%%s\n" %s >> /etc/hosts`, strings.Join(parts, " "))
	return ctr("exec", cname, "sh", "-c", script)
}

// ---------- IP + readiness ----------------------------------------------------------

var ipv4RE = regexp.MustCompile(`^(?:\d{1,3}\.){3}\d{1,3}(?:/\d+)?$`)

// extractIPv4 walks inspect JSON key-aware: gateway/DNS subtrees are skipped
// entirely, IPs found under keys containing "address" are preferred over any
// other stray match, so we deterministically get the container address and
// never the network gateway.
func extractIPv4(v any) string {
	var preferred, fallback []string
	var walk func(key string, o any)
	walk = func(key string, o any) {
		lk := strings.ToLower(key)
		if strings.Contains(lk, "gateway") || strings.Contains(lk, "dns") || strings.Contains(lk, "nameserver") {
			return
		}
		switch t := o.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(k, t[k])
			}
		case []any:
			for _, x := range t {
				walk(key, x)
			}
		case string:
			s := strings.TrimSpace(t)
			if !ipv4RE.MatchString(s) {
				return
			}
			ip := strings.SplitN(s, "/", 2)[0]
			if strings.HasPrefix(ip, "127.") || ip == "0.0.0.0" {
				return
			}
			if strings.Contains(lk, "address") {
				preferred = append(preferred, ip)
			} else {
				fallback = append(fallback, ip)
			}
		}
	}
	walk("", v)
	if len(preferred) > 0 {
		return preferred[0]
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return ""
}

func getIP(r runner, cname string) string {
	if r.dry {
		return "<" + cname + "-ip>"
	}
	ok, out := r.run(ctr("inspect", cname))
	if !ok || out == "" {
		return ""
	}
	var blob any
	if json.Unmarshal([]byte(out), &blob) == nil {
		return extractIPv4(blob)
	}
	if m := regexp.MustCompile(`(?:\d{1,3}\.){3}\d{1,3}`).FindString(out); m != "" {
		return m
	}
	return ""
}

func waitTCP(ip string, port uint32, timeout time.Duration, label string) {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(ip, fmt.Sprint(port))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 1500*time.Millisecond)
		if err == nil {
			conn.Close()
			okay("%s is accepting connections on :%d", label, port)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	warn("%s: no TCP answer on %s after %s — continuing anyway", label, addr, timeout)
}

// ---------- shared plumbing -----------------------------------------------------------

func cnameOf(p *types.Project, name string) string {
	if cn := p.Services[name].ContainerName; cn != "" {
		return cn
	}
	return p.Name + "-" + name
}

// lsLineRunning reports whether `container ls --all` output shows cname on a
// line whose STATE is running — same tolerant text matching as collectState,
// so we don't depend on the (still-shifting) ls JSON schema.
// lsLineFor returns the `container ls` line whose ID column (first field) is
// exactly cname — a plain substring match would let "proj-app" claim a
// "proj-app2" line.
func lsLineFor(lsOut, cname string) string {
	for _, line := range strings.Split(lsOut, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == cname {
			return line
		}
	}
	return ""
}

func lsLineRunning(lsOut, cname string) bool {
	return strings.Contains(strings.ToLower(lsLineFor(lsOut, cname)), "running")
}

func envKey(name string) string {
	k := regexp.MustCompile(`[^A-Z0-9]`).ReplaceAllString(strings.ToUpper(name), "_")
	if k != "" && k[0] >= '0' && k[0] <= '9' {
		k = "_" + k // "2cool" must not become the invalid var name 2COOL_HOST
	}
	return k + "_HOST"
}

// hostsWarned dedupes the per-service /etc/hosts warning — a shell-less
// image would otherwise warn once per injection (N times on a big stack).
var hostsWarned = map[string]bool{}

func wireHosts(r runner, cname string, pairs [][2]string, svc string) {
	if len(pairs) == 0 {
		return
	}
	// Tolerate the two benign, well-understood failure modes so they get one
	// dim warning instead of a loud "command failed" + raw runtime error.
	ok, msg := r.run(hostsInjectCmd(cname, pairs), "permission denied", "failed to find target executable")
	if ok || r.dry || hostsWarned[svc] {
		return
	}
	hostsWarned[svc] = true
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "failed to find target executable"):
		warn("[%s] image has no shell — service-name DNS unavailable; peers can use the <SERVICE>_HOST env vars", svc)
	case strings.Contains(low, "permission denied"):
		warn("[%s] /etc/hosts not writable (image runs as non-root) — use the <SERVICE>_HOST env vars", svc)
	default:
		warn("[%s] could not write /etc/hosts — use the <SERVICE>_HOST env vars instead", svc)
	}
}

// ---------- subcommands ------------------------------------------------------------------

func cmdUp(p *types.Project, r runner, publish bool, waitTimeout time.Duration) {
	network := p.Name + "-net"
	info("project %s%s%s  (%d services)  network %s", bold, p.Name, reset, len(p.Services), network)
	r.run(ctr("network", "create", network), "exist")

	order := toposort(p)
	info("start order: %s", strings.Join(order, " \u2192 "))

	for _, vn := range namedVolumes(p) {
		r.run(ctr("volume", "create", vn), "exist")
	}

	ips := map[string]string{}
	var started []string

	for _, name := range order {
		svc := p.Services[name]
		warnUnsupported(name, svc)
		cname := cnameOf(p, name)

		// honour service_healthy via TCP polling before starting the dependent
		for dep, cfg := range svc.DependsOn {
			if cfg.Condition == types.ServiceConditionHealthy && !r.dry {
				if ip := ips[dep]; ip != "" && len(p.Services[dep].Ports) > 0 {
					port := p.Services[dep].Ports[0].Target
					info("waiting for %s (service_healthy \u2192 TCP :%d, max %s)", dep, port, waitTimeout)
					waitTCP(ip, port, waitTimeout, dep)
				} else {
					warn("[%s] cannot health-wait on '%s' (no IP/port known) — starting anyway", name, dep)
				}
			}
		}

		image := svc.Image
		if svc.Build != nil {
			if image == "" {
				image = p.Name + "-" + name
			}
			fmt.Printf("%sbuild%s %s\n", bold, reset, name)
			if ok, _ := r.run(buildCmd(image, svc, p.WorkingDir)); !ok && !r.dry {
				os.Exit(1)
			}
		} else if image == "" {
			fail("service '%s' has neither image nor build", name)
			os.Exit(1)
		}

		// <DEP>_HOST fallback env vars — always injected (shell-less image safety net)
		extra := map[string]string{}
		for dep := range svc.DependsOn {
			if ip := ips[dep]; ip != "" {
				extra[envKey(dep)] = ip
			}
		}

		fmt.Printf("%srun%s   %s  %s(%s)%s\n", bold, reset, name, dim, cname, reset)
		if ok, msg := r.run(runCmd(p, cname, network, image, svc, extra, publish), "already exists"); !ok && !r.dry {
			// idempotent up, like docker-compose: an existing container is
			// started (no-op if it's already running), not a fatal error
			if strings.Contains(strings.ToLower(msg), "already exists") {
				info("[%s] container exists — starting it", name)
				if started, smsg := r.run(ctr("start", cname), "running", "started"); !started &&
					!strings.Contains(strings.ToLower(smsg), "running") {
					fail("[%s] failed to start existing container — `down` to clean, then `up` again", name)
					os.Exit(1)
				}
			} else {
				fail("[%s] failed to start — aborting (already-started services keep running; `down` to clean)", name)
				os.Exit(1)
			}
		}

		ips[name] = getIP(r, cname)
		if ips[name] == "" && !r.dry {
			warn("[%s] could not determine IP — service-name DNS for it will be missing", name)
		}

		// RACE FIX: wire hosts NOW, bidirectionally — but only when there are
		// peers to reach; a single-service stack has nobody to talk to, and
		// exec-ing into it just risks noise (shell-less images, non-root).
		if len(order) > 1 {
			known := [][2]string{}
			for _, s := range append(append([]string{}, started...), name) {
				if ips[s] != "" {
					known = append(known, [2]string{s, ips[s]})
				}
			}
			wireHosts(r, cname, known, name)
			if ips[name] != "" {
				for _, prev := range started {
					wireHosts(r, cnameOf(p, prev), [][2]string{{name, ips[name]}}, prev)
				}
			}
		}
		started = append(started, name)
	}

	// verify the services actually stayed up — a container can exit right
	// after `run` succeeds (bad command, crash on boot), and "stack up" lied.
	notRunning := map[string]bool{}
	if !r.dry {
		if ok, lsOut := r.run(ctr("ls", "--all")); ok {
			for _, name := range order {
				if !lsLineRunning(lsOut, cnameOf(p, name)) {
					notRunning[name] = true
				}
			}
		}
	}

	fmt.Println()
	if len(notRunning) > 0 {
		warn("%d service(s) not running — check: acompose logs <svc>", len(notRunning))
		// the classic case: nginx-style apps resolve peer names while booting,
		// before our /etc/hosts injection can land (and the runtime regenerates
		// that file on every boot, so a plain restart cannot win the race)
		fmt.Fprintf(os.Stderr, "  %sif it resolves peer names at startup (e.g. nginx proxy_pass), use the <SERVICE>_HOST env vars — see 'Reverse proxy' in the README%s\n", dim, reset)
	} else {
		okay("stack up")
	}
	width := 0
	for _, n := range order {
		if len(n) > width {
			width = len(n)
		}
	}
	for _, name := range order {
		var shown []string
		if publish {
			for _, prt := range p.Services[name].Ports {
				if prt.Published != "" {
					shown = append(shown, "localhost:"+prt.Published)
				}
			}
		}
		tail := ""
		if len(shown) > 0 {
			tail = fmt.Sprintf("  %s%s%s", dim, strings.Join(shown, ", "), reset)
		}
		ip := ips[name]
		if ip == "" {
			ip = "?"
		}
		state := ""
		if !r.dry { // dry-run started nothing; a state column would be a lie
			state = green + "✓ running" + reset + "  "
			if notRunning[name] {
				state = red + "✗ stopped" + reset + "  "
			}
		}
		fmt.Printf("  %-*s  %s%s%s%s%s\n", width, name, state, green, ip, reset, tail)
	}
	// discoverability: when the project's local DNS domain exists, every
	// service is reachable from the HOST as <cname>.<domain> — say so. One
	// probe total (dnsDomainListed caches nothing per service), and never in
	// dry-run: dry prints each runner call, and the probe would pollute the
	// asserted command transcript. No domain → no line (no nagging).
	if !r.dry {
		if listed, ok := dnsDomainListed(r, dnsDomain(p)); ok && listed {
			fmt.Printf("  %shost DNS: %s%s\n", dim, strings.Join(dnsHostNames(p, dnsDomain(p)), ", "), reset)
		}
	}
	fmt.Printf("\n%scontainers reach each other by service name via /etc/hosts; <SERVICE>_HOST env vars are the fallback for shell-less images%s\n", dim, reset)
	fmt.Printf("%safter sleep/wake or restarts, run: acompose refresh%s\n", dim, reset)
}

func cmdRefresh(p *types.Project, r runner) {
	info("re-reading IPs for %s%s%s and rewriting /etc/hosts entries", bold, p.Name, reset)
	rewireAll(p, r)
	okay("refreshed")
}

// rewireAll re-reads every service's IP, scrubs stale service-name lines from
// each container's /etc/hosts, and re-injects the full current set.
func rewireAll(p *types.Project, r runner) {
	names := toposort(p)
	if len(names) < 2 {
		return // no peers — nothing to wire
	}
	ips := map[string]string{}
	for _, name := range names {
		if ip := getIP(r, cnameOf(p, name)); ip != "" {
			ips[name] = ip
		} else {
			warn("[%s] no IP (not running?)", name)
		}
	}
	var pairs [][2]string
	var escaped []string
	for _, name := range names {
		if ips[name] != "" {
			pairs = append(pairs, [2]string{name, ips[name]})
			escaped = append(escaped, regexp.QuoteMeta(name))
		}
	}
	pattern := strings.Join(escaped, "|")
	if pattern == "" {
		pattern = "NOMATCH"
	}
	for _, name := range names {
		cname := cnameOf(p, name)
		cleanup := fmt.Sprintf(`grep -vE '\s(%s)$' /etc/hosts > /tmp/h && cat /tmp/h > /etc/hosts`, pattern)
		// benign on shell-less / non-root images; wireHosts warns once below
		r.run(ctr("exec", cname, "sh", "-c", cleanup), "failed to find target executable", "permission denied")
		wireHosts(r, cname, pairs, name)
	}
}

func cmdDown(p *types.Project, r runner, removeVolumes bool) {
	info("tearing down %s%s%s", bold, p.Name, reset)
	order := toposort(p)
	for i := len(order) - 1; i >= 0; i-- {
		cname := cnameOf(p, order[i])
		r.run(ctr("stop", cname), "no")
		r.run(ctr("delete", cname), "no")
		fmt.Printf("  removed %s\n", order[i])
	}
	r.run(ctr("network", "delete", p.Name+"-net"), "no")
	if vols := namedVolumes(p); len(vols) > 0 {
		if removeVolumes {
			for _, vn := range vols {
				if ok, _ := r.run(ctr("volume", "delete", vn), "no"); ok {
					fmt.Printf("  removed volume %s\n", vn)
				}
			}
		} else {
			fmt.Printf("  %snamed volumes kept (%s) — `down -v` removes them%s\n", dim, strings.Join(vols, ", "), reset)
		}
	}
	okay("down")
}

// recreateService creates a service's container from scratch the way `up`
// would: network and named volumes ensured, <DEP>_HOST env injected, then the
// same runCmd path. It does NOT touch /etc/hosts wiring — callers rewire
// afterwards (shared by ensureServiceRunning and `dev`'s rebuild action).
func recreateService(p *types.Project, r runner, name string, publish bool) (bool, string) {
	svc, exists := p.Services[name]
	if !exists {
		return false, "unknown service " + name
	}
	image := svc.Image
	if image == "" {
		if svc.Build == nil {
			return false, "service has neither image nor build"
		}
		image = p.Name + "-" + name
	}
	r.run(ctr("network", "create", p.Name+"-net"), "exist")
	for _, v := range svc.Volumes {
		if v.Type == "volume" && v.Source != "" {
			if cfg, ok := p.Volumes[v.Source]; !ok || !bool(cfg.External) {
				r.run(ctr("volume", "create", volName(p, v.Source)), "exist")
			}
		}
	}
	extra := map[string]string{}
	for dep := range svc.DependsOn {
		if ip := getIP(r, cnameOf(p, dep)); ip != "" {
			extra[envKey(dep)] = ip
		}
	}
	if ok, msg := r.run(runCmd(p, cnameOf(p, name), p.Name+"-net", image, svc, extra, publish)); !ok {
		return false, msg
	}
	return true, "recreated"
}

// ensureServiceRunning makes a single service run regardless of its current
// state: a stopped container is started; a missing one (deleted, or never
// created) is recreated through recreateService. On success the project's
// /etc/hosts wiring is refreshed so peers see the (possibly new) IP.
func ensureServiceRunning(p *types.Project, r runner, name string, publish bool) (bool, string) {
	if _, exists := p.Services[name]; !exists {
		return false, "unknown service " + name
	}
	cname := cnameOf(p, name)

	if ok, _ := r.run(ctr("start", cname), "not found", "no such", "already running", "exist"); ok {
		if len(p.Services) > 1 {
			rewireAll(p, r)
		}
		return true, "started"
	}

	// nothing to start — recreate it the way `up` would
	ok, msg := recreateService(p, r, name, publish)
	if ok && len(p.Services) > 1 {
		rewireAll(p, r)
	}
	return ok, msg
}

// cmdWatch supervises restart: policies the runtime itself does not enforce
// (autoheal): poll `container ls --all`, restart anything supervised that is
// down, then rewire /etc/hosts so peers see the restarted service's new IP.
func cmdWatch(p *types.Project, r runner, interval time.Duration) {
	if r.dry {
		fail("watch needs a live runtime — it cannot be combined with --dry-run")
		os.Exit(2)
	}
	order := toposort(p)
	var supervised []string
	for _, name := range order {
		rs := p.Services[name].Restart
		if rs == "always" || rs == "unless-stopped" || strings.HasPrefix(rs, "on-failure") {
			supervised = append(supervised, name)
		}
	}
	if len(supervised) == 0 {
		info("no service declares a restart: policy — supervising all services")
		supervised = order
	}
	info("supervising: %s  (poll every %s, Ctrl-C to quit)", strings.Join(supervised, ", "), interval)
	for {
		time.Sleep(interval)
		ok, lsOut := r.run(ctr("ls", "--all"))
		if !ok {
			continue
		}
		rewire := false
		for _, name := range supervised {
			cname := cnameOf(p, name)
			if lsLineRunning(lsOut, cname) {
				continue
			}
			info("[%s] not running — restarting %s", name, cname)
			if ok, _ := r.run(ctr("start", cname)); !ok {
				continue
			}
			if ip := getIP(r, cname); ip != "" {
				okay("[%s] restarted, IP %s", name, ip)
				rewire = true
			} else {
				warn("[%s] restarted but no IP yet — run 'acompose refresh' once it settles", name)
			}
		}
		if rewire {
			rewireAll(p, r)
		}
	}
}

// Only "digest" keys identify the tag's manifest/index; matching every sha256
// in the JSON would also catch layer blobs, which grow when an explicit pull
// fetches more of the multi-arch index than `run`'s implicit pull did.
var digestKeyRE = regexp.MustCompile(`"digest"\s*:\s*"(sha256:[0-9a-fA-F]+)"`)

// imageDigests inspects an image and returns the set of manifest digests in
// the JSON; ok is false when the inspect failed or yielded no digest (image
// not present locally).
func imageDigests(r runner, image string) (map[string]bool, bool) {
	ok, out := r.run(ctr("image", "inspect", image), "no")
	if !ok {
		return nil, false
	}
	set := map[string]bool{}
	for _, m := range digestKeyRE.FindAllStringSubmatch(out, -1) {
		set[m[1]] = true
	}
	return set, len(set) > 0
}

func sameDigests(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for d := range a {
		if !b[d] {
			return false
		}
	}
	return true
}

// cmdUpdate pulls newer images (or rebuilds built ones) and recreates the
// services whose digests changed (dockcheck), then rewires /etc/hosts.
func cmdUpdate(p *types.Project, r runner, publish bool) {
	network := p.Name + "-net"
	order := toposort(p)
	info("checking %d service(s) of %s%s%s for updates", len(order), bold, p.Name, reset)

	ips := map[string]string{}
	if !r.dry {
		for _, name := range order {
			ips[name] = getIP(r, cnameOf(p, name))
		}
	}

	var updated, current []string
	for _, name := range order {
		svc := p.Services[name]
		cname := cnameOf(p, name)
		image := svc.Image
		changed := false

		if svc.Build != nil {
			if image == "" {
				image = p.Name + "-" + name
			}
			fmt.Printf("%sbuild%s %s\n", bold, reset, name)
			ok, _ := r.run(buildCmd(image, svc, p.WorkingDir))
			changed = ok
		} else {
			if image == "" {
				fail("service '%s' has neither image nor build", name)
				os.Exit(1)
			}
			if r.dry {
				r.run(ctr("image", "pull", image))
				changed = true // digest comparison is impossible dry — show the full recreate
			} else {
				before, hadLocal := imageDigests(r, image)
				pullOK, _ := r.run(ctr("image", "pull", image))
				switch {
				case !pullOK:
					changed = false
				case !hadLocal:
					changed = true // image was missing locally; the pull fetched it
				default:
					after, ok := imageDigests(r, image)
					changed = ok && !sameDigests(before, after)
				}
			}
		}

		if !changed {
			current = append(current, name)
			continue
		}
		updated = append(updated, name)

		fmt.Printf("%srecreate%s %s  %s(%s)%s\n", bold, reset, name, dim, cname, reset)
		r.run(ctr("stop", cname), "no")
		r.run(ctr("delete", cname), "no")

		extra := map[string]string{}
		for dep := range svc.DependsOn {
			if ip := ips[dep]; ip != "" {
				extra[envKey(dep)] = ip
			}
		}
		if ok, _ := r.run(runCmd(p, cname, network, image, svc, extra, publish)); !ok && !r.dry {
			fail("[%s] failed to start after update — check: acompose logs %s", name, name)
			continue
		}
		ips[name] = getIP(r, cname)
	}

	if len(updated) > 0 && !r.dry {
		rewireAll(p, r)
	}
	fmt.Println()
	if len(updated) > 0 {
		okay("updated: %s", strings.Join(updated, ", "))
	} else {
		okay("everything already current")
	}
	if len(current) > 0 {
		fmt.Printf("  %salready current: %s%s\n", dim, strings.Join(current, ", "), reset)
	}
}

func cmdPs(p *types.Project, r runner) {
	ok, out := r.run(ctr("ls", "--all"))
	if !ok {
		return
	}
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return
	}
	// match by exact container name, not substring — covers container_name
	// overrides and won't pick up other projects' lines by accident
	mine := map[string]bool{}
	for name := range p.Services {
		mine[cnameOf(p, name)] = true
	}
	fmt.Println(lines[0])
	for _, line := range lines[1:] {
		if f := strings.Fields(line); len(f) > 0 && mine[f[0]] {
			fmt.Println(line)
		}
	}
}

// startStopRun powers `acompose start|stop SERVICE` — the single-service
// counterpart of up/down (also what the menubar plugin's per-service actions
// invoke). start works from any state via ensureServiceRunning: a stopped
// container is started, a missing one recreated the way `up` would. Returns
// the process exit code: 0 ok, 1 failure/unknown service, 2 usage.
func startStopRun(p *types.Project, r runner, verb string, args []string, publish bool) int {
	if len(args) < 1 {
		fail("usage: acompose %s SERVICE", verb)
		return 2
	}
	name := args[0]
	if _, ok := p.Services[name]; !ok {
		fail("unknown service '%s' — services: %s", name, strings.Join(toposort(p), ", "))
		return 1
	}
	if verb == "start" {
		ok, msg := ensureServiceRunning(p, r, name, publish)
		if !ok {
			fail("[%s] %s", name, msg)
			return 1
		}
		okay("%s %s", name, msg)
		return 0
	}
	if ok, _ := r.run(ctr("stop", cnameOf(p, name))); !ok {
		return 1 // runner already printed the loud error
	}
	okay("%s stopped", name)
	return 0
}

func passthrough(args []string) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run()
}

// ---------- main -----------------------------------------------------------------------------

// version is stamped by the linker on release builds (see .goreleaser.yaml
// and the Makefile); "dev" means a local, untagged build.
var version = "dev"

// demoCompose is what `acompose init` scaffolds: the smallest stack that
// shows something in a browser. whoami prints the container's own IP —
// exactly the thing acompose is built around.
const demoCompose = `# a minimal stack to try acompose
#
#   acompose up        then open http://localhost:8080
#   acompose down      when you're done
#
services:
  hello:
    image: traefik/whoami # tiny multi-arch demo server; the page shows the container's IP
    ports:
      - "8080:80"

  # add more services and acompose wires service-name DNS between them, e.g.:
  #
  #   db:
  #     image: postgres:16
  #     environment:
  #       POSTGRES_PASSWORD: devpass
  #   app:
  #     image: your-app
  #     depends_on: [db]   # app reaches it at db:5432
`

var defaultComposeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

func cmdInit() {
	for _, n := range defaultComposeNames {
		if _, err := os.Stat(n); err == nil {
			fail("%s already exists here — refusing to overwrite", n)
			os.Exit(1)
		}
	}
	if err := os.WriteFile("docker-compose.yml", []byte(demoCompose), 0o644); err != nil {
		fail("%v", err)
		os.Exit(1)
	}
	okay("wrote docker-compose.yml (a minimal demo stack)")
	fmt.Printf("\n  %sacompose up%s        start it, then open %shttp://localhost:8080%s\n", bold, reset, cyan, reset)
	fmt.Printf("  %sacompose down%s      tear it down\n", bold, reset)
}

// parseIntervalArg validates a --interval value: a whole number of seconds,
// at least 1 — fmt.Sscanf-style leniency silently accepted garbage and
// 0/negative values, turning the watch/dev poll loops into busy loops.
func parseIntervalArg(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("--interval needs a whole number of seconds, got %q", s)
	}
	if n < 1 {
		return 0, fmt.Errorf("--interval must be at least 1 second, got %d", n)
	}
	return n, nil
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}
	sub := args[0]
	if sub == "version" || sub == "--version" || sub == "-V" {
		fmt.Printf("acompose %s\n", version)
		return
	}
	if sub == "init" {
		cmdInit()
		return
	}
	if sub == "doctor" { // environment check — must work without a compose file
		cmdDoctor()
		return
	}
	if sub == "help" || sub == "-h" || sub == "--help" {
		// explicit help is success and goes to stdout (so `acompose help | less`
		// works); a wrong invocation still hits usage() → stderr + exit 2.
		// `acompose help <command>` prints that command's detail.
		topic := ""
		if len(args) > 1 {
			topic = args[1]
		}
		printHelp(os.Stdout, topic)
		return
	}
	rest := args[1:]

	var files []string
	var project string
	dry, noPublish, follow := false, false, false
	removeVolumes := false
	waitTimeout := 30 * time.Second
	intervalSec, intervalSet := 10, false
	var positional []string

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--file":
			i++
			files = append(files, rest[i])
		case "-p", "--project":
			i++
			project = rest[i]
		case "--dry-run":
			dry = true
		case "--no-publish":
			noPublish = true
		case "-f", "--follow":
			follow = true
		case "-d", "--detach": // accepted for docker-compose muscle memory
		case "-v", "--volumes":
			removeVolumes = true
		case "--wait-timeout":
			i++
			fmt.Sscanf(rest[i], "%d", &waitTimeout)
			waitTimeout *= time.Second / time.Duration(1)
		case "--interval":
			i++
			n, err := parseIntervalArg(rest[i])
			if err != nil {
				fail("%v", err)
				os.Exit(2)
			}
			intervalSec, intervalSet = n, true
		case "--":
			positional = append(positional, rest[i+1:]...)
			i = len(rest)
		default:
			positional = append(positional, rest[i])
		}
	}

	p := loadProject(files, project)
	r := runner{dry: dry}

	switch sub {
	case "up":
		cmdUp(p, r, !noPublish, waitTimeout)
	case "check": // exit 0 = no blockers, 1 = at least one blocker
		cmdCheck(p)
	case "down":
		cmdDown(p, r, removeVolumes)
	case "refresh":
		cmdRefresh(p, r)
	case "start", "stop":
		if code := startStopRun(p, r, sub, positional, !noPublish); code != 0 {
			os.Exit(code)
		}
	case "menubar":
		if code := menubarRun(p); code != 0 {
			os.Exit(code)
		}
	case "watch":
		cmdWatch(p, r, time.Duration(intervalSec)*time.Second)
	case "dev":
		// dev polls fast by default (1s — it's an inner-loop tool); an
		// explicit --interval overrides, sharing watch's flag.
		devInterval := time.Second
		if intervalSet {
			devInterval = time.Duration(intervalSec) * time.Second
		}
		cmdDev(p, r, positional, devInterval, !noPublish)
	case "update":
		cmdUpdate(p, r, !noPublish)
	case "stats":
		var cnames []string
		for _, name := range toposort(p) {
			cnames = append(cnames, cnameOf(p, name))
		}
		passthrough(append(ctr("stats"), cnames...))
	case "ps":
		cmdPs(p, r)
	case "import-volumes":
		cmdImportVolumes(p, r, positional)
	case "dns":
		dnsSub := ""
		if len(positional) > 0 {
			dnsSub = positional[0]
		}
		cmdDNS(p, r, dnsSub)
	case "top", "tui":
		cmdTop(p)
	case "ui":
		addr := "127.0.0.1:4242"
		explicit := len(positional) > 0
		if explicit {
			addr = positional[0]
		}
		cmdUI(p, addr, explicit)
	case "build":
		for _, name := range toposort(p) {
			svc := p.Services[name]
			if svc.Build != nil {
				img := svc.Image
				if img == "" {
					img = p.Name + "-" + name
				}
				fmt.Printf("%sbuild%s %s\n", bold, reset, name)
				r.run(buildCmd(img, svc, p.WorkingDir))
			}
		}
	case "logs":
		if len(positional) < 1 {
			usage()
		}
		cmd := ctr("logs")
		if follow {
			cmd = append(cmd, "--follow")
		}
		passthrough(append(cmd, cnameOf(p, positional[0])))
	case "exec":
		if len(positional) < 2 {
			usage()
		}
		passthrough(append(ctr("exec", "--tty", "--interactive", cnameOf(p, positional[0])), positional[1:]...))
	default:
		usage()
	}
}

// printUsage writes the grouped command overview. b/c/d/etc. color vars are
// empty under non-TTY (and in tests), so this is plain there.
func printUsage(w io.Writer) {
	h := func(s string) string { return bold + s + reset }
	fmt.Fprintf(w, `%s — run your docker-compose.yml on Apple's container CLI  (%s)

%s
  acompose init             scaffold a demo stack to try it out
  acompose up               start your stack          %sthen: acompose ui%s
  acompose down [-v]         stop it  %s(-v also removes named volumes)%s

%s
  up     [--file F]... [-p NAME] [--dry-run] [--no-publish] [--wait-timeout S]
  down   [--file F]... [-p NAME] [--dry-run] [-v]
  build                     build images only, no run
  update [--dry-run]        pull newer images, recreate only what changed
  start|stop SERVICE        act on a single service
  refresh                   re-grab IPs + rewrite /etc/hosts after sleep/wake

%s
  ps                        list this project's containers
  stats                     live resource usage
  logs   SERVICE [-f]       container logs (-f to follow)
  exec   SERVICE -- CMD     run a command inside a service
  top                       interactive terminal dashboard (lazydocker-style)
  ui     [ADDR]             live web dashboard (default 127.0.0.1:4242)
  menubar                   SwiftBar/xbar plugin output (contrib/swiftbar)
  check                     compatibility report — will this file translate?

%s
  dev    [SERVICE...]       hot reload via develop.watch (sync/rebuild/restart)
  watch  [--interval S]     supervise restart: policies (autoheal)

%s
  dns    [setup|status|teardown]    host DNS names via container system dns

%s
  doctor                    check this machine is ready
  import-volumes [VOL...]   copy named-volume data from Docker/OrbStack

  version          %sprint the version%s
  help [COMMAND]   %sthis screen, or detail for one command%s

%sDocs%s https://github.com/htlin222/acompose
`,
		h("acompose"), version,
		h("QUICK START"), dim, reset, dim, reset,
		h("RUN A STACK"),
		h("INSPECT"),
		h("DEVELOP"),
		h("NETWORKING"),
		h("MIGRATE FROM DOCKER / ORBSTACK"),
		dim, reset, dim, reset,
		dim, reset)
}

// commandHelp holds the per-command detail printed by `acompose help <cmd>`.
// Commands not listed fall back to the overview.
var commandHelp = map[string]string{
	"up": `acompose up — start the stack

  acompose up [--file F]... [-p NAME] [--dry-run] [--no-publish] [--wait-timeout S]

Starts every service in depends_on order. service_healthy conditions are
approximated by TCP-polling the dependency's first published port. Each
container's real IP is wired into every peer's /etc/hosts (and <SERVICE>_HOST
env vars are injected as a fallback for shell-less images).

  --file F          a compose file to use (repeatable); overrides auto-discovery
  -p NAME           project name (default: the directory name)
  --dry-run         print the exact 'container' commands without running them
  --no-publish      don't publish ports: to the host
  --wait-timeout S  seconds to wait on a service_healthy dependency (default 30)

  up is idempotent: an already-created container is started, not re-created.`,

	"down": `acompose down — stop and remove the stack

  acompose down [--file F]... [-p NAME] [--dry-run] [-v]

Tears down in reverse dependency order and removes the project network.

  -v   also delete named volumes (otherwise they are kept; data survives)`,

	"dev": `acompose dev — hot reload from develop.watch

  acompose dev [SERVICE...] [--interval S]

Watches the paths in each service's develop.watch section and reacts:
  sync          copy changed files into the running container
  rebuild       rebuild the image and recreate the container
  restart       restart the container
  sync+restart  copy, then restart

A polling watcher (no fsnotify dependency). .git / node_modules / .DS_Store
are always ignored; trigger 'ignore:' patterns add to that. --interval sets
the poll period in seconds (default 1). Refuses --dry-run.`,

	"dns": `acompose dns — host-side DNS names for your services

  acompose dns [status|setup|teardown]

Wraps the runtime's native 'container system dns' so a service is reachable
from your browser at <container-name>.<project>. setup needs admin once
(it prints the exact 'sudo container system dns create ...' if so).

  status     (default) show whether the project domain is configured + names
  setup      create the local DNS domain for this project
  teardown   remove it`,

	"check": `acompose check — compatibility report (no changes made)

  acompose check [--file F]... [-p NAME]

Reads your compose file and reports, per service, what translates cleanly,
what is approximated, and what would block — without touching the runtime.
Great for deciding whether a project can move over before you run anything.
Exit 0 when there are no blockers, 1 when there are.`,

	"import-volumes": `acompose import-volumes — bring your data across

  acompose import-volumes [VOLUME-KEY...] [--dry-run]

Copies named-volume DATA from Docker/OrbStack into Apple container volumes
(docker tars the volume, container untars it). Docker must still be installed
when you run this. With no args, every named volume is migrated; pass keys to
limit it. Refuses to overwrite a non-empty target volume.`,

	"top": `acompose top — interactive terminal dashboard

  acompose top   (alias: acompose tui)

A lazydocker-style TUI for the running stack, in your terminal. Each service is
a row with a status dot, its real IP (or state), and published ports; the
selected row is highlighted. Reuses the same data the web 'ui' does.

  ↑/↓ (or k/j)   move the selection
  s              toggle the selected service: running → stop, else → start
  l / enter      focus the logs pane for the selected service (esc/h to go back)
  r              refresh state now
  q / ctrl+c     quit

State refreshes every 2s; in the logs pane the last 200 lines refresh every 2s
and ↑/↓ pgup/pgdn scroll. Needs an interactive terminal — in a pipe or CI it
exits with a hint to use 'acompose ps' / 'acompose ui' instead.`,

	"ui": `acompose ui — live web dashboard

  acompose ui [ADDR]

Serves a dashboard (default 127.0.0.1:4242): every service as a card with its
real IP, status, ports, logs, and start/stop. Binding a non-loopback ADDR
exposes an unauthenticated control API and is warned about — keep it on
localhost.`,

	"doctor": `acompose doctor — is this machine ready?

  acompose doctor

Checks architecture, macOS version, the container CLI and its version against
the tested one, the system service, and whether a compose file is present.
Works without a compose file. Exit 1 if anything is broken.`,
}

// printHelp prints the overview, or one command's detail for `help <cmd>`.
func printHelp(w io.Writer, topic string) {
	if topic == "" {
		printUsage(w)
		return
	}
	topic = strings.TrimLeft(topic, "-")
	if detail, ok := commandHelp[topic]; ok {
		fmt.Fprintln(w, detail)
		return
	}
	// unknown or detail-less command — point back at the overview
	fmt.Fprintf(w, "no extra detail for %q; here's the overview:\n\n", topic)
	printUsage(w)
}

func usage() {
	printUsage(os.Stderr)
	os.Exit(2)
}
