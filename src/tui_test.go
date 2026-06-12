package main

// Hermetic unit tests for `acompose top`. None of these run tea.Program.Run or
// touch a real container: the model's data access is injected (fetchState /
// fetchLogs / doAction), and Update is exercised as a pure reducer over
// synthetic tea.KeyMsg + our own message types. View() is asserted to render
// (and never panic) across the required matrix.

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// ---------- builders ---------------------------------------------------------

// stubState builds a uiState with the given (name,state) pairs in order.
func stubState(project string, svcs ...[2]string) uiState {
	st := uiState{Project: project, Network: project + "-net", Time: "12:00:00"}
	for _, s := range svcs {
		st.Order = append(st.Order, s[0])
		st.Services = append(st.Services, svcState{Name: s[0], Cname: project + "-" + s[0], State: s[1]})
	}
	return st
}

// newStubModel returns a model with no-op data funcs and a non-zero size so
// View() takes the real (non-placeholder) path. The state is preloaded.
func newStubModel(st uiState) topModel {
	m := initialModelBare("test")
	m.fetchState = func() uiState { return st }
	m.fetchLogs = func(string) []string { return nil }
	m.doAction = func(string, string) (bool, string) { return true, "ok" }
	m.st = st
	m.project = st.Project
	m.width, m.height = 80, 24
	m.layoutViewport()
	return m
}

// initialModelBare constructs a model without a *types.Project (tests don't
// need the live wiring); it mirrors initialModel's viewport setup.
func initialModelBare(project string) topModel {
	return newStubModelSkeleton(project)
}

func newStubModelSkeleton(project string) topModel {
	m := topModel{project: project}
	m.fetchState = func() uiState { return uiState{} }
	m.fetchLogs = func(string) []string { return nil }
	m.doAction = func(string, string) (bool, string) { return true, "" }
	m.logViewport = viewport.New(0, 0)
	return m
}

// send drives one message through Update and type-asserts back to topModel.
func send(t *testing.T, m topModel, msg tea.Msg) (topModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	tm, ok := next.(topModel)
	if !ok {
		t.Fatalf("Update returned %T, want topModel", next)
	}
	return tm, cmd
}

// key builds a tea.KeyMsg for a rune key (e.g. 's', 'j').
func key(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// ---------- navigation -------------------------------------------------------

func TestTopNavigationClamps(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}, [2]string{"api", "stopped"}, [2]string{"web", "missing"}))

	// down past the end clamps at last index
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyDown}) // 4th down, only 3 services
	if m.sel != 2 {
		t.Fatalf("sel after 3 downs = %d, want 2 (clamped)", m.sel)
	}
	// 'j' is also down — already clamped, stays
	m, _ = send(t, m, key('j'))
	if m.sel != 2 {
		t.Fatalf("sel after extra j = %d, want 2", m.sel)
	}

	// up past the top clamps at 0
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyUp})
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.sel != 0 {
		t.Fatalf("sel after 3 ups = %d, want 0 (clamped)", m.sel)
	}
	m, _ = send(t, m, key('k')) // 'k' is up
	if m.sel != 0 {
		t.Fatalf("sel after extra k = %d, want 0", m.sel)
	}
}

func TestTopStateMsgClampsSelection(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"a", "running"}, [2]string{"b", "running"}, [2]string{"c", "running"}))
	m.sel = 2 // on the last of 3

	// a refresh arrives with only one service → selection must clamp to 0
	m, _ = send(t, m, stateMsg{st: stubState("proj", [2]string{"a", "running"})})
	if m.sel != 0 {
		t.Fatalf("sel after shrinking state = %d, want 0", m.sel)
	}

	// and clamps to 0 with zero services (no panic, no negative index)
	m, _ = send(t, m, stateMsg{st: stubState("proj")})
	if m.sel != 0 {
		t.Fatalf("sel with zero services = %d, want 0", m.sel)
	}
}

