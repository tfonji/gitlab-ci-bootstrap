// Command gitlab-ci-bootstrap adds a CI/CD template bundle (.gitlab-ci.yml
// and whatever else it needs, e.g. settings.xml) to one project, as a merge
// request. Meant to be run from a GitLab CI pipeline where a human picks
// the target project and a template from a dropdown (see .gitlab-ci.yml) --
// unlike gitlab-post-migration, this is a per-project, human-driven tool,
// not a batch reconciler.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"github.com/tfonji/gitlab-ci-bootstrap/internal/bootstrap"
	"github.com/tfonji/gitlab-ci-bootstrap/internal/config"
	"github.com/tfonji/gitlab-ci-bootstrap/internal/gitlabclient"
	"github.com/tfonji/gitlab-ci-bootstrap/internal/report"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "suggest":
		err = runSuggest(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "apply":
		err = runApply(os.Args[2:])
	case "list-templates":
		err = runListTemplates(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `gitlab-ci-bootstrap <command> [flags]

Commands:
  suggest        print a best-guess template for a project (informational only)
  plan           diff a template's bundle against a project, write plan.json
  apply          commit a prior plan's files and open one MR
  list-templates print every template name from configs/templates.yaml`)
}

func terminal() *report.Terminal {
	return report.NewTerminal(os.Stderr, report.UseColor(os.Stderr))
}

func newBootstrapper(gitlabURL, token, templatesFile string) (*bootstrap.Bootstrapper, error) {
	c, err := gitlabclient.New(gitlabURL, token)
	if err != nil {
		return nil, err
	}
	templates, err := config.LoadTemplates(templatesFile)
	if err != nil {
		return nil, err
	}
	return bootstrap.New(c, templates), nil
}

func runSuggest(args []string) error {
	fs := flag.NewFlagSet("suggest", flag.ExitOnError)
	gitlabURL := fs.String("gitlab-url", envOr("CI_SERVER_URL", "https://gitlab.com"), "GitLab base URL")
	token := fs.String("token", os.Getenv("GITLAB_TOKEN"), "GitLab API token")
	templatesFile := fs.String("templates", "configs/templates.yaml", "templates config file")
	project := fs.String("project", os.Getenv("PROJECT_ID"), "target project ID or path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	b, err := newBootstrapper(*gitlabURL, *token, *templatesFile)
	if err != nil {
		return err
	}
	projectID, err := resolveProjectID(context.Background(), b, *project)
	if err != nil {
		return err
	}

	name, reason, err := b.Suggest(context.Background(), projectID)
	if err != nil {
		return err
	}
	if name == "" {
		fmt.Printf("no suggestion: %s\n", reason)
		return nil
	}
	fmt.Printf("suggested template: %s (%s)\n", name, reason)
	return nil
}

func runPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	gitlabURL := fs.String("gitlab-url", envOr("CI_SERVER_URL", "https://gitlab.com"), "GitLab base URL")
	token := fs.String("token", os.Getenv("GITLAB_TOKEN"), "GitLab API token")
	templatesFile := fs.String("templates", "configs/templates.yaml", "templates config file")
	project := fs.String("project", os.Getenv("PROJECT_ID"), "target project ID or path; comma-separate several to apply the same template to each")
	template := fs.String("template", os.Getenv("TEMPLATE_NAME"), "template name (see list-templates)")
	out := fs.String("out", "plan.json", "output plan file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *template == "" {
		return fmt.Errorf("--template is required")
	}

	refs := splitProjects(*project)
	if len(refs) == 0 {
		return fmt.Errorf("--project is required")
	}

	b, err := newBootstrapper(*gitlabURL, *token, *templatesFile)
	if err != nil {
		return err
	}

	ctx := context.Background()
	term := terminal()
	plans := make([]*bootstrap.Plan, 0, len(refs))
	failed := 0
	for _, ref := range refs {
		plan, err := planProject(ctx, b, ref, *template)
		if err != nil {
			plan = &bootstrap.Plan{ProjectPath: ref, Template: *template, Error: err.Error()}
			failed++
		}
		term.WritePlan(plan)
		plans = append(plans, plan)
	}
	if len(plans) > 1 {
		term.WritePlanSummary(plans)
	}
	if err := writeJSON(*out, plans); err != nil {
		return err
	}
	// A partial failure is recorded in the plan file and reported by apply;
	// only fail the job when there is nothing at all to apply.
	if failed == len(plans) {
		return fmt.Errorf("planning failed for every project")
	}
	return nil
}

