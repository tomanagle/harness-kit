# Releases

The TypeScript client's npm package name is [`@tomanagle/harness-kit`](https://www.npmjs.com/package/@tomanagle/harness-kit).
Its publishing workflow is [`.github/workflows/publish-npm.yml`](../../.github/workflows/publish-npm.yml).
The npm package contains the client and type declarations; install the Go sidecar separately.

The published npm `@tomanagle/harness-kit@0.2.0` and historical root-module Go
releases remain available. The npm 0.2.0 registry artifact was verified against
the staged build and tested with the released v0.2.0 sidecar, including live
settings, per-turn budgets, cache eviction and transcript replay after restart.
These are recorded release checks; the monorepo move does not create a new release.

## Monorepo paths and Go migration

The repository has two product components: the standalone SDK spans `core/`
(Go packages and local sidecar) plus root `packages/` (clients), while `server/`
is a placeholder for a future coordination server. Workflows stay at the
repository root under `.github/workflows/`. The server has no release or deployment
artifact yet, and applications can use the kit independently.

| Component                 | Current source/module                                       | Future tag example                |
| ------------------------- | ----------------------------------------------------------- | --------------------------------- |
| Go core and local sidecar | `github.com/tomanagle/harness-kit/core`                     | `core/v0.3.0`                     |
| OpenAI SDK adapter        | `github.com/tomanagle/harness-kit/core/providers/openai`    | `core/providers/openai/v0.3.0`    |
| Anthropic SDK adapter     | `github.com/tomanagle/harness-kit/core/providers/anthropic` | `core/providers/anthropic/v0.3.0` |
| Go sidecar client         | `github.com/tomanagle/harness-kit/packages/client-go`       | `packages/client-go/v0.1.0`       |
| TypeScript client         | `packages/client-ts`, npm `@tomanagle/harness-kit`          | `npm-v0.3.0`                      |

The new Go module paths have **not been tagged**. The tag names above describe
future releases, not versions available to install now. In particular, do not
assume `go install github.com/tomanagle/harness-kit/core/cmd/harness-kit@latest`
will resolve until the first `core/v…` tag is published. Build current source:

```sh
git clone https://github.com/tomanagle/harness-kit.git
cd harness-kit/core
go build -o bin/harness-kit ./cmd/harness-kit
./bin/harness-kit runtimes
```

The root `make` and `hk` wrappers also build into `core/bin/`. To migrate a Go
application, add `/core` after the repository path in its core and provider imports.
While the new modules are untagged, work from a checkout: the standalone Go
modules in root `examples/cli`, `examples/model` and `examples/wails` resolve the
core through local replacements to `../../core`. A host application can likewise
use explicit local Go module replacements. For example, from an application's module directory:

```sh
go mod edit -require=github.com/tomanagle/harness-kit/core@v0.0.0
go mod edit -replace=github.com/tomanagle/harness-kit/core=/absolute/path/to/harness-kit/core
go mod tidy
```

The `v0.0.0` requirement here is a local development placeholder resolved by the
replacement, not a published version. Provider users need corresponding local
replacements for the chosen `core/providers/...` module as well. Once the new
modules are released, replace these local directives with their published versions.
The npm import remains `@tomanagle/harness-kit`; no package rename is required.

The standalone Go client has no core-module dependency. Until its first tag,
use a local replacement for
`github.com/tomanagle/harness-kit/packages/client-go` pointing to this checkout's
`packages/client-go` directory; see its [README](../../packages/client-go/README.md).
The CLI's former `core/rpc/client` implementation is now private at
`core/internal/rpcclient`; application code uses the standalone generated client.
See [package boundaries](packages.md) for the source-level migration of budget,
context-window and process helpers in the unreleased core module.

Before either client release, run `make generate`, commit the updated schema and
generated clients, and run `make check-generated test-clients`. Consumers use the
committed generated code; they do not run the generator at install time.

## First npm publish

Before a package exists on npm, use the `NPM_TOKEN` GitHub repository secret to
bootstrap it. The token must belong to the npm account `tomanagle`, have permission
to create a public package in its personal `@tomanagle` scope, and satisfy the
account's publishing policy.

In GitHub Actions, select **Publish npm → Run workflow**, use `main`, and enable
**bootstrap**. From the CLI:

