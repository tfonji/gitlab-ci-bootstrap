// Package report renders plan/apply output for job logs and consoles.
package report

import (
	"fmt"
	"io"
	"os"
	"strings"

	"gitlab-ci-bootstrap/internal/bootstrap"
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

// WritePlan prints nothing for a project that was filtered out of a batch: in a
// big group the skips would drown the real work, so they appear only in the
// batch summary.
func (t *Terminal) WritePlan(plan *bootstrap.Plan) {
	if plan.Skipped != "" {
		return
	}
	t.header("GITLAB CI BOOTSTRAP PLAN", plan)

	if plan.Error != "" {
		t.println(t.paint(ansiBold+ansiRed, "✗ Plan failed") + " — " + t.paint(ansiRed, plan.Error))
		t.println("")
		return
	}

	if plan.CsprojIssue != "" {
		t.println(t.paint(ansiYellow, "! .csproj not set") + " — " + plan.CsprojIssue + " (the MR asks for it by hand)")
	}
	if plan.SolutionIssue != "" {
		t.println(t.paint(ansiYellow, "! solution not set") + " — " + plan.SolutionIssue + " (the MR asks for it by hand)")
	}
	t.println(t.paint(ansiBold, "Files"))
	create, update, unchanged, manual := 0, 0, 0, 0
	for _, f := range plan.Files {
		symbol, color := fileSymbolColor(f.Action)
		t.println(fmt.Sprintf("  %s %s — %s", t.paint(color, symbol), f.TargetPath, f.Description))
		switch f.Action {
		case "create":
			create++
		case "update":
			update++
		case "skipped":
			manual++
		default:
			unchanged++
		}
	}
	t.rule()
	t.println(fmt.Sprintf("%s — %d create, %d update, %d unchanged, %d need a manual edit (no changes made)",
		t.paint(ansiBold+ansiBlue, "~ Plan complete"), create, update, unchanged, manual))
	t.println("")
}

func (t *Terminal) WriteResult(plan *bootstrap.Plan, result *bootstrap.Result) {
	if plan.Skipped != "" {
		return
	}
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

// WritePlanSummary prints one line per project after a multi-project plan.
func (t *Terminal) WritePlanSummary(plans []*bootstrap.Plan) {
	t.println(t.paint(ansiBold+ansiCyan, fmt.Sprintf("BATCH PLAN SUMMARY (%d projects)", len(plans))))
	t.rule()
	planned, skipped, failed := 0, 0, 0
	for _, p := range plans {
		switch {
		case p.Error != "":
			failed++
			t.println(fmt.Sprintf("%s %s — %s", t.paint(ansiRed, "✗"), p.ProjectPath, t.paint(ansiRed, p.Error)))
		case p.Skipped != "":
			skipped++
			t.println(fmt.Sprintf("%s %s — skipped: %s", t.paint(ansiDim, "-"), p.ProjectPath, p.Skipped))
		default:
			planned++
			changes := 0
			for _, f := range p.Files {
				if f.Action == "create" || f.Action == "update" {
					changes++
				}
			}
			symbol, color := "+", ansiGreen
			if changes == 0 {
				symbol, color = "·", ansiDim
			}
			t.println(fmt.Sprintf("%s %s — %d file(s) to change", t.paint(color, symbol), p.ProjectPath, changes))
		}
	}
	t.rule()
	t.println(fmt.Sprintf("%d planned, %d skipped, %d failed", planned, skipped, failed))
	t.println("")
}

// WriteApplySummary prints one line per project after a multi-project apply,
// with each merge request URL listed together so they're all in one place.
func (t *Terminal) WriteApplySummary(results []*bootstrap.Result) {
	t.println(t.paint(ansiBold+ansiCyan, fmt.Sprintf("BATCH APPLY SUMMARY (%d projects)", len(results))))
	t.rule()
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
		switch r.Status {
		case "applied":
			t.println(fmt.Sprintf("%s %s — %s", t.paint(ansiGreen, "✓"), r.ProjectPath, t.paint(ansiCyan, r.MRURL)))
		case "failed":
			t.println(fmt.Sprintf("%s %s — %s", t.paint(ansiRed, "✗"), r.ProjectPath, t.paint(ansiRed, r.Error)))
		default:
			line := fmt.Sprintf("%s %s — %s", t.paint(ansiDim, "·"), r.ProjectPath, r.Description)
			if r.MRURL != "" {
				line += " " + t.paint(ansiCyan, r.MRURL)
			}
			t.println(line)
		}
	}
	t.rule()
	t.println(fmt.Sprintf("%d applied, %d skipped/unchanged, %d failed",
		counts["applied"], len(results)-counts["applied"]-counts["failed"], counts["failed"]))
	t.println("")
}

func (t *Terminal) header(title string, plan *bootstrap.Plan) {
	t.println("")
	t.println(t.paint(ansiBold+ansiCyan, title))
	t.rule()
	project := plan.ProjectPath
	if plan.ProjectID != 0 {
		project = fmt.Sprintf("%s (id %d)", plan.ProjectPath, plan.ProjectID)
	}
	t.println(fmt.Sprintf("%s %s", t.paint(ansiBold, "Project: "), project))
	t.println(fmt.Sprintf("%s %s", t.paint(ansiBold, "Template:"), plan.Template))
	if plan.Version != "" {
		t.println(fmt.Sprintf("%s %s (%s)", t.paint(ansiBold, "Version: "), plan.Version, plan.VersionReason))
	}
	if plan.Csproj != "" {
		t.println(fmt.Sprintf("%s %s", t.paint(ansiBold, ".csproj: "), plan.Csproj))
	}
	if plan.Solution != "" {
		t.println(fmt.Sprintf("%s %s", t.paint(ansiBold, "Solution:"), plan.Solution))
	}
	if plan.Branch != "" {
		t.println(fmt.Sprintf("%s %s → %s", t.paint(ansiBold, "Branch:  "), plan.Branch, plan.BaseBranch))
	}
	t.rule()
}

func fileSymbolColor(action string) (symbol, color string) {
	switch action {
	case "create":
		return "+", ansiGreen
	case "update":
		return "~", ansiYellow
	case "skipped":
		return "!", ansiYellow
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
