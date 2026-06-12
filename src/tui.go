package main

// `acompose top` (alias `tui`) — an interactive, lazydocker-style terminal
// dashboard for the running stack. It reuses the EXACT data layer the web `ui`
// is built on: collectState(p) for status/IP/ports, `container logs` for the
// logs pane, and ensureServiceRunning for the recreate-capable start path.
//
// TESTABILITY: a Bubble Tea event loop is otherwise opaque, so the model's
// side-effecting data access is injected as three struct-field functions
// (fetchState / fetchLogs / doAction), defaulting to the real ones. Update()
// is a pure reducer over the model given those funcs, driven by synthetic
// tea.KeyMsg and our own message types (stateMsg/logsMsg/actionMsg). Tests
// build a model with stubbed funcs, send messages, and assert field
// transitions — no tea.Program.Run and no live runtime needed.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/compose-spec/compose-go/v2/types"
)

// ---------- messages (the only things Update reacts to besides keys) ---------

type stateMsg struct{ st uiState }
type logsMsg struct {
	service string
	lines   []string
}
type actionMsg struct {
	service string
	ok      bool
	detail  string
}

// tickMsg drives the 2s polling cadence (state always; logs while in logs mode).
type tickMsg time.Time

// ---------- view mode --------------------------------------------------------

type tuiMode int

const (
	modeList tuiMode = iota
	modeLogs
)

// ---------- model ------------------------------------------------------------

type topModel struct {
	// injected data access — defaults wired in initialModel; tests stub these
	fetchState func() uiState
	fetchLogs  func(service string) []string
	doAction   func(service, op string) (bool, string)

	project   string
	st        uiState
	sel       int     // selected service index
	mode      tuiMode // list | logs
	busy      bool    // an action is in flight
	status    string  // transient status line
	statusErr bool    // render status in red

	logViewport viewport.Model
	logService  string // service the logViewport currently shows

	width, height int
	ready         bool // viewport sized at least once
}

// ---------- palette (lipgloss; degrades to no-color when unsupported) --------

var (
	tuiRunDot    = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	tuiDownDot   = lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // dim-red
	tuiDim       = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // grey
	tuiAddr      = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	tuiHeader    = lipgloss.NewStyle().Bold(true)
	tuiSelected  = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236")).Foreground(lipgloss.Color("15"))
	tuiErr       = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	tuiOK        = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	tuiFooter    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	tuiPaneTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
)

// ---------- construction -----------------------------------------------------

// initialModel builds the live model: the three data funcs are bound to the
// real collectState / `container logs` / ensureServiceRunning paths.
func initialModel(p *types.Project) topModel {
	m := topModel{
		project: p.Name,
		fetchState: func() uiState {
			return collectState(p)
		},
		fetchLogs: func(service string) []string {
			return tailLogsFor(p, service)
		},
		doAction: func(service, op string) (bool, string) {
			return defaultAction(p, service, op)
		},
	}
	m.logViewport = viewport.New(0, 0)
	return m
}

// tailLogsFor mirrors ui.go's /api/logs: the last 200 lines of `container logs`.
func tailLogsFor(p *types.Project, service string) []string {
	out, _ := exec.Command("container", "logs", cnameOf(p, service)).CombinedOutput()
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	return lines
}

// defaultAction is the live action path: start uses ensureServiceRunning (the
// same recreate-capable path the web UI POST uses), stop calls `container stop`.
func defaultAction(p *types.Project, service, op string) (bool, string) {
	if op == "start" {
		return ensureServiceRunning(p, runner{}, service, true)
	}
	out, err := exec.Command("container", "stop", cnameOf(p, service)).CombinedOutput()
	if err != nil {
		return false, strings.TrimSpace(string(out))
	}
	return true, "stopped"
}

// ---------- tea.Model: Init / Update / View ----------------------------------

func (m topModel) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tickCmd())
}

// refreshCmd fetches fresh state off the Update goroutine.
func (m topModel) refreshCmd() tea.Cmd {
	fetch := m.fetchState
	return func() tea.Msg { return stateMsg{st: fetch()} }
}

