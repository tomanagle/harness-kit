# Releases

The TypeScript client's npm package name is [`@tomanagle/harness-kit`](https://www.npmjs.com/package/@tomanagle/harness-kit).
Its publishing workflow is [`.github/workflows/publish-npm.yml`](../.github/workflows/publish-npm.yml).
The npm package contains the client and type declarations; install the Go sidecar separately.

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

| Field | Value |
| --- | --- |
| Organization or user | `tomanagle` |
| Repository | `harness-kit` |
| Workflow filename | `publish-npm.yml` |
| Environment name | Leave empty |
| Allowed actions | Enable direct publishing with `npm publish` |

Normal runs use GitHub OIDC with `id-token: write`, so they do not need
`NPM_TOKEN`. The workflow uses a GitHub-hosted runner, Node 24, npm 11, and public
provenance. A newly added trusted publisher currently needs a successful publish
within two days; configure it when the next version is ready. See
[npm's trusted publishing documentation](https://docs.npmjs.com/trusted-publishers/).

## Subsequent npm releases

Update the package and lockfile together, commit the change, and push a matching
tag:

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

## Go binaries

Pushing a semantic version tag such as `v0.2.0` triggers
[`.github/workflows/release-binaries.yml`](../.github/workflows/release-binaries.yml).
It runs Go tests and portability checks, then builds macOS, Linux and Windows
binaries for amd64 and arm64. The GitHub release contains archives, the MIT
license and README, plus `SHA256SUMS`. Prerelease tags create prereleases.

```sh
git tag v0.2.0
git push origin v0.2.0
```

The Go module can also be installed at that version:

```sh
go install github.com/tomanagle/harness-kit/cmd/harness-kit@v0.2.0
```
