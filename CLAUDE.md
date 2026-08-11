# CLAUDE.md

This file provides guidance to Claude Code when working in this repository.

## What this is

`plutus-collector` is Plutus's customer-installed Kubernetes cost-allocation push agent — see
README.md for the full design ("Why a separate repo" explains why this is split out from the
main `plutus-cloud/plutus` monorepo). This repo is currently **private**; it goes **public** in
the same change that flips the `kubernetes` cost source's `is_live` flag in the main repo.
Because this repo is customer-facing (or will be shortly), its commit history is part of the
product's public trust signal, not just an internal log — hold it to that standard.

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
