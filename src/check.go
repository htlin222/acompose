package main

// `acompose check` — compose-file compatibility report.
//
// Someone on Docker/OrbStack points acompose at their existing compose file
// and learns — WITHOUT running anything — exactly how well it will translate.
// It never touches the runtime (no ctr() calls), so it works on any machine,
// including a Linux box evaluating a file.
//
// Exit codes: 0 = no blockers, 1 = at least one blocker finding — the
// suite-wide convention (doctor, import-volumes, dns failures) of 1 for
// "found problems"; 2 stays reserved for usage errors and refusals.
//
// analyzeService below is the single source of truth for the per-service
// platform-gap analysis; warnUnsupported (main.go) renders the same findings
// as `up`-time warnings.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

type findingLevel int

const (
	// works, but through a different mechanism than Docker's
	levelApproximated findingLevel = iota
	// lost in translation; the app may still work without it
	levelIgnored
	// likely to fail at runtime
	levelBlocker
)

// finding is one detected gap between the compose file and what acompose can
// honour on Apple's container runtime.
type finding struct {
	level   findingLevel
	feature string // e.g. "healthcheck", "restart 'always'", "deploy.replicas", "platform linux/amd64"
	detail  string // human sentence shown by `acompose check`
	// warnText is the exact legacy message warnUnsupported emits during `up`
	// (tests and CI grep for these). Empty means `up` handles it at the point
	// of use instead (runCmd warns per anonymous mount, cmdUp fails hard on a
	// missing image) — warnUnsupported must not duplicate those.
	warnText string
}

// analyzeService inspects one service and returns every finding, in a fixed
// presentation order. The deploy.* findings keep the legacy part order so
// warnUnsupported can aggregate them into today's exact warning text.
func analyzeService(name string, s types.ServiceConfig) []finding {
	_ = name // findings are service-relative; the name is the caller's framing
	var fs []finding

	if s.Image == "" && s.Build == nil {
		fs = append(fs, finding{levelBlocker, "image",
			"service has neither image nor build — nothing to run", ""})
	}
	if s.HealthCheck != nil && !s.HealthCheck.Disable {
		fs = append(fs, finding{levelApproximated, "healthcheck",
			"approximated by TCP polling on the first published port",
			"exec-style healthcheck ignored — service_healthy is approximated by TCP polling"})
	}
	if s.Restart != "" {
		fs = append(fs, finding{levelApproximated, fmt.Sprintf("restart '%s'", s.Restart),
			"not enforced by the runtime — `acompose watch` supervises from the outside",
			fmt.Sprintf("restart: '%s' not enforced by the runtime — run 'acompose watch' to supervise", s.Restart)})
	}
	if d := s.Deploy; d != nil {
		// deploy.resources.limits (cpus, memory) IS translated by runCmd —
		// only the parts the platform cannot honour become findings.
		ignored := func(part string) {
			fs = append(fs, finding{levelIgnored, "deploy." + part,
				"ignored — only resources.limits (cpus, memory) are applied", ""})
		}
		if d.Mode != "" && d.Mode != "replicated" {
			ignored("mode")
		}
		if d.Replicas != nil && *d.Replicas != 1 {
			ignored("replicas")
		}
		if len(d.Labels) > 0 {
			ignored("labels")
		}
		if d.UpdateConfig != nil {
			ignored("update_config")
		}
		if d.RollbackConfig != nil {
			ignored("rollback_config")
		}
		if d.Resources.Reservations != nil {
			ignored("resources.reservations")
		}
		if l := d.Resources.Limits; l != nil && (l.Pids != 0 || len(l.Devices) > 0 || len(l.GenericResources) > 0) {
			ignored("resources.limits.pids/devices")
		}
		if d.RestartPolicy != nil {
			ignored("restart_policy")
		}
		if len(d.Placement.Constraints) > 0 || len(d.Placement.Preferences) > 0 || d.Placement.MaxReplicas != 0 {
			ignored("placement")
		}
		if d.EndpointMode != "" {
			ignored("endpoint_mode")
		}
	}
	if len(s.Secrets) > 0 || len(s.Configs) > 0 {
		fs = append(fs, finding{levelIgnored, "secrets/configs",
			"ignored — not mounted",
			"secrets/configs ignored — not mounted"})
	}
	if len(s.Entrypoint) > 0 {
		fs = append(fs, finding{levelIgnored, "entrypoint",
			"ignored — override via command: instead",
			"entrypoint: ignored — override via command: instead"})
	}
	if s.User != "" {
		fs = append(fs, finding{levelIgnored, "user",
			"ignored — runs as the image's default user",
			"user: ignored — runs as the image's default user"})
	}
	for _, m := range s.Volumes {
		if m.Type == "volume" && m.Source == "" {
			fs = append(fs, finding{levelIgnored, "volumes",
				fmt.Sprintf("anonymous volume on '%s' — skipped (name it in the compose file)", m.Target), ""})
		}
	}
	if custom := customNetworks(serviceNetworkKeys(s)); len(custom) > 0 {
		fs = append(fs, finding{levelIgnored, "networks",
			"custom networks — acompose puts every service on one shared project network",
			fmt.Sprintf("custom networks (%s) ignored — every service joins one shared project network", strings.Join(custom, ", "))})
	}
	if pf := s.Platform; strings.Contains(pf, "amd64") || strings.Contains(pf, "x86") {
		fs = append(fs, finding{levelBlocker, "platform " + pf,
			"x86 images are not seamless on this runtime — may fail to run",
			fmt.Sprintf("platform '%s': x86 images are NOT seamless on Apple container — may fail to run", pf)})
	}
	return fs
}

