// Package bootstrap implements the tool's whole job: given a project and a
// human-picked template name, work out which of the template's files are
// missing/stale (Plan), then commit all of them in one commit and open a
// single MR (Apply). Each bundle file's content comes from wherever its
// FileSpec.Source says: for the main .gitlab-ci.yml (Source == "include",
// the default), the content is a short GENERATED `include:project` stub
// pointing at the centralized dso-templates/ci-cd-components/pipeline-templates
// project -- the shared template's content is referenced, never copied, so
// this tool makes no API call to that project at all. Locally-sourced files
// (Source == "local") this tool owns, like settings.xml, are read verbatim
// from this repo's own checkout.
//
// This is deliberately NOT a reconciler over many projects -- template
// choice is a per-project, human decision (made via the pipeline UI's
// TEMPLATE_NAME dropdown), not a batch setting. Suggest exists only to hint
// at a likely template before that human decision; it never drives Plan or
// Apply on its own.
package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-ci-bootstrap/internal/config"
	"gitlab-ci-bootstrap/internal/gitlabclient"
	"gitlab-ci-bootstrap/internal/version"
)

type Bootstrapper struct {
	Client    *gitlabclient.Client
	Templates *config.Templates
}

func New(c *gitlabclient.Client, templates *config.Templates) *Bootstrapper {
	return &Bootstrapper{Client: c, Templates: templates}
}

// FileDiff is the outcome of comparing one bundle file against the
// project's current default branch.
type FileDiff struct {
	TargetPath string `json:"target_path"`
	// Action is "create", "update", "unchanged", or "skipped" (an existing
	// version file that has no literal version to rewrite -- needs a manual edit).
	Action      string `json:"action"`
	Description string `json:"description"`
	// Edit marks an existing repo file (pom.xml, package.json, ...) whose
	// version is rewritten in place, as opposed to a file generated from the
	// template bundle.
	Edit bool `json:"edit,omitempty"`
}

// Plan is the full set of file diffs for one project + template pick.
type Plan struct {
	ProjectID   int64      `json:"project_id"`
	ProjectPath string     `json:"project_path"`
	Template    string     `json:"template"`
	Branch      string     `json:"branch"`
	BaseBranch  string     `json:"base_branch"`
	Files       []FileDiff `json:"files"`
	// LatestTag is the repo's most recent tag ("" if none); Version is what
	// the version files are set to, derived from it (see version.Next).
	LatestTag     string `json:"latest_tag,omitempty"`
	Version       string `json:"version,omitempty"`
	VersionReason string `json:"version_reason,omitempty"`
	// Error is set instead of Files when planning this project failed (e.g.
	// the project couldn't be resolved) -- a batch run records it and moves on
	// so one bad project doesn't block the rest; Apply reports it as failed.
	Error string `json:"error,omitempty"`
}

// Result is the outcome of Apply.
type Result struct {
	ProjectID   int64  `json:"project_id"`
	ProjectPath string `json:"project_path"`
	Template    string `json:"template"`
	Status      string `json:"status"` // "applied", "skipped", "unchanged", "failed"
	Description string `json:"description"`
	MRURL       string `json:"mr_url,omitempty"`
	Error       string `json:"error,omitempty"`
}

func branchName(templateName string) string {
	return fmt.Sprintf("feature/add-%s", templateName)
}

