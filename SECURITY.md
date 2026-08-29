# Security policy

`plutus-collector` runs inside your own infrastructure and holds credentials — a Plutus API key,
and for the LiteLLM collector a gateway admin key. We take reports about it seriously and would
rather hear about a problem early than read about it later.

## Reporting a vulnerability

Email **security@plutus-cloud.com**. Please include enough detail to reproduce: the collector and
version (the image tag, or `git rev-parse HEAD` if you built it), what you observed, and what you
expected.

**Please do not open a public issue for a suspected vulnerability.** Use email first; we will
tell you when it is safe to discuss publicly.

If you would rather report through GitHub, use
[private vulnerability reporting](https://github.com/plutus-cloud/plutus-collector/security/advisories/new)
on this repository — it is visible only to maintainers until published.

### What to expect

Same commitments as the main Plutus policy, which this deliberately does not restate in different
words — the two must not drift into contradicting each other:

| | |
|---|---|
| Acknowledgement | within 3 business days |
| Initial assessment | within 10 business days |
| Fix target — critical (credential exposure, RCE, anything that reaches beyond this agent) | as fast as we can ship a release; days, not weeks |
| Fix target — everything else | prioritised honestly, with a real date in the reply |

We are a small team and would rather give you a real date than an SLA we cannot keep. We will
credit you in the release notes if you would like to be credited, and we will not ask you to keep
quiet indefinitely — tell us if you have a disclosure deadline and we will work to it.

We do not currently run a paid bounty programme.

If you would rather encrypt the report, say so in a first email with no details and we will send
a key.

## Scope

In scope: anything in this repository — both collectors, the Helm chart, the published images,
and the release workflows that build and sign them.

Out of scope here, but still worth reporting to the same address: the Plutus web application and
API (`console.plutus-cloud.com`), which are a separate codebase with their own policy.

Also out of scope: vulnerabilities in OpenCost or LiteLLM themselves. Please report those to
those projects — though if one is reachable *through* this collector in a way their maintainers
would not anticipate, that is very much in scope and we would like to know.

## What this software does and does not do

Stated plainly, because it is what most reports turn out to be about:

- **Neither collector accepts inbound connections** other than its own `/healthz` and `/metrics`
  endpoints. Plutus never connects to it; it only makes outbound HTTPS requests.
- **The Kubernetes agent has no Kubernetes API access** — no `ServiceAccount` RBAC of its own. It
  calls OpenCost's HTTP API and nothing else in the cluster.
- **The LiteLLM collector reads request-level spend records** to aggregate them, in process. Those
  records are never written to disk and never leave your network; only daily per-team totals are
  pushed. Its LiteLLM admin key is the most privileged credential either collector holds.
- **`PLUTUS_INGEST_URL` is rejected unless it is `https://`**, because it carries a bearer token.

## Verifying what you run

Every published image and chart is signed with [cosign](https://docs.sigstore.dev/) keyless
signing, bound to this repository's release workflow — not to any individual's key:

```bash
cosign verify ghcr.io/plutus-cloud/plutus-collector@<digest> \
  --certificate-identity-regexp 'https://github.com/plutus-cloud/plutus-collector/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A CycloneDX SBOM for each image is attached to its GitHub release. Base images are pinned by
digest, and a nightly job scans the *published* images rather than a fresh build, so what is
reported is what you would actually pull.
