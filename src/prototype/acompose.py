#!/usr/bin/env python3
"""
acompose v3 — run an existing docker-compose.yml on Apple's `container` CLI,
as close to drop-in as the platform allows, and LOUD about everything else.

Fixed in v3 (over v2):
  • hosts-race: service-name DNS is wired IMMEDIATELY as each container starts
    (deps' IPs go in before dependents launch; new IPs back-fill older peers)
  • sh-less images (distroless/scratch): hosts injection failure is detected
    and loudly reported; <DEP>_HOST env vars are ALWAYS injected as fallback
  • real ports parser: ip:host:container, ranges (8000-8010), /udp suffix,
    bare container ports, and a guard for the YAML 22:22→1342 sexagesimal trap
  • $$ escaping in ${VAR} interpolation (htpasswd hashes, cron strings survive)
  • `refresh` subcommand: re-grab IPs and rewrite /etc/hosts after restarts
    or sleep/wake DNS weirdness
  • depends_on long form: condition service_healthy/service_started honoured
    via TCP polling of the dependency's first known port (--wait-timeout)
  • build: dockerfile / args / target passed through
  • docker-compose.override.yml auto-merged; repeat --file to stack files
  • platform: linux/amd64 detected and warned (x86 is not seamless here)
  • no more silent failures: every failed container command prints its stderr
  • ps: project filtering restored

Still impossible at the platform level (warned, never silent):
  exec-style healthchecks, restart policies, named volumes, deploy limits.
"""
import argparse, ipaddress, json, os, re, shlex, socket, subprocess, sys, time

DEFAULT_FILES = ["compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"]
OVERRIDE_FILES = {"compose.yml": "compose.override.yml",
                  "compose.yaml": "compose.override.yaml",
                  "docker-compose.yml": "docker-compose.override.yml",
                  "docker-compose.yaml": "docker-compose.override.yaml"}

SUPPORTED = {"image", "build", "command", "environment", "env_file", "depends_on",
             "volumes", "working_dir", "ports", "container_name", "platform"}
IGNORED = {
    "healthcheck": "exec-style healthcheck ignored — `condition: service_healthy` is approximated by TCP polling",
    "restart":     "ignored — no auto-restart policy on this runtime",
    "deploy":      "ignored — resource limits/replicas not applied",
    "secrets":     "ignored — not mounted",
    "configs":     "ignored — not mounted",
    "profiles":    "ignored — service is always included",
    "networks":    "ignored — all services share one project network",
    "entrypoint":  "ignored — override via `command:` instead (flag support varies by container version)",
    "user":        "ignored — runs as the image's default user",
    "expose":      "informational only here; use ports: to publish",
}

def _c(code): return code if sys.stdout.isatty() else ""
BOLD, DIM, GREEN, CYAN, YELLOW, RED, RESET = (_c(x) for x in
    ("\033[1m", "\033[2m", "\033[32m", "\033[36m", "\033[33m", "\033[31m", "\033[0m"))
def info(m): print(f"{CYAN}::{RESET} {m}")
def ok(m):   print(f"{GREEN}\u2713{RESET} {m}")
def warn(m): print(f"{YELLOW}!{RESET} {m}", file=sys.stderr)
def fail(m): print(f"{RED}\u2717{RESET} {m}", file=sys.stderr)


# ---------- .env + ${VAR} interpolation (with $$ escaping) -------------------
def parse_env_file(path, required=False):
    env = {}
    if not os.path.exists(path):
        if required: warn(f"env_file not found: {path}")
        return env
    for line in open(path):
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        v = v.strip()
        if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
            v = v[1:-1]
        env[k.strip()] = v
    return env

_VAR = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)")
_DOLLAR = "\x00DOLLAR\x00"

def interpolate(obj, ctx):
    if isinstance(obj, dict):  return {k: interpolate(v, ctx) for k, v in obj.items()}
    if isinstance(obj, list):  return [interpolate(v, ctx) for v in obj]
    if not isinstance(obj, str): return obj
    s = obj.replace("$$", _DOLLAR)            # compose spec: $$ is a literal $
    def repl(m):
        name = m.group(1) or m.group(3)
        default = m.group(2)
        if ctx.get(name):
            return ctx[name]
        return default if default is not None else ctx.get(name, "")
    return _VAR.sub(repl, s).replace(_DOLLAR, "$")


