package main

// `acompose menubar` — SwiftBar/xbar plugin output: a menu bar presence with
// zero cgo. SwiftBar (github.com/swiftbar/SwiftBar) renders a menu from any
// executable's stdout; contrib/swiftbar/acompose.5s.sh is the plugin shim
// that cd's into the project and execs this subcommand every 5 seconds.
//
// Format recap (SwiftBar/xbar shared subset — no sfimage=, plain unicode dots,
// so the same output works in both):
//   - lines before the first `---` are the menu bar title
//   - after it, one item per line; `|`-separated parameters (color=, font=,
//     size=, href=, bash=, param1..N=, terminal=false, refresh=true)
//   - a `--` prefix nests the line as a submenu item; `---` is a separator
//
// The renderer is a pure function over the existing collectState() uiState so
// tests need no runtime, and it never uses the package ANSI color vars —
// escape codes would show up as garbage in the menu bar.

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// shQuote single-quotes s for /bin/sh, escaping embedded single quotes via
// the standard '\” dance — project paths with spaces (or quotes) must
// survive the bash= round-trip intact.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// menubarDot maps a svcState.State to its status dot.
func menubarDot(state string) string {
	switch state {
	case "running":
		return "🟢"
	case "stopped":
		return "🔴"
	}
	return "⚪" // missing
}

// menubarUnreachable is the full plugin output when `container ls` fails: a
// menu bar plugin must never hard-fail, so this renders the problem instead.
const menubarUnreachable = "⛴ ?\n" +
	"---\n" +
	"container runtime unreachable — is the system service running? | color=red\n"

// menubarAction renders the shared bash= parameter tail that runs one
// acompose subcommand inside the project directory, silently, then refreshes
// the menu.
func menubarAction(binPath, workDir, args string) string {
	return fmt.Sprintf(`bash=/bin/sh param1=-c param2="cd %s && %s %s" terminal=false refresh=true`,
		shQuote(workDir), shQuote(binPath), args)
}

// renderMenubar turns a collected uiState into the complete SwiftBar plugin
// output. Pure: same state in, byte-identical text out.
func renderMenubar(st uiState, binPath, workDir string) string {
	running, total := 0, len(st.Services)
	for _, s := range st.Services {
		if s.State == "running" {
			running++
		}
	}

	var b strings.Builder

	// menu bar title — red when degraded so the glanceable bit is the alert
	fmt.Fprintf(&b, "⛴ %d/%d", running, total)
	if running < total {
		b.WriteString(" | color=red")
	}
	b.WriteString("\n---\n")

	fmt.Fprintf(&b, "%s — %d/%d running | size=12\n", st.Project, running, total)
	b.WriteString("---\n")

	// per-service status line + one submenu action (stop when running,
	// start otherwise — start recovers stopped AND missing via the
	// `acompose start` subcommand's ensureServiceRunning path)
	for _, s := range st.Services {
		detail := s.State
		if s.State == "running" && s.IP != "" {
			detail = s.IP
		}
		fmt.Fprintf(&b, "%s %s  %s | font=Menlo size=12\n", menubarDot(s.State), s.Name, detail)
		verb := "start"
		if s.State == "running" {
			verb = "stop"
		}
		fmt.Fprintf(&b, "-- %s | %s\n", verb, menubarAction(binPath, workDir, verb+" "+s.Name))
	}
	b.WriteString("---\n")

	b.WriteString("Open dashboard (acompose ui) | href=http://127.0.0.1:4242\n")

	// published ports across all services: deduped by host port (a host port
	// can only be bound once anyway), numerically sorted, service-labelled
	portSvc := map[string]string{}
	var hostPorts []string
	for _, s := range st.Services {
		for _, prt := range s.Ports {
			if prt.Host == "" {
				continue
			}
			if _, dup := portSvc[prt.Host]; dup {
				continue
			}
			portSvc[prt.Host] = s.Name
			hostPorts = append(hostPorts, prt.Host)
		}
	}
	sort.Slice(hostPorts, func(i, j int) bool {
		a, errA := strconv.Atoi(hostPorts[i])
		c, errC := strconv.Atoi(hostPorts[j])
		if errA == nil && errC == nil {
			return a < c
		}
		return hostPorts[i] < hostPorts[j]
	})
	for _, h := range hostPorts {
		fmt.Fprintf(&b, "localhost:%s → %s | href=http://localhost:%s\n", h, portSvc[h], h)
	}
	b.WriteString("---\n")

	fmt.Fprintf(&b, "Start all (up) | %s\n", menubarAction(binPath, workDir, "up"))
	fmt.Fprintf(&b, "Stop all (down) | %s\n", menubarAction(binPath, workDir, "down"))
	fmt.Fprintf(&b, "Refresh /etc/hosts | %s\n", menubarAction(binPath, workDir, "refresh"))
	return b.String()
}

// menubarRun is the cmd-level entry point. It always returns 0: a SwiftBar
// plugin that exits non-zero shows a broken-plugin icon, so even an
// unreachable runtime is rendered as menu content instead.
func menubarRun(p *types.Project) int {
	// collectState tolerates a failing `container ls` by reporting every
	// service as missing — for the menu bar that would be a lie ("all down")
	// when the truth is "runtime unreachable", so probe explicitly first.
	if err := exec.Command("container", "ls", "--all").Run(); err != nil {
		fmt.Print(menubarUnreachable)
		return 0
	}
	binPath, err := os.Executable()
	if err != nil || binPath == "" {
		binPath = "acompose" // fall back to PATH lookup at click time
	}
	fmt.Print(renderMenubar(collectState(p), binPath, p.WorkingDir))
	return 0
}
