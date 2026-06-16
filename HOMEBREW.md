# Homebrew support (staged on `homebrew-support`)

This branch adds a **Homebrew install channel** on top of `main`. It is kept off
`main` on purpose so the default install path stays the single curl script. When
you're ready to offer `brew install`, follow the steps below.

> **Does this affect `gpm update`?** No. Self-update (`gpm update`) and the curl
> installer download the release binary + `checksums.txt` directly from GitHub
> Releases — they do not use Homebrew. Homebrew is only an *additional* way to
> install. Users who installed via Homebrew update with `brew upgrade gpm`;
> everyone else uses `gpm update`. Both work independently.

## What this branch changes (vs `main`)

- **`.goreleaser.yaml`** — adds a `brews:` block that generates a formula and
  pushes it to the tap repo on every release.
- **`.github/workflows/release.yml`** — passes `HOMEBREW_TAP_GITHUB_TOKEN` to
  GoReleaser so it can push to the (separate) tap repo.
- **`README.md`** — documents `brew install parichit13/tap/gpm`.

Nothing in the gpm binary itself changes.

## One-time setup (do this before merging)

### 1. Create the tap repo

Homebrew taps are a separate repo named `homebrew-<name>`. Create an empty
public repo:

    github.com/parichit13/homebrew-tap

(That `homebrew-` prefix is what lets users run `brew install parichit13/tap/gpm`
— the `tap` in that command maps to `homebrew-tap`.)

### 2. Create a Personal Access Token for the tap

GoReleaser runs in the **gpm** repo's Actions but must push the formula to the
**homebrew-tap** repo, so the default `GITHUB_TOKEN` isn't enough.

- Fine-grained PAT: grant **Contents: read & write** on `parichit13/homebrew-tap`.
- Or a classic PAT with the `repo` scope.

### 3. Add the token as a secret on the gpm repo

GitHub → `parichit13/gpm` → Settings → Secrets and variables → Actions →
**New repository secret**:

- Name: `HOMEBREW_TAP_GITHUB_TOKEN`
- Value: the PAT from step 2

## Merge it into main

```bash
git checkout main
git merge homebrew-support      # clean: this branch is main + one commit
git push origin main
```

Because `homebrew-support` was branched from the current `main` and only *adds*
the Homebrew bits, the merge applies cleanly (no conflicts) as long as `main`
hasn't separately edited the same `.goreleaser.yaml` / `release.yml` / README
lines. If it has, resolve by keeping both the existing content and the `brews:`
block / token / README section.

## Cut a release to verify

```bash
git tag v0.1.0 && git push origin v0.1.0
```

CI builds the binaries, publishes the GitHub Release, and GoReleaser pushes the
formula to `parichit13/homebrew-tap`. Then confirm:

```bash
brew install parichit13/tap/gpm
gpm version
```

## Validate the config locally (optional)

```bash
brew install goreleaser    # or: go install github.com/goreleaser/goreleaser/v2@latest
goreleaser check           # validates .goreleaser.yaml including the brews block
```

## Keeping this branch fresh

If `main` advances before you merge, rebase this branch so the merge stays a
clean fast-forward-style apply:

```bash
git checkout homebrew-support
git rebase main
```