// ---------- action -----------------------------------------------------------

func TestTopActionStartStop(t *testing.T) {
	type call struct{ service, op string }
	var calls []call
	record := func(service, op string) (bool, string) {
		calls = append(calls, call{service, op})
		return true, op + "ped" // "stopped" / "started"-ish; only used for verb
	}

	m := newStubModel(stubState("proj", [2]string{"api", "running"}, [2]string{"db", "stopped"}))
	m.doAction = record

	// 's' on a running service → stop, busy + status set
	m, cmd := send(t, m, key('s'))
	if !m.busy {
		t.Error("expected busy after pressing s")
	}
	if !strings.Contains(m.status, "stopping api") {
		t.Errorf("status = %q, want stopping api", m.status)
	}
	// the returned cmd carries the action; execute it to get the actionMsg
	msg := cmd()
	am, ok := msg.(actionMsg)
	if !ok {
		t.Fatalf("action cmd returned %T, want actionMsg", msg)
	}
	if len(calls) != 1 || calls[0] != (call{"api", "stop"}) {
		t.Fatalf("calls = %+v, want one {api stop}", calls)
	}
	// applying the actionMsg clears busy and sets the result status
	m, _ = send(t, m, am)
	if m.busy {
		t.Error("busy should clear after actionMsg")
	}
	if !strings.Contains(m.status, "api") {
		t.Errorf("status after actionMsg = %q", m.status)
	}

	// move to the stopped service and press 's' → start
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m, cmd = send(t, m, key('s'))
	if !strings.Contains(m.status, "starting db") {
		t.Errorf("status = %q, want starting db", m.status)
	}
	_ = cmd()
	if len(calls) != 2 || calls[1] != (call{"db", "start"}) {
		t.Fatalf("calls = %+v, want second {db start}", calls)
	}
}

func TestTopActionErrorIsRed(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"api", "stopped"}))
	m, _ = send(t, m, actionMsg{service: "api", ok: false, detail: "image pull failed"})
	if !m.statusErr {
		t.Error("statusErr should be true on a failed action")
	}
	if !strings.Contains(m.status, "image pull failed") {
		t.Errorf("status = %q, want the error detail", m.status)
	}

	// ok action with a "stop" detail reports "stopped"
	m, _ = send(t, m, actionMsg{service: "api", ok: true, detail: "stopped"})
	if m.statusErr {
		t.Error("statusErr should clear on a successful action")
	}
	if !strings.Contains(m.status, "api stopped") {
		t.Errorf("status = %q, want api stopped", m.status)
	}
}

func TestTopActionIgnoredWhenBusy(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"api", "running"}))
	called := 0
	m.doAction = func(string, string) (bool, string) { called++; return true, "" }
	m.busy = true
	m, cmd := send(t, m, key('s'))
	if cmd != nil {
		t.Error("expected no action cmd while busy")
	}
	if called != 0 {
		t.Error("doAction must not be invoked while busy")
	}
	_ = m
}

// ---------- mode switching ---------------------------------------------------

