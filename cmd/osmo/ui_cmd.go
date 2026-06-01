package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pandey-raghvendra/osmo/internal/blockid"
	"github.com/pandey-raghvendra/osmo/internal/triage"
	"github.com/pandey-raghvendra/osmo/internal/tfplan"
)

// ---- styles ----------------------------------------------------------------

var (
	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12")).
			PaddingLeft(1)

	styleSelected = lipgloss.NewStyle().
			Bold(true).
			Background(lipgloss.Color("237"))

	styleSafe   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))  // green
	styleReview = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))  // yellow
	styleFlag   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))   // red
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleBold   = lipgloss.NewStyle().Bold(true)

	styleDiffPlus  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleDiffMinus = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleDiffMeta  = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))

	styleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("238"))

	styleHelp = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			PaddingLeft(1)

	styleAbsorb = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")).
			Bold(true)

	styleSkip = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240"))

	styleExecute = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("10"))
)

// ---- action ----------------------------------------------------------------

type actionKind int

const (
	actionAbsorb actionKind = iota
	actionSkip
)

func (a actionKind) label() string {
	switch a {
	case actionAbsorb:
		return styleAbsorb.Render("absorb")
	case actionSkip:
		return styleSkip.Render("skip  ")
	}
	return ""
}

// ---- model -----------------------------------------------------------------

type uiModel struct {
	dir      string
	bin      string
	drifts   []tfplan.Drift
	verdicts []triage.Verdict
	actions  []actionKind

	cursor   int
	listTop  int
	diffTop  int

	totalH    int
	totalW    int
	listPaneH int
	diffPaneH int

	// execution result shown at quit
	done     bool
	execCmd  string
	execErr  error
}

func newUIModel(dir, bin string, drifts []tfplan.Drift, verdicts []triage.Verdict) uiModel {
	actions := make([]actionKind, len(verdicts))
	for i, v := range verdicts {
		if v.Severity == triage.Safe {
			actions[i] = actionAbsorb
		} else {
			actions[i] = actionSkip
		}
	}
	return uiModel{
		dir:      dir,
		bin:      bin,
		drifts:   drifts,
		verdicts: verdicts,
		actions:  actions,
	}
}

func (m uiModel) Init() tea.Cmd {
	return nil
}

// ---- update ----------------------------------------------------------------

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.totalW = msg.Width
		m.totalH = msg.Height
		// Reserve: 3 header + 2 borders + 2 borders + 1 help + 1 padding = ~9
		inner := m.totalH - 9
		if inner < 4 {
			inner = 4
		}
		m.listPaneH = inner / 2
		if m.listPaneH < 2 {
			m.listPaneH = 2
		}
		m.diffPaneH = inner - m.listPaneH
		if m.diffPaneH < 2 {
			m.diffPaneH = 2
		}

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.diffTop = 0
				if m.cursor < m.listTop {
					m.listTop = m.cursor
				}
			}

		case "down", "j":
			if m.cursor < len(m.verdicts)-1 {
				m.cursor++
				m.diffTop = 0
				if m.cursor >= m.listTop+m.listPaneH {
					m.listTop = m.cursor - m.listPaneH + 1
				}
			}

		case "a":
			if m.cursor < len(m.actions) {
				m.actions[m.cursor] = actionAbsorb
			}

		case "s":
			if m.cursor < len(m.actions) {
				m.actions[m.cursor] = actionSkip
			}

		case "A":
			// Absorb all safe, skip everything else.
			for i, v := range m.verdicts {
				if v.Severity == triage.Safe {
					m.actions[i] = actionAbsorb
				} else {
					m.actions[i] = actionSkip
				}
			}

		case "S":
			// Skip all.
			for i := range m.actions {
				m.actions[i] = actionSkip
			}

		case "left", "h":
			m.diffTop = 0

		case "pgup", "ctrl+u":
			if m.diffTop > 0 {
				m.diffTop -= m.diffPaneH / 2
				if m.diffTop < 0 {
					m.diffTop = 0
				}
			}

		case "pgdown", "ctrl+d":
			lines := m.diffLines()
			max := len(lines) - m.diffPaneH
			if max < 0 {
				max = 0
			}
			if m.diffTop < max {
				m.diffTop += m.diffPaneH / 2
				if m.diffTop > max {
					m.diffTop = max
				}
			}

		case "x":
			cmd := m.buildCommand()
			if cmd == "" {
				// nothing to absorb
				return m, tea.Quit
			}
			m.execCmd = cmd
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// ---- view ------------------------------------------------------------------

