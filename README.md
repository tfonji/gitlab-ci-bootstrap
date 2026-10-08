# gitlab-ci-bootstrap

Adds a CI/CD template bundle to one GitLab project, as a single merge
request -- `.gitlab-ci.yml`, plus whatever else that template needs (e.g.
`.m2/settings.xml` for the Maven templates). The `.gitlab-ci.yml` this tool
adds does **not** copy the shared template's content -- it's a short
generated stub that `include:`s it by reference from the centralized
`dso-templates/ci-cd-components/pipeline-templates` project:

```yaml
include:
  - project: dso-templates/ci-cd-components/pipeline-templates
    ref: master
    file: 'templates/java-gradle-openshift-ci-cd.gitlab.yml'
```

That means no API call to that project at all (nothing to fetch), and
future changes to the shared template apply automatically without needing
to re-run this tool. Additional files this tool owns, like `settings.xml`
with Nexus/Artifactory credentials, are genuinely local to this repo (see
[files/](files/)) and copied verbatim, since they have no reason to live in
or be referenced from the shared templates project.

This is a **separate tool from [gitlab-post-migration](../gitlab-post-migration)**
on purpose: template choice is a per-project, human decision made via a
pipeline dropdown, and a template can bundle several files together -- that
doesn't fit gitlab-post-migration's model of "one setting, reconciled across
many projects in a batch."

## How it works

Run this tool's pipeline (see [.gitlab-ci.yml](.gitlab-ci.yml)) with:

- `PROJECT_ID` -- the target project (s): numeric ID or `group/subgroup/project` path, comma-separated for several
- `GROUP_ID` -- optional; a group whose projects are all targets (alone or together with `PROJECT_ID`). Without it, only
  `PROJECT_ID` is used.
    - `INCLUDE_SUBGROUPS` (default `true`) -- also include subgroup projects
    - Archived projects, empty repositories and forks are always skipped (not configurable); they appear as "skipped" in
      the batch summary and are not planned. This applies to group members only: a project named in `PROJECT_ID` is
      always planned.
    - Group mode needs no detection, always overwrites an existing `.gitlab-ci.yml`, and has no project cap.
- `EXCLUDE_PROJECT_IDS` -- project IDs or paths never to touch, whether named directly or reached through a group
- `TEMPLATE_NAME` -- picked from the dropdown (see [configs/templates.yaml](configs/templates.yaml) for what each one bundles)

Stages: `build → suggest → plan → apply`

- `suggest` is informational only -- it prints a best-guess template (based
  on signature files like `pom.xml`, `package.json`, `.csproj` content) to
  the job log so you can sanity-check your `TEMPLATE_NAME` pick before
  running `plan`. It never decides anything and never fails the pipeline.
- `plan` diffs every file in the chosen template's bundle against the
  project's current default branch and writes `plan.json` -- nothing is
  changed yet. Check the job log: each file is reported as `create`,
  `update`, or `unchanged`.
- `apply` (`when: manual`) commits every non-unchanged file from the plan in
  **one commit** and opens **one MR**. Re-running is safe: an already-open
  MR from a prior run is detected and skipped rather than duplicated, and
  file actions are re-checked against the branch's actual current state (not
  just the plan) so a retry after a partial failure won't try to re-create a
  file a previous attempt already added.
