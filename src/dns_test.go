package main

// `acompose dns` tests — all hermetic: the runner either runs dry (pure
// printer) or executes the fakeContainer PATH shadow; the real Apple
// `container` binary is never touched. dnsRun returns the exit code, so no
// subprocess re-exec is needed to assert failure paths.

import (
	"testing"
	"time"
)

// dnsFixtureYAML: api depends on db, so toposort (and therefore every printed
// name list) is db → api — the determinism the assertions below rely on.
const dnsFixtureYAML = `services:
  db:
    image: postgres
  api:
    image: nginx
    depends_on: [db]
`

func TestDNSDomain(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	if got := dnsDomain(p); got != "proj" {
		t.Errorf("dnsDomain = %q, want proj", got)
	}
}

func TestDNSOutputLists(t *testing.T) {
	cases := []struct {
		name, out string
		want      bool
	}{
		{"bare domain line", "proj\n", true},
		{"tabular output with header", "DOMAIN  DEFAULT\nproj    true\n", true},
		{"among other domains", "alpha\nproj\nzulu\n", true},
		{"absent", "other\n", false},
		{"no substring match", "projx\nxproj\n", false},
		{"empty output", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dnsOutputLists(tc.out, "proj"); got != tc.want {
				t.Errorf("dnsOutputLists(%q, proj) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestDNSHostNames(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	got := dnsHostNames(p, "proj")
	want := []string{"proj-db.proj", "proj-api.proj"} // toposort order
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("dnsHostNames = %v, want %v", got, want)
	}
}

func TestDNSPermissionError(t *testing.T) {
	for msg, want := range map[string]bool{
		"dns create: must run as an administrator": true,
		"Error: permission denied":                 true,
		"operation not permitted":                  true,
		"domain proj not found":                    false,
		"":                                         false,
	} {
		if got := dnsPermissionError(msg); got != want {
			t.Errorf("dnsPermissionError(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestDNSStatusConfigured(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	// list shows the domain; db is running with an IP, api was never created
	fakeContainer(t, `if [ "$1" = system ]; then echo proj; exit 0; fi
case "$1" in
  ls) echo 'ID IMAGE STATE'; echo 'proj-db postgres running' ;;
  inspect) echo '{"address":"192.168.64.5/24"}' ;;
esac
exit 0`)
	var code int
	stdout, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "status") })
	if code != 0 {
		t.Errorf("status exit code = %d, want 0", code)
	}
	mustContain(t, stdout, "stdout",
		"local DNS domain 'proj' configured",
		"proj-db.proj",
		"192.168.64.5",
		"proj-api.proj",
		"not running")
	mustOrder(t, stdout, "stdout", "proj-db.proj", "proj-api.proj") // toposort order
	mustNotContain(t, stderr, "stderr", "command failed")
}

func TestDNSStatusNotConfigured(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	fakeContainer(t, `exit 0`)                   // dns list prints nothing
	for _, sub := range []string{"status", ""} { // bare `acompose dns` == status
		t.Run("sub="+sub, func(t *testing.T) {
			var code int
			stdout, _ := captureOutput(t, func() { code = dnsRun(p, runner{}, sub) })
			if code != 0 {
				t.Errorf("status exit code = %d, want 0", code)
			}
			mustContain(t, stdout, "stdout",
				"local DNS domain 'proj' not configured",
				"acompose dns setup")
			mustNotContain(t, stdout, "stdout", "proj-db.proj")
		})
	}
}

func TestDNSStatusListFailureIsQuietButNonZero(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	fakeContainer(t, `echo "Error: dns subsystem unavailable"; exit 1`)
	var code int
	_, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "status") })
	if code != 1 {
		t.Errorf("status exit code = %d, want 1", code)
	}
	mustContain(t, stderr, "stderr", "could not query local DNS domains")
	mustNotContain(t, stderr, "stderr", "command failed") // probe failure is tolerated
}

func TestDNSSetupAlreadyConfigured(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	// create would refuse — proving setup never calls it when already listed
	fakeContainer(t, `if [ "$1" = system ] && [ "$3" = list ]; then echo proj; exit 0; fi
if [ "$1" = system ] && [ "$3" = create ]; then echo "must run as an administrator"; exit 1; fi
case "$1" in
  ls) echo 'ID IMAGE STATE'; echo 'proj-db postgres running' ;;
  inspect) echo '{"address":"192.168.64.5/24"}' ;;
esac
exit 0`)
	var code int
	stdout, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "setup") })
	if code != 0 {
		t.Errorf("setup exit code = %d, want 0", code)
	}
	mustContain(t, stdout, "stdout",
		"local DNS domain 'proj' already configured",
		"proj-db.proj",
		"192.168.64.5")
	mustNotContain(t, stderr, "stderr", "sudo", "command failed")
}

func TestDNSSetupCreateSucceeds(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	fakeContainer(t, `if [ "$1" = system ] && [ "$3" = list ]; then exit 0; fi
if [ "$1" = system ] && [ "$3" = create ]; then exit 0; fi
case "$1" in
  ls) echo 'ID IMAGE STATE'; echo 'proj-db postgres running' ;;
  inspect) echo '{"address":"192.168.64.5/24"}' ;;
esac
exit 0`)
	var code int
	stdout, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "setup") })
	if code != 0 {
		t.Errorf("setup exit code = %d, want 0", code)
	}
	mustContain(t, stdout, "stdout",
		"local DNS domain 'proj' created",
		"proj-db.proj",
		"192.168.64.5",
		"proj-api.proj",
		"not running",
		"new containers pick this up automatically; restart existing ones with acompose down && acompose up")
	mustOrder(t, stdout, "stdout", "proj-db.proj", "proj-api.proj")
	mustNotContain(t, stderr, "stderr", "sudo", "command failed")
}

