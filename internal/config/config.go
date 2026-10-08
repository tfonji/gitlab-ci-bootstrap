// Package config loads the template bundle definitions this tool offers.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Templates is every CI/CD template bundle this tool can add to a project.
type Templates struct {
	// RemoteSource is the centralized pipeline-templates project every
	// template's main .gitlab-ci.yml (Source == SourceInclude, the default)
	// points at via a GitLab CI `include:project` reference -- see
	// bootstrap.includeStub. This project's content is never fetched or
	// copied; only its path/ref/file are referenced.
	RemoteSource RemoteSource `yaml:"remote_source"`
	Templates    []Template   `yaml:"templates"`
}

type RemoteSource struct {
	ProjectPath string `yaml:"project_path"` // "dso-templates/ci-cd-components/pipeline-templates"
	Ref         string `yaml:"ref"`          // "master"
}

// Template is one selectable pipeline-UI option: a name and every file it
// adds/replaces in the target project -- picking the template adds the
// whole bundle, there's no separate per-file choice.
type Template struct {
	Name  string     `yaml:"name"`
	Files []FileSpec `yaml:"files"`

	// MRChecklist lists CI/CD variables this tool cannot supply a value for
	// (per-project app config, per-environment secrets/hosts) -- when set,
	// Apply prints them as a checklist in the opened MR's description so
	// the engineer knows what's still left to configure by hand.
	MRChecklist *MRChecklist `yaml:"mr_checklist,omitempty"`

	// Detect/DetectContentContains are used only by the `suggest` command,
	// an informational hint printed for whoever triggers the pipeline --
	// they never decide what gets applied. The human's template pick
	// (TEMPLATE_NAME pipeline variable) is the only thing Plan/Apply act on.
	// CsprojVersionElement is the element name written when a .csproj has no
	// version yet ("Version" when empty). It must be the spelling the
	// template's own version bump looks for.
	CsprojVersionElement string `yaml:"csproj_version_element,omitempty"`

	// CsprojApplicationName makes sure the detected .csproj has an
	// <ApplicationName> (the file name without .csproj when it has none), which
	// the template's pipeline names the build artifact after.
	CsprojApplicationName bool `yaml:"csproj_application_name,omitempty"`

	Detect                []string `yaml:"detect,omitempty"`
	DetectContentContains string   `yaml:"detect_content_contains,omitempty"`
}

// MRChecklist is the set of CI/CD variables a template's MR description
// should remind the engineer to configure manually -- split by scope:
// group-level (should already exist above this project), per-project
// (one value for the whole app), and per-environment (one value per
// deployment target, set via GitLab's per-variable "Environment scope").
type MRChecklist struct {
	// GroupLevel names variables this template's scripts read that are
	// expected to already exist as group-level CI/CD variables (shared
	// across many projects/templates) -- listed so the reviewer can
	// confirm rather than assume.
	GroupLevel []string `yaml:"group_level,omitempty"`
	PerProject []string `yaml:"per_project,omitempty"`
	// PerEnvironment is nil when the template has no per-environment
	// variables.
	PerEnvironment *PerEnvironmentChecklist `yaml:"per_environment,omitempty"`
	// Notes are free-text callouts for anything the plain variable-name
	// checklist can't express, e.g. a naming constraint between two
	// variables, a placeholder to fill in inside a bundled file, or a
	// caveat about how the shared template's environments are set up.
	Notes []string `yaml:"notes,omitempty"`
}

// PerEnvironmentChecklist names the deployment targets a template's
// per-environment variables need a value for, and which variables those
// are. Environments here are named by deployment target/tier (e.g.
// "staging-dr"), NOT copied verbatim from the shared template's
// `environment.name:` field -- those have known bugs (see Notes) that make
// them unreliable as GitLab environment-scope identifiers on their own.
type PerEnvironmentChecklist struct {
	Environments []string `yaml:"environments,omitempty"`
	Variables    []string `yaml:"variables,omitempty"`
}

const (
	// SourceInclude (the default, when Source is empty) means this file's
	// content is GENERATED as a GitLab CI `include:project` stub pointing
	// at RemoteSource + SourcePath -- the shared template's content is
	// referenced, never copied, so future changes to the shared template
	// apply automatically without needing to re-run this tool.
	SourceInclude = "include"
	// SourceLocal means this file's content is copied verbatim from this
	// repo's own checkout at SourcePath.
	SourceLocal = "local"
)

