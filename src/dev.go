package main

// `acompose dev` — hot reload driven by the compose spec's develop.watch
// section (the same one `docker compose watch` reads):
//
//   services:
//     api:
//       build: .
//       develop:
//         watch:
//           - path: ./src
//             action: sync          # changed files copied into the container
//             target: /app/src
//           - path: go.mod
//             action: rebuild       # image rebuilt, container recreated
//
// Supported actions: rebuild, sync, restart, sync+restart. Anything else
// (e.g. sync+exec) is warned about once and skipped.
//
// The platform has no inotify bridge and acompose takes no dependencies, so
// change detection is a POLLING scanner: every interval each trigger's path
// is walked into a path→(mtime,size) fingerprint map and diffed against the
// previous walk. Debounce keeps half-written saves out of a rebuild: changes
// are collected into a pending set and acted on at the first QUIET tick (a
// poll that found nothing new), or after 2 ticks regardless — so a steady
// stream of writes cannot starve the loop.
//
// Per service, one decision per round, rebuild dominating: any changed
// rebuild trigger → one rebuild (buildCmd, stop+delete, recreateService —
// the exact recreate path `up` and the ui use); otherwise changed sync
// triggers → one `container cp` per file (sync+restart adds a stop/start
// after); otherwise a changed restart trigger → stop/start. After any
// rebuild or restart the project is rewired so peers see the new IP.
// Failures warn and the loop keeps running — a broken build must not kill
// dev mode.
//
// Style follows dns.go/importvol.go: pure pieces (scanTree/diffScans,
// planActions) + a testable per-poll body (devTick); the infinite for+sleep
// in cmdDev stays thin.

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// ---------- scanner (pure) ----------------------------------------------------

// fingerprint is what "the file changed" means to the poller: a different
// mtime or size. Content hashing would cost a full read per file per second.
type fingerprint struct {
	mtime int64 // ModTime().UnixNano()
	size  int64
}

// devAlwaysIgnore is skipped under every trigger, no configuration needed.
var devAlwaysIgnore = []string{".git", "node_modules", ".DS_Store"}

// devIgnored reports whether a trigger-relative path is filtered out.
//
// The built-in defaults (.git, node_modules, .DS_Store) match by EXACT path
// segment only — substring matching would silently never watch
// .github/workflows/ci.yml, deploy.gitlab-ci.yml or node_modules_backup/f.
//
// USER patterns keep the documented, deliberately simple semantics: a
// pattern hits when filepath.Match accepts it against ANY path segment
// (so "build", "*.tmp" work at any depth), or when it is a plain substring
// of the slash-form relative path (so "cache" also covers "mycache/f").
// Full gitignore semantics are out of scope.
func devIgnored(rel string, ignore []string) bool {
	rel = filepath.ToSlash(rel)
	segs := strings.Split(rel, "/")
	for _, seg := range segs {
		for _, builtin := range devAlwaysIgnore {
			if seg == builtin {
				return true
			}
		}
	}
	for _, pat := range ignore {
		for _, seg := range segs {
			if ok, _ := filepath.Match(pat, seg); ok {
				return true
			}
		}
		if strings.Contains(rel, pat) {
			return true
		}
	}
	return false
}

// scanTree walks root — a single file or a directory — and returns a
// path→fingerprint map of every regular file under it, minus ignores.
// A missing root is an empty scan, not an error: the path may appear later
// (and its appearance is then a detected change).
func scanTree(root string, ignore []string) (map[string]fingerprint, error) {
	out := map[string]fingerprint{}
	fi, err := os.Stat(root)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		out[root] = fingerprint{fi.ModTime().UnixNano(), fi.Size()}
		return out, nil
	}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // vanished mid-walk — the next tick sees the delete
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		if devIgnored(rel, ignore) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out[p] = fingerprint{info.ModTime().UnixNano(), info.Size()}
		return nil
	})
	return out, walkErr
}

// diffScans returns the sorted paths that were added, modified, or deleted
// between two scans.
func diffScans(old, new map[string]fingerprint) []string {
	set := map[string]bool{}
	for p, fp := range new {
		if o, ok := old[p]; !ok || o != fp {
			set[p] = true
		}
	}
	for p := range old {
		if _, ok := new[p]; !ok {
			set[p] = true
		}
	}
	changed := make([]string, 0, len(set))
	for p := range set {
		changed = append(changed, p)
	}
	sort.Strings(changed)
	return changed
}

// ---------- trigger resolution -------------------------------------------------

// resolvedTrigger is one develop.watch entry made concrete: absolute on-host
// root, validated action.
type resolvedTrigger struct {
	service string
	action  types.WatchAction
	path    string // absolute host path of the watched file or directory
	target  string // container-side destination (sync forms only)
	ignore  []string
}

