package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"egg-emulator/internal/config"
	"egg-emulator/internal/report"
	"egg-emulator/internal/runner"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ────────────────────────────────────────────────────────────────────────────
// Styles
// ────────────────────────────────────────────────────────────────────────────

var (
	appStyle = lipgloss.NewStyle().Padding(1, 2)

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFDF5")).
			Background(lipgloss.Color("#25A065")).
			Padding(0, 1)

	focusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#25A065")).
			Padding(0, 1)

	unfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#3C3C3C")).
			Padding(0, 1)

	metricsStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D7FF")).Bold(true)
	labelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#7D7D7D")).Width(12)
	valueStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF"))
	actStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00"))
	passStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00")).Bold(true)
	failStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF0000")).Bold(true)
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#555555"))
)

// ────────────────────────────────────────────────────────────────────────────
// State types
// ────────────────────────────────────────────────────────────────────────────

type uiState int

const (
	stateSelecting uiState = iota
	stateRunning
	stateViewingLog
)

type focusPane int

const (
	focusAvailable focusPane = iota
	focusTested
)

// ────────────────────────────────────────────────────────────────────────────
// List item types
// ────────────────────────────────────────────────────────────────────────────

type eggItem struct{ egg *config.Egg }

func (i eggItem) Title() string       { return i.egg.Name }
func (i eggItem) Description() string { return fmt.Sprintf("%d images", len(i.egg.DockerImages)) }
func (i eggItem) FilterValue() string { return i.egg.Name }

type logItem struct {
	path string
	name string
}

func (i logItem) Title() string       { return i.name }
func (i logItem) Description() string { return i.path }
func (i logItem) FilterValue() string { return i.name }

// testCard holds live test progress plus a scrolling console history.
type testCard struct {
	progress runner.Progress
	history  []string
}

func (c testCard) Title() string       { return c.progress.EggName }
func (c testCard) Description() string { return fmt.Sprintf("[%s] %s", c.progress.Status, c.progress.Message) }
func (c testCard) FilterValue() string { return c.progress.EggName }

// ────────────────────────────────────────────────────────────────────────────
// Model
// ────────────────────────────────────────────────────────────────────────────

// Model is the root Bubble Tea model.
type Model struct {
	state        uiState
	focus        focusPane
	eggList      list.Model
	testedList   list.Model
	progressList list.Model
	logList      list.Model
	viewport     viewport.Model
	r            *runner.Runner
	eggs         []*config.Egg
	width        int
	height       int
	autoTesting  bool
	autoQueue    []*config.Egg
	// autoReports accumulates per-egg reports during an auto-test run
	// so a combined summary.report.json can be written at the end.
	autoReports  []*report.EggReport
	pendingPass  string // egg name to move after delay
	totalTests   int
	passedTests  int
	failedTests  int
}

// New returns an initial Model.
func New(r *runner.Runner, eggs []*config.Egg) Model {
	items := make([]list.Item, len(eggs))
	for i, e := range eggs {
		items[i] = eggItem{egg: e}
	}

	el := list.New(items, list.NewDefaultDelegate(), 0, 0)
	el.Title = "Available Eggs  [Enter] test  [A] auto-test  [L] logs  [Tab] switch"
	el.AdditionalFullHelpKeys = func() []key.Binding {
		return []key.Binding{
			key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "auto test all")),
			key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "view logs")),
			key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch pane")),
		}
	}

	tl := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	tl.Title = "Successfully Tested"

	pl := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	pl.Title = "Test Progress"

	ll := list.New([]list.Item{}, list.NewDefaultDelegate(), 0, 0)
	ll.Title = "Select Log to View"

	return Model{
		state:        stateSelecting,
		focus:        focusAvailable,
		eggList:      el,
		testedList:   tl,
		progressList: pl,
		logList:      ll,
		r:            r,
		eggs:         eggs,
	}
}

// resetRunner creates a fresh Runner instance that shares the same configuration
// as the current one. This ensures each test run has its own set of channels,
// avoiding sending on or reading from closed channels when auto-testing eggs
// multiple times in the same TUI session.
func (m *Model) resetRunner() {
	if m.r == nil {
		return
	}
	m.r = runner.New(m.r.EggsDir, m.r.PatternsDir, m.r.LogsDir, m.r.Concurrent)
}

