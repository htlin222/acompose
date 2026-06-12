package main

// Tests for `acompose doctor`: every pure verdict function is table-tested,
// and the full doctorRun is exercised against fakeContainer scripts with an
// injected doctorEnv so the run is deterministic on any OS/arch (CI is
// Ubuntu, the dev box is a Mac — neither may leak into assertions).

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckArch(t *testing.T) {
	t.Run("arm64 passes", func(t *testing.T) {
		v := checkArch("arm64")
		if v.status != vOK || !strings.Contains(v.msg, "arm64") {
			t.Errorf("checkArch(arm64) = %+v, want vOK", v)
		}
	})
	t.Run("amd64 fails with hint", func(t *testing.T) {
		v := checkArch("amd64")
		if v.status != vFail {
			t.Errorf("checkArch(amd64).status = %v, want vFail", v.status)
		}
		mustContain(t, v.msg+" "+v.hint, "verdict", "amd64", "Apple Silicon")
	})
}

func TestParseMacOSVersion(t *testing.T) {
	cases := []struct {
		name, in string
		want     verdictStatus
	}{
		{"26.0 passes", "26.0", vOK},
		{"26.1.1 with trailing newline passes", "26.1.1\n", vOK},
		{"27 passes", "27.0", vOK},
		{"15.5 too old", "15.5", vFail},
		{"14 too old", "14.7.2", vFail},
		{"empty output", "", vWarn},
		{"garbage", "beta-build", vWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := parseMacOSVersion(tc.in)
			if v.status != tc.want {
				t.Errorf("parseMacOSVersion(%q) = %+v, want status %v", tc.in, v, tc.want)
			}
			if tc.want != vOK && v.hint == "" {
				t.Errorf("non-OK verdict must carry a hint: %+v", v)
			}
		})
	}
	t.Run("too-old hint names the requirement", func(t *testing.T) {
		mustContain(t, parseMacOSVersion("15.5").hint, "hint", "macOS 26")
	})
}

func TestCheckMacOS(t *testing.T) {
	t.Run("non-darwin fails", func(t *testing.T) {
		v := checkMacOS("linux", func() (string, error) { t.Fatal("must not run sw_vers"); return "", nil })
		if v.status != vFail {
			t.Errorf("status = %v, want vFail", v.status)
		}
		mustContain(t, v.msg+" "+v.hint, "verdict", "linux", "only runs on macOS")
	})
	t.Run("darwin asks sw_vers", func(t *testing.T) {
		v := checkMacOS("darwin", func() (string, error) { return "26.0\n", nil })
		if v.status != vOK || !strings.Contains(v.msg, "macOS 26.0") {
			t.Errorf("verdict = %+v, want vOK macOS 26.0", v)
		}
	})
	t.Run("sw_vers failure is a warning, not a crash", func(t *testing.T) {
		v := checkMacOS("darwin", func() (string, error) { return "", errors.New("exec failed") })
		if v.status != vWarn {
			t.Errorf("status = %v, want vWarn", v.status)
		}
	})
}

func TestParseContainerVersion(t *testing.T) {
	cases := []struct {
		name, in, want string
		found          bool
	}{
		{"real output shape", "container CLI version 1.0.0 (build: release, commit: deadbee)", "1.0.0", true},
		{"bare semver", "1.2.3", "1.2.3", true},
		{"pre-release keeps the core", "version 0.5.0-beta.1", "0.5.0", true},
		{"missing version", "container CLI", "", false},
		{"empty", "", "", false},
		{"garbage", "no numbers here", "", false},
		{"two-part version is not semver", "version 1.0", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := parseContainerVersion(tc.in)
			if got != tc.want || found != tc.found {
				t.Errorf("parseContainerVersion(%q) = (%q, %v), want (%q, %v)", tc.in, got, found, tc.want, tc.found)
			}
		})
	}
}