func TestDNSSetupNeedsAdmin(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	fakeContainer(t, `if [ "$1" = system ] && [ "$3" = list ]; then exit 0; fi
echo "dns create: must run as an administrator"; exit 1`)
	var code int
	_, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "setup") })
	if code != 1 {
		t.Errorf("setup exit code = %d, want 1", code)
	}
	mustContain(t, stderr, "stderr",
		"needs admin rights",
		"sudo container system dns create proj",
		"one-time, creates /etc/resolver/proj")
	mustNotContain(t, stderr, "stderr", "command failed") // tolerated, not loud
}

func TestDNSTeardown(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)

	t.Run("success", func(t *testing.T) {
		fakeContainer(t, `exit 0`)
		var code int
		stdout, _ := captureOutput(t, func() { code = dnsRun(p, runner{}, "teardown") })
		if code != 0 {
			t.Errorf("teardown exit code = %d, want 0", code)
		}
		mustContain(t, stdout, "stdout", "local DNS domain 'proj' removed")
	})

	t.Run("not found is tolerated", func(t *testing.T) {
		fakeContainer(t, `echo "domain proj not found"; exit 1`)
		var code int
		stdout, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "teardown") })
		if code != 0 {
			t.Errorf("teardown exit code = %d, want 0", code)
		}
		mustContain(t, stdout, "stdout", "was not configured — nothing to remove")
		mustNotContain(t, stderr, "stderr", "command failed")
	})

	t.Run("permission error prints the sudo command", func(t *testing.T) {
		fakeContainer(t, `echo "operation not permitted"; exit 1`)
		var code int
		_, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "teardown") })
		if code != 1 {
			t.Errorf("teardown exit code = %d, want 1", code)
		}
		mustContain(t, stderr, "stderr",
			"needs admin rights",
			"sudo container system dns delete proj")
		mustNotContain(t, stderr, "stderr", "command failed")
	})
}

func TestDNSUnknownSubcommand(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	var code int
	_, stderr := captureOutput(t, func() { code = dnsRun(p, runner{}, "bogus") })
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	mustContain(t, stderr, "stderr", "unknown dns subcommand 'bogus'")
}

// dnsRun refuses --dry-run outright (like dev/watch): a dry runner pretends
// every command succeeded, so `dns setup --dry-run` would fabricate
// "✓ ... created" for something that never ran.
func TestDNSDryRunRefused(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	for _, sub := range []string{"", "status", "setup", "teardown"} {
		t.Run("sub="+sub, func(t *testing.T) {
			var code int
			stdout, stderr := captureOutput(t, func() { code = dnsRun(p, runner{dry: true}, sub) })
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			mustContain(t, stderr, "stderr", "dns needs a live runtime — it cannot be combined with --dry-run")
			mustNotContain(t, stdout, "stdout", "created", "configured", "removed")
		})
	}
}

// cmdUp in dry-run must not probe dns at all: dry prints every runner call,
// and the CI assertions on the examples transcript must stay byte-stable.
func TestCmdUpDryRunHasNoDNSProbe(t *testing.T) {
	p := loadUpFixture(t)
	stdout, _ := captureOutput(t, func() {
		cmdUp(p, runner{dry: true}, true, time.Second)
	})
	mustNotContain(t, stdout, "stdout", "system dns", "host DNS:")
	// the protected transcript shape is intact (CI greps the examples/ run
	// for the same patterns; the fixture's app port is 8080:80)
	mustContain(t, stdout, "stdout",
		"start order: db → app",
		"--publish 5432:5432",
		"--publish 127.0.0.1:8080:80",
		"container build --tag proj-app")
}

func TestCmdUpRealShowsHostDNSWhenConfigured(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	fakeContainer(t, `if [ "$1" = system ]; then echo proj; exit 0; fi
case "$1" in
  inspect) echo '{"address":"192.168.64.9/24"}' ;;
  ls) echo 'ID IMAGE STATE'; echo 'proj-db postgres running'; echo 'proj-api nginx running' ;;
esac
exit 0`)
	stdout, _ := captureOutput(t, func() {
		cmdUp(p, runner{}, true, time.Nanosecond)
	})
	mustContain(t, stdout, "stdout",
		"stack up",
		"host DNS: proj-db.proj, proj-api.proj") // toposort order, one line
}

func TestCmdUpRealNoHostDNSWhenUnconfigured(t *testing.T) {
	p := projectFromYAML(t, dnsFixtureYAML)
	fakeContainer(t, `case "$1" in
  inspect) echo '{"address":"192.168.64.9/24"}' ;;
  ls) echo 'ID IMAGE STATE'; echo 'proj-db postgres running'; echo 'proj-api nginx running' ;;
esac
exit 0`) // dns list prints nothing → domain not configured
	stdout, _ := captureOutput(t, func() {
		cmdUp(p, runner{}, true, time.Nanosecond)
	})
	mustContain(t, stdout, "stdout", "stack up")
	mustNotContain(t, stdout, "stdout", "host DNS:")
}