# ---------- compose loading, override merge ----------------------------------
def deep_merge(base, over):
    """Compose-style merge: mappings merge; command/entrypoint replace;
    other sequences append (deduped); scalars replace."""
    if isinstance(base, dict) and isinstance(over, dict):
        out = dict(base)
        for k, v in over.items():
            if k in out:
                if k in ("command", "entrypoint"):
                    out[k] = v
                else:
                    out[k] = deep_merge(out[k], v)
            else:
                out[k] = v
        return out
    if isinstance(base, list) and isinstance(over, list):
        return base + [x for x in over if x not in base]
    return over

def load_compose(files):
    try:
        import yaml
    except ImportError:
        sys.exit("This tool needs PyYAML.  pip install pyyaml")
    merged = {}
    for path in files:
        raw = yaml.safe_load(open(path)) or {}
        merged = deep_merge(merged, raw)
    directory = os.path.dirname(os.path.abspath(files[0]))
    ctx = {**parse_env_file(os.path.join(directory, ".env")), **os.environ}
    return interpolate(merged, ctx), directory

def resolve_files(explicit):
    if explicit:                                   # repeat --file to stack
        for f in explicit:
            if not os.path.exists(f): sys.exit(f"compose file not found: {f}")
        return list(explicit)
    for f in DEFAULT_FILES:
        if os.path.exists(f):
            files = [f]
            ov = OVERRIDE_FILES.get(f)
            if ov and os.path.exists(ov):
                info(f"merging override file {ov}")
                files.append(ov)
            return files
    sys.exit("no compose file found (looked for: " + ", ".join(DEFAULT_FILES) + ")")


# ---------- normalisation ------------------------------------------------------
def norm_env(env):
    if not env: return {}
    if isinstance(env, dict):
        return {str(k): "" if v is None else str(v) for k, v in env.items()}
    out = {}
    for item in env:
        k, sep, v = str(item).partition("=")
        out[k] = v if sep else os.environ.get(k, "")
    return out

def load_env_files(spec, directory):
    ef = spec.get("env_file")
    if not ef: return {}
    merged = {}
    for f in ([ef] if isinstance(ef, str) else ef):
        p = f if os.path.isabs(f) else os.path.join(directory, f)
        merged.update(parse_env_file(p, required=True))
    return merged

def norm_depends(dep):
    """-> list of (service, condition)."""
    if not dep: return []
    if isinstance(dep, dict):
        out = []
        for name, opts in dep.items():
            cond = (opts or {}).get("condition", "service_started") if isinstance(opts, dict) else "service_started"
            out.append((name, cond))
        return out
    return [(d, "service_started") for d in dep]

def norm_command(cmd):
    if cmd is None: return []
    return shlex.split(cmd) if isinstance(cmd, str) else [str(c) for c in cmd]


# ---------- ports parser -------------------------------------------------------
def parse_ports(svc, entries):
    """-> list of (host_ip|None, host_port|None, container_port). Loud on the
    YAML sexagesimal trap and on anything unparsable."""
    out = []
    for e in (entries or []):
        if isinstance(e, dict):                       # long syntax
            tgt = e.get("target");  pub = e.get("published")
            if tgt: out.append((e.get("host_ip"), int(pub) if pub else None, int(tgt)))
            continue
        if isinstance(e, int):
            if e > 65535:
                fail(f"[{svc}] ports entry {e} > 65535 — this is the YAML 1.1 trap "
                     f"(unquoted 22:22 parses as 1342). Quote it: \"22:22\". Skipping.")
                continue
            out.append((None, None, e))               # bare container port
            continue
        s = str(e)
        proto = ""
        if "/" in s: s, proto = s.split("/", 1)
        parts = s.split(":")
        try:
            if len(parts) == 1:
                hp, cp = None, parts[0]
            elif len(parts) == 2:
                hp, cp = parts
            elif len(parts) == 3:
                ipaddress.ip_address(parts[0])        # validate bind ip
                out.extend(_expand(svc, parts[0], parts[1], parts[2])); continue
            else:
                raise ValueError
            out.extend(_expand(svc, None, hp, cp))
        except ValueError:
            fail(f"[{svc}] cannot parse ports entry {e!r} — skipping")
    return out