func TestCheckCLIVersion(t *testing.T) {
	t.Run("tested version passes", func(t *testing.T) {
		v := checkCLIVersion("container CLI version " + testedContainerVersion + " (build: release)")
		if v.status != vOK || !strings.Contains(v.msg, testedContainerVersion) {
			t.Errorf("verdict = %+v, want vOK mentioning %s", v, testedContainerVersion)
		}
	})
	t.Run("other version warns, does not fail", func(t *testing.T) {
		v := checkCLIVersion("container CLI version 2.0.0")
		if v.status != vWarn {
			t.Errorf("status = %v, want vWarn", v.status)
		}
		mustContain(t, v.msg+" "+v.hint, "verdict",
			"2.0.0", "untested version",
			"tested against "+testedContainerVersion, "open an issue")
	})
	t.Run("unparseable output warns", func(t *testing.T) {
		v := checkCLIVersion("flag provided but not defined")
		if v.status != vWarn || !strings.Contains(v.msg, "unknown") {
			t.Errorf("verdict = %+v, want vWarn unknown", v)
		}
	})
}

func TestCheckSystemStatus(t *testing.T) {
	cases := []struct {
		name, in string
		want     verdictStatus
	}{
		{"running", "apiserver is running", vOK},
		{"running with noise around it", "Verifying...\napiserver is running\n", vOK},
		{"explicitly not running", "apiserver is not running", vFail},
		{"stopped", "service stopped", vFail},
		{"empty output", "", vFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := checkSystemStatus(tc.in)
			if v.status != tc.want {
				t.Errorf("checkSystemStatus(%q) = %+v, want status %v", tc.in, v, tc.want)
			}
		})
	}
	t.Run("failure hint says how to start it", func(t *testing.T) {
		mustContain(t, checkSystemStatus("").hint, "hint", "container system start")
	})
}

func TestCheckComposeFile(t *testing.T) {
	t.Run("empty directory warns with init hint", func(t *testing.T) {
		chdir(t, t.TempDir())
		v := checkComposeFile()
		if v.status != vWarn {
			t.Errorf("status = %v, want vWarn", v.status)
		}
		mustContain(t, v.msg+" "+v.hint, "verdict", "no compose file here", "acompose init")
	})
	for _, name := range defaultComposeNames {
		t.Run("finds "+name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("services: {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			chdir(t, dir)
			v := checkComposeFile()
			if v.status != vOK || v.msg != "found "+name {
				t.Errorf("verdict = %+v, want vOK found %s", v, name)
			}
		})
	}
}

// testDoctorEnv simulates a healthy Apple Silicon Mac; the `container` CLI
// itself comes from the fakeContainer PATH shadow.
func testDoctorEnv() doctorEnv {
	return doctorEnv{
		goarch: "arm64",
		goos:   "darwin",
		swVers: func() (string, error) { return "26.1\n", nil },
		cliVersion: func() (string, error) {
			out, err := exec.Command("container", "--version").CombinedOutput()
			return string(out), err
		},
		lookPath: exec.LookPath,
		r:        runner{},
	}
}

func TestDoctorRunAllGood(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	fakeContainer(t, `case "$1" in
  --version) echo "container CLI version 1.0.0 (build: release, commit: deadbee)" ;;
  system) echo "apiserver is running" ;;
esac
exit 0`)
	var problems int
	stdout, stderr := captureOutput(t, func() {
		problems = doctorRun(testDoctorEnv())
	})
	if problems != 0 {
		t.Errorf("problems = %d, want 0; output:\n%s", problems, stdout)
	}
	mustContain(t, stdout, "stdout",
		"✓ architecture: arm64 (Apple Silicon)",
		"✓ macOS 26.1",
		"✓ container CLI: ",
		"✓ container CLI version 1.0.0 (tested)",
		"✓ container system service is running",
		"✓ found docker-compose.yml",
		"ready — try: acompose up")
	mustNotContain(t, stdout, "stdout", "!", "✗", "problem(s) found")
	mustNotContain(t, stderr, "stderr", "command failed")
}

func TestDoctorRunOldCLIVersion(t *testing.T) {
	chdir(t, t.TempDir())
	fakeContainer(t, `case "$1" in
  --version) echo "container CLI version 0.9.0" ;;
  system) echo "apiserver is running" ;;
esac
exit 0`)
	var problems int
	stdout, _ := captureOutput(t, func() {
		problems = doctorRun(testDoctorEnv())
	})
	if problems != 0 {
		t.Errorf("problems = %d, want 0 (untested version is a warning)", problems)
	}
	mustContain(t, stdout, "stdout",
		"! container CLI version 0.9.0 — untested version",
		"tested against 1.0.0",
		"! no compose file here",   // chdir'd to an empty dir
		"ready — try: acompose up") // warnings do not block readiness
	mustNotContain(t, stdout, "stdout", "✗")
}

