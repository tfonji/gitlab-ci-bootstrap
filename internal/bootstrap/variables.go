package bootstrap

import (
	"context"
	"fmt"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go"

	"gitlab-ci-bootstrap/internal/config"
)

// VarDiff is the planned state of one CI/CD variable a template sets in GitLab.
type VarDiff struct {
	Scope string `json:"scope"` // config.ScopeGroup or config.ScopeProject
	// Target is the group's full path or the project's path.
	Target           string `json:"target"`
	Name             string `json:"name"`
	Value            string `json:"value"`
	EnvironmentScope string `json:"environment_scope"`
	Masked           bool   `json:"masked,omitempty"`
	Protected        bool   `json:"protected,omitempty"`
	Overwrite        bool   `json:"overwrite"`
	// Action is "create", "update", "unchanged", "skipped" (exists and is left
	// alone), "unknown" (current value couldn't be read; Apply will try), or
	// "unavailable" (can't be set at all, e.g. no group to hold it).
	Action      string `json:"action"`
	Description string `json:"description"`
}

// VarResult is the outcome of applying one VarDiff.
type VarResult struct {
	Scope  string `json:"scope"`
	Target string `json:"target"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Masked bool   `json:"masked,omitempty"`
	// Status is "created", "updated", "unchanged", "skipped" or "failed".
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ShownValue is the value for display: never a masked one.
func (v VarDiff) ShownValue() string   { return shownValue(v.Value, v.Masked) }
func (r VarResult) ShownValue() string { return shownValue(r.Value, r.Masked) }

func shownValue(value string, masked bool) string {
	if masked {
		return "***"
	}
	return value
}

type currentVar struct {
	Value     string
	Masked    bool
	Protected bool
}

func envScope(v config.CIVariable) string {
	if v.EnvironmentScope == "" {
		return "*"
	}
	return v.EnvironmentScope
}

// groupTarget returns the full path of the group that holds a group-scoped
// variable for a project in namespace ns, or "" when there is none (a personal
// namespace).
func groupTarget(ns *gitlab.ProjectNamespace, level string) string {
	if ns == nil || ns.Kind != "group" || ns.FullPath == "" {
		return ""
	}
	if level == config.GroupParent {
		return ns.FullPath
	}
	top, _, _ := strings.Cut(ns.FullPath, "/")
	return top
}

// planVariables compares each of the template's variables with what GitLab
// holds now.
func (b *Bootstrapper) planVariables(ctx context.Context, proj *gitlab.Project, tmpl *config.Template) []VarDiff {
	var diffs []VarDiff
	for _, v := range tmpl.Variables {
		d := VarDiff{
			Scope: v.Scope, Name: v.Name, Value: v.Value, EnvironmentScope: envScope(v),
			Masked: v.Masked, Protected: v.Protected, Overwrite: v.ShouldOverwrite(),
		}
		if v.Scope == config.ScopeProject {
			d.Target = proj.PathWithNamespace
		} else if d.Target = groupTarget(proj.Namespace, v.Group); d.Target == "" {
			d.Action = "unavailable"
			d.Description = "the project is in a personal namespace, so there is no group to hold it"
			diffs = append(diffs, d)
			continue
		}

		cur, found, err := b.currentVariable(ctx, proj.ID, d)
		switch {
		case err != nil:
			d.Action, d.Description = "unknown", "could not read the current value ("+err.Error()+"), will try to set it"
		case !found:
			d.Action, d.Description = "create", "missing, will be created"
		case cur.Value == d.Value && cur.Masked == d.Masked && cur.Protected == d.Protected:
			d.Action, d.Description = "unchanged", "already set"
		case !d.Overwrite:
			d.Action, d.Description = "skipped", fmt.Sprintf("exists as %s, left alone (overwrite is off)", shownValue(cur.Value, cur.Masked))
		default:
			d.Action, d.Description = "update", fmt.Sprintf("%s → %s", shownValue(cur.Value, cur.Masked), d.ShownValue())
		}
		diffs = append(diffs, d)
	}
	return diffs
}

func (b *Bootstrapper) currentVariable(ctx context.Context, projectID int64, d VarDiff) (cur currentVar, found bool, err error) {
	filter := &gitlab.VariableFilter{EnvironmentScope: d.EnvironmentScope}
	switch d.Scope {
	case config.ScopeGroup:
		gv, _, err := b.Client.REST.GroupVariables.GetVariable(d.Target, d.Name, &gitlab.GetGroupVariableOptions{Filter: filter}, gitlab.WithContext(ctx))
		if isNotFound(err) {
			return cur, false, nil
		}
		if err != nil {
			return cur, false, err
		}
		return currentVar{gv.Value, gv.Masked, gv.Protected}, true, nil
	default:
		pv, _, err := b.Client.REST.ProjectVariables.GetVariable(projectID, d.Name, &gitlab.GetProjectVariableOptions{Filter: filter}, gitlab.WithContext(ctx))
		if isNotFound(err) {
			return cur, false, nil
		}
		if err != nil {
			return cur, false, err
		}
		return currentVar{pv.Value, pv.Masked, pv.Protected}, true, nil
	}
}

// applyVariables sets every planned variable. A group variable shared by many
// projects in one run is written once. A failure is recorded, not returned: the
// MR is still opened and asks for that variable by hand.
func (b *Bootstrapper) applyVariables(ctx context.Context, plan *Plan) []VarResult {
	var results []VarResult
	for _, d := range plan.Variables {
		results = append(results, b.applyVariable(ctx, plan, d))
	}
	return results
}

func (b *Bootstrapper) applyVariable(ctx context.Context, plan *Plan, d VarDiff) VarResult {
	r := VarResult{Scope: d.Scope, Target: d.Target, Name: d.Name, Value: d.Value, Masked: d.Masked}
	switch d.Action {
	case "unchanged":
		r.Status = "unchanged"
		return r
	case "skipped":
		r.Status, r.Detail = "skipped", d.Description
		return r
	case "unavailable":
		r.Status, r.Detail = "failed", d.Description
		return r
	}

	key := strings.Join([]string{d.Scope, d.Target, d.Name, d.EnvironmentScope, d.Value}, "\x00")
	if d.Scope == config.ScopeProject {
		key += fmt.Sprint(plan.ProjectID)
	}
	b.varMu.Lock()
	defer b.varMu.Unlock()
	if done, ok := b.varDone[key]; ok {
		done.Status, done.Detail = "unchanged", "already set earlier in this run"
		return done
	}

	if !d.Overwrite && d.Action == "unknown" {
		if _, found, err := b.currentVariable(ctx, plan.ProjectID, d); err == nil && found {
			r.Status, r.Detail = "skipped", "exists, left alone (overwrite is off)"
			return r
		}
	}
	created, err := b.upsertVariable(ctx, plan.ProjectID, d)
	switch {
	case err != nil:
		r.Status, r.Detail = "failed", err.Error()
		return r // not cached: a later project may succeed with a fixed token
	case created:
		r.Status = "created"
	default:
		r.Status = "updated"
	}
	b.varDone[key] = r
	return r
}

// upsertVariable updates the variable, creating it if it doesn't exist yet.
func (b *Bootstrapper) upsertVariable(ctx context.Context, projectID int64, d VarDiff) (created bool, err error) {
	filter := &gitlab.VariableFilter{EnvironmentScope: d.EnvironmentScope}
	opt := gitlab.WithContext(ctx)
	switch d.Scope {
	case config.ScopeGroup:
		_, _, err = b.Client.REST.GroupVariables.UpdateVariable(d.Target, d.Name, &gitlab.UpdateGroupVariableOptions{
			Value: &d.Value, Masked: &d.Masked, Protected: &d.Protected, Filter: filter,
		}, opt)
		if isNotFound(err) {
			_, _, err = b.Client.REST.GroupVariables.CreateVariable(d.Target, &gitlab.CreateGroupVariableOptions{
				Key: &d.Name, Value: &d.Value, Masked: &d.Masked, Protected: &d.Protected, EnvironmentScope: &d.EnvironmentScope,
			}, opt)
			return err == nil, err
		}
	default:
		_, _, err = b.Client.REST.ProjectVariables.UpdateVariable(projectID, d.Name, &gitlab.UpdateProjectVariableOptions{
			Value: &d.Value, Masked: &d.Masked, Protected: &d.Protected, Filter: filter,
		}, opt)
		if isNotFound(err) {
			_, _, err = b.Client.REST.ProjectVariables.CreateVariable(projectID, &gitlab.CreateProjectVariableOptions{
				Key: &d.Name, Value: &d.Value, Masked: &d.Masked, Protected: &d.Protected, EnvironmentScope: &d.EnvironmentScope,
			}, opt)
			return err == nil, err
		}
	}
	return false, err
}

// variableSummary is the short form for a Result description, e.g.
// "variables: 5 created, 1 failed", or "" when there were none to report.
func variableSummary(results []VarResult) string {
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	var parts []string
	for _, s := range []string{"created", "updated", "failed", "skipped"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "variables: " + strings.Join(parts, ", ")
}

// changedVariables reports whether any variable was created or updated.
func changedVariables(results []VarResult) bool {
	for _, r := range results {
		if r.Status == "created" || r.Status == "updated" {
			return true
		}
	}
	return false
}

// writeVariablesSection asks for the variables the tool could not set to be
// set by hand.
func writeVariablesSection(b *strings.Builder, results []VarResult) {
	var items []string
	for _, r := range results {
		if r.Status != "failed" {
			continue
		}
		where := "project `" + r.Target + "`"
		if r.Scope == config.ScopeGroup {
			where = "group `" + r.Target + "`"
		}
		items = append(items, fmt.Sprintf("- [ ] Set the CI/CD variable `%s` to `%s` in %s (Settings > CI/CD > Variables) -- the tool could not set it: %s\n", r.Name, r.ShownValue(), where, r.Detail))
	}
	if len(items) == 0 {
		return
	}
	b.WriteString("\n## CI/CD variables to set by hand\n\n")
	for _, it := range items {
		b.WriteString(it)
	}
}
