package main

// Shared test helpers. Nothing in here ever executes the real `container`
// binary: every command-running test goes through runner{dry: true}, which
// prints the would-be command to stdout instead of exec'ing it.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// captureOutput swaps os.Stdout/os.Stderr for pipes around fn and returns
// everything written to each. The package color variables were evaluated at
// init() against the real (non-TTY under `go test`) stdout, so all captured
// output is ANSI-free regardless of the swap.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	// drain concurrently so a large write can never fill the pipe buffer
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := io.ReadAll(rOut); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(rErr); errCh <- string(b) }()

	fn()
	wOut.Close()
	wErr.Close()
	return <-outCh, <-errCh
}

// projectFromFiles writes the given files into a fresh temp dir and loads
// "docker-compose.yml" from it through the production loadProject path.
func projectFromFiles(t *testing.T, name string, files map[string]string) *types.Project {
	t.Helper()
	dir := t.TempDir()
	for fn, content := range files {
		if err := os.WriteFile(filepath.Join(dir, fn), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return loadProject([]string{filepath.Join(dir, "docker-compose.yml")}, name)
}

// projectFromYAML is the single-file convenience form of projectFromFiles.
func projectFromYAML(t *testing.T, yaml string) *types.Project {
	t.Helper()
	return projectFromFiles(t, "proj", map[string]string{"docker-compose.yml": yaml})
}

// chdir switches the working directory for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatal(err)
		}
	})
}

// mustContain / mustNotContain keep the dry-run assertions readable.
func mustContain(t *testing.T, haystack, what string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("%s: missing %q in:\n%s", what, n, haystack)
		}
	}
}

func mustNotContain(t *testing.T, haystack, what string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			t.Errorf("%s: unexpected %q in:\n%s", what, n, haystack)
		}
	}
}

// mustOrder asserts that each needle appears, and in the given order.
func mustOrder(t *testing.T, haystack, what string, needles ...string) {
	t.Helper()
	last, lastNeedle := -1, "(start)"
	for _, n := range needles {
		i := strings.Index(haystack, n)
		if i < 0 {
			t.Errorf("%s: missing %q in:\n%s", what, n, haystack)
			return
		}
		if i < last {
			t.Errorf("%s: %q appears before %q, want after, in:\n%s", what, n, lastNeedle, haystack)
			return
		}
		last, lastNeedle = i, n
	}
}

// upFixtureYAML is the canonical two-service stack used by the dry-run
// end-to-end tests: dependency ordering, healthcheck condition, build
// section, named + bind volumes, host-IP port form, ${VAR:-default}
// interpolation, and enough unsupported keys to trip every warnUnsupported
// branch we care about.
const upFixtureYAML = `services:
  db:
    image: postgres:${AC_TEST_TAG}
    container_name: mydb
    restart: always
    ports:
      - "5432:5432"
    volumes:
      - data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD", "pg_isready"]
      interval: 5s
    deploy:
      replicas: 1
  app:
    build:
      context: .
    user: nobody
    entrypoint: /entry.sh
    working_dir: /srv
    ports:
      - "127.0.0.1:8080:80"
    environment:
      - GREETING=${AC_TEST_GREETING:-hello}
    volumes:
      - ./web:/usr/share/nginx/html
    depends_on:
      db:
        condition: service_healthy
volumes:
  data:
`

// loadUpFixture loads upFixtureYAML with a sibling .env supplying AC_TEST_TAG.
func loadUpFixture(t *testing.T) *types.Project {
	t.Helper()
	return projectFromFiles(t, "proj", map[string]string{
		"docker-compose.yml": upFixtureYAML,
		".env":               "AC_TEST_TAG=16\n",
	})
}