func planProject(ctx context.Context, b *bootstrap.Bootstrapper, ref, template string) (*bootstrap.Plan, error) {
	projectID, err := resolveProjectID(ctx, b, ref)
	if err != nil {
		return nil, err
	}
	return b.Plan(ctx, projectID, template)
}

func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	gitlabURL := fs.String("gitlab-url", envOr("CI_SERVER_URL", "https://gitlab.com"), "GitLab base URL")
	token := fs.String("token", os.Getenv("GITLAB_TOKEN"), "GitLab API token")
	templatesFile := fs.String("templates", "configs/templates.yaml", "templates config file")
	planFile := fs.String("plan", "plan.json", "plan file produced by `plan`")
	out := fs.String("out", "result.json", "output result file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	b, err := newBootstrapper(*gitlabURL, *token, *templatesFile)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(*planFile)
	if err != nil {
		return fmt.Errorf("reading plan file %s: %w", *planFile, err)
	}
	var plans []*bootstrap.Plan
	if err := json.Unmarshal(data, &plans); err != nil {
		return fmt.Errorf("decoding plan file %s: %w", *planFile, err)
	}

	ctx := context.Background()
	term := terminal()
	results := make([]*bootstrap.Result, 0, len(plans))
	failed := 0
	for _, plan := range plans {
		result := applyProject(ctx, b, plan)
		if result.Status == "failed" {
			failed++
		}
		term.WriteResult(plan, result)
		results = append(results, result)
	}
	if len(results) > 1 {
		term.WriteApplySummary(results)
	}
	if err := writeJSON(*out, results); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d project(s) failed", failed, len(results))
	}
	return nil
}

// applyProject never returns an error: a failure becomes a "failed" Result so
// the remaining projects in a batch still get applied.
func applyProject(ctx context.Context, b *bootstrap.Bootstrapper, plan *bootstrap.Plan) *bootstrap.Result {
	failure := func(msg string) *bootstrap.Result {
		return &bootstrap.Result{ProjectID: plan.ProjectID, ProjectPath: plan.ProjectPath, Template: plan.Template,
			Status: "failed", Description: "could not apply", Error: msg}
	}
	if plan.Error != "" {
		return failure("planning failed: " + plan.Error)
	}
	result, err := b.Apply(ctx, plan)
	if err != nil {
		return failure(err.Error())
	}
	return result
}

// splitProjects parses a comma-separated project list, dropping blanks and
// duplicates while keeping order.
func splitProjects(s string) []string {
	var refs []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		refs = append(refs, p)
	}
	return refs
}

func runListTemplates(args []string) error {
	fs := flag.NewFlagSet("list-templates", flag.ExitOnError)
	templatesFile := fs.String("templates", "configs/templates.yaml", "templates config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	templates, err := config.LoadTemplates(*templatesFile)
	if err != nil {
		return err
	}
	for _, t := range templates.Templates {
		fmt.Println(t.Name)
	}
	return nil
}

// resolveProjectID accepts either a numeric project ID or a
// path_with_namespace and always returns the numeric ID, since some API
// calls (repository tree, commits) are cleanest with it.
func resolveProjectID(ctx context.Context, b *bootstrap.Bootstrapper, project string) (int64, error) {
	if id, err := strconv.ParseInt(project, 10, 64); err == nil {
		return id, nil
	}
	proj, _, err := b.Client.REST.Projects.GetProject(project, nil, gitlab.WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("resolving project %q: %w", project, err)
	}
	return proj.ID, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
