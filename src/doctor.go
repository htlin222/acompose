package main

// `acompose doctor` — environment readiness check for someone evaluating
// acompose (possibly coming from OrbStack/Docker). It must work WITHOUT a
// compose file, so main() dispatches it before loadProject.
//
// Every check is split into a pure verdict function (table-testable) and a
// thin gatherer in doctorRun; the gatherers are injectable via doctorEnv so
// the full run is testable on any OS/arch (CI is Ubuntu).

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// testedContainerVersion is the Apple `container` CLI release acompose is
// developed and tested against — other versions get a warning, not a failure.
const testedContainerVersion = "1.0.0"

type verdictStatus int

const (
	vOK verdictStatus = iota
	vWarn
	vFail
)

// verdict is one doctor check's outcome: a status, a message, and (for
// non-OK statuses) a one-line hint on how to proceed.
type verdict struct {
	status verdictStatus
	msg    string
	hint   string
}

// checkArch: Apple container runs VMs via Virtualization.framework — Apple
// Silicon only.
func checkArch(goarch string) verdict {
	if goarch == "arm64" {
		return verdict{vOK, "architecture: arm64 (Apple Silicon)", ""}
	}
	return verdict{vFail, "architecture: " + goarch, "Apple container needs Apple Silicon"}
}

// parseMacOSVersion turns `sw_vers -productVersion` output into a verdict:
// Apple container requires macOS 26 or newer.
func parseMacOSVersion(s string) verdict {
	s = strings.TrimSpace(s)
	major, err := strconv.Atoi(strings.SplitN(s, ".", 2)[0])
	if s == "" || err != nil {
		return verdict{vWarn, fmt.Sprintf("macOS: unrecognized version %q", s), "could not parse `sw_vers -productVersion` output"}
	}
	if major >= 26 {
		return verdict{vOK, "macOS " + s, ""}
	}
	return verdict{vFail, "macOS " + s, "Apple container needs macOS 26 or newer"}
}

// checkMacOS is the gatherer around parseMacOSVersion: non-darwin is an
// immediate failure, darwin asks sw_vers.
func checkMacOS(goos string, swVers func() (string, error)) verdict {
	if goos != "darwin" {
		return verdict{vFail, "operating system: " + goos, "acompose only runs on macOS"}
	}
	out, err := swVers()
	if err != nil {
		return verdict{vWarn, "macOS: version unknown", "could not run sw_vers: " + err.Error()}
	}
	return parseMacOSVersion(out)
}

var semverRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

// parseContainerVersion extracts the semver from `container --version`
// output, e.g. "container CLI version 1.0.0 (build: release, ...)" → "1.0.0".
func parseContainerVersion(out string) (string, bool) {
	v := semverRE.FindString(out)
	return v, v != ""
}

func checkCLIVersion(out string) verdict {
	v, found := parseContainerVersion(out)
	if !found {
		return verdict{vWarn, "container CLI version: unknown", fmt.Sprintf("could not find a version in %q", strings.TrimSpace(out))}
	}
	if v == testedContainerVersion {
		return verdict{vOK, "container CLI version " + v + " (tested)", ""}
	}
	return verdict{vWarn, "container CLI version " + v + " — untested version", "acompose was tested against " + testedContainerVersion + "; if something breaks, open an issue"}
}

// checkSystemStatus interprets `container system status` output. The stopped
// state usually reads "... not running ...", which still contains "running" —
// hence the explicit negative guard.
func checkSystemStatus(out string) verdict {
	low := strings.ToLower(out)
	if strings.Contains(low, "running") && !strings.Contains(low, "not running") {
		return verdict{vOK, "container system service is running", ""}
	}
	return verdict{vFail, "container system service not running", "start it: container system start"}
}

// checkComposeFile looks for any default compose file name in the current
// directory — a missing one is a warning, not a problem: doctor must be
// useful before a project exists.
func checkComposeFile() verdict {
	for _, n := range defaultComposeNames {
		if _, err := os.Stat(n); err == nil {
			return verdict{vOK, "found " + n, ""}
		}
	}
	return verdict{vWarn, "no compose file here", "try: acompose init"}
}

// doctorEnv carries the gatherers doctorRun needs, so tests can substitute
// any OS/arch/CLI combination without owning a Mac.
type doctorEnv struct {
	goarch string
	goos   string
	swVers func() (string, error)
	// cliVersion gathers `container --version` output directly (like swVers)
	// rather than through the runner: any failure shape is a vWarn verdict
	// here, and the runner's tolerate mechanism cannot promise to keep an
	// arbitrary failure message from turning the report loud.
	cliVersion func() (string, error)
	lookPath   func(string) (string, error)
	r          runner
}

func defaultDoctorEnv() doctorEnv {
	return doctorEnv{
		goarch: runtime.GOARCH,
		goos:   runtime.GOOS,
		swVers: func() (string, error) {
			out, err := exec.Command("sw_vers", "-productVersion").Output()
			return string(out), err
		},
		cliVersion: func() (string, error) {
			out, err := exec.Command("container", "--version").CombinedOutput()
			return string(out), err
		},
		lookPath: exec.LookPath,
		r:        runner{},
	}
}

// doctorRun executes every check, prints one ✓/!/✗ line each, and returns the
// number of hard problems (✗). Warnings (!) never affect the count.
func doctorRun(env doctorEnv) (problems int) {
	info("acompose doctor — is this machine ready?")
	report := func(v verdict) {
		switch v.status {
		case vOK:
			fmt.Printf("%s✓%s %s\n", green, reset, v.msg)
		case vWarn:
			fmt.Printf("%s!%s %s — %s%s%s\n", yellow, reset, v.msg, dim, v.hint, reset)
		case vFail:
			problems++
			fmt.Printf("%s✗%s %s — %s%s%s\n", red, reset, v.msg, dim, v.hint, reset)
		}
	}

	report(checkArch(env.goarch))
	report(checkMacOS(env.goos, env.swVers))

	if cliPath, err := env.lookPath("container"); err != nil {
		report(verdict{vFail, "container CLI not found", "install it: https://github.com/apple/container/releases"})
		report(verdict{vWarn, "container CLI version: skipped", "container CLI not found"})
		report(verdict{vWarn, "container system service: skipped", "container CLI not found"})
	} else {
		report(verdict{vOK, "container CLI: " + cliPath, ""})
		// gathered via env.cliVersion, not the runner: a failing --version
		// must yield the "unknown" warning, never a loud "command failed:"
		verOut, _ := env.cliVersion()
		report(checkCLIVersion(verOut))
		// a stopped service exits non-zero with a "not running" message —
		// tolerate it so doctor reports the state instead of "command failed"
		_, statusOut := env.r.run(ctr("system", "status"), "not running", "not registered", "stopped")
		report(checkSystemStatus(statusOut))
	}

	report(checkComposeFile())

	fmt.Println()
	if problems == 0 {
		okay("ready — try: acompose up")
	} else {
		fail("%d problem(s) found", problems)
	}
	return problems
}

func cmdDoctor() {
	if doctorRun(defaultDoctorEnv()) > 0 {
		os.Exit(1)
	}
}