func serviceNetworkKeys(s types.ServiceConfig) []string {
	keys := make([]string, 0, len(s.Networks))
	for n := range s.Networks {
		keys = append(keys, n)
	}
	return keys
}

// customNetworks filters the implicit "default" network out and sorts what
// remains — compose-go's normalization names the implicit network "default"
// at both the service and the project level.
func customNetworks(keys []string) []string {
	var custom []string
	for _, n := range keys {
		if n != "default" {
			custom = append(custom, n)
		}
	}
	sort.Strings(custom)
	return custom
}

// analyzeProject returns project-level findings (the "project" pseudo-entry):
// top-level networks beyond the default are flattened onto one network.
// External top-level volumes are fully supported — not a finding.
func analyzeProject(p *types.Project) []finding {
	keys := make([]string, 0, len(p.Networks))
	for n := range p.Networks {
		keys = append(keys, n)
	}
	custom := customNetworks(keys)
	if len(custom) == 0 {
		return nil
	}
	return []finding{{levelIgnored, "networks",
		fmt.Sprintf("top-level networks (%s) — acompose flattens everything onto one shared project network", strings.Join(custom, ", ")), ""}}
}

// countPresentFeatures counts the fully supported compose features a service
// actually uses — the per-service base of the "X/Y compose features in this
// file translate cleanly" denominator. Each feature counts once.
func countPresentFeatures(s types.ServiceConfig) int {
	n := 0
	if s.Image != "" {
		n++
	}
	if s.Build != nil {
		n++
	}
	if len(s.Ports) > 0 {
		n++
	}
	if len(s.Environment) > 0 { // env_file is merged in here by compose-go
		n++
	}
	for _, m := range s.Volumes { // named + bind mounts (anonymous are findings)
		if m.Type == "bind" || (m.Type == "volume" && m.Source != "") {
			n++
			break
		}
	}
	if len(s.DependsOn) > 0 {
		n++
	}
	if len(s.Command) > 0 {
		n++
	}
	if s.WorkingDir != "" {
		n++
	}
	if s.ContainerName != "" {
		n++
	}
	if d := s.Deploy; d != nil && d.Resources.Limits != nil &&
		(d.Resources.Limits.NanoCPUs > 0 || d.Resources.Limits.MemoryBytes > 0) {
		n++ // deploy.resources.limits cpus/memory are translated to real flags
	}
	return n
}

// checkRun prints the full compatibility report and returns the number of
// blocker findings. It performs no runtime calls whatsoever.
func checkRun(p *types.Project) (blockers int) {
	order := toposort(p)
	info("acompose check — %s%s%s  (%d services)", bold, p.Name, reset, len(order))
	if len(order) == 0 {
		// short-circuit: a blank per-service report plus a "0/0 features"
		// ratio would read like a broken run, not an empty project
		fmt.Println("nothing to check")
		return 0
	}
	fmt.Println()

	printFinding := func(f finding) {
		switch f.level {
		case levelApproximated:
			fmt.Printf("    %s~%s %s: %s\n", yellow, reset, f.feature, f.detail)
		case levelIgnored:
			fmt.Printf("    %s!%s %s: %s\n", yellow, reset, f.feature, f.detail)
		case levelBlocker:
			fmt.Printf("    %s✗%s %s: %s\n", red, reset, f.feature, f.detail)
		}
	}

	var clean, approximated, withBlockers int // per-service buckets
	var base, findings, approxFindings int    // feature-ratio tallies
	for _, name := range order {
		s := p.Services[name]
		base += countPresentFeatures(s)
		fmt.Printf("  %s%s%s\n", bold, name, reset)
		fs := analyzeService(name, s)
		if len(fs) == 0 {
			fmt.Printf("    %s✓%s translates cleanly\n", green, reset)
			clean++
			continue
		}
		hasBlocker := false
		for _, f := range fs {
			printFinding(f)
			findings++
			switch f.level {
			case levelApproximated:
				approxFindings++
			case levelBlocker:
				blockers++
				hasBlocker = true
			}
		}
		if hasBlocker {
			withBlockers++
		} else {
			approximated++
		}
	}

	// project-level findings count toward the feature ratio but are not a
	// service, so they stay out of the clean/approximated/blockers buckets
	if pfs := analyzeProject(p); len(pfs) > 0 {
		fmt.Printf("  %sproject%s\n", bold, reset)
		for _, f := range pfs {
			printFinding(f)
			findings++
			if f.level == levelApproximated {
				approxFindings++
			}
			if f.level == levelBlocker {
				blockers++
			}
		}
	}

	fmt.Println()
	fmt.Printf("summary: %d clean · %d approximated · %d with blockers\n", clean, approximated, withBlockers)
	if base+findings > 0 { // never print a 0/0 ratio
		fmt.Printf("%d/%d compose features in this file translate cleanly or approximated\n", base+approxFindings, base+findings)
	}
	return blockers
}

func cmdCheck(p *types.Project) {
	if checkRun(p) > 0 {
		os.Exit(1)
	}
}