// resolveTriggers extracts the supported develop.watch triggers of the named
// services, resolving relative paths against the project working dir
// (compose-go usually pre-resolves them to absolute; the join is the
// belt-and-braces fallback). Unsupported actions warn once per action name.
func resolveTriggers(p *types.Project, names []string) []resolvedTrigger {
	var out []resolvedTrigger
	warned := map[string]bool{}
	sorted := append([]string{}, names...)
	sort.Strings(sorted)
	for _, name := range sorted {
		svc := p.Services[name]
		if svc.Develop == nil {
			continue
		}
		for _, trig := range svc.Develop.Watch {
			switch trig.Action {
			case types.WatchActionSync, types.WatchActionRebuild,
				types.WatchActionRestart, types.WatchActionSyncRestart:
			default:
				if !warned[string(trig.Action)] {
					warned[string(trig.Action)] = true
					warn("develop.watch action '%s' is not supported — supported: rebuild, restart, sync, sync+restart", trig.Action)
				}
				continue
			}
			if (trig.Action == types.WatchActionSync || trig.Action == types.WatchActionSyncRestart) && trig.Target == "" {
				warn("[%s] develop.watch sync trigger '%s' has no target — skipping it", name, trig.Path)
				continue
			}
			root := trig.Path
			if !filepath.IsAbs(root) {
				root = filepath.Join(p.WorkingDir, root)
			}
			out = append(out, resolvedTrigger{
				service: name,
				action:  trig.Action,
				path:    filepath.Clean(root),
				target:  trig.Target,
				ignore:  trig.Ignore,
			})
		}
	}
	return out
}

// ---------- decision (pure) ----------------------------------------------------

// devAction is one decided reaction for one service in one round.
type devAction struct {
	kind    types.WatchAction // rebuild | sync | restart | sync+restart
	service string
	paths   []string // changed host paths, sorted (sync kinds: parallel to dests)
	dests   []string // sync kinds only: container destination per path
}