func (m Model) Init() tea.Cmd { return nil }

// ────────────────────────────────────────────────────────────────────────────
// Messages
// ────────────────────────────────────────────────────────────────────────────

type progressMsg runner.Progress
type reportMsg struct{ r *report.EggReport }
type backToSelectMsg struct{}

// waitProgress reads one event from r.ProgressChan.
// It captures r (not the channel directly) so it always reads from the
// current channel even after Run() replaces it with a fresh one.
func waitProgress(r *runner.Runner) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-r.ProgressChan
		if !ok {
			return nil
		}
		return progressMsg(p)
	}
}

// waitReport reads one report from r.ReportChan.
func waitReport(r *runner.Runner) tea.Cmd {
	return func() tea.Msg {
		rp, ok := <-r.ReportChan
		if !ok {
			return nil
		}
		return reportMsg{rp}
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Update
// ────────────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		half := msg.Width/2 - 4
		m.eggList.SetSize(half, msg.Height-6)
		m.testedList.SetSize(half, msg.Height-6)
		m.progressList.SetSize(msg.Width-4, msg.Height-4)
		m.logList.SetSize(msg.Width-4, msg.Height/3)
		m.viewport.Width = msg.Width - 4
		m.viewport.Height = msg.Height - m.height/3 - 10

	case tea.KeyMsg:
		cmds = append(cmds, m.handleKey(msg)...)

	case progressMsg:
		cmds = append(cmds, m.handleProgress(runner.Progress(msg))...)

	case reportMsg:
		// Collect report; if auto-testing and all eggs done, save summary
		if m.autoTesting {
			m.autoReports = append(m.autoReports, msg.r)
		}
		cmds = append(cmds, waitReport(m.r))

	case backToSelectMsg:
		cmds = append(cmds, m.handleBackToSelect()...)
	}

	// Delegate remaining input to the focused widget
	var cmd tea.Cmd
	switch m.state {
	case stateSelecting:
		if m.focus == focusAvailable {
			m.eggList, cmd = m.eggList.Update(msg)
		} else {
			m.testedList, cmd = m.testedList.Update(msg)
		}
	case stateRunning:
		m.progressList, cmd = m.progressList.Update(msg)
	case stateViewingLog:
		m.logList, cmd = m.logList.Update(msg)
		m.viewport, _ = m.viewport.Update(msg)
	}
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m *Model) handleKey(msg tea.KeyMsg) []tea.Cmd {
	switch msg.String() {
	case "ctrl+c", "q":
		if m.state != stateSelecting {
			m.autoTesting = false
			m.autoQueue = nil
			m.state = stateSelecting
			return nil
		}
		return []tea.Cmd{tea.Quit}

	case "tab":
		if m.state == stateSelecting {
			if m.focus == focusAvailable {
				m.focus = focusTested
			} else {
				m.focus = focusAvailable
			}
		}

	case "l":
		if m.state == stateSelecting {
			m.loadLogs()
			m.state = stateViewingLog
		}

	case "a":
		if m.state == stateSelecting && m.focus == focusAvailable && len(m.eggList.Items()) > 0 {
			m.autoTesting = true
			m.totalTests, m.passedTests, m.failedTests = 0, 0, 0
			m.autoReports = nil
			m.autoQueue = make([]*config.Egg, 0, len(m.eggList.Items()))
			for _, item := range m.eggList.Items() {
				e := item.(eggItem).egg
				m.autoQueue = append(m.autoQueue, e)
				m.totalTests += len(e.DockerImages)
			}
			return []tea.Cmd{m.startNextAutoTest()}
		}

	case "enter":
		return m.handleEnter()
	}
	return nil
}

