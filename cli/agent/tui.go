package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

// turnTimeout bounds a single chat turn (model + tool calls).
const turnTimeout = 10 * time.Minute

// ---- stream event messages (pushed from the runner goroutine) ----

type streamTextMsg string
type streamReasoningMsg string
type streamToolMsg struct{ name, input string }
type streamToolResultMsg struct{ name string }
type streamDoneMsg struct{ err error }

// ---- styles ----

var (
	userStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	assistStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	toolStyle   = lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("8"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6")).Padding(0, 1)
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")).Padding(0, 1)
)

type tuiModel struct {
	runner *Runner
	sub    chan tea.Msg
	ctx    context.Context

	vp    viewport.Model
	input textinput.Model
	rd    *glamour.TermRenderer

	width, height int
	ready         bool

	blocks    []string        // finalized transcript blocks
	live      strings.Builder // in-progress assistant markdown (raw)
	reasoning strings.Builder // in-progress reasoning (shown faintly)
	streaming bool

	info string // static status info (model/embed)
	warn string // optional warning (e.g. lexical-only)
}

// RunTUI starts the chat TUI. info is the status-bar label; warn is an optional
// warning banner (empty if none).
func RunTUI(ctx context.Context, runner *Runner, info, warn string) error {
	ti := textinput.New()
	ti.Placeholder = "Ask about the graph…  (Ctrl+C to quit)"
	ti.Prompt = "› "
	ti.Focus()
	ti.CharLimit = 4000

	m := &tuiModel{
		runner: runner,
		sub:    make(chan tea.Msg, 64),
		ctx:    ctx,
		input:  ti,
		info:   info,
		warn:   warn,
	}

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.waitForActivity())
}

// waitForActivity blocks on the next streamed event. Exactly one of these is
// kept outstanding at all times so sends from the runner goroutine never block
// indefinitely.
func (m *tuiModel) waitForActivity() tea.Cmd {
	return func() tea.Msg { return <-m.sub }
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyEnter:
			if m.streaming {
				return m, nil // ignore input mid-turn
			}
			prompt := strings.TrimSpace(m.input.Value())
			if prompt == "" {
				return m, nil
			}
			return m, m.submit(prompt)
		}

	case streamTextMsg:
		m.live.WriteString(string(msg))
		m.refresh()
		return m, m.waitForActivity()

	case streamReasoningMsg:
		m.reasoning.WriteString(string(msg))
		m.refresh()
		return m, m.waitForActivity()

	case streamToolMsg:
		m.flushReasoning()
		m.blocks = append(m.blocks, toolStyle.Render(fmt.Sprintf("→ %s %s", msg.name, oneLine(msg.input, 160))))
		m.refresh()
		return m, m.waitForActivity()

	case streamToolResultMsg:
		m.blocks = append(m.blocks, toolStyle.Render(fmt.Sprintf("  ✓ %s returned", msg.name)))
		m.refresh()
		return m, m.waitForActivity()

	case streamDoneMsg:
		m.finishTurn(msg.err)
		return m, m.waitForActivity()
	}

	// Delegate remaining messages to the input and viewport.
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)
	m.vp, cmd = m.vp.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m *tuiModel) submit(prompt string) tea.Cmd {
	m.blocks = append(m.blocks, userStyle.Render("you› ")+prompt)
	m.input.Reset()
	m.streaming = true

	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, turnTimeout)
		defer cancel()
		m.runner.Stream(ctx, prompt, StreamHandler{
			OnText:       func(t string) { m.sub <- streamTextMsg(t) },
			OnReasoning:  func(t string) { m.sub <- streamReasoningMsg(t) },
			OnToolCall:   func(n, in string) { m.sub <- streamToolMsg{n, in} },
			OnToolResult: func(n, _ string) { m.sub <- streamToolResultMsg{n} },
			OnDone:       func(err error) { m.sub <- streamDoneMsg{err} },
		})
	}()
	m.refresh()
	return nil
}

func (m *tuiModel) finishTurn(err error) {
	m.flushReasoning()
	if s := m.live.String(); strings.TrimSpace(s) != "" {
		m.blocks = append(m.blocks, m.renderMarkdown(s))
	}
	m.live.Reset()
	if err != nil {
		m.blocks = append(m.blocks, errStyle.Render("error: "+err.Error()))
	}
	m.streaming = false
	m.refresh()
}

func (m *tuiModel) flushReasoning() {
	if s := strings.TrimSpace(m.reasoning.String()); s != "" {
		m.blocks = append(m.blocks, toolStyle.Render("("+oneLine(s, 400)+")"))
	}
	m.reasoning.Reset()
}

func (m *tuiModel) renderMarkdown(s string) string {
	if m.rd == nil {
		return assistStyle.Render(s)
	}
	out, err := m.rd.Render(s)
	if err != nil {
		return assistStyle.Render(s)
	}
	return strings.TrimRight(out, "\n")
}

// refresh rebuilds the viewport content and scrolls to the bottom.
func (m *tuiModel) refresh() {
	if !m.ready {
		return
	}
	parts := make([]string, 0, len(m.blocks)+1)
	parts = append(parts, m.blocks...)
	if s := m.live.String(); s != "" {
		parts = append(parts, assistStyle.Render(s)) // live text as plain (markdown rendered on completion)
	}
	m.vp.SetContent(strings.Join(parts, "\n\n"))
	m.vp.GotoBottom()
}

func (m *tuiModel) resize(w, h int) {
	m.width, m.height = w, h
	statusH := 1
	if m.warn != "" {
		statusH = 2
	}
	inputH := 1
	vpH := h - statusH - inputH - 1
	if vpH < 3 {
		vpH = 3
	}

	if !m.ready {
		m.vp = viewport.New(w, vpH)
		m.ready = true
	} else {
		m.vp.Width = w
		m.vp.Height = vpH
	}
	m.input.Width = w - 4

	// (Re)build the markdown renderer at the current width.
	if rd, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(max(20, w-2)),
	); err == nil {
		m.rd = rd
	}
	m.refresh()
}

func (m *tuiModel) View() string {
	if !m.ready {
		return "starting…"
	}
	var b strings.Builder
	b.WriteString(m.vp.View())
	b.WriteByte('\n')

	status := m.info
	if m.streaming {
		status += " · thinking…"
	} else {
		status += " · ready"
	}
	b.WriteString(statusStyle.Width(m.width).Render(status))
	if m.warn != "" {
		b.WriteByte('\n')
		b.WriteString(warnStyle.Width(m.width).Render("⚠ " + m.warn))
	}
	b.WriteByte('\n')
	b.WriteString(m.input.View())
	return b.String()
}
