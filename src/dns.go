package main

// `acompose dns` — host-side DNS names for services, the platform-native
// equivalent of OrbStack's *.orb.local. `container system dns create <domain>`
// registers a local DNS domain and installs a macOS /etc/resolver/<domain>
// hook; from then on the HOST resolves every container as <cname>.<domain>.
//
// PRECISION: what resolves is the CONTAINER name, not the service name — for
// acompose's default naming that is <project>-<service>.<domain>; a service
// with container_name shortens it (container_name: db → db.<domain>).
// Verified against container CLI 1.0.0: `container system dns --help` offers
// only create/delete/list of whole domains — no per-container alias support —
// so acompose reports the real, resolvable names rather than prettier ones
// that would not resolve.
//
// Creating/deleting a domain needs admin rights ("must run as an
// administrator"); acompose never sudoes on its own — on a permission error
// it prints the exact sudo command for the user to run once.

import (
	"fmt"
	"os"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// dnsDomain is the project's local DNS domain: the compose project name
// (already lowercase per the compose spec; ToLower is belt and braces).
func dnsDomain(p *types.Project) string { return strings.ToLower(p.Name) }

// dnsProbeTolerate keeps read-only dns probes quiet when they fail (older CLI
// without the subcommand, stopped system service): the `up` discovery hook
// and the status report must never turn a working flow loud. Real container
// CLI failure messages virtually always contain one of these.
var dnsProbeTolerate = []string{"error", "usage", "unknown", "unrecognized", "not", "failed", "stopped"}

// dnsDomainListed runs `container system dns list` (exactly once) and reports
// whether domain appears in the output; ok is false when the list command
// itself failed.
func dnsDomainListed(r runner, domain string) (listed, ok bool) {
	ok, out := r.run(ctr("system", "dns", "list"), dnsProbeTolerate...)
	if !ok {
		return false, false
	}
	return dnsOutputLists(out, domain), true
}

// dnsOutputLists matches the domain as a whole field on a line, never as a
// substring — "proj" must not claim a "projx" line (mirrors lsLineFor's
// reasoning; any header line is naturally skipped).
func dnsOutputLists(out, domain string) bool {
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.Fields(line) {
			if f == domain {
				return true
			}
		}
	}
	return false
}

// dnsHostNames returns the host-resolvable name of every service in
// topological order: <cname>.<domain>.
func dnsHostNames(p *types.Project, domain string) []string {
	var names []string
	for _, svc := range toposort(p) {
		names = append(names, cnameOf(p, svc)+"."+domain)
	}
	return names
}

// dnsPermissionError recognizes the runtime's admin-rights refusals.
func dnsPermissionError(msg string) bool {
	low := strings.ToLower(msg)
	for _, s := range []string{"must run as an administrator", "permission", "not permitted"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// dnsNotFound recognizes "the domain does not exist" — benign for teardown.
func dnsNotFound(msg string) bool {
	low := strings.ToLower(msg)
	for _, s := range []string{"not found", "no such", "does not exist", "not configured"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// dnsPrintNames prints one line per service, in topological order, with its
// host-resolvable name and current IP. One `ls` call total; getIP runs only
// for containers the listing shows as running — inspecting a missing
// container would fail loudly for an entirely expected state.
func dnsPrintNames(p *types.Project, r runner, domain string) {
	order := toposort(p)
	var lsOut string
	if ok, out := r.run(ctr("ls", "--all"), dnsProbeTolerate...); ok {
		lsOut = out
	}
	hosts := make([]string, len(order))
	width := 0
	for i, svc := range order {
		hosts[i] = cnameOf(p, svc) + "." + domain
		if len(hosts[i]) > width {
			width = len(hosts[i])
		}
	}
	for i, svc := range order {
		cname := cnameOf(p, svc)
		state := "not running"
		if lsLineRunning(lsOut, cname) {
			if ip := getIP(r, cname); ip != "" {
				state = ip
			} else {
				state = "running (no IP yet)"
			}
		}
		fmt.Printf("  %-*s → %s\n", width, hosts[i], state)
	}
}

func dnsStatus(p *types.Project, r runner, domain string) int {
	listed, ok := dnsDomainListed(r, domain)
	if !ok {
		warn("could not query local DNS domains — is the container system service running?")
		return 1
	}
	if !listed {
		info("local DNS domain '%s' not configured — services have no host-side names", domain)
		fmt.Printf("  %sset it up: acompose dns setup%s\n", dim, reset)
		return 0
	}
	okay("local DNS domain '%s' configured", domain)
	dnsPrintNames(p, r, domain)
	return 0
}

func dnsSetup(p *types.Project, r runner, domain string) int {
	if listed, ok := dnsDomainListed(r, domain); ok && listed {
		okay("local DNS domain '%s' already configured", domain)
		dnsPrintNames(p, r, domain)
		return 0
	}
	ok, msg := r.run(ctr("system", "dns", "create", domain),
		"must run as an administrator", "permission", "not permitted")
	if !ok {
		if dnsPermissionError(msg) {
			fail("creating a local DNS domain needs admin rights — run this once:")
			fmt.Fprintf(os.Stderr, "  %ssudo container system dns create %s%s\n", bold, domain, reset)
			fmt.Fprintf(os.Stderr, "  %sone-time, creates /etc/resolver/%s — then re-run: acompose dns setup%s\n", dim, domain, reset)
			return 1
		}
		return 1 // untolerated failure: runner already printed the loud error
	}
	okay("local DNS domain '%s' created", domain)
	dnsPrintNames(p, r, domain)
	fmt.Printf("  %s(new containers pick this up automatically; restart existing ones with acompose down && acompose up)%s\n", dim, reset)
	return 0
}

func dnsTeardown(p *types.Project, r runner, domain string) int {
	ok, msg := r.run(ctr("system", "dns", "delete", domain),
		"must run as an administrator", "permission", "not permitted",
		"not found", "no such", "does not exist", "not configured")
	if ok {
		okay("local DNS domain '%s' removed", domain)
		return 0
	}
	if dnsPermissionError(msg) {
		fail("removing the local DNS domain needs admin rights — run:")
		fmt.Fprintf(os.Stderr, "  %ssudo container system dns delete %s%s\n", bold, domain, reset)
		return 1
	}
	if dnsNotFound(msg) {
		okay("local DNS domain '%s' was not configured — nothing to remove", domain)
		return 0
	}
	return 1 // untolerated failure: runner already printed the loud error
}

// dnsRun dispatches one dns subcommand and returns the process exit code —
// factored out of cmdDNS so tests can assert codes without a subprocess.
// Like dev/watch it refuses --dry-run: every subcommand needs real probe
// output to decide anything, and a dry runner would fabricate success
// ("✓ created" for something that never ran).
func dnsRun(p *types.Project, r runner, sub string) int {
	if r.dry {
		fail("dns needs a live runtime — it cannot be combined with --dry-run")
		return 2
	}
	domain := dnsDomain(p)
	switch sub {
	case "", "status":
		return dnsStatus(p, r, domain)
	case "setup":
		return dnsSetup(p, r, domain)
	case "teardown":
		return dnsTeardown(p, r, domain)
	}
	fail("unknown dns subcommand '%s' — use setup, status, or teardown", sub)
	return 2
}

func cmdDNS(p *types.Project, r runner, sub string) {
	if code := dnsRun(p, r, sub); code != 0 {
		os.Exit(code)
	}
}