func (m *Model) handleEnter() []tea.Cmd {
	switch m.state {
	case stateSelecting:
		var egg *config.Egg
		if m.focus == focusAvailable && len(m.eggList.Items()) > 0 {
			egg = m.eggList.SelectedItem().(eggItem).egg
		} else if m.focus == focusTested && len(m.testedList.Items()) > 0 {
			egg = m.testedList.SelectedItem().(eggItem).egg
		} else {
			return nil
		}
		m.state = stateRunning
		m.resetRunner()
		m.progressList.SetItems(nil)
		m.totalTests = len(egg.DockerImages)
		m.passedTests, m.failedTests = 0, 0
		go m.r.Run(context.Background(), []*config.Egg{egg})
		return []tea.Cmd{
			waitProgress(m.r),
			waitReport(m.r),
		}

	case stateViewingLog:
		if sel, ok := m.logList.SelectedItem().(logItem); ok {
			data, _ := os.ReadFile(sel.path)
			m.viewport.SetContent(string(data))
			m.viewport.GotoTop()
		}
	}
	return nil
}

func (m *Model) handleProgress(p runner.Progress) []tea.Cmd {
	if p.Total > 0 {
		m.totalTests = p.Total
	}
	switch p.Status {
	case runner.StatusPassed:
		if p.Message == "Completed" {
			m.pendingPass = p.EggName
			return []tea.Cmd{tea.Tick(2*time.Second, func(time.Time) tea.Msg { return backToSelectMsg{} })}
		}
		m.passedTests++
	case runner.StatusFailed:
		if p.Message == "Completed with failures" {
			return []tea.Cmd{tea.Tick(2*time.Second, func(time.Time) tea.Msg { return backToSelectMsg{} })}
		}
		m.failedTests++
	case runner.StatusError:
		m.failedTests++
	}

	// Update or insert testCard
	items := m.progressList.Items()
	for i, item := range items {
		card := item.(testCard)
		if card.progress.EggName == p.EggName {
			newCard := testCard{progress: p, history: card.history}
			if p.Activity != "" {
				last := ""
				if len(newCard.history) > 0 {
					last = newCard.history[len(newCard.history)-1]
				}
				if p.Activity != last {
					newCard.history = append(newCard.history, p.Activity)
					if len(newCard.history) > 20 {
						newCard.history = newCard.history[1:]
					}
				}
			}
			m.progressList.SetItem(i, newCard)
			return []tea.Cmd{waitProgress(m.r)}
		}
	}
	m.progressList.InsertItem(len(items), testCard{
		progress: p,
		history:  []string{p.Activity},
	})
	return []tea.Cmd{waitProgress(m.r)}
}

func (m *Model) handleBackToSelect() []tea.Cmd {
	if m.pendingPass != "" {
		for i, item := range m.eggList.Items() {
			if item.(eggItem).egg.Name == m.pendingPass {
				m.eggList.RemoveItem(i)
				m.testedList.InsertItem(len(m.testedList.Items()), item)
				break
			}
		}
		m.pendingPass = ""
	}
	if m.autoTesting && len(m.autoQueue) > 0 {
		m.autoQueue = m.autoQueue[1:]
		if len(m.autoQueue) > 0 {
			return []tea.Cmd{m.startNextAutoTest()}
		}
		// Auto-test finished — save combined summary
		m.autoTesting = false
		if len(m.autoReports) > 0 {
			if path, err := report.SaveSummary(m.r.LogsDir, m.autoReports); err != nil {
				fmt.Fprintf(os.Stderr, "[WARN] could not save summary report: %v\n", err)
			} else {
				fmt.Printf("[REPORT] Summary saved: %s\n", path)
			}
		}
	}
	m.state = stateSelecting
	return nil
}

func (m *Model) startNextAutoTest() tea.Cmd {
	if len(m.autoQueue) == 0 {
		m.autoTesting = false
		return nil
	}
	egg := m.autoQueue[0]
	m.state = stateRunning
	m.resetRunner()
	m.progressList.SetItems(nil)
	go m.r.Run(context.Background(), []*config.Egg{egg})
	return tea.Batch(
		waitProgress(m.r),
		waitReport(m.r),
	)
}

// ────────────────────────────────────────────────────────────────────────────
// View
// ────────────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	switch m.state {
	case stateSelecting:
		return m.viewSelecting()
	case stateRunning:
		return appStyle.Render(m.viewRunning())
	case stateViewingLog:
		return appStyle.Render(lipgloss.JoinVertical(lipgloss.Left,
			m.logList.View(),
			dimStyle.Render("─── Log Content (↑↓ scroll) ───"),
			m.viewport.View(),
		))
	}
	return ""
}

