// Package report renders plan/apply output for job logs and consoles.
package report

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tfonji/gitlab-ci-bootstrap/internal/bootstrap"
)

const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiCyan   = "\033[36m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
)

const ruleWidth = 70

// Terminal writes a colored, symbol-prefixed view of a plan or an apply
// result: a header naming the project/template/branch, the per-file outcome,
// a one-line summary, and (after apply) the merge request URL on its own
// line so it can be clicked straight from the job log.
type Terminal struct {
	w     io.Writer
	color bool
}

func NewTerminal(w io.Writer, color bool) *Terminal {
	return &Terminal{w: w, color: color}
}

// UseColor decides whether ANSI color should be emitted to f: never when
// NO_COLOR is set, always when FORCE_COLOR is set or under GitLab CI (whose
// job log viewer renders ANSI even though stderr isn't a TTY there), and
// otherwise only when f is an interactive terminal.
func UseColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" || os.Getenv("GITLAB_CI") == "true" {
		return true
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (t *Terminal) WritePlan(plan *bootstrap.Plan) {
	t.header("GITLAB CI BOOTSTRAP PLAN", plan)

	t.println(t.paint(ansiBold, "Files"))
	create, update, unchanged := 0, 0, 0
	for _, f := range plan.Files {
		symbol, color := fileSymbolColor(f.Action)
		t.println(fmt.Sprintf("  %s %s — %s", t.paint(color, symbol), f.TargetPath, f.Description))
		switch f.Action {
		case "create":
			create++
		case "update":
			update++
		default:
			unchanged++
		}
	}
	t.rule()
	t.println(fmt.Sprintf("%s — %d create, %d update, %d unchanged (no changes made)",
		t.paint(ansiBold+ansiBlue, "~ Plan complete"), create, update, unchanged))
	t.println("")
}

func (t *Terminal) WriteResult(plan *bootstrap.Plan, result *bootstrap.Result) {
	t.header("GITLAB CI BOOTSTRAP APPLY", plan)

	switch result.Status {
	case "applied":
		t.println(t.paint(ansiBold+ansiGreen, "✓ Applied") + " — " + result.Description)
	case "failed":
		t.println(t.paint(ansiBold+ansiRed, "✗ Failed") + " — " + result.Description)
		if result.Error != "" {
			t.println("  " + t.paint(ansiRed, result.Error))
		}
	default:
		t.println(t.paint(ansiBold+ansiDim, "· "+strings.ToUpper(result.Status[:1])+result.Status[1:]) + " — " + result.Description)
	}
	if result.MRURL != "" {
		t.println("")
		t.println(t.paint(ansiBold, "Merge request: ") + t.paint(ansiCyan, result.MRURL))
	}
	t.println("")
}

func (t *Terminal) header(title string, plan *bootstrap.Plan) {
	t.println("")
	t.println(t.paint(ansiBold+ansiCyan, title))
	t.rule()
	t.println(fmt.Sprintf("%s %s (id %d)", t.paint(ansiBold, "Project: "), plan.ProjectPath, plan.ProjectID))
	t.println(fmt.Sprintf("%s %s", t.paint(ansiBold, "Template:"), plan.Template))
	t.println(fmt.Sprintf("%s %s → %s", t.paint(ansiBold, "Branch:  "), plan.Branch, plan.BaseBranch))
	t.rule()
}

func fileSymbolColor(action string) (symbol, color string) {
	switch action {
	case "create":
		return "+", ansiGreen
	case "update":
		return "~", ansiYellow
	default:
		return "·", ansiDim
	}
}

func (t *Terminal) paint(code, s string) string {
	if !t.color {
		return s
	}
	return code + s + ansiReset
}

func (t *Terminal) rule() {
	t.println(strings.Repeat("─", ruleWidth))
}

func (t *Terminal) println(s string) {
	fmt.Fprintln(t.w, s)
}
