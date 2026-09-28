# gitlab-ci-bootstrap

Adds a CI/CD template bundle to one GitLab project, as a single merge
request -- `.gitlab-ci.yml`, plus whatever else that template needs (e.g.
`.m2/settings.xml` for the Maven templates). All template content lives in
this repo: `.gitlab-ci.yml` files under [templates/](templates/), other
bundled files (settings.xml, .npmrc, etc.) under [files/](files/) -- read
straight off disk in the CI job's own checkout, no fetch from any other
project.

This is a **separate tool from [gitlab-post-migration](../gitlab-post-migration)**
on purpose: template choice is a per-project, human decision made via a
pipeline dropdown, and a template can bundle several files together -- that
doesn't fit gitlab-post-migration's model of "one setting, reconciled across
many projects in a batch."

## How it works

Run this tool's pipeline (see [.gitlab-ci.yml](.gitlab-ci.yml)) with:

- `PROJECT_ID` -- the target project (numeric ID or `group/subgroup/project` path)
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

## Configuration

- [configs/templates.yaml](configs/templates.yaml) -- all 29 templates, each
  with the list of files it bundles. Every file has a `target_path` (where
  it lands in the destination project) and a `source_path` (a path in this
  repo -- `templates/*.gitlab.yml` or `files/*`).
- [templates/](templates/) -- the `.gitlab-ci.yml` content for every
  template. **All 29 are placeholder stubs right now** (see Known gaps).
- [files/](files/) -- content for the other bundled files, versioned
  and reviewed like code, same as the rest of this repo. All placeholders
  right now too (see Known gaps):

  | File | Bundled with | Auto-discovered by the build tool? |
  |---|---|---|
  | `maven-settings.xml` → `.m2/settings.xml` | all 5 Maven templates | No -- pipeline needs `mvn -s .m2/settings.xml` |
  | `npmrc` → `.npmrc` | all 5 Node templates | Yes, npm reads it from cwd automatically |
  | `nuget.config` → `nuget.config` | all 5 .NET templates | Yes, auto-discovered walking up from cwd |
  | `pip.conf` → `pip.conf` | all 4 Python templates | No -- pipeline needs `PIP_CONFIG_FILE=$CI_PROJECT_DIR/pip.conf`; pip.conf also can't expand `${VAR}` for credentials, unlike the others |
  | `flyway.conf` → `flyway.conf` | `flyway-cd` only | No -- pipeline needs `flyway -configFiles=$CI_PROJECT_DIR/flyway.conf`; holds per-app DB `url`/`user` only, password comes from the `FLYWAY_PASSWORD` CI variable Flyway reads natively |

  **Gradle (`gradle.properties`), Ansible (`ansible.cfg`), Ant/Ivy
  (`ivysettings.xml`), and Terraform (remote state backend / private module
  registry auth) were discussed and deliberately left out for now** --
  revisit when there's a concrete need.
- `TEMPLATE_NAME`'s dropdown `options:` in `.gitlab-ci.yml` must be kept in
  sync by hand with the template names in `configs/templates.yaml` -- GitLab
  CI can't generate dropdown options from a file at pipeline-definition
  time.
- `GITLAB_TOKEN` CI/CD variable (masked/protected) -- needs `api` scope and
  `write_repository` on the target project(s). (No access to any other
  project needed -- template content is local to this repo.)

## Local usage

```
go build -o bin/gitlab-ci-bootstrap ./cmd/gitlab-ci-bootstrap
export GITLAB_TOKEN=...

./bin/gitlab-ci-bootstrap list-templates
./bin/gitlab-ci-bootstrap suggest --project=group/my-app
./bin/gitlab-ci-bootstrap plan --project=group/my-app --template=java-maven-ci --out=plan.json
./bin/gitlab-ci-bootstrap apply --plan=plan.json --out=result.json
```

## Known gaps

- Every file under `templates/` and `files/` is a placeholder stub (a
  trivial `echo "TODO"` pipeline, or a `settings.xml`/`.npmrc`/etc. with
  `TODO` mirror URLs and credentials) -- replace each with the real
  converted AZDO pipeline content and real registry credentials before
  relying on this for anything.
- **Several bundled files only take effect if the template's own script
  actually references them** -- `.npmrc` and `nuget.config` are
  auto-discovered by npm/NuGet with no extra step, but `.m2/settings.xml`,
  `pip.conf`, and `flyway.conf` all need an explicit flag/env-var in that
  template's `templates/*.gitlab.yml` (`mvn -s .m2/settings.xml`,
  `PIP_CONFIG_FILE=...`, `flyway -configFiles=...`) or bundling them is a
  no-op.
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