func TestTopLogsModeSwitch(t *testing.T) {
	var fetched string
	m := newStubModel(stubState("proj", [2]string{"db", "running"}, [2]string{"api", "running"}))
	m.fetchLogs = func(s string) []string { fetched = s; return []string{"line"} }

	// move to api, press 'l' → logs mode + a logs fetch for api
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m, cmd := send(t, m, key('l'))
	if m.mode != modeLogs {
		t.Fatalf("mode = %v, want modeLogs", m.mode)
	}
	if m.logService != "api" {
		t.Fatalf("logService = %q, want api", m.logService)
	}
	if cmd == nil {
		t.Fatal("expected a logs fetch cmd")
	}
	if msg, ok := cmd().(logsMsg); !ok || msg.service != "api" {
		t.Fatalf("logs cmd returned %#v, want logsMsg for api", cmd())
	}
	if fetched != "api" {
		t.Errorf("fetchLogs called with %q, want api", fetched)
	}

	// esc returns to the list
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeList {
		t.Fatalf("mode after esc = %v, want modeList", m.mode)
	}

	// enter also opens logs
	m, _ = send(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeLogs {
		t.Fatalf("mode after enter = %v, want modeLogs", m.mode)
	}
	// 'h' goes back too
	m, _ = send(t, m, key('h'))
	if m.mode != modeList {
		t.Fatalf("mode after h = %v, want modeList", m.mode)
	}
}

// ---------- logs population --------------------------------------------------

func TestTopLogsMsgPopulatesAndIgnoresOther(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}, [2]string{"api", "running"}))
	// enter logs mode for db
	m, _ = send(t, m, key('l'))
	if m.logService != "db" {
		t.Fatalf("logService = %q, want db", m.logService)
	}

	// a logsMsg for the focused service populates the viewport
	m, _ = send(t, m, logsMsg{service: "db", lines: []string{"hello", "world"}})
	if !strings.Contains(m.logViewport.View(), "hello") {
		t.Errorf("viewport should contain the db logs, got %q", m.logViewport.View())
	}

	// a logsMsg for a different service is ignored (content unchanged)
	before := m.logViewport.View()
	m, _ = send(t, m, logsMsg{service: "api", lines: []string{"OTHER"}})
	if m.logViewport.View() != before {
		t.Error("logsMsg for a non-focused service must be ignored")
	}
}

// ---------- refresh / tick ---------------------------------------------------

func TestTopRefreshKey(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}))
	m, cmd := send(t, m, key('r'))
	if !strings.Contains(m.status, "refreshing") {
		t.Errorf("status = %q, want refreshing", m.status)
	}
	if cmd == nil {
		t.Fatal("refresh should return a fetch cmd")
	}
	if _, ok := cmd().(stateMsg); !ok {
		t.Fatalf("refresh cmd returned %T, want stateMsg", cmd())
	}
}

func TestTopTickReschedules(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}))
	// list mode: tick returns a batch (refresh + next tick). Non-nil cmd is enough.
	_, cmd := send(t, m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick should reschedule and refresh")
	}
	// logs mode: tick should also drive a logs refresh
	m.mode = modeLogs
	m.logService = "db"
	_, cmd = send(t, m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick in logs mode should also fetch logs")
	}
}

// ---------- quit -------------------------------------------------------------

func TestTopQuit(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}))
	if _, cmd := send(t, m, key('q')); cmd == nil {
		t.Error("q should return tea.Quit")
	}
	if _, cmd := send(t, m, tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("ctrl+c should return tea.Quit")
	}
}

// ---------- View matrix (never panics) ---------------------------------------

func TestTopViewMatrix(t *testing.T) {
	cases := []struct {
		name string
		st   uiState
	}{
		{"zero services", stubState("proj")},
		{"all missing", stubState("proj", [2]string{"a", "missing"}, [2]string{"b", "missing"})},
		{"mixed states", stubState("proj", [2]string{"db", "running"}, [2]string{"api", "stopped"}, [2]string{"web", "missing"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newStubModel(tc.st)
			out := m.View()
			// project name always appears
			if !strings.Contains(out, "proj") {
				t.Errorf("view missing project name:\n%s", out)
			}
			// footer hints always appear
			if !strings.Contains(out, "quit") {
				t.Errorf("view missing footer hints:\n%s", out)
			}
			if len(tc.st.Services) > 0 && !strings.Contains(out, tc.st.Services[0].Name) {
				t.Errorf("view missing service name %q:\n%s", tc.st.Services[0].Name, out)
			}
		})
	}
}

func TestTopViewWithPorts(t *testing.T) {
	st := stubState("proj", [2]string{"db", "running"})
	st.Services[0].IP = "192.168.64.4"
	st.Services[0].Ports = []portInfo{{Host: "5432", Target: 5432}}
	m := newStubModel(st)
	out := m.View()
	if !strings.Contains(out, "192.168.64.4") {
		t.Errorf("view should show the IP:\n%s", out)
	}
	if !strings.Contains(out, "5432") {
		t.Errorf("view should show the published port:\n%s", out)
	}
}

func TestTopViewNarrowWidth(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}, [2]string{"api", "stopped"}))
	m.width, m.height = 40, 10
	m.layoutViewport()
	// must not panic at a cramped width
	if out := m.View(); out == "" {
		t.Error("narrow view should still render something")
	}
}

