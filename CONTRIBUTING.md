# Contributing to Valmin

Bug reports, documentation fixes, tests, and code changes are welcome. For a large
feature or a change to deployment, permissions, or world storage, open an issue
first to discuss the expected behavior and scope.

## Report a bug or suggest a feature

Use the repository's [issue forms](https://github.com/valminhq/valmin/issues/new/choose).
Check existing issues and the [troubleshooting guide](docs/troubleshooting.md)
before opening a report.

A useful bug report includes the Valmin version or commit, reproduction steps,
expected behavior, and the actual error. The quickest way to supply the first of those,
and the deployment details around it, is the
[support bundle](docs/troubleshooting.md#support-bundle): it is built to be attached
unedited. Add browser details for UI problems, or
game and mod versions for game-related problems. You do not need to upgrade a
live server or disable its mods just to file a report.

For a feature request, explain the task you want to accomplish and the current
workaround. You do not need to design the implementation.

Issues and attachments are public. Remove passwords, session cookies, setup and
invite tokens, and webhook secrets. Do not attach the panel database, encryption
key, private keys, or a full data-directory archive.

## Set up development

Fork the repository, clone your fork, and create a branch from `main`:

```sh
git clone https://github.com/YOUR_USERNAME/valmin.git
cd valmin
git switch -c fix/describe-the-change
```

Follow the [development guide](docs/development.md) for tool versions, the local
account setup, and stub game containers. Start with stubs unless your change needs
real game behavior. Use a separate development data directory and copies of saves;
do not test restore, deletion, or migration changes against your only world copy.

The [architecture overview](docs/architecture.md) describes the packages and data
flow. The [API guide](docs/api.md) covers sessions, CSRF, and asynchronous jobs.

## Make a focused change

- Keep the change scoped to one problem. Avoid unrelated refactoring or formatting.
- Follow the surrounding code and the repository's formatter configuration.
- Add regression coverage for behavior changes, especially failures involving saves,
  permissions, jobs, and recovery. Documentation-only changes do not need Go tests.
- Check permissions in new API handlers and avoid exposing secrets in responses or logs.
- Update the relevant public guide when setup, configuration, or user-visible behavior changes.
- Follow each shell script's shebang. Scripts using `/bin/sh` must remain POSIX-compatible.
- Explain any necessary linter suppression and name the specific linter.

## Run checks

For code changes, run from the repository root:

```sh
make build
make lint
make test
```

`make build` installs frontend dependencies and embeds the built UI in `bin/valmind`.
`make lint` checks Go formatting and lint, frontend formatting and lint, and Svelte
and TypeScript types. `make test` runs Go and frontend unit tests.

Use the additional checks relevant to your change:

| Change                                                  | Check                                                   |
| ------------------------------------------------------- | ------------------------------------------------------- |
| Docker, provisioning, lifecycle, or filesystem behavior | `make test-integration`                                 |
| Provisioning or file ownership under the panel UID      | `make test-integration-as-panel` after `make dev-setup` |
| Concurrent backup, job, or mod behavior                 | `make race`                                             |
| Mod configuration parser                                | `make fuzz FUZZ_TIME=30s`                               |
| Release packaging                                       | `make release-check` with GoReleaser installed          |
| Shell scripts                                           | ShellCheck on the changed scripts                       |
| Documentation or issue forms                            | Check links, commands, and YAML syntax                  |

The integration targets use a real Docker daemon and stub game downloads. Check
[CI](.github/workflows/ci.yml) for the tool versions and release checks used by the
repository. If a check cannot run in your environment, state that in the PR.

The repository also provides [pre-commit hooks](.pre-commit-config.yaml) for Go,
ShellCheck, YAML, whitespace, and secret detection. With pre-commit and the hooks'
required tools installed, enable them with:

```sh
pre-commit install
pre-commit run --all-files
```

Hooks supplement the Make targets; they do not replace the frontend checks.
Review any files modified by hooks before committing.

## Open a pull request

Target `main`. Explain the problem, the resulting behavior, and how you checked it.
Link the issue if there is one. Add screenshots for visible UI changes and call
out any deployment changes or data migration requirements. Draft PRs are useful
when you want feedback before the work is ready.

Use a concise commit subject with a type and an optional scope, consistent with
the repository's existing history:

```text
fix(backups): keep server stopped after restore
feat(mods): show dependency conflicts before install
docs: explain LAN certificate setup
```

Common types are `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, and `ci`.
Keep the subject specific to what changed.