// Plan compares every file in the named template's bundle against the
// project's current default branch.
func (b *Bootstrapper) Plan(ctx context.Context, projectID int64, templateName string) (*Plan, error) {
	tmpl := b.Templates.Find(templateName)
	if tmpl == nil {
		return nil, fmt.Errorf("unknown template %q (see list-templates)", templateName)
	}

	proj, _, err := b.Client.REST.Projects.GetProject(projectID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetching project: %w", err)
	}

	plan := &Plan{
		ProjectID:   projectID,
		ProjectPath: proj.PathWithNamespace,
		Template:    templateName,
		Branch:      branchName(templateName),
		BaseBranch:  proj.DefaultBranch,
	}

	latest, err := b.latestTag(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("looking up latest tag: %w", err)
	}
	plan.LatestTag = latest
	plan.Version, plan.VersionReason = version.Next(latest)

	for _, f := range tmpl.Files {
		desired, err := b.content(f, plan.Version)
		if err != nil {
			return nil, err
		}

		file, _, err := b.Client.REST.RepositoryFiles.GetFile(projectID, f.TargetPath, &gitlab.GetFileOptions{
			Ref: gitlab.Ptr(proj.DefaultBranch),
		}, gitlab.WithContext(ctx))

		switch {
		case err == nil && decodedContent(file) == desired:
			plan.Files = append(plan.Files, FileDiff{TargetPath: f.TargetPath, Action: "unchanged", Description: "already matches template"})
		case err == nil:
			plan.Files = append(plan.Files, FileDiff{TargetPath: f.TargetPath, Action: "update", Description: "differs from template, will be replaced"})
		case isNotFound(err):
			plan.Files = append(plan.Files, FileDiff{TargetPath: f.TargetPath, Action: "create", Description: "missing, will be created from template"})
		default:
			return nil, fmt.Errorf("checking %s: %w", f.TargetPath, err)
		}
	}

	edits, err := b.planVersionEdits(ctx, plan, tmpl)
	if err != nil {
		return nil, err
	}
	plan.Files = append(plan.Files, edits...)
	return plan, nil
}

// latestTag returns the name of the repo's most recent tag by commit date,
// or "" when it has none.
func (b *Bootstrapper) latestTag(ctx context.Context, projectID int64) (string, error) {
	tags, _, err := b.Client.REST.Tags.ListTags(projectID, &gitlab.ListTagsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 1},
		OrderBy:     gitlab.Ptr("updated"),
		Sort:        gitlab.Ptr("desc"),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return "", err
	}
	if len(tags) == 0 {
		return "", nil
	}
	return tags[0].Name, nil
}

// planVersionEdits finds the repo's existing version-carrying files (pom.xml,
// package.json, .csproj, properties files, ...) on the default branch and
// plans rewriting each to plan.Version. Files a template bundles itself
// (version.txt, .bumpversion.cfg, ...) are handled in content(), not here.
func (b *Bootstrapper) planVersionEdits(ctx context.Context, plan *Plan, tmpl *config.Template) ([]FileDiff, error) {
	tree, err := b.listTree(ctx, plan.ProjectID)
	if err != nil {
		if isNotFound(err) { // empty repository
			return nil, nil
		}
		return nil, fmt.Errorf("listing repository tree: %w", err)
	}

	bundled := map[string]bool{}
	for _, f := range tmpl.Files {
		bundled[f.TargetPath] = true
	}
	propertyFiles := templatePropertyFiles(tmpl)

	var diffs []FileDiff
	for _, node := range tree {
		if node.Type != "blob" || bundled[node.Path] {
			continue
		}
		edit := version.EditorFor(node.Path, propertyFiles)
		if edit == nil {
			continue
		}
		file, _, err := b.Client.REST.RepositoryFiles.GetFile(plan.ProjectID, node.Path, &gitlab.GetFileOptions{
			Ref: gitlab.Ptr(plan.BaseBranch),
		}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", node.Path, err)
		}
		content := decodedContent(file)
		out, old, err := edit([]byte(content), plan.Version)
		switch {
		case err != nil:
			diffs = append(diffs, FileDiff{TargetPath: node.Path, Action: "skipped", Edit: true,
				Description: "version not updated, set it by hand: " + err.Error()})
		case string(out) == content:
			diffs = append(diffs, FileDiff{TargetPath: node.Path, Action: "unchanged", Edit: true,
				Description: fmt.Sprintf("version already %s", plan.Version)})
		default:
			diffs = append(diffs, FileDiff{TargetPath: node.Path, Action: "update", Edit: true,
				Description: fmt.Sprintf("version %s → %s", old, plan.Version)})
		}
	}
	return diffs, nil
}

// templatePropertyFiles returns the template's PROPERTY_FILE override(s), the
// properties file(s) its pipeline reads the version from.
func templatePropertyFiles(tmpl *config.Template) []string {
	var paths []string
	for _, f := range tmpl.Files {
		for _, v := range f.ExtraVariables {
			if v.Name == "PROPERTY_FILE" && v.Value != "" {
				paths = append(paths, v.Value)
			}
		}
	}
	return paths
}

