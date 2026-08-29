# Contributing

Conventions for working in this repository. README.md covers the design, the package layout and
how to add a third collector; this covers how changes are written, described and merged.

**This repository is public, and its history is part of the product.** These agents run inside
customers' own infrastructure, so the commit log, the code comments and the pull requests are
things a prospective user reads before installing them — not an internal record. That shapes most
of what follows.

## Commits

- **Every commit is authored `Plutus <support@plutus-cloud.com>`.** This is a project identity,
  not a personal one — the agents here are a product, and the history is part of what a customer
  evaluates before installing them. Set it per-checkout before your first commit, since it does
  not carry over to a fresh clone or worktree:
  ```
  git config user.name "Plutus"
  git config user.email "support@plutus-cloud.com"
  ```
- **No trailers.** Commit messages end at the message — no `Co-Authored-By:`, no tool or session
  metadata. Some editors and tooling append these by default; check before you commit.
- **Explain why, not just what.** A message that says only what changed is a worse `git log` than
  the diff it summarises. What belongs in one, and what does not, is the next section.

## Writing for a public repo

Everything you write here is customer-readable and permanent: **commit messages, code comments,
PR titles and descriptions, CI logs, and issue replies**. There is no private surface. This file
is public too, and is bound by its own rule below.

The habit worth unlearning here is explaining *the failure that motivated the code* — normal in a
private codebase, and here it publishes a defect log about software customers are being asked to
install inside their own network.

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
- **Anything about unreleased or internal Plutus work** — features not yet shipped, the web
  application, roadmap, internal service names. A message here should make sense to someone who
  has only ever seen this repository.
- **Restatements of these conventions.** Follow them; a commit message is not the place to
  discuss them.
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