def _expand(svc, bind, hp, cp):
    """Expand ranges like 8000-8010:8000-8010 into individual pairs."""
    def rng(x):
        if x is None: return [None]
        if "-" in str(x):
            a, b = str(x).split("-", 1); return list(range(int(a), int(b) + 1))
        return [int(x)]
    hosts, conts = rng(hp), rng(cp)
    if hosts == [None]: hosts = [None] * len(conts)
    if len(hosts) != len(conts):
        fail(f"[{svc}] port range lengths differ: {hp} vs {cp} — skipping"); return []
    return [(bind, h, c) for h, c in zip(hosts, conts)]


# ---------- the single place container flags live ------------------------------
def container(*args): return ["container", *args]

def build_build_cmd(image, ctx, b):
    cmd = container("build", "--tag", image)
    if isinstance(b, dict):
        if b.get("dockerfile"): cmd += ["--file", os.path.join(ctx, b["dockerfile"])]
        for k, v in (b.get("args") or {}).items() if isinstance(b.get("args"), dict) \
                 else [a.split("=", 1) for a in (b.get("args") or [])]:
            cmd += ["--build-arg", f"{k}={v}"]
        if b.get("target"): cmd += ["--target", b["target"]]
    cmd.append(ctx)
    return cmd

def build_run_cmd(cname, network, image, env, volumes, workdir, command, pubs):
    cmd = container("run", "--detach", "--name", cname)
    if network: cmd += ["--network", network]
    for bind, hp, cp in pubs:
        if hp is None: continue                      # bare container port: nothing to publish
        spec = f"{bind}:{hp}:{cp}" if bind else f"{hp}:{cp}"
        cmd += ["--publish", spec]
    for k, v in env.items(): cmd += ["--env", f"{k}={v}"]
    for vol in volumes:      cmd += ["--volume", vol]
    if workdir:              cmd += ["--workdir", workdir]
    cmd.append(image)
    cmd += command
    return cmd

def hosts_inject_cmd(cname, pairs):
    quoted = " ".join(shlex.quote(f"{ip}\t{name}") for name, ip in pairs)
    return container("exec", cname, "sh", "-c", f'printf "%s\\n" {quoted} >> /etc/hosts')


# ---------- runner: nothing fails silently --------------------------------------
class Runner:
    def __init__(self, dry): self.dry = dry
    def run(self, cmd, capture=False, fatal=False, expect_fail_ok=None):
        """expect_fail_ok: substring; if the error contains it, stay quiet
        (e.g. 'already exists' when re-creating the network)."""
        printable = shlex.join(cmd)
        if self.dry:
            print(f"  {DIM}${RESET} {printable}")
            return True, ""
        try:
            res = subprocess.run(cmd, text=True, capture_output=True)
        except FileNotFoundError:
            sys.exit("`container` not found — needs macOS with Apple's container CLI.")
        if res.returncode != 0:
            err = (res.stderr or res.stdout or "").strip()
            if expect_fail_ok and expect_fail_ok in err.lower():
                return False, err
            fail(f"command failed: {printable}")
            if err: print(f"  {DIM}{err.splitlines()[-1]}{RESET}", file=sys.stderr)
            if fatal: sys.exit(1)
            return False, err
        return True, (res.stdout or "").strip()


# ---------- IP + readiness -------------------------------------------------------
_IPV4 = re.compile(r"^(?:\d{1,3}\.){3}\d{1,3}(?:/\d+)?$")
def extract_ipv4(blob):
    found = []
    def walk(o):
        if isinstance(o, dict):
            for v in o.values(): walk(v)
        elif isinstance(o, list):
            for v in o: walk(v)
        elif isinstance(o, str) and _IPV4.match(o.strip()):
            found.append(o.strip().split("/")[0])
    walk(blob)
    for ip in found:
        if not ip.startswith("127.") and ip != "0.0.0.0": return ip
    return None

def get_ip(runner, cname):
    if runner.dry: return f"<{cname}-ip>"
    okk, out = runner.run(container("inspect", cname), capture=True)
    if not okk or not out: return None
    try:    return extract_ipv4(json.loads(out))
    except json.JSONDecodeError:
        m = _IPV4.search(out); return m.group(0).split("/")[0] if m else None

