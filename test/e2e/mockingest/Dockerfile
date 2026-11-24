# Test-only image for the end-to-end run; never published. Build from the repository root:
#   docker build -f test/e2e/mockingest/Dockerfile -t plutus-mock-ingest:e2e .
FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod ./
COPY test/e2e/mockingest ./test/e2e/mockingest
RUN CGO_ENABLED=0 go build -trimpath -o /out/mockingest ./test/e2e/mockingest

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/mockingest /mockingest
ENTRYPOINT ["/mockingest"]
