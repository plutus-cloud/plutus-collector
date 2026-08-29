# CLAUDE.md

This file provides guidance to Claude Code when working in this repository.

## What this is

`plutus-collector` holds Plutus's customer-installed push agents — see README.md for the full
design ("Why a separate repo" explains why this is split out from the main `plutus-cloud/plutus`
monorepo).

**This repo is public.** Its commit history, code comments, PR descriptions and CI logs are all
part of the product's public trust signal rather than an internal log — hold everything you write
here to that standard. See "Writing for a public repo" below, which is the single easiest thing
to get wrong when carrying habits over from the monorepo.

**Two binaries, one shared core.** `cmd/plutus-collector` (OpenCost, Kubernetes) and
`cmd/plutus-litellm-collector` (a self-hosted LiteLLM proxy) share `internal/pusher`,
`internal/ingest`, `internal/metrics` and `internal/config`'s `Common`. When adding a third
source: implement `pusher.Source`, put the payload shape and mapping in its own `internal/`
package beside the client, add a `Load<X>` in `internal/config`, and add a `cmd/` entrypoint plus
a Dockerfile stage. Do **not** add a `MODE` env var — the required variables differ per source
(`CLUSTER_NAME`/`CURRENCY` are mandatory for OpenCost and meaningless for LiteLLM), and branching
validation on a mode is what the one-loader-per-binary split exists to avoid.

**Dockerfile stage order is load-bearing.** `docker build` with no `--target` builds the last
stage, which is deliberately the Kubernetes image, so a build command predating the LiteLLM
collector still produces what it always did. Build the other with `--target litellm`.

## Commit conventions (read before running `git commit`)

- **Author/committer identity**: `Plutus <support@plutus-cloud.com>`, not whatever a session's
  local/global git config defaults to. Before committing, check `git config user.email` in the
  current working copy — if it isn't `support@plutus-cloud.com`, set it locally first:
  ```
  git config user.name "Plutus"
  git config user.email "support@plutus-cloud.com"
  ```
  This is a repo-local setting and does not carry over automatically to a fresh clone or a new
  worktree — check and set it every time you're working from a checkout that hasn't had this
  set yet, not just once.
- **No AI-authorship trailer.** Do **not** append `Co-Authored-By: Claude ...` or
  `Claude-Session: ...` lines to commit messages in this repo — this overrides the default
  commit-message template a Claude Code session would otherwise use. This is a deliberate
  product decision (an unrelated personal Gmail address and an AI co-authorship trailer were
  both found and removed from this repo's initial commit for exactly this reason), not an
  oversight to "fix" back in.

## Writing for a public repo (read before `git commit` or `gh pr create`)

Everything you write here is customer-readable and permanent: **commit messages, code comments,
PR titles and descriptions, CI logs, and issue replies**. There is no private surface. This file
is public too, and is bound by its own rule below.

The monorepo's house style explains *the failure that motivated the code* — which is right there
and wrong here. Carried over unchanged it publishes a defect log about a product customers are
being asked to install inside their own network.

**The rule: state the invariant, not the incident.** Same information, forward-looking.

| Don't write | Write |
|---|---|
| "this never worked — found by using it" | "a chart version must be valid semver, so the chart steps run only on tag refs" |
| "took the whole job down at the last step with the images already pushed" | "skipped rather than fatal, so the image republish still completes" |
| "this repo had none of these despite shipping X" | "adds X" |
| "the scanner has been reporting these for days" | (nothing — the fix speaks for itself) |

Specifically, do not write:

- **Our own operational failures, and never with a timeline.** How long something was broken, how
  many findings a scan reported, what a release did wrong. A fix needs no incident report attached.
- **Anything about unreleased or internal Plutus work** — features not yet shipped, the web app's
  UI, roadmap, internal service names. A commit here should make sense to someone who has only
  ever seen this repo.
- **The identity conventions themselves.** Follow the commit conventions above; do not describe
  them, or the reason for them, in a file or message that ships publicly.
- **Security findings in our own artifacts, narrated as history.** Ship the fix. The advisory
  process in SECURITY.md is how those get communicated deliberately.

Do keep writing, because these are what make the repo readable and are not incident reports:

- **Forward-looking warnings to whoever edits this next** — "putting the new stage last would
  publish the wrong binary under this tag", "this list must agree with X or the write is rejected".
  A trap someone is about to walk into is documentation.
- **Real, current limitations a customer needs** — an unverified upstream API, an estimate rather
  than an invoiced figure, a missing chart. Understating these is worse than admitting them; that
  is honesty about the product, not a defect log about ourselves.
- **Why a design is the way it is**, where the alternative is genuinely tempting.

## Pull requests and merging

- **The PR description becomes the permanent commit message.** This repo squash-merges, and the
  squash message defaults to the PR title plus its body — so a PR description is not scratch
  space, it is the commit message that lands on `main`. Everything above applies to it, and it
  should be short: the detail belongs in the commits and the code, which reviewers can read.
- **Squash-merge, not rebase or merge commit.** Squash preserves the branch commits' author
  (`Plutus <support@plutus-cloud.com>`) and sets the committer to `GitHub`. Rebase-merge rewrites
  committer information onto every commit, and a UI merge commit is authored by whoever clicked
  the button — both attribute work to the person merging rather than to the project.
- **Let CI finish before merging.** Actions is free on public repos, so there is no reason to
  skip a check here, and `test.yml`/`security.yml` are the only gate — the repo has no branch
  protection.