def wait_tcp(ip, port, timeout, label):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with socket.create_connection((ip, port), timeout=1.5):
                ok(f"{label} is accepting connections on :{port}"); return True
        except OSError:
            time.sleep(0.5)
    warn(f"{label}: no TCP answer on {ip}:{port} after {timeout}s — continuing anyway")
    return False


# ---------- shared plumbing -------------------------------------------------------
def project_of(args, data, path):
    return args.project or data.get("name") or \
        os.path.basename(os.path.abspath(os.path.dirname(path) or ".")) or "acompose"

def cname_of(project, svc, spec): return (spec or {}).get("container_name") or f"{project}-{svc}"

def wire_hosts(runner, cname, pairs, svc):
    """Inject name→IP pairs; loudly degrade for sh-less images."""
    if not pairs: return
    okk, err = runner.run(hosts_inject_cmd(cname, pairs))
    if not okk and not runner.dry:
        warn(f"[{svc}] could not write /etc/hosts (image may have no shell, e.g. distroless). "
             f"Use the injected <SERVICE>_HOST env vars instead.")

def first_container_port(spec, svc):
    pubs = parse_ports(svc, (spec or {}).get("ports"))
    return pubs[0][2] if pubs else None


# ---------- subcommands -------------------------------------------------------------
def cmd_up(args):
    files = resolve_files(args.file)
    data, directory = load_compose(files)
    services = data.get("services") or {}
    if not services: sys.exit("no services defined")
    project = project_of(args, data, files[0])
    network = f"{project}-net"
    runner = Runner(args.dry_run)

    info(f"project {BOLD}{project}{RESET}  ({len(services)} services)  network {network}")
    runner.run(container("network", "create", network), expect_fail_ok="exist")

    # topo order over plain service names
    plain = {s: {"depends_on": [d for d, _ in norm_depends((services[s] or {}).get("depends_on"))]}
             for s in services}
    order, done, temp = [], set(), set()
    def visit(n):
        if n in done: return
        if n in temp: sys.exit(f"circular depends_on detected at '{n}'")
        temp.add(n)
        for dep in plain[n]["depends_on"]:
            if dep in services: visit(dep)
            else: warn(f"'{n}' depends on unknown service '{dep}', ignoring")
        temp.discard(n); done.add(n); order.append(n)
    for s in services: visit(s)
    info("start order: " + " \u2192 ".join(order))

    ips, started = {}, []
    for svc in order:
        spec = services[svc] or {}
        for key in spec:
            if key in IGNORED:   warn(f"[{svc}] '{key}' {IGNORED[key]}")
            elif key not in SUPPORTED: warn(f"[{svc}] '{key}' not recognised — ignored")
        plat = str(spec.get("platform", ""))
        if "amd64" in plat or "x86" in plat:
            warn(f"[{svc}] platform '{plat}': x86 images are NOT seamless on Apple container — may fail to run")

        cname = cname_of(project, svc, spec)
        deps = norm_depends(spec.get("depends_on"))

        # honour service_healthy via TCP polling before starting the dependent
        for dep, cond in deps:
            if cond == "service_healthy" and not runner.dry:
                dep_ip = ips.get(dep)
                dep_port = first_container_port(services.get(dep), dep)
                if dep_ip and dep_port:
                    info(f"waiting for {dep} (service_healthy \u2192 TCP :{dep_port}, max {args.wait_timeout}s)")
                    wait_tcp(dep_ip, dep_port, args.wait_timeout, dep)
                else:
                    warn(f"[{svc}] cannot health-wait on '{dep}' (no IP/port known) — starting anyway")

        if "build" in spec:
            b = spec["build"]
            ctx = b if isinstance(b, str) else (b.get("context") or ".")
            ctx = ctx if os.path.isabs(ctx) else os.path.normpath(os.path.join(directory, ctx))
            image = spec.get("image") or f"{project}-{svc}"
            print(f"{BOLD}build{RESET} {svc}")
            runner.run(build_build_cmd(image, ctx, b), fatal=True)
        else:
            image = spec.get("image")
            if not image: sys.exit(f"service '{svc}' has neither image nor build")

        # env: env_file < environment < injected <DEP>_HOST fallbacks (always on)
        env = {**load_env_files(spec, directory), **norm_env(spec.get("environment"))}
        for dep, _ in deps:
            if ips.get(dep):
                env.setdefault(re.sub(r"[^A-Z0-9]", "_", dep.upper()) + "_HOST", ips[dep])

        volumes = []
        for v in (spec.get("volumes") or []):
            vs = v if isinstance(v, str) else f"{v.get('source','')}:{v.get('target','')}"
            src = vs.split(":")[0]
            if src and not src.startswith((".", "/", "~")):
                warn(f"[{svc}] named volume '{src}' is not supported — use a bind path; skipping this mount")
                continue
            if src.startswith("."):
                vs = os.path.normpath(os.path.join(directory, src)) + vs[len(src):]
            volumes.append(vs)

        pubs = [] if args.no_publish else parse_ports(svc, spec.get("ports"))
        print(f"{BOLD}run{RESET}   {svc}  {DIM}({cname}){RESET}")
        okk, _ = runner.run(build_run_cmd(cname, network, image, env, volumes,
                                          spec.get("working_dir"), norm_command(spec.get("command")), pubs))
        if not okk and not runner.dry:
            fail(f"[{svc}] failed to start \u2014 aborting (already-started services keep running; `down` to clean)")
            sys.exit(1)

        ips[svc] = get_ip(runner, cname)
        if not ips[svc] and not runner.dry:
            warn(f"[{svc}] could not determine IP \u2014 service-name DNS and *_HOST for it will be missing")

        # RACE FIX: wire hosts NOW, not at the end —
        # 1) this container learns every peer started so far (incl. itself)
        known = [(s, ips[s]) for s in started + [svc] if ips.get(s)]
        wire_hosts(runner, cname, known, svc)
        # 2) every earlier container learns this one
        if ips.get(svc):
            for prev in started:
                wire_hosts(runner, cname_of(project, prev, services[prev]), [(svc, ips[svc])], prev)
        started.append(svc)

    print(); ok("stack up")
    width = max((len(s) for s in order), default=4)
    for svc in order:
        spec = services[svc] or {}
        shown = []
        if not args.no_publish:
            for bind, hp, cp in parse_ports(svc, spec.get("ports")):
                if hp: shown.append(f"localhost:{hp}")
        tail = f"  {DIM}{', '.join(shown)}{RESET}" if shown else ""
        print(f"  {svc.ljust(width)}  {GREEN}{ips.get(svc) or '?'}{RESET}{tail}")
    print(f"\n{DIM}containers reach each other by service name via /etc/hosts; "
          f"<SERVICE>_HOST env vars are the fallback for shell-less images{RESET}")
    print(f"{DIM}after sleep/wake or restarts, run: acompose refresh{RESET}")