// Apply commits every non-unchanged file in the plan in a single commit
// (creating the branch from BaseBranch if it doesn't exist yet) and opens
// one MR. Idempotent: an already-open MR from the plan's branch is treated
// as "already done," and file actions are re-checked against the branch's
// current state (not just the plan) so re-running after a partial failure
// doesn't try to re-create a file that a previous attempt already added.
func (b *Bootstrapper) Apply(ctx context.Context, plan *Plan) (*Result, error) {
	result := &Result{ProjectID: plan.ProjectID, ProjectPath: plan.ProjectPath, Template: plan.Template}

	openMRs, _, err := b.Client.REST.MergeRequests.ListProjectMergeRequests(plan.ProjectID, &gitlab.ListProjectMergeRequestsOptions{
		SourceBranch: gitlab.Ptr(plan.Branch),
		State:        gitlab.Ptr("opened"),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("checking for existing MR: %w", err)
	}
	if len(openMRs) > 0 {
		result.Status = "skipped"
		result.Description = fmt.Sprintf("MR !%d already open", openMRs[0].IID)
		result.MRURL = openMRs[0].WebURL
		return result, nil
	}

	branchExists := true
	if _, _, err := b.Client.REST.Branches.GetBranch(plan.ProjectID, plan.Branch, gitlab.WithContext(ctx)); err != nil {
		branchExists = false
	}
	checkRef := plan.BaseBranch
	if branchExists {
		checkRef = plan.Branch
	}

	var actions []*gitlab.CommitActionOptions
	for _, f := range plan.Files {
		if f.Action == "unchanged" || f.Action == "skipped" {
			continue
		}
		var content string
		if f.Edit {
			edited, changed, err := b.editedContent(ctx, plan, f.TargetPath, checkRef)
			if err != nil {
				return nil, err
			}
			if !changed {
				continue
			}
			content = edited
		} else {
			content, err = b.templateContentFor(plan.Template, f.TargetPath, plan.Version)
			if err != nil {
				return nil, err
			}
		}

		action := gitlab.FileUpdate
		if _, _, err := b.Client.REST.RepositoryFiles.GetFile(plan.ProjectID, f.TargetPath, &gitlab.GetFileOptions{
			Ref: gitlab.Ptr(checkRef),
		}, gitlab.WithContext(ctx)); isNotFound(err) {
			action = gitlab.FileCreate
		}

		actions = append(actions, &gitlab.CommitActionOptions{
			Action:   gitlab.Ptr(action),
			FilePath: gitlab.Ptr(f.TargetPath),
			Content:  gitlab.Ptr(content),
		})
	}

	if len(actions) == 0 {
		result.Status = "unchanged"
		result.Description = "every file already matches the template, nothing to do"
		return result, nil
	}

	commitOpts := &gitlab.CreateCommitOptions{
		Branch:        gitlab.Ptr(plan.Branch),
		CommitMessage: gitlab.Ptr(fmt.Sprintf("Add %s CI/CD template", plan.Template)),
		Actions:       actions,
	}
	if !branchExists {
		commitOpts.StartBranch = gitlab.Ptr(plan.BaseBranch)
	}
	if _, _, err := b.Client.REST.Commits.CreateCommit(plan.ProjectID, commitOpts, gitlab.WithContext(ctx)); err != nil {
		return nil, fmt.Errorf("committing template files: %w", err)
	}

	mrOpts := &gitlab.CreateMergeRequestOptions{
		Title:              gitlab.Ptr(fmt.Sprintf("Add %s CI/CD template", plan.Template)),
		SourceBranch:       gitlab.Ptr(plan.Branch),
		TargetBranch:       gitlab.Ptr(plan.BaseBranch),
		RemoveSourceBranch: gitlab.Ptr(true),
	}
	if desc := mrDescription(b.Templates.Find(plan.Template), plan); desc != "" {
		mrOpts.Description = gitlab.Ptr(desc)
	}

	mr, _, err := b.Client.REST.MergeRequests.CreateMergeRequest(plan.ProjectID, mrOpts, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("opening merge request: %w", err)
	}

	result.Status = "applied"
	result.Description = fmt.Sprintf("opened MR !%d with %d file(s)", mr.IID, len(actions))
	result.MRURL = mr.WebURL
	return result, nil
}

// editedContent re-reads an in-place version file from ref (the MR branch if
// it already exists, so a re-run doesn't undo or double-apply an earlier
// attempt) and returns it rewritten to plan.Version.
func (b *Bootstrapper) editedContent(ctx context.Context, plan *Plan, filePath, ref string) (content string, changed bool, err error) {
	file, _, err := b.Client.REST.RepositoryFiles.GetFile(plan.ProjectID, filePath, &gitlab.GetFileOptions{
		Ref: gitlab.Ptr(ref),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", filePath, err)
	}
	current := decodedContent(file)
	edit := version.EditorFor(filePath, templatePropertyFiles(b.Templates.Find(plan.Template)))
	if edit == nil {
		return "", false, fmt.Errorf("no version editor for %s", filePath)
	}
	out, _, err := edit([]byte(current), plan.Version)
	if err != nil {
		return "", false, fmt.Errorf("setting version in %s: %w", filePath, err)
	}
	return string(out), string(out) != current, nil
}

// mrDescription renders a template's MRChecklist (if any) as a checklist
// for the MR body -- these are CI/CD variables this tool has no value for
// (per-project app config, per-environment secrets/hosts), so the best it
// can do is remind whoever reviews the MR to configure them by hand. Every
// template gets a standing reminder to set the project's CMDB ID Topic,
// regardless of whether it has a checklist.
func mrDescription(tmpl *config.Template, plan *Plan) string {
	var b strings.Builder
	b.WriteString("This MR adds the CI/CD template files. The following still need to be " +
		"configured manually in this project's CI/CD settings -- this tool has no way " +
		"to know their values.\n")

	b.WriteString("\n## Project settings\n\n" +
		"- [ ] Add this application's CMDB ID as a Topic on the project (Settings > General > Topics).\n")

	writeVersionSection(&b, plan)

	var checklist *config.MRChecklist
	if tmpl != nil {
		checklist = tmpl.MRChecklist
	}
	writeGroupLevelSection(&b, checklist)

	if checklist == nil {
		return b.String()
	}
	if len(checklist.PerProject) > 0 {
		b.WriteString("\n## Per-project CI/CD variables\n\n")
		for _, v := range checklist.PerProject {
			fmt.Fprintf(&b, "- [ ] `%s`\n", v)
		}
	}
	if checklist.PerEnvironment != nil && len(checklist.PerEnvironment.Variables) > 0 {
		envs := checklist.PerEnvironment.Environments
		envList := make([]string, len(envs))
		for i, e := range envs {
			envList[i] = fmt.Sprintf("`%s`", e)
		}
		fmt.Fprintf(&b, "\n## Per-environment CI/CD variables\n\n"+
			"Set a value for each deployment target: %s. In GitLab, set each "+
			"variable's Environment scope (Settings > CI/CD > Variables) to match "+
			"the target's environment name, or `*` if the same value applies "+
			"everywhere:\n\n", strings.Join(envList, ", "))
		for _, v := range checklist.PerEnvironment.Variables {
			fmt.Fprintf(&b, "- [ ] `%s`\n", v)
		}
	}
	if len(checklist.Notes) > 0 {
		b.WriteString("\n## Notes\n\n")
		for _, n := range checklist.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

// globalGroupVariables are group-level CI/CD variables every template
// needs, each with the value it must have -- like the CMDB ID reminder, a
// standing requirement across all MRs, so it lives here in code rather than
// being repeated in every template's group_level list.
var globalGroupVariables = []struct{ Name, Value string }{
	{"IS_ARTIFACTORY_ENABLED", "true"},
}

// writeGroupLevelSection lists the group-level variables that should already
// exist above the project: the global ones (with their required values), then
// the template's own.
func writeGroupLevelSection(b *strings.Builder, checklist *config.MRChecklist) {
	var templateVars []string
	if checklist != nil {
		templateVars = checklist.GroupLevel
	}
	b.WriteString("\n## Group-level CI/CD variables\n\n" +
		"These should already exist above this project (group or instance level) -- " +
		"confirm rather than assume:\n\n")
	global := map[string]bool{}
	for _, v := range globalGroupVariables {
		global[v.Name] = true
		fmt.Fprintf(b, "- [ ] `%s` -- set to `%s`\n", v.Name, v.Value)
	}
	for _, v := range templateVars {
		if !global[v] {
			fmt.Fprintf(b, "- [ ] `%s`\n", v)
		}
	}
}

// writeVersionSection tells the reviewer what version was written where and
// lists the version files that need a manual edit.
func writeVersionSection(b *strings.Builder, plan *Plan) {
	if plan == nil || plan.Version == "" {
		return
	}
	var updated, manual []string
	for _, f := range plan.Files {
		switch {
		case f.Action == "skipped":
			manual = append(manual, fmt.Sprintf("- [ ] Set the version to `%s` by hand in `%s` -- %s\n", plan.Version, f.TargetPath, strings.TrimPrefix(f.Description, "version not updated, set it by hand: ")))
		case f.Action != "unchanged" && (f.Edit || version.Bundled(f.TargetPath) != nil):
			updated = append(updated, "`"+f.TargetPath+"`")
		}
	}
	b.WriteString("\n## Version\n\n")
	fmt.Fprintf(b, "- [ ] Version set to `%s` (%s)", plan.Version, plan.VersionReason)
	if len(updated) > 0 {
		fmt.Fprintf(b, " in %s", strings.Join(updated, ", "))
	}
	b.WriteString(". Confirm this is the intended version.\n")
	for _, m := range manual {
		b.WriteString(m)
	}
}

// content resolves one bundle file's desired content, per FileSpec.Source:
// a local file is read verbatim; an include-sourced file's "content" is a
// short generated `include:project` stub -- no network call, since nothing
// is fetched from the shared templates project, only referenced.
func (b *Bootstrapper) content(f config.FileSpec, ver string) (string, error) {
	if f.IsLocal() {
		data, err := os.ReadFile(f.SourcePath)
		if err != nil {
			return "", fmt.Errorf("reading local file %s: %w", f.SourcePath, err)
		}
		if edit := version.Bundled(f.TargetPath); edit != nil && ver != "" {
			out, _, err := edit(data, ver)
			if err != nil {
				return "", fmt.Errorf("setting version in bundled file %s: %w", f.SourcePath, err)
			}
			data = out
		}
		return string(data), nil
	}
	return includeStub(b.Templates.RemoteSource, f.SourcePath, f.ExtraVariables), nil
}

// globalExtraVariables are written into the `variables:` block of every
// generated .gitlab-ci.yml stub, for every template, ahead of that
// template's own ExtraVariables -- analogous to the CMDB ID reminder in
// mrDescription, this is a standing requirement across all MRs this tool
// opens, so it lives here in code rather than being copy-pasted into each
// template entry in templates.yaml.
var globalExtraVariables = []config.ExtraVariable{
	{
		Name:        "LOG_LEVEL",
		Value:       "INFO",
		Description: "Application log level (INFO | WARN | DEBUG | ERROR)",
		Options:     []string{"INFO", "WARN", "DEBUG", "ERROR"},
	},
}

// includeStub generates a GitLab CI file that does nothing but include the
// shared template by reference, e.g.:
//
//	variables:
//	  DEPLOY_VARIABLE:
//	    value: "development"
//	    ...
//
//	include:
//	  - project: dso-templates/ci-cd-components/pipeline-templates
//	    ref: master
//	    file: 'templates/java-gradle-openshift-ci-cd.gitlab.yml'
//
// The include is deliberately last: the shared project's content is never
// copied into the target project, so template updates there apply
// automatically without this tool needing to re-run, and putting variable
// overrides above the include (rather than after it) matches how these
// files are conventionally hand-written. When extra is non-empty, the
// `variables:` block above it overrides whatever default the shared
// template sets for each of those names. A variable with only a Value
// renders as a plain scalar; one with a Description and/or Options
// renders in GitLab's extended form instead, so overriding a "Run
// pipeline"-prompted variable (e.g. DEPLOY_VARIABLE) keeps -- or corrects
// -- its description/dropdown rather than flattening it away.
func includeStub(remote config.RemoteSource, file string, extra []config.ExtraVariable) string {
	var b strings.Builder
	all := append(append([]config.ExtraVariable{}, globalExtraVariables...), extra...)
	if len(all) > 0 {
		b.WriteString("variables:\n")
		for _, v := range all {
			if v.Description == "" && len(v.Options) == 0 {
				fmt.Fprintf(&b, "  %s: %q\n", v.Name, v.Value)
				continue
			}
			fmt.Fprintf(&b, "  %s:\n    value: %q\n", v.Name, v.Value)
			if v.Description != "" {
				fmt.Fprintf(&b, "    description: %q\n", v.Description)
			}
			if len(v.Options) > 0 {
				b.WriteString("    options:\n")
				for _, o := range v.Options {
					fmt.Fprintf(&b, "      - %q\n", o)
				}
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "include:\n  - project: %s\n    ref: %s\n    file: '%s'\n", remote.ProjectPath, remote.Ref, file)
	return b.String()
}

func (b *Bootstrapper) templateContentFor(templateName, targetPath, ver string) (string, error) {
	tmpl := b.Templates.Find(templateName)
	if tmpl == nil {
		return "", fmt.Errorf("unknown template %q", templateName)
	}
	for _, f := range tmpl.Files {
		if f.TargetPath == targetPath {
			return b.content(f, ver)
		}
	}
	return "", fmt.Errorf("template %q has no file spec for target %q", templateName, targetPath)
}

func decodedContent(f *gitlab.File) string {
	// The API always returns Content base64-encoded (Encoding == "base64");
	// the client does not decode it for us.
	if f.Encoding != "base64" {
		return f.Content
	}
	raw, err := base64.StdEncoding.DecodeString(f.Content)
	if err != nil {
		return f.Content
	}
	return string(raw)
}

// isNotFound checks for go-gitlab's sentinel gitlab.ErrNotFound, which
// CheckResponse (gitlab.go in the client) returns for EVERY HTTP 404 --
// never a *gitlab.ErrorResponse with status 404. An earlier version of
// this function checked for *ErrorResponse instead, which never matched,
// so every missing file (the normal case for a project that doesn't have
// the target file yet) was misreported as a hard error instead of "create".
func isNotFound(err error) bool {
	return errors.Is(err, gitlab.ErrNotFound)
}

// Suggest is an informational hint only -- it never drives Plan/Apply. It
// runs the same signature-file heuristic the batch reconciler used to use
// internally, ported here as a starting point for whoever triggers the
// pipeline to pre-fill the TEMPLATE_NAME dropdown with a likely guess.
func (b *Bootstrapper) Suggest(ctx context.Context, projectID int64) (templateName, reason string, err error) {
	tree, err := b.listTree(ctx, projectID)
	if err != nil {
		return "", "", fmt.Errorf("listing repository tree: %w", err)
	}

	for _, tmpl := range b.Templates.Templates {
		for _, pattern := range tmpl.Detect {
			for _, node := range tree {
				if node.Type != "blob" {
					continue
				}
				candidate := node.Path
				if !strings.Contains(pattern, "/") {
					candidate = path.Base(node.Path)
				}
				matched, _ := path.Match(pattern, candidate)
				if !matched {
					continue
				}
				if tmpl.DetectContentContains == "" {
					return tmpl.Name, fmt.Sprintf("matched %s against %q (%s)", node.Path, tmpl.Name, pattern), nil
				}
				has, err := b.fileContains(ctx, projectID, node.Path, tmpl.DetectContentContains)
				if err != nil {
					return "", "", err
				}
				if has {
					return tmpl.Name, fmt.Sprintf("matched %s against %q (%s, contains %q)", node.Path, tmpl.Name, pattern, tmpl.DetectContentContains), nil
				}
			}
		}
	}
	return "", "no file in the repo matched any template's detect rules -- pick manually", nil
}

func (b *Bootstrapper) fileContains(ctx context.Context, projectID int64, filePath, substr string) (bool, error) {
	raw, _, err := b.Client.REST.RepositoryFiles.GetRawFile(projectID, filePath, nil, gitlab.WithContext(ctx))
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", filePath, err)
	}
	return strings.Contains(string(raw), substr), nil
}

func (b *Bootstrapper) listTree(ctx context.Context, projectID int64) ([]*gitlab.TreeNode, error) {
	var all []*gitlab.TreeNode
	opts := &gitlab.ListTreeOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		Recursive:   gitlab.Ptr(true),
	}
	for {
		page, resp, err := b.Client.REST.Repositories.ListTree(projectID, opts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}