// FileSpec is one file the template bundle manages. Most templates have a
// single include-sourced file (the main .gitlab-ci.yml, referencing the
// separate dso-templates/ci-cd-components/pipeline-templates project); a
// template can also bundle locally-sourced files this tool owns (e.g.
// settings.xml with Artifactory credentials) that have no reason to
// live in, or be referenced from, that shared templates project.
type FileSpec struct {
	TargetPath string `yaml:"target_path"`      // path in the destination project
	SourcePath string `yaml:"source_path"`      // the `file:` value if Source == "include"; a path in this repo if "local"
	Source     string `yaml:"source,omitempty"` // "include" (default) or "local"

	// ExtraVariables are written into a top-level `variables:` block appended
	// to this file's generated `include:` stub, overriding a default the
	// shared template sets in its own `variables:` block (e.g. GITLAB_PUBLISH)
	// for every project that adds this template. Only meaningful when
	// Source == SourceInclude; ignored for a local file.
	ExtraVariables []ExtraVariable `yaml:"extra_variables,omitempty"`

	// CsprojPlaceholder marks a local file containing the literal text
	// `<path to .csproj file>` (bootstrap.CsprojPlaceholder) that is replaced
	// with the project's detected .csproj path.
	CsprojPlaceholder bool `yaml:"csproj_placeholder,omitempty"`
}

// ExtraVariable is one variable rendered into an include stub's `variables:`
// override block. With only Name/Value set, it renders as a plain scalar
// (`NAME: "value"`) -- fine for a flag or path the shared template's script
// just reads, like GITLAB_PUBLISH or DEVENV_PATH. Value alone still matters
// even when blank: "" is a valid (and common) override -- it makes a bash
// `[ "$VAR" != "true" ]` check take the false branch instead of whatever
// that variable's own default in the shared template evaluates to.
//
// Some shared-template variables (DEPLOY_VARIABLE, CREATE_RELEASE) are
// instead declared there in GitLab's extended form -- value + description,
// sometimes + options -- specifically so GitLab prompts for them as a
// described/dropdown field on the "Run pipeline" screen. GitLab merges
// global `variables:` between an included file and the including project's
// own file key-by-key: the project's own definition fully REPLACES that
// key, not a deep merge of value/description/options. So overriding one of
// these with a bare scalar would silently drop its description and
// dropdown from the trigger form. Setting Description and/or Options here
// reproduces (or corrects -- e.g. a stale options list) that same extended
// form instead of flattening it.
type ExtraVariable struct {
	Name        string   `yaml:"name"`
	Value       string   `yaml:"value"`
	Description string   `yaml:"description,omitempty"`
	Options     []string `yaml:"options,omitempty"`

	// From, when set, takes the value from what was detected in the target
	// project instead of Value:
	//   "csproj"   the repo-relative path of the .csproj to version/build
	//   "solution" the file name of the .sln at the repo root (e.g. IBE.sln)
	// It is blank when nothing could be determined, so the MR asks for it by
	// hand (and, for csproj, the shared template falls back to its own discovery).
	From string `yaml:"from,omitempty"`
}

// ResolvesCsproj reports whether any part of the template needs the target
// project's .csproj path, so Plan knows to look for it.
func (t *Template) ResolvesCsproj() bool {
	if t.CsprojApplicationName {
		return true
	}
	for _, f := range t.Files {
		if f.CsprojPlaceholder {
			return true
		}
		for _, v := range f.ExtraVariables {
			if v.From == FromCsproj {
				return true
			}
		}
	}
	return false
}

// ExtraVariable.From values.
const (
	FromCsproj   = "csproj"
	FromSolution = "solution"
)

// ResolvesSolution reports whether any variable takes its value from the
// target project's solution file.
func (t *Template) ResolvesSolution() bool {
	for _, f := range t.Files {
		for _, v := range f.ExtraVariables {
			if v.From == FromSolution {
				return true
			}
		}
	}
	return false
}

func (f FileSpec) IsLocal() bool {
	return f.Source == SourceLocal
}

func LoadTemplates(path string) (*Templates, error) {
	var tpl Templates
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading templates %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &tpl); err != nil {
		return nil, fmt.Errorf("parsing templates %s: %w", path, err)
	}
	return &tpl, nil
}

func (t *Templates) Find(name string) *Template {
	for i := range t.Templates {
		if t.Templates[i].Name == name {
			return &t.Templates[i]
		}
	}
	return nil
}