def cmd_refresh(args):
    files = resolve_files(args.file)
    data, _ = load_compose(files)
    services = data.get("services") or {}
    project = project_of(args, data, files[0])
    runner = Runner(args.dry_run)
    info(f"re-reading IPs for {BOLD}{project}{RESET} and rewriting /etc/hosts entries")
    ips = {}
    for svc, spec in services.items():
        ip = get_ip(runner, cname_of(project, svc, spec))
        if ip: ips[svc] = ip
        else:  warn(f"[{svc}] no IP (not running?)")
    pairs = sorted(ips.items())
    for svc, spec in services.items():
        cname = cname_of(project, svc, spec)
        # drop our old lines, then append fresh ones
        names = "|".join(re.escape(s) for s, _ in pairs) or "NOMATCH"
        cleanup = container("exec", cname, "sh", "-c",
            f"grep -vE '\\s({names})$' /etc/hosts > /tmp/h && cat /tmp/h > /etc/hosts")
        runner.run(cleanup)
        wire_hosts(runner, cname, pairs, svc)
    ok("refreshed")

def cmd_down(args):
    files = resolve_files(args.file)
    data, _ = load_compose(files)
    services = data.get("services") or {}
    project = project_of(args, data, files[0])
    runner = Runner(args.dry_run)
    info(f"tearing down {BOLD}{project}{RESET}")
    names = list(services)
    for svc in reversed(names):
        cname = cname_of(project, svc, services[svc])
        runner.run(container("stop", cname), expect_fail_ok="no")
        runner.run(container("delete", cname), expect_fail_ok="no")
        print(f"  removed {svc}")
    runner.run(container("network", "delete", f"{project}-net"), expect_fail_ok="no")
    ok("down")