func (m Model) viewSelecting() string {
	availStyle := unfocusedBorder
	testedStyle := unfocusedBorder
	if m.focus == focusAvailable {
		availStyle = focusedBorder
	} else {
		testedStyle = focusedBorder
	}
	half := m.width/2 - 6
	return appStyle.Render(lipgloss.JoinHorizontal(lipgloss.Top,
		availStyle.Width(half).Render(m.eggList.View()),
		"  ",
		testedStyle.Width(half).Render(m.testedList.View()),
	))
}

func (m Model) viewRunning() string {
	var views []string
	summary := lipgloss.JoinHorizontal(lipgloss.Left,
		labelStyle.Render("Tests:"),
		valueStyle.Render(fmt.Sprintf("%d total", m.totalTests)),
		"  ",
		passStyle.Render(fmt.Sprintf("✓ %d", m.passedTests)),
		"  ",
		failStyle.Render(fmt.Sprintf("✗ %d", m.failedTests)),
	)
	if m.autoTesting {
		summary += "  " + dimStyle.Render(fmt.Sprintf("[auto: %d remaining]", len(m.autoQueue)))
	}

	for _, item := range m.progressList.Items() {
		card := item.(testCard)
		p := card.progress

		header := titleStyle.Render(fmt.Sprintf(" Egg: %s ", p.EggName))

		statusLine := lipgloss.JoinHorizontal(lipgloss.Left,
			labelStyle.Render("Status:"),
			valueStyle.Render(string(p.Status)+" — "+p.Message),
		)
		imageLine := lipgloss.JoinHorizontal(lipgloss.Left,
			labelStyle.Render("Image:"),
			valueStyle.Render(fmt.Sprintf("%s  (%d/%d)", p.Image, p.Current, p.Total)),
		)

		// Metrics boxes
		cpuStr := fmt.Sprintf("%.2f%%", p.CPUUsage)
		memMB := p.MemoryUsage / 1024 / 1024
		limMB := p.MemoryLimit / 1024 / 1024
		ramStr := fmt.Sprintf("%.1fMB / %.1fMB", memMB, limMB)

		cpuBox := metricBox("CPU", cpuStr, 20)
		ramBox := metricBox("RAM", ramStr, 30)
		metrics := lipgloss.JoinHorizontal(lipgloss.Top, cpuBox, " ", ramBox)

		// Console history (last 10 lines, right-padded)
		hist := card.history
		const consoleLines = 10
		lines := make([]string, consoleLines)
		start := len(hist) - consoleLines
		if start < 0 {
			start = 0
		}
		offset := consoleLines - (len(hist) - start)
		for i, l := range hist[start:] {
			lines[offset+i] = actStyle.Render(">> ") + l
		}
		for i, l := range lines {
			if l == "" {
				lines[i] = dimStyle.Render("  ~")
			}
		}
		console := lipgloss.NewStyle().
			Background(lipgloss.Color("#111111")).
			Padding(0, 1).
			Width(m.width - 12).
			Render(strings.Join(lines, "\n"))

		body := focusedBorder.Width(m.width - 8).Render(lipgloss.JoinVertical(lipgloss.Left,
			summary, "",
			statusLine,
			imageLine, "",
			metrics,
			"\nConsole:\n",
			console,
		))
		views = append(views, header+"\n"+body)
	}

	if len(views) == 0 {
		return dimStyle.Render("Waiting for test output...")
	}
	return lipgloss.JoinVertical(lipgloss.Left, views...)
}

// metricBox renders a labeled metrics tile.
func metricBox(label, value string, width int) string {
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("#25A065")).
		Padding(0, 1).
		Width(width).
		Render(lipgloss.JoinVertical(lipgloss.Center,
			dimStyle.Render(label),
			metricsStyle.Render(value),
		))
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────────

func (m *Model) loadLogs() {
	entries, _ := os.ReadDir(m.r.LogsDir)
	items := make([]list.Item, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".log" {
			items = append(items, logItem{
				path: filepath.Join(m.r.LogsDir, e.Name()),
				name: e.Name(),
			})
		}
	}
	m.logList.SetItems(items)
}