// pathUnder reports whether p is root itself or lives below it.
func pathUnder(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// syncDest computes the container-side destination of a changed file:
// target + the file's path relative to the trigger root; a file trigger
// (root == changed path) maps to the target itself.
func syncDest(trig resolvedTrigger, changed string) string {
	rel, err := filepath.Rel(trig.path, changed)
	if err != nil || rel == "." {
		return trig.target
	}
	return path.Join(trig.target, filepath.ToSlash(rel))
}

// planActions folds a round's changed paths into at most one action per
// service: rebuild beats everything; otherwise sync triggers batch their
// files into one action (upgraded to sync+restart when a sync+restart or
// restart trigger also fired); otherwise a bare restart. Output is sorted
// by service — deterministic.
func planActions(changed []string, triggers []resolvedTrigger) []devAction {
	type plan struct {
		rebuild, restart bool
		matched          map[string]bool    // every changed path that hit any trigger
		syncs            map[[2]string]bool // {host path, container dest}
	}
	plans := map[string]*plan{}
	for _, trig := range triggers {
		for _, ch := range changed {
			if !pathUnder(trig.path, ch) {
				continue
			}
			if rel, err := filepath.Rel(trig.path, ch); err == nil && rel != "." && devIgnored(rel, trig.ignore) {
				continue
			}
			pl := plans[trig.service]
			if pl == nil {
				pl = &plan{matched: map[string]bool{}, syncs: map[[2]string]bool{}}
				plans[trig.service] = pl
			}
			pl.matched[ch] = true
			switch trig.action {
			case types.WatchActionRebuild:
				pl.rebuild = true
			case types.WatchActionRestart:
				pl.restart = true
			case types.WatchActionSync, types.WatchActionSyncRestart:
				pl.syncs[[2]string{ch, syncDest(trig, ch)}] = true
				if trig.action == types.WatchActionSyncRestart {
					pl.restart = true
				}
			}
		}
	}
	services := make([]string, 0, len(plans))
	for s := range plans {
		services = append(services, s)
	}
	sort.Strings(services)

	var out []devAction
	for _, s := range services {
		pl := plans[s]
		all := make([]string, 0, len(pl.matched))
		for p := range pl.matched {
			all = append(all, p)
		}
		sort.Strings(all)
		switch {
		case pl.rebuild:
			out = append(out, devAction{kind: types.WatchActionRebuild, service: s, paths: all})
		case len(pl.syncs) > 0:
			pairs := make([][2]string, 0, len(pl.syncs))
			for pr := range pl.syncs {
				pairs = append(pairs, pr)
			}
			sort.Slice(pairs, func(i, j int) bool {
				if pairs[i][0] != pairs[j][0] {
					return pairs[i][0] < pairs[j][0]
				}
				return pairs[i][1] < pairs[j][1]
			})
			act := devAction{kind: types.WatchActionSync, service: s}
			if pl.restart {
				act.kind = types.WatchActionSyncRestart
			}
			for _, pr := range pairs {
				act.paths = append(act.paths, pr[0])
				act.dests = append(act.dests, pr[1])
			}
			out = append(out, act)
		case pl.restart:
			out = append(out, devAction{kind: types.WatchActionRestart, service: s, paths: all})
		}
	}
	return out
}

// ---------- executor -------------------------------------------------------------

// devRelPaths renders host paths relative to the project dir for narration.
// Trigger paths come back from compose-go with symlinked components resolved
// (macOS: /var → /private/var) while WorkingDir may not be — try both roots.
func devRelPaths(p *types.Project, paths []string) []string {
	roots := []string{p.WorkingDir}
	if wd, err := filepath.EvalSymlinks(p.WorkingDir); err == nil && wd != p.WorkingDir {
		roots = append(roots, wd)
	}
	out := make([]string, len(paths))
	for i, pa := range paths {
		out[i] = pa
		for _, root := range roots {
			if rel, err := filepath.Rel(root, pa); err == nil && !strings.HasPrefix(rel, "..") {
				out[i] = rel
				break
			}
		}
	}
	return out
}

// devRestart stop/starts a service's container and rewires peers on success.
func devRestart(p *types.Project, r runner, service, cname string) {
	r.run(ctr("stop", cname), "no")
	if ok, _ := r.run(ctr("start", cname)); !ok {
		warn("[%s] restart failed — check: acompose logs %s", service, service)
		return
	}
	if len(p.Services) > 1 {
		rewireAll(p, r) // peers need the (possibly new) IP
	}
	okay("[%s] restarted", service)
}

// devExec carries out one planned action. Every failure path warns and
// returns — dev mode survives broken builds, missing files, failed cps.
func devExec(p *types.Project, r runner, act devAction, publish bool) {
	svc := p.Services[act.service]
	cname := cnameOf(p, act.service)
	info("[%s] %s changed → %s", act.service, strings.Join(devRelPaths(p, act.paths), ", "), act.kind)

	switch act.kind {
	case types.WatchActionRebuild:
		if svc.Build != nil {
			image := svc.Image
			if image == "" {
				image = p.Name + "-" + act.service
			}
			if ok, _ := r.run(buildCmd(image, svc, p.WorkingDir)); !ok {
				warn("[%s] build failed — keeping the current container running", act.service)
				return
			}
		} else {
			warn("[%s] rebuild trigger but no build: section — recreating from image '%s'", act.service, svc.Image)
		}
		r.run(ctr("stop", cname), "no")
		r.run(ctr("delete", cname), "no")
		if ok, msg := recreateService(p, r, act.service, publish); !ok {
			warn("[%s] recreate failed: %s — fix and save again, or run: acompose up", act.service, msg)
			return
		}
		if len(p.Services) > 1 {
			rewireAll(p, r) // peers need the new IP
		}
		okay("[%s] rebuilt and recreated", act.service)

	case types.WatchActionSync, types.WatchActionSyncRestart:
		synced := 0
		for i, local := range act.paths {
			rel := devRelPaths(p, []string{local})[0]
			if _, err := os.Stat(local); err != nil {
				// `container cp` copies INTO the container; it cannot remove —
				// a host-side delete needs a restart/rebuild trigger to land.
				warn("[%s] %s was deleted — cp cannot remove files in the container", act.service, rel)
				continue
			}
			// `container cp --help` (CLI 1.x): "Copy files/folders between a
			// container and the local filesystem" with the docker-compatible
			// shape `container cp <local-path> <name>:<dest>` — the one place
			// this argv lives, mirroring ctr() usage elsewhere.
			if ok, _ := r.run(ctr("cp", local, cname+":"+act.dests[i])); !ok {
				warn("[%s] sync of %s failed", act.service, rel)
				continue
			}
			synced++
		}
		if act.kind == types.WatchActionSyncRestart {
			devRestart(p, r, act.service, cname)
		} else if synced > 0 {
			okay("[%s] synced %d file(s)", act.service, synced)
		}

	case types.WatchActionRestart:
		devRestart(p, r, act.service, cname)
	}
}

// ---------- the poll loop ----------------------------------------------------------

// devState is everything one poll needs from the previous one.
type devState struct {
	scans   []map[string]fingerprint // baseline per trigger (same index)
	scanOK  []bool                   // false = no valid baseline yet (scan errored)
	pending map[string]bool          // changes waiting for a quiet tick
	waited  int                      // ticks the pending set has existed
}

// newDevState takes the baseline scans — files that already exist when dev
// starts are state, not changes. A trigger whose scan errors is marked
// UNINITIALIZED (scanOK false), never given a fabricated empty baseline:
// the first later successful scan is adopted as the baseline without
// reporting its whole tree as changes.
func newDevState(triggers []resolvedTrigger) *devState {
	st := &devState{pending: map[string]bool{}}
	for _, trig := range triggers {
		scan, err := scanTree(trig.path, trig.ignore)
		ok := err == nil
		if err != nil {
			warn("[%s] cannot scan %s: %v", trig.service, trig.path, err)
		}
		st.scans = append(st.scans, scan)
		st.scanOK = append(st.scanOK, ok)
	}
	return st
}

// devTick is one poll: rescan every trigger, fold fresh diffs into the
// pending set, and act once the debounce window closes — the first tick that
// found nothing new, or the 2nd tick regardless (so continuous writes cannot
// starve the loop). Returns the executed actions (nil while debouncing).
func devTick(p *types.Project, r runner, triggers []resolvedTrigger, st *devState, publish bool) []devAction {
	fresh := false
	for i, trig := range triggers {
		scan, err := scanTree(trig.path, trig.ignore)
		if err != nil {
			// warn only on the ok→error TRANSITION — a trigger that stays
			// unscannable must not spam every tick
			if st.scanOK[i] {
				warn("[%s] cannot scan %s: %v", trig.service, trig.path, err)
				st.scanOK[i] = false
			}
			continue
		}
		if !st.scanOK[i] {
			// first successful scan of an uninitialized trigger: ADOPT it as
			// the baseline — diffing against the error-state scan would report
			// the whole tree as a spurious mass change
			st.scans[i] = scan
			st.scanOK[i] = true
			continue
		}
		for _, ch := range diffScans(st.scans[i], scan) {
			st.pending[ch] = true
			fresh = true
		}
		st.scans[i] = scan
	}
	if len(st.pending) == 0 {
		st.waited = 0
		return nil
	}
	st.waited++
	if fresh && st.waited < 2 {
		return nil // a save burst is in flight — give it one quiet tick
	}
	changed := make([]string, 0, len(st.pending))
	for ch := range st.pending {
		changed = append(changed, ch)
	}
	sort.Strings(changed)
	st.pending = map[string]bool{}
	st.waited = 0

	actions := planActions(changed, triggers)
	for _, act := range actions {
		devExec(p, r, act, publish)
	}
	return actions
}

// ---------- command surface ----------------------------------------------------------

const devExampleSnippet = `  add a develop.watch section to a service, e.g.:

    services:
      api:
        build: .
        develop:
          watch:
            - path: ./src
              action: sync          # copy changed files into the container
              target: /app/src
            - path: go.mod
              action: rebuild       # rebuild the image and recreate
`

// devSetup validates the invocation and resolves the watch triggers,
// returning the exit code to die with (0 = proceed into the loop) —
// factored out of cmdDev so tests can assert codes without a subprocess.
func devSetup(p *types.Project, r runner, args []string) ([]resolvedTrigger, int) {
	if r.dry {
		fail("dev needs a live runtime — it cannot be combined with --dry-run")
		return nil, 2
	}
	names := toposort(p)
	if len(args) > 0 {
		want := map[string]bool{}
		for _, a := range args {
			if _, ok := p.Services[a]; !ok {
				fail("unknown service '%s' — services: %s", a, strings.Join(names, ", "))
				return nil, 1
			}
			want[a] = true
		}
		var keep []string
		for _, n := range names {
			if want[n] {
				keep = append(keep, n)
			}
		}
		names = keep
	}
	triggers := resolveTriggers(p, names)
	if len(triggers) == 0 {
		fail("no develop.watch triggers found — nothing to hot-reload")
		fmt.Fprint(os.Stderr, devExampleSnippet)
		return nil, 1
	}
	return triggers, 0
}

// devNarrateStartup says exactly what will be watched and how often.
func devNarrateStartup(p *types.Project, triggers []resolvedTrigger, interval time.Duration) {
	info("dev mode: %d trigger(s)  (poll every %s, Ctrl-C to quit)", len(triggers), interval)
	for _, trig := range triggers {
		tail := ""
		if trig.target != "" {
			tail = " → " + trig.target
		}
		fmt.Printf("  [%s] %s  %s%s%s%s\n", trig.service, devRelPaths(p, []string{trig.path})[0], dim, trig.action, tail, reset)
	}
}

func cmdDev(p *types.Project, r runner, args []string, interval time.Duration, publish bool) {
	triggers, code := devSetup(p, r, args)
	if code != 0 {
		os.Exit(code)
	}
	devNarrateStartup(p, triggers, interval)
	st := newDevState(triggers)
	for {
		time.Sleep(interval)
		devTick(p, r, triggers, st, publish)
	}
}