```sh
gh workflow run publish-npm.yml --repo tomanagle/harness-kit --ref main -f bootstrap=true
```

The workflow checks the package identity, runs the TypeScript and sidecar
integration tests, builds the client, and installs and imports the packed tarball
before staging that same artifact with `npm stage publish`. The token is exposed
only to the bootstrap stage step. Staging defers npm's 2FA check to approval, so
the workflow does not need a one-time password.

Sign in to npm as `tomanagle`, open **Staged Packages**, review the version from
the run, and click **Approve**. npm requires your 2FA authentication to make that
version public. Alternatively, use `npm stage approve <stage-id>` from a signed-in
local npm CLI; the workflow output contains the stage ID.

For a new package, npm creates a public `0.0.0-stage` placeholder. The real staged
version remains unavailable until approved. Do not rerun publishing for a staged
version: it already reserves that version number. See
[npm's staged publishing documentation](https://docs.npmjs.com/staged-publishing/).

If npm rejects the first scoped publish with `E404`, check that the token belongs
to the npm account `tomanagle` and can create public packages in its personal
`@tomanagle` scope. The package uses that account's scope; no npm organization is
required.
Update the GitHub repository secret and rerun the bootstrap workflow; no source
change or version bump is needed for a version that was never staged or published.

## Authorize npm trusted publishing

After the first publish, open the package's **Settings → Trusted publishing** on
npm and add a GitHub Actions publisher:

| Field                | Value                                       |
| -------------------- | ------------------------------------------- |
| Organization or user | `tomanagle`                                 |
| Repository           | `harness-kit`                               |
| Workflow filename    | `publish-npm.yml`                           |
| Environment name     | Leave empty                                 |
| Allowed actions      | Enable direct publishing with `npm publish` |

Normal runs use GitHub OIDC with `id-token: write`, so they do not need
`NPM_TOKEN`. The workflow uses a GitHub-hosted runner, Node 24, npm 11, and public
provenance. A newly added trusted publisher currently needs a successful publish
within two days; configure it when the next version is ready. See
[npm's trusted publishing documentation](https://docs.npmjs.com/trusted-publishers/).

## Subsequent npm releases

When preparing a future npm release, update the package and lockfile together,
commit the change, and push a matching tag. From the repository root:

```sh
cd packages/client-ts
npm version patch --no-git-tag-version
cd ../..
git add packages/client-ts/package.json packages/client-ts/package-lock.json
git commit -m "Release npm client 0.2.1"
git push origin main
git tag npm-v0.2.1
git push origin npm-v0.2.1
```

The `npm-v` prefix keeps client releases independent from Go binary releases and
future packages in the `@tomanagle` scope. The workflow rejects tags that do not
match `package.json`. Stable versions publish to `latest`; prereleases publish to
`next`. You can also run the workflow manually on the release commit with
**bootstrap disabled**. Every publish requires a new package version.

## Future Go releases

For the new layout, a core module tag such as `core/v0.3.0` triggers
[`.github/workflows/release-binaries.yml`](../../.github/workflows/release-binaries.yml).
It runs Go tests and portability checks, then builds macOS, Linux and Windows
binaries for amd64 and arm64. The GitHub release contains archives, the MIT
license and README, plus `SHA256SUMS`. Prerelease tags create prereleases.

The tag prefix must match the module's directory. Before tagging providers,
update each provider module's core requirement to an available `core/v…` version;
a local `replace` directive is not inherited by consumers. Provider tags are
independent of core binary tags and use the full module subdirectory, for example
`core/providers/openai/v0.3.0` and `core/providers/anthropic/v0.3.0`.

The standalone Go client's tags use `packages/client-go/v…`; they do not trigger
the sidecar binary workflow and can advance independently. No standalone Go
client version has been tagged or published as part of adding generation.

These are instructions for a future release; no new version was published as part
of reorganizing the repository.

## Historical Go releases

Existing `v0.1.0` and `v0.2.0` tags retain the old root module path and source layout.
Existing provider tags, such as `providers/openai/v0.1.0` and
`providers/anthropic/v0.1.0`, likewise retain their original paths. Do not move or
retag these releases. The released v0.2.0 CLI remains installable at its original
path:

```sh
go install github.com/tomanagle/harness-kit/cmd/harness-kit@v0.2.0
```