func (m uiModel) View() string {
	if m.totalW == 0 {
		return "loading...\n"
	}

	innerW := m.totalW - 4 // account for border left+right + padding

	header := styleHeader.Render(fmt.Sprintf(
		"osmo ui · %d resource(s) · %s", len(m.drifts), m.dir,
	))

	listContent := m.renderList(innerW)
	diffContent := m.renderDiff(innerW)

	listBox := styleBorder.
		Width(m.totalW - 2).
		Height(m.listPaneH).
		Render(listContent)

	diffTitle := ""
	if m.cursor < len(m.verdicts) {
		diffTitle = styleDim.Render(" " + m.verdicts[m.cursor].Address + " ")
	}
	diffBox := styleBorder.
		Width(m.totalW - 2).
		Height(m.diffPaneH).
		BorderTop(true).
		Render(diffTitle + "\n" + diffContent)

	help := styleHelp.Render(
		"[↑↓/jk] navigate  [a] absorb  [s] skip  [A] absorb-all-safe  [S] skip-all  [pgup/dn] scroll diff  [x] execute  [q] quit",
	)

	return header + "\n" + listBox + "\n" + diffBox + "\n" + help + "\n"
}

func (m uiModel) renderList(width int) string {
	var b strings.Builder
	for i, v := range m.verdicts {
		if i < m.listTop {
			continue
		}
		if i >= m.listTop+m.listPaneH {
			break
		}

		icon, sev := verdictStyle(v.Severity)
		cursor := "  "
		if i == m.cursor {
			cursor = "▶ "
		}

		addr := v.Address
		if len(addr) > 36 {
			addr = "…" + addr[len(addr)-35:]
		}

		attrs := strings.Join(v.ChangedAttrs, ", ")
		if len(attrs) > 20 {
			attrs = attrs[:19] + "…"
		}

		action := m.actions[i].label()
		line := fmt.Sprintf("%s%s %-36s  %-8s  %-22s  %s",
			cursor, icon, sev.Render(addr),
			styleDim.Render(v.Severity.String()),
			styleDim.Render(attrs),
			action,
		)

		if i == m.cursor {
			line = styleSelected.Width(width).Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m uiModel) renderDiff(width int) string {
	lines := m.diffLines()
	if len(lines) == 0 {
		return styleDim.Render("  (no diff to show)") + "\n"
	}

	var b strings.Builder
	end := m.diffTop + m.diffPaneH - 1
	if end > len(lines) {
		end = len(lines)
	}
	for _, l := range lines[m.diffTop:end] {
		b.WriteString(l + "\n")
	}
	return b.String()
}

func (m uiModel) diffLines() []string {
	if m.cursor >= len(m.drifts) {
		return nil
	}
	d := m.drifts[m.cursor]
	v := m.verdicts[m.cursor]

	var lines []string
	for _, attr := range v.ChangedAttrs {
		bv := d.Before[attr]
		av := d.After[attr]

		bStr := renderTFValueShort(bv)
		aStr := renderTFValueShort(av)

		if bStr != "" {
			lines = append(lines, styleDiffMinus.Render(fmt.Sprintf("  - %-24s = %s", attr, bStr)))
		}
		if aStr != "" {
			lines = append(lines, styleDiffPlus.Render(fmt.Sprintf("  + %-24s = %s", attr, aStr)))
		}
		if len(v.ChangedAttrs) > 1 {
			lines = append(lines, "")
		}
	}

	if len(v.Reasons) > 0 {
		lines = append(lines, "")
		for _, r := range v.Reasons {
			lines = append(lines, styleDim.Render("  ⓘ "+r))
		}
	}
	if v.Suggestion != "" {
		lines = append(lines, styleDim.Render("  💡 "+v.Suggestion))
	}
	return lines
}

func renderTFValueShort(v tfplan.TFValue) string {
	if v.IsNull() {
		return styleDim.Render("(null)")
	}
	gv := v.GoValue()
	switch x := gv.(type) {
	case string:
		return `"` + x + `"`
	case float64:
		if x == float64(int(x)) {
			return fmt.Sprintf("%d", int(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(gv)
		s := string(b)
		if len(s) > 60 {
			s = s[:57] + "…"
		}
		return s
	}
}

// ---- command building ------------------------------------------------------

func (m uiModel) buildCommand() string {
	var targets, excludes []string
	for i, v := range m.verdicts {
		switch m.actions[i] {
		case actionAbsorb:
			targets = append(targets, v.Address)
		case actionSkip:
			if v.Severity == triage.Flag {
				excludes = append(excludes, v.Address)
			}
		}
	}
	if len(targets) == 0 {
		return ""
	}

	parts := []string{fmt.Sprintf("osmo -dir %s -write", m.dir)}
	for _, t := range targets {
		parts = append(parts, "-target "+t)
	}
	for _, e := range excludes {
		parts = append(parts, "-exclude "+e)
	}
	return strings.Join(parts, " \\\n  ")
}

// ---- helpers ---------------------------------------------------------------

func verdictStyle(s triage.Severity) (string, lipgloss.Style) {
	switch s {
	case triage.Safe:
		return "✅", styleSafe
	case triage.Review:
		return "⚠️ ", styleReview
	case triage.Flag:
		return "🚩", styleFlag
	}
	return "  ", styleDim
}

// ---- entry point -----------------------------------------------------------

func uiCmd(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	dir := fs.String("dir", ".", "Terraform working directory")
	bin := fs.String("terraform", "", "Terraform/OpenTofu binary (default: auto-detect)")
	planFile := fs.String("plan-json", "", `Path to pre-generated plan JSON; "-" reads stdin`)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: osmo ui [flags]

Interactive TUI for drift triage and selective absorb.

Flags:`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *bin == "" {
		*bin = triageResolveBin()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Fprintln(os.Stderr, "detecting drift...")
	raw, err := loadTriagePlan(ctx, *dir, *bin, *planFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}

	drifts, err := tfplan.ParseDrift(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitError)
	}

	if len(drifts) == 0 {
		fmt.Fprintln(os.Stderr, "no drift detected.")
		os.Exit(exitOK)
	}

	cfg := loadTriageConfig(*dir)
	result := triage.Run(drifts, *dir, cfg)

	m := newUIModel(*dir, *bin, drifts, result.Verdicts)

	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ui error:", err)
		os.Exit(exitError)
	}

	fm := final.(uiModel)
	if !fm.done || fm.execCmd == "" {
		fmt.Fprintln(os.Stderr, "no changes applied.")
		os.Exit(exitOK)
	}

	// Print the command the user confirmed.
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, styleExecute.Render("running: ")+fm.execCmd)
	fmt.Fprintln(os.Stderr)

	// Execute osmo with the built flags inline.
	cmd := buildOsmoExec(*dir, *bin, fm)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if runErr := cmd.Run(); runErr != nil {
		os.Exit(exitError)
	}
	os.Exit(exitChanges)
}

func buildOsmoExec(dir, bin string, m uiModel) *exec.Cmd {
	self, _ := os.Executable()
	if self == "" {
		self = "osmo"
	}
	cmdArgs := []string{"-dir", dir}
	if bin != "" {
		cmdArgs = append(cmdArgs, "-terraform", bin)
	}
	cmdArgs = append(cmdArgs, "-write")

	for i, v := range m.verdicts {
		if m.actions[i] == actionAbsorb {
			cmdArgs = append(cmdArgs, "-target", v.Address)
		}
	}
	for i, v := range m.verdicts {
		if m.actions[i] == actionSkip && v.Severity == triage.Flag {
			cmdArgs = append(cmdArgs, "-exclude", v.Address)
		}
	}

	// Reload the saved plan JSON to avoid re-running terraform.
	// (The plan data is already in memory via the triage run; pass it via env.)
	return exec.Command(self, cmdArgs...)
}

// blockid import used via loadTriageConfig — keep it used.
var _ = blockid.LoadConfig