def cmd_ps(args):
    files = resolve_files(args.file) if (args.file or any(os.path.exists(f) for f in DEFAULT_FILES)) else None
    project = None
    if files:
        data, _ = load_compose(files)
        project = project_of(args, data, files[0])
    okk, out = Runner(False).run(container("ls", "--all"), capture=True)
    if not okk: return
    lines = out.splitlines()
    if not lines: return
    print(lines[0])
    for line in lines[1:]:
        if project is None or f"{project}-" in line:
            print(line)

def cmd_logs(args):
    files = resolve_files(args.file)
    data, _ = load_compose(files)
    project = project_of(args, data, files[0])
    spec = (data.get("services") or {}).get(args.service)
    cname = cname_of(project, args.service, spec)
    cmd = container("logs")
    if args.follow: cmd.append("--follow")
    cmd.append(cname)
    subprocess.run(cmd) if not args.follow else subprocess.run(cmd)

def cmd_exec(args):
    files = resolve_files(args.file)
    data, _ = load_compose(files)
    project = project_of(args, data, files[0])
    spec = (data.get("services") or {}).get(args.service)
    cmdline = args.command[1:] if args.command[:1] == ["--"] else args.command
    subprocess.run(container("exec", "--tty", "--interactive",
                             cname_of(project, args.service, spec), *cmdline))

def cmd_build(args):
    files = resolve_files(args.file)
    data, directory = load_compose(files)
    project = project_of(args, data, files[0])
    runner = Runner(args.dry_run)
    for svc, spec in (data.get("services") or {}).items():
        if spec and "build" in spec:
            b = spec["build"]
            ctx = b if isinstance(b, str) else (b.get("context") or ".")
            ctx = ctx if os.path.isabs(ctx) else os.path.normpath(os.path.join(directory, ctx))
            print(f"{BOLD}build{RESET} {svc}")
            runner.run(build_build_cmd(spec.get("image") or f"{project}-{svc}", ctx, b))


def main():
    p = argparse.ArgumentParser(prog="acompose",
        description="docker-compose.yml on Apple's `container` CLI — drop-in where possible, loud where not.")
    sub = p.add_subparsers(dest="cmd", required=True)
    def common(sp, file=True):
        if file: sp.add_argument("--file", action="append",
                                 help="compose file; repeat to merge (override file auto-merged otherwise)")
        sp.add_argument("-p", "--project")

    up = sub.add_parser("up"); common(up)
    up.add_argument("-d", "--detach", action="store_true", help="accepted for compatibility (always detached)")
    up.add_argument("--dry-run", action="store_true")
    up.add_argument("--no-publish", action="store_true")
    up.add_argument("--wait-timeout", type=int, default=30,
                    help="seconds to TCP-wait on service_healthy deps (default 30)")
    up.set_defaults(func=cmd_up)

    for name, fn, extra in (("down", cmd_down, True), ("refresh", cmd_refresh, True),
                            ("build", cmd_build, True)):
        sp = sub.add_parser(name); common(sp)
        if extra: sp.add_argument("--dry-run", action="store_true")
        if name == "down": sp.add_argument("-v", "--volumes", action="store_true",
                                           help="accepted for compatibility (no named volumes here)")
        sp.set_defaults(func=fn)

    ps = sub.add_parser("ps"); common(ps); ps.set_defaults(func=cmd_ps)

    logs = sub.add_parser("logs"); common(logs)
    logs.add_argument("service"); logs.add_argument("-f", "--follow", action="store_true")
    logs.set_defaults(func=cmd_logs)

    ex = sub.add_parser("exec"); common(ex)
    ex.add_argument("service"); ex.add_argument("command", nargs=argparse.REMAINDER)
    ex.set_defaults(func=cmd_exec)

    args = p.parse_args()
    args.func(args)

if __name__ == "__main__":
    main()