// A `container --version` that fails outright (non-zero exit, garbage
// output) must degrade to the "unknown" warning — never a loud mid-report
// "command failed:".
func TestDoctorRunVersionProbeFailureIsQuiet(t *testing.T) {
	chdir(t, t.TempDir())
	fakeContainer(t, `case "$1" in
  --version) echo "Error: unrecognized flag"; exit 1 ;;
  system) echo "apiserver is running" ;;
esac
exit 0`)
	var problems int
	stdout, stderr := captureOutput(t, func() {
		problems = doctorRun(testDoctorEnv())
	})
	if problems != 0 {
		t.Errorf("problems = %d, want 0 (an unparseable version is a warning)", problems)
	}
	mustContain(t, stdout, "stdout",
		"! container CLI version: unknown",
		"✓ container system service is running",
		"ready — try: acompose up")
	mustNotContain(t, stderr, "stderr", "command failed")
	mustNotContain(t, stdout, "stdout", "✗")
}

func TestDoctorRunServiceStopped(t *testing.T) {
	chdir(t, t.TempDir())
	fakeContainer(t, `case "$1" in
  --version) echo "container CLI version 1.0.0" ;;
  system) echo "apiserver is not running" >&2; echo "apiserver is not running"; exit 1 ;;
esac
exit 0`)
	var problems int
	stdout, stderr := captureOutput(t, func() {
		problems = doctorRun(testDoctorEnv())
	})
	if problems != 1 {
		t.Errorf("problems = %d, want 1", problems)
	}
	mustContain(t, stdout, "stdout",
		"✗ container system service not running — start it: container system start")
	mustContain(t, stderr, "stderr", "1 problem(s) found")
	mustNotContain(t, stdout, "stdout", "ready — try: acompose up")
	// the stopped state is an expected, tolerated failure — never a loud one
	mustNotContain(t, stderr, "stderr", "command failed")
}

func TestDoctorRunContainerMissing(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("PATH", t.TempDir()) // empty PATH: exec.LookPath cannot find `container`
	var problems int
	stdout, stderr := captureOutput(t, func() {
		problems = doctorRun(testDoctorEnv())
	})
	if problems != 1 {
		t.Errorf("problems = %d, want 1", problems)
	}
	mustContain(t, stdout, "stdout",
		"✗ container CLI not found — install it: https://github.com/apple/container/releases",
		"! container CLI version: skipped",
		"! container system service: skipped")
	mustContain(t, stderr, "stderr", "1 problem(s) found")
}

func TestDoctorRunEverythingWrong(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("PATH", t.TempDir())
	env := doctorEnv{
		goarch:   "amd64",
		goos:     "linux",
		swVers:   func() (string, error) { return "", errors.New("no sw_vers") },
		lookPath: exec.LookPath,
		r:        runner{},
	}
	var problems int
	_, stderr := captureOutput(t, func() {
		problems = doctorRun(env)
	})
	if problems != 3 { // arch + OS + missing CLI
		t.Errorf("problems = %d, want 3", problems)
	}
	mustContain(t, stderr, "stderr", "3 problem(s) found")
}

// cmdDoctor exits 1 when problems exist — re-exec'd with an empty PATH so the
// `container` CLI is guaranteed missing regardless of the host machine.
func TestCmdDoctorExitCodeSubprocess(t *testing.T) {
	if os.Getenv("ACOMPOSE_TEST_DOCTOR") == "1" {
		os.Setenv("PATH", os.Getenv("ACOMPOSE_TEST_EMPTY_DIR"))
		cmdDoctor()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCmdDoctorExitCodeSubprocess$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"ACOMPOSE_TEST_DOCTOR=1",
		"ACOMPOSE_TEST_EMPTY_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("want exit code 1, got err=%v, output:\n%s", err, out)
	}
	mustContain(t, string(out), "subprocess output", "container CLI not found", "problem(s) found")
}
