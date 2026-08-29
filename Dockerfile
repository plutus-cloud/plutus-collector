# Multi-stage build: a static Go binary in a distroless final image, matching every other
# in-cluster component this agent runs alongside (Alloy, OpenCost — see the design doc's §2 and
# CLAUDE.md's k3s/security-scanning notes). No shell, no package manager, minimal CVE surface —
# this is the first Plutus-authored code running inside a customer's cluster, so the image
# itself is part of the trust story.

# Pinned (not a floating `1.26-alpine` tag) so Dependabot's `docker` ecosystem has something to
# actually bump. Was 1.22.12 until nightly.yml's first real run caught 15 stdlib CVEs baked into
# that toolchain (1 CRITICAL — CVE-2025-68121, a crypto/tls certificate validation bug) — the Go
# *toolchain* version ends up in the compiled binary's stdlib, so this isn't a base-OS-package
# question the way it would be for most Dockerfiles. Bump this promptly when Dependabot proposes
# a newer patch; nightly.yml's `trivy image` scan is what will catch the next one.
#
# Also pinned to the resolved manifest-list digest, on top of the tag: a tag alone is mutable
# and a registry-side republish under the same tag would ship a different image than the one
# actually scanned/verified. The tag stays alongside it (`image:tag@sha256:...`) purely for
# readability — Dependabot's `docker` ecosystem still resolves and bumps the digest when a new
# patch is proposed, so this doesn't lose the update path the paragraph above is about.
FROM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS builder

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod ./
RUN go mod download 2>/dev/null || true

COPY . .

# CGO_ENABLED=0 for a fully static binary (no libc dependency, required for distroless/static).
# -trimpath and -ldflags="-s -w" strip filesystem paths and debug symbols from the binary.
# Both collectors are built from this one stage — they share internal/ingest, internal/pusher and
# internal/metrics, so compiling them together is cheaper than two builds and guarantees the two
# binaries were cut from the same commit of that shared code.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/plutus-collector \
    ./cmd/plutus-collector

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/plutus-litellm-collector \
    ./cmd/plutus-litellm-collector

# Also pinned to its resolved manifest-list digest, same reasoning and same
# tag-plus-digest syntax as the builder stage above — Dependabot's `docker` ecosystem bumps
# the digest, the tag stays for readability.
# ONE pin for the runtime base, shared by both images below.
#
# Not two copies of the same `FROM gcr.io/distroless/...@sha256:...` line, and this is a
# correctness point rather than tidiness: a second copy is a second thing for Dependabot to bump
# and a second thing for it to miss, and a stale runtime base is the quiet failure mode — the
# image still builds, the scanners still pass, and it just no longer has the fixes the other one
# got. With a single pin the two images cannot diverge.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime-base

# ─── Two images, one Dockerfile ─────────────────────────────────────────────
#
# Each collector gets its own single-binary image with its own ENTRYPOINT, rather than one image
# carrying both behind a --entrypoint override or a MODE variable. Two reasons: an image that
# ships a binary it never runs is unnecessary attack surface on something running inside a
# customer's network, and `docker run <image>` should do the obvious thing without a flag.
#
# STAGE ORDER IS LOAD-BEARING. `docker build` with no --target builds the LAST stage, so the
# Kubernetes image stays last and a build command that predates the LiteLLM collector produces
# exactly what it always did. Putting the new stage last would silently republish the litellm
# binary under the plutus-collector tag.
#
# Build the LiteLLM one explicitly:
#   docker build --target litellm -t ghcr.io/plutus-cloud/plutus-litellm-collector .
FROM runtime-base AS litellm

COPY --from=builder /out/plutus-litellm-collector /plutus-litellm-collector

EXPOSE 9100

ENTRYPOINT ["/plutus-litellm-collector"]

FROM runtime-base AS final

# distroless/static-debian12:nonroot already runs as a non-root uid (65532) with no shell, no
# package manager, and no writable filesystem beyond what's mounted — nothing here needs to
# write to disk (config is env-only, no cache/log files).
COPY --from=builder /out/plutus-collector /plutus-collector

EXPOSE 9100

ENTRYPOINT ["/plutus-collector"]