func TestTopViewPlaceholderWhenUnsized(t *testing.T) {
	m := newStubModelSkeleton("proj") // width 0, never sized
	out := m.View()
	if !strings.Contains(out, "initializing") {
		t.Errorf("unsized model should render a placeholder, got %q", out)
	}
}

func TestTopViewLogsMode(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}))
	m, _ = send(t, m, key('l'))
	m, _ = send(t, m, logsMsg{service: "db", lines: []string{"log A", "log B"}})
	out := m.View()
	if !strings.Contains(out, "logs · db") {
		t.Errorf("logs view should show the pane title:\n%s", out)
	}
	if !strings.Contains(out, "back") {
		t.Errorf("logs view should show the back hint:\n%s", out)
	}
}

// View renders both the ok and error status lines, and a header whose right
// half overflows a very narrow width (the gap-clamp branch).
func TestTopViewStatusLines(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}))
	m.status = "db stopped"
	m.statusErr = false
	if !strings.Contains(m.View(), "db stopped") {
		t.Error("ok status line should render")
	}
	m.status = "db: boom"
	m.statusErr = true
	if !strings.Contains(m.View(), "db: boom") {
		t.Error("error status line should render")
	}
	// extremely narrow width forces the header gap to clamp to 1
	m.width = 4
	m.layoutViewport()
	if m.View() == "" {
		t.Error("ultra-narrow header should still render")
	}
}

// determinism: two renders of the same model are byte-identical
func TestTopViewDeterministic(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}, [2]string{"api", "stopped"}))
	if a, b := m.View(), m.View(); a != b {
		t.Errorf("View not deterministic:\n--a--\n%s\n--b--\n%s", a, b)
	}
}

// WindowSizeMsg sets dimensions and marks the model ready.
func TestTopWindowSize(t *testing.T) {
	m := newStubModelSkeleton("proj")
	m, _ = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 || m.height != 40 || !m.ready {
		t.Errorf("after WindowSizeMsg: w=%d h=%d ready=%v", m.width, m.height, m.ready)
	}
}

// logs-mode scroll keys delegate to the viewport without leaving logs mode.
func TestTopLogsScrollStaysInMode(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}))
	m, _ = send(t, m, key('l'))
	m, _ = send(t, m, logsMsg{service: "db", lines: strings.Split(strings.Repeat("x\n", 100), "\n")})
	for _, kt := range []tea.KeyType{tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown} {
		m, _ = send(t, m, tea.KeyMsg{Type: kt})
		if m.mode != modeLogs {
			t.Fatalf("scroll key %v left logs mode", kt)
		}
	}
}

// Init returns a batch (initial fetch + first tick).
func TestTopInit(t *testing.T) {
	m := newStubModel(stubState("proj", [2]string{"db", "running"}))
	if m.Init() == nil {
		t.Error("Init should return a startup cmd")
	}
}

// selectedName is empty (and keys are inert) when there are no services.
func TestTopSelectedNameEmpty(t *testing.T) {
	m := newStubModel(stubState("proj"))
	if m.selectedName() != "" {
		t.Errorf("selectedName with no services = %q, want empty", m.selectedName())
	}
	// 's' and 'l' on an empty list do nothing
	if _, cmd := send(t, m, key('s')); cmd != nil {
		t.Error("s on empty list should be inert")
	}
	if _, cmd := send(t, m, key('l')); cmd != nil {
		t.Error("l on empty list should be inert")
	}
}