// logsCmd fetches logs for service off the Update goroutine.
func (m topModel) logsCmd(service string) tea.Cmd {
	fetch := m.fetchLogs
	return func() tea.Msg { return logsMsg{service: service, lines: fetch(service)} }
}

// actionCmd runs a start/stop off the Update goroutine.
func (m topModel) actionCmd(service, op string) tea.Cmd {
	act := m.doAction
	return func() tea.Msg {
		ok, detail := act(service, op)
		return actionMsg{service: service, ok: ok, detail: detail}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// selectedName returns the currently selected service name, or "" if none.
func (m topModel) selectedName() string {
	if m.sel < 0 || m.sel >= len(m.st.Services) {
		return ""
	}
	return m.st.Services[m.sel].Name
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layoutViewport()
		m.ready = true
		return m, nil

	case stateMsg:
		m.st = msg.st
		m.clampSelection()
		return m, nil

	case logsMsg:
		// ignore logs that arrive for a service we are no longer focused on
		if m.mode != modeLogs || msg.service != m.logService {
			return m, nil
		}
		m.logViewport.SetContent(strings.Join(msg.lines, "\n"))
		m.logViewport.GotoBottom()
		return m, nil

	case actionMsg:
		m.busy = false
		m.statusErr = !msg.ok
		if msg.ok {
			verb := "started"
			if strings.Contains(strings.ToLower(msg.detail), "stop") {
				verb = "stopped"
			}
			m.status = msg.service + " " + verb
		} else {
			detail := msg.detail
			if detail == "" {
				detail = "failed"
			}
			m.status = msg.service + ": " + detail
		}
		return m, m.refreshCmd()

	case tickMsg:
		cmds := []tea.Cmd{m.refreshCmd(), tickCmd()}
		if m.mode == modeLogs && m.logService != "" {
			cmds = append(cmds, m.logsCmd(m.logService))
		}
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey is the pure key reducer — split out so it stays readable and the
// logs-mode viewport scrolling can delegate to the bubble.
func (m topModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	}

	if m.mode == modeLogs {
		switch msg.String() {
		case "esc", "h":
			m.mode = modeList
			return m, nil
		case "up", "down", "pgup", "pgdown", "k", "j":
			var cmd tea.Cmd
			m.logViewport, cmd = m.logViewport.Update(msg)
			return m, cmd
		}
		// any other key in logs mode is inert
		return m, nil
	}

	// list mode
	switch msg.String() {
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
		return m, nil
	case "down", "j":
		if m.sel < len(m.st.Services)-1 {
			m.sel++
		}
		return m, nil
	case "r":
		m.status = "refreshing…"
		m.statusErr = false
		return m, m.refreshCmd()
	case "s":
		name := m.selectedName()
		if name == "" || m.busy {
			return m, nil
		}
		op := "start"
		if m.st.Services[m.sel].State == "running" {
			op = "stop"
		}
		m.busy = true
		m.statusErr = false
		if op == "stop" {
			m.status = "stopping " + name + "…"
		} else {
			m.status = "starting " + name + "…"
		}
		return m, m.actionCmd(name, op)
	case "l", "enter":
		name := m.selectedName()
		if name == "" {
			return m, nil
		}
		m.mode = modeLogs
		m.logService = name
		m.logViewport.SetContent("loading logs…")
		m.layoutViewport()
		return m, m.logsCmd(name)
	}
	return m, nil
}

// clampSelection keeps sel within the current service list (a refresh can
// shrink the list below the old index).
func (m *topModel) clampSelection() {
	if len(m.st.Services) == 0 {
		m.sel = 0
		return
	}
	if m.sel >= len(m.st.Services) {
		m.sel = len(m.st.Services) - 1
	}
	if m.sel < 0 {
		m.sel = 0
	}
}

// layoutViewport sizes the logs viewport to the area below the header and above
// the footer. Guards against tiny/zero terminals so it never panics.
func (m *topModel) layoutViewport() {
	w := m.width
	if w < 1 {
		w = 1
	}
	// reserve: header(1) + blank(1) + title(1) + footer(2) = 5 lines
	h := m.height - 5
	if h < 1 {
		h = 1
	}
	m.logViewport.Width = w
	m.logViewport.Height = h
}

// ---------- View -------------------------------------------------------------

func (m topModel) View() string {
	if m.width == 0 {
		// fresh model, never sized — a sane placeholder, never a panic
		return "acompose top — initializing… (press q to quit)"
	}
	if m.mode == modeLogs {
		return m.viewLogs()
	}
	return m.viewList()
}

func (m topModel) header() string {
	running := 0
	for _, s := range m.st.Services {
		if s.State == "running" {
			running++
		}
	}
	clock := m.st.Time
	if clock == "" {
		clock = time.Now().Format("15:04:05")
	}
	left := tuiHeader.Render("acompose ─ " + m.project)
	right := tuiDim.Render(fmt.Sprintf("%d/%d running · %s", running, len(m.st.Services), clock))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// statusLine renders the transient action/refresh status (blank when empty).
func (m topModel) statusLine() string {
	if m.status == "" {
		return ""
	}
	if m.statusErr {
		return tuiErr.Render(m.status)
	}
	return tuiOK.Render(m.status)
}

func (m topModel) viewList() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n\n")

	if len(m.st.Services) == 0 {
		b.WriteString(tuiDim.Render("no services found — run `acompose up` in the project directory, then press r"))
		b.WriteString("\n")
	} else {
		for i, s := range m.st.Services {
			b.WriteString(m.serviceRow(i, s))
			b.WriteString("\n")
		}
	}

	if sl := m.statusLine(); sl != "" {
		b.WriteString("\n" + sl + "\n")
	}

	b.WriteString("\n")
	b.WriteString(tuiFooter.Render("↑/↓ move · s start/stop · l logs · r refresh · q quit"))
	return b.String()
}

// serviceRow renders one service line: status dot, name, IP/state, ports.
func (m topModel) serviceRow(i int, s svcState) string {
	dot := tuiDownDot.Render("●")
	if s.State == "running" {
		dot = tuiRunDot.Render("●")
	}

	addr := s.IP
	if addr == "" {
		addr = s.State
	}

	var ports []string
	for _, p := range s.Ports {
		ports = append(ports, fmt.Sprintf("%s→%d", p.Host, p.Target))
	}
	portStr := ""
	if len(ports) > 0 {
		portStr = "  " + strings.Join(ports, " ")
	}

	tail := "  " + tuiAddr.Render(addr) + tuiDim.Render(portStr)
	if i == m.sel {
		// selection marker is robust even without color support
		return tuiSelected.Render("▸ "+s.Name) + tail
	}
	name := fmt.Sprintf("%-14s", s.Name)
	return "  " + dot + " " + name + tail
}

func (m topModel) viewLogs() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n\n")
	b.WriteString(tuiPaneTitle.Render("logs · " + m.logService))
	b.WriteString("\n")
	b.WriteString(m.logViewport.View())
	b.WriteString("\n")
	b.WriteString(tuiFooter.Render("↑/↓ pgup/pgdn scroll · esc/h back · q quit"))
	return b.String()
}

// ---------- thin command wiring (out of coverage expectations) ---------------

// topRun is the testable guard: it refuses a non-interactive terminal (pipes,
// CI) with a graceful message and exit code 1 BEFORE launching tea. The
// interactive branch (tea.NewProgram.Run) is the only part left uncovered.
func topRun(p *types.Project) int {
	if !isTTY {
		fail("acompose top needs an interactive terminal; try: acompose ps / acompose ui")
		return 1
	}
	prog := tea.NewProgram(initialModel(p), tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		fail("%v", err)
		return 1
	}
	return 0
}

func cmdTop(p *types.Project) {
	if code := topRun(p); code != 0 {
		os.Exit(code)
	}
}
