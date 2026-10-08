package bootstrap

import (
	"context"
	"fmt"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go"
)

// GroupOptions controls which projects a group expands to. Archived projects,
// empty repositories and forks are always left out; that isn't configurable.
type GroupOptions struct {
	IncludeSubgroups bool
}

// PlanRequest is one batch: the same template applied to every named
// project and every project in every named group.
type PlanRequest struct {
	Template string
	Projects []string // numeric IDs or paths
	Groups   []string // numeric IDs or paths; each expands to its projects
	Exclude  []string // project IDs or paths never planned, wherever they came from
	Group    GroupOptions
}

// ResolveProjectID accepts either a numeric project ID or a
// path_with_namespace and always returns the numeric ID, since some API
// calls (repository tree, commits) are cleanest with it.
func (b *Bootstrapper) ResolveProjectID(ctx context.Context, project string) (int64, error) {
	if id, err := strconv.ParseInt(project, 10, 64); err == nil {
		return id, nil
	}
	proj, _, err := b.Client.REST.Projects.GetProject(project, nil, gitlab.WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("resolving project %q: %w", project, err)
	}
	return proj.ID, nil
}

// PlanAll plans every project in the request, calling onPlan as each one
// finishes so a long group run streams its progress. A project that can't be
// planned (or a group that can't be listed) becomes a Plan with Error set; one
// that is filtered out becomes a Plan with Skipped set -- neither stops the
// rest. Projects are planned once even if named twice or reached through
// several groups. Filters (archived/empty/fork) apply only to projects that
// came from a group: naming a project explicitly overrides them. Exclude wins
// over everything.
func (b *Bootstrapper) PlanAll(ctx context.Context, req PlanRequest, onPlan func(*Plan)) []*Plan {
	var plans []*Plan
	emit := func(p *Plan) {
		plans = append(plans, p)
		if onPlan != nil {
			onPlan(p)
		}
	}
	excluded := map[string]bool{}
	for _, e := range req.Exclude {
		excluded[e] = true
	}
	seen := map[int64]bool{}

	plan := func(id int64, path, skip string) {
		if seen[id] {
			return
		}
		seen[id] = true
		switch {
		case excluded[strconv.FormatInt(id, 10)] || (path != "" && excluded[path]):
			emit(&Plan{ProjectID: id, ProjectPath: path, Template: req.Template, Skipped: "excluded via EXCLUDE_PROJECT_IDS"})
		case skip != "":
			emit(&Plan{ProjectID: id, ProjectPath: path, Template: req.Template, Skipped: skip})
		default:
			p, err := b.Plan(ctx, id, req.Template)
			if err != nil {
				p = &Plan{ProjectID: id, ProjectPath: path, Template: req.Template, Error: err.Error()}
			}
			emit(p)
		}
	}

	for _, ref := range req.Projects {
		id, err := b.ResolveProjectID(ctx, ref)
		if err != nil {
			emit(&Plan{ProjectPath: ref, Template: req.Template, Error: err.Error()})
			continue
		}
		path := ""
		if _, numeric := strconv.ParseInt(ref, 10, 64); numeric != nil {
			path = ref
		}
		plan(id, path, "")
	}

	for _, g := range req.Groups {
		projects, err := b.listGroupProjects(ctx, g, req.Group)
		if err != nil {
			emit(&Plan{ProjectPath: "group " + g, Template: req.Template, Error: err.Error()})
			continue
		}
		for _, p := range projects {
			plan(p.ID, p.PathWithNamespace, groupSkipReason(p))
		}
	}
	return plans
}

// groupSkipReason returns why a group-derived project is filtered out, or "".
func groupSkipReason(p *gitlab.Project) string {
	switch {
	case p.Archived:
		return "archived project"
	case p.EmptyRepo:
		return "empty repository"
	case p.ForkedFromProject != nil:
		return "fork of " + p.ForkedFromProject.PathWithNamespace
	}
	return ""
}

// listGroupProjects returns every project in the group, following pagination.
// Projects merely shared into the group from elsewhere are left out -- they
// belong to another group.
func (b *Bootstrapper) listGroupProjects(ctx context.Context, group string, o GroupOptions) ([]*gitlab.Project, error) {
	opts := &gitlab.ListGroupProjectsOptions{
		ListOptions:      gitlab.ListOptions{PerPage: 100},
		IncludeSubGroups: gitlab.Ptr(o.IncludeSubgroups),
		WithShared:       gitlab.Ptr(false),
	}
	var all []*gitlab.Project
	for {
		page, resp, err := b.Client.REST.Groups.ListGroupProjects(group, opts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("listing projects of group %q: %w", group, err)
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}