- Some templates need CI/CD variables this tool has no value for -- a
  variable expected to already exist above this project, an app-specific
  build target, a per-environment deploy host/credential. When a
  template's `configs/templates.yaml` entry sets `mr_checklist`, `apply`
  writes those into the opened MR's description as a checklist, in three
  parts: `group_level` (should already exist at group/instance level --
  confirm, don't assume), `per_project` (one value for the whole app), and
  `per_environment` (an `environments:` list naming each deployment target
  by tier, e.g. `staging-dr`, plus the `variables:` that need a value per
  target -- set via GitLab's per-variable Environment scope, or `*` for
  the same value everywhere). Plus any free-text `notes` for anything that
  doesn't fit a plain variable name (a naming constraint between two
  variables, a caveat about the shared template's own environment setup).
  See `dotnet-core-iis-ci-cd`/`dotnet-framework-iis-ci-cd` for a worked
  example.

## Configuration

- [configs/templates.yaml](configs/templates.yaml) -- `remote_source` names
  the centralized pipeline-templates project + ref used in the generated
  `include:` stub; below that, all 31 templates, each with the list of
  files it bundles. Every file has a `target_path` (where it lands in the
  destination project) and a `source_path`; unless a file sets
  `source: local`, `source_path` becomes the `include:` stub's `file:`
  value (see above) rather than being fetched. Only
  Maven/Node/Python/.NET/Java-Ant/Flyway templates currently bundle a local
  extra file alongside the generated `.gitlab-ci.yml`. An `include`-sourced
  file can also set `extra_variables` -- a list of `name`/`value` pairs
  written into a top-level `variables:` block appended after the `include:`
  in the generated file, overriding whatever default the shared template
  sets for that name. `name`/`value` alone renders as a plain scalar
  (`NAME: "value"`) -- fine for a flag or path the shared template's script
  just reads (e.g. `java-ant-tomcat-ci-cd` sets `GITLAB_PUBLISH: ""` to
  route package publishing to Artifactory instead of GitLab's package
  registry; `ssis-ci-cd` sets `DEVENV_PATH: ""` for the same reason).
  Some shared-template variables (`DEPLOY_VARIABLE`, `CREATE_RELEASE`) are
  instead declared there in GitLab's extended form -- `value` +
  `description`, sometimes + `options` -- so GitLab prompts for them as a
  described/dropdown field on the "Run pipeline" screen; overriding one
  with a bare scalar would silently drop that description/dropdown, since
  GitLab replaces a variable key wholesale rather than deep-merging it. Add
  `description` and/or `options` alongside `value` to reproduce (or correct
  -- e.g. `ssis-ci-cd`'s stale `staging-pp` option) that same extended form
  instead. A template can also set `mr_checklist` (`group_level`,
  `per_project`, `per_environment`, `notes`) -- see "How it works" above.
- [files/](files/) -- content for the locally-sourced bundle files, versioned
  and reviewed like code, same as the rest of this repo. All placeholders
  right now (see Known gaps):

  | File | Bundled with | Auto-discovered by the build tool? |
  |---|---|---|
  | `maven-settings.xml` → `.m2/settings.xml` | all 5 Maven templates | No -- pipeline needs `mvn -s .m2/settings.xml` |
  | `npmrc` → `.npmrc` | all 5 Node templates | Yes, npm reads it from cwd automatically |
  | `nuget.config` → `nuget.config` | all 5 .NET templates | Yes, auto-discovered walking up from cwd |
  | `pip.conf` → `pip.conf` | all 4 Python templates | No -- pipeline needs `PIP_CONFIG_FILE=$CI_PROJECT_DIR/pip.conf`; pip.conf also can't expand `${VAR}` for credentials, unlike the others |
  | `flyway.conf` → `flyway.conf` | `flyway-cd` only | No -- pipeline needs `flyway -configFiles=$CI_PROJECT_DIR/flyway.conf`; holds per-app DB `url`/`user` only, password comes from the `FLYWAY_PASSWORD` CI variable Flyway reads natively |
  | `bumpversion.cfg` → `.bumpversion.cfg` | `dotnet-framework-iis-ci-cd` only | No -- read directly by the `bumpversion` CLI from the repo root; its `<path to .csproj file>` placeholder is filled in automatically from the detected .csproj (see "Detecting the .csproj") |
  | `bumpversion-ant-properties.cfg` → `.bumpversion.cfg` | `java-ant-tomcat-ci-cd`, `java-ant-fileDeploy-ci-cd` | No -- read directly by the `bumpversion` CLI from the repo root; assumes the app's properties file (see `PROPERTY_FILE`) is at `app.properties` -- update the `[bumpversion:file:...]` path if it lives elsewhere, called out in the MR's `mr_checklist` notes |

  **Gradle (`gradle.properties`), Ansible (`ansible.cfg`), Ant/Ivy
  (`ivysettings.xml`), and Terraform (remote state backend / private module
  registry auth) were discussed and deliberately left out for now** --
  revisit when there's a concrete need.
- `TEMPLATE_NAME`'s dropdown `options:` in `.gitlab-ci.yml` must be kept in
  sync by hand with the template names in `configs/templates.yaml` -- GitLab
  CI can't generate dropdown options from a file at pipeline-definition
  time.
- `GITLAB_TOKEN` CI/CD variable (masked/protected) -- needs `api` scope and
  `write_repository` on the target project(s) only. This tool itself never
  reads `dso-templates/ci-cd-components/pipeline-templates` (the
  `include:` stub only references it) -- but separately, each **target**
  project's own pipeline needs GitLab-level permission to include from that
  project when it actually runs (GitLab's own `include:project` access
  rules), which is unrelated to this tool's token and worth confirming with
  whoever maintains the shared templates project if a target project's
  pipeline fails to resolve the include after this MR merges.

## Local usage

```
go build -o bin/gitlab-ci-bootstrap ./cmd/gitlab-ci-bootstrap
export GITLAB_TOKEN=...

./bin/gitlab-ci-bootstrap list-templates
./bin/gitlab-ci-bootstrap suggest --project=group/my-app
./bin/gitlab-ci-bootstrap plan --project=group/my-app --template=java-maven-ci --out=plan.json
./bin/gitlab-ci-bootstrap plan --group=group/team --exclude-projects=123,group/team/legacy --template=java-maven-ci --out=plan.json
./bin/gitlab-ci-bootstrap apply --plan=plan.json --out=result.json
```

## Detecting the .csproj

Templates that need the app's .csproj path (`dotnet-framework-iis-ci-cd`'s
`.bumpversion.cfg` placeholder, `dotnet-core-iis-ci-cd`'s `version_file`
variable) get it from the repo automatically. Declared in `templates.yaml`
with `csproj_placeholder: true` on a local file, or `from: csproj` on an
extra variable.

- Files under `bin/`, `obj/`, `packages/` and `node_modules/` are ignored,
  and so are test projects (test SDK/framework references, `IsTestProject`,
  or a `.Tests` name).
- One project left: that one. Several: the single deployable one (web SDK,
  web project type, `Web.config` beside it, or an `Exe`/`WinExe` output).
- Otherwise (none found, only tests, or several deployable) nothing is
  guessed: the placeholder stays, `version_file` stays blank (the shared
  template then falls back to its own discovery), and the MR gets a
  ".csproj path" checklist item naming the candidates.

## Known gaps

- Every file under `files/` still has `TODO` placeholders (mirror URLs,
  credentials) except `nuget.config`, which has the real Artifactory NuGet
  feed -- fill in real values for the rest before relying on this.
  `bumpversion.cfg` is a separate case: its `<path to .csproj file>`
  placeholder is per-app, so it is filled in from the detected .csproj (see "Detecting the .csproj").
- **Several of these files only take effect if the *remote* pipeline
  template's script actually references them** -- `.npmrc` and
  `nuget.config` are auto-discovered by npm/NuGet with no extra step, but
  `.m2/settings.xml`, `pip.conf`, and `flyway.conf` all need an explicit
  flag/env-var in the corresponding `dso-templates/ci-cd-components/pipeline-templates`
  script (`mvn -s .m2/settings.xml`, `PIP_CONFIG_FILE=...`,
  `flyway -configFiles=...`) or bundling them is a no-op. Worth confirming
  with whoever maintains that project.
- Since the `.gitlab-ci.yml` this tool writes is a generated `include:`
  stub, `plan`/`apply` no longer verify that
  `configs/templates.yaml`'s `source_path` actually exists in
  `dso-templates/ci-cd-components/pipeline-templates` -- a wrong path
  (renamed file, typo) won't surface here at all; it'll only show up when
  the **target** project's pipeline runs and fails to resolve the include.
  Worth a periodic spot-check that `source_path` values still match that
  project's real file names.
- `flyway-cd`'s bundle **replaces** `flyway.conf` on apply -- since its
  `detect` rule only matches when a project already has one (with real,
  per-app DB settings), review the plan output's action (`update` vs
  `create`) before approving apply for that template specifically.
- `suggest`'s detection rules were ported from gitlab-post-migration's old
  auto-detection (same caveats: heuristic, can misfire on monorepos) --
  it's explicitly advisory here, never authoritative.

## Planned enhancements

- A persisted report, updated on every `plan`/`apply` run, tracking
  cumulative manual effort saved vs. doing this by hand in the GitLab UI --
  to help make the case for using this tool. Not built yet; deferred
  on purpose (2026-09-19).