// ---------- default wiring (fakeContainer; no real runtime) ------------------

// The live initialModel must wire fetchLogs/doAction to the real container
// paths. We prove doAction("x","stop") shells `container stop <cname>`, and
// start delegates to ensureServiceRunning (stopped → started).
func TestTopDefaultWiring(t *testing.T) {
	p := projectFromYAML(t, "services:\n  x:\n    image: nginx\n")

	t.Run("stop shells container stop", func(t *testing.T) {
		// record argv into a temp file the fake writes; assert it stopped proj-x
		fakeContainer(t, `if [ "$1" = stop ]; then echo "$2" > "$TMP_ARGS"; exit 0; fi
exit 0`)
		argFile := t.TempDir() + "/args"
		t.Setenv("TMP_ARGS", argFile)
		m := initialModel(p)
		var ok bool
		var detail string
		captureOutput(t, func() { ok, detail = m.doAction("x", "stop") })
		if !ok || detail != "stopped" {
			t.Fatalf("stop = (%v, %q), want (true, stopped)", ok, detail)
		}
		got, _ := os.ReadFile(argFile)
		if strings.TrimSpace(string(got)) != "proj-x" {
			t.Errorf("container stop arg = %q, want proj-x", strings.TrimSpace(string(got)))
		}
	})

	t.Run("start delegates to ensureServiceRunning", func(t *testing.T) {
		// `start` succeeds (a stopped container is started); ensureServiceRunning
		// returns "started". Single-service project skips rewire.
		fakeContainer(t, `case "$1" in
  start) echo "started" ;;
esac
exit 0`)
		m := initialModel(p)
		var ok bool
		var detail string
		captureOutput(t, func() { ok, detail = m.doAction("x", "start") })
		if !ok || detail != "started" {
			t.Fatalf("start = (%v, %q), want (true, started)", ok, detail)
		}
	})

	t.Run("fetchLogs tails container logs", func(t *testing.T) {
		fakeContainer(t, `if [ "$1" = logs ]; then printf 'L1\nL2\nL3\n'; fi`)
		m := initialModel(p)
		lines := m.fetchLogs("x")
		if len(lines) != 3 || lines[0] != "L1" || lines[2] != "L3" {
			t.Errorf("fetchLogs = %v, want [L1 L2 L3]", lines)
		}
	})

	t.Run("fetchState collects live state", func(t *testing.T) {
		fakeContainer(t, `case "$1" in
  ls) echo 'ID IMAGE STATE'; echo 'proj-x nginx running' ;;
  inspect) echo '{"address":"192.168.64.7/24"}' ;;
esac
exit 0`)
		m := initialModel(p)
		st := m.fetchState()
		if len(st.Services) != 1 || st.Services[0].State != "running" || st.Services[0].IP != "192.168.64.7" {
			t.Errorf("fetchState = %+v, want one running service with IP", st.Services)
		}
	})
}

// ---------- non-TTY guard ----------------------------------------------------

// isTTY is a package var evaluated at init — false under `go test` — so topRun
// takes the non-interactive branch: it must print a hint and return exit 1
// WITHOUT launching tea.
func TestTopRunNonTTYGuard(t *testing.T) {
	if isTTY {
		t.Skip("test stdout is a TTY; the guard branch is not reachable here")
	}
	p := projectFromYAML(t, "services:\n  x:\n    image: nginx\n")
	var code int
	_, stderr := captureOutput(t, func() { code = topRun(p) })
	if code != 1 {
		t.Fatalf("topRun exit code = %d, want 1 in a non-TTY", code)
	}
	mustContain(t, stderr, "stderr", "needs an interactive terminal", "acompose ps")
}
