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
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS builder

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod ./
RUN go mod download 2>/dev/null || true

COPY . .

# CGO_ENABLED=0 for a fully static binary (no libc dependency, required for distroless/static).
# -trimpath and -ldflags="-s -w" strip filesystem paths and debug symbols from the binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/plutus-collector \
    ./cmd/plutus-collector

# Also pinned to its resolved manifest-list digest, same reasoning and same
# tag-plus-digest syntax as the builder stage above — Dependabot's `docker` ecosystem bumps
# the digest, the tag stays for readability.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a AS final

# distroless/static-debian12:nonroot already runs as a non-root uid (65532) with no shell, no
# package manager, and no writable filesystem beyond what's mounted — nothing here needs to
# write to disk (config is env-only, no cache/log files).
COPY --from=builder /out/plutus-collector /plutus-collector

EXPOSE 9100

ENTRYPOINT ["/plutus-collector"]
