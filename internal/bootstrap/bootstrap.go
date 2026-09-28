// Package bootstrap implements the tool's whole job: given a project and a
// human-picked template name, work out which of the template's files are
// missing/stale (Plan), then commit all of them in one commit and open a
// single MR (Apply). Every bundle file's content is read from this tool's
// own repo checkout (config.FileSpec.SourcePath) -- template content used
// to be fetched from a separate centralized GitLab project, but that added
// a failure mode (wrong path/permissions/ref against a project this tool
// doesn't control) for no real benefit once the templates moved here.
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

	"github.com/tfonji/gitlab-ci-bootstrap/internal/config"
	"github.com/tfonji/gitlab-ci-bootstrap/internal/gitlabclient"
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
	TargetPath  string `json:"target_path"`
	Action      string `json:"action"` // "create", "update", or "unchanged"
	Description string `json:"description"`
}

// Plan is the full set of file diffs for one project + template pick.
type Plan struct {
	ProjectID   int64      `json:"project_id"`
	ProjectPath string     `json:"project_path"`
	Template    string     `json:"template"`
	Branch      string     `json:"branch"`
	BaseBranch  string     `json:"base_branch"`
	Files       []FileDiff `json:"files"`
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
	return fmt.Sprintf("bootstrap/add-%s", templateName)
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

	for _, f := range tmpl.Files {
		desired, err := readFile(f)
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
	return plan, nil
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
		if f.Action == "unchanged" {
			continue
		}
		content, err := b.templateContentFor(plan.Template, f.TargetPath)
		if err != nil {
			return nil, err
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

	mr, _, err := b.Client.REST.MergeRequests.CreateMergeRequest(plan.ProjectID, &gitlab.CreateMergeRequestOptions{
		Title:              gitlab.Ptr(fmt.Sprintf("Add %s CI/CD template", plan.Template)),
		SourceBranch:       gitlab.Ptr(plan.Branch),
		TargetBranch:       gitlab.Ptr(plan.BaseBranch),
		RemoveSourceBranch: gitlab.Ptr(true),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("opening merge request: %w", err)
	}

	result.Status = "applied"
	result.Description = fmt.Sprintf("opened MR !%d with %d file(s)", mr.IID, len(actions))
	result.MRURL = mr.WebURL
	return result, nil
}

// readFile reads one bundle file's content from this tool's own repo
// checkout -- always the version at whatever commit the CI job checked out,
// versioned the same way as the rest of this tool.
func readFile(f config.FileSpec) (string, error) {
	data, err := os.ReadFile(f.SourcePath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", f.SourcePath, err)
	}
	return string(data), nil
}

func (b *Bootstrapper) templateContentFor(templateName, targetPath string) (string, error) {
	tmpl := b.Templates.Find(templateName)
	if tmpl == nil {
		return "", fmt.Errorf("unknown template %q", templateName)
	}
	for _, f := range tmpl.Files {
		if f.TargetPath == targetPath {
			return readFile(f)
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
