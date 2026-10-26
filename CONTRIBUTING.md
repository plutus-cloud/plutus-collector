# Contributing

Thanks for your interest in `plutus-collector`. It is a set of small agents that run inside your
own infrastructure and push cost data to [Plutus](https://plutus-cloud.com). The README covers the
design, the package layout and how to add a collector; this file covers how to build, test and
propose changes.

## Building and testing

You need Go 1.26 or newer.

```
go build ./...
go vet ./...
gofmt -l .
go test -race ./...
```

The Helm chart has rendered-output tests. They need `helm` (CI uses v4.2.2) and, optionally,
`kubeconform`:

```
test/chart/run.sh
```

The end-to-end test installs the chart on a throwaway [kind](https://kind.sigs.k8s.io/) cluster
and checks what reaches a mock ingest endpoint. It needs docker, kind, kubectl, helm, jq, openssl
and curl, and takes about ten minutes:

```
test/e2e/run.sh
```

## Proposing a change

- For anything large, such as a new collector or a change to the push protocol or chart values,
  open an issue first so the approach can be agreed before you invest time.
- Keep each pull request to one logical change.
- Include tests for new behavior and make sure the checks above pass.
- Update the README or chart documentation when behavior or configuration changes.

## Commit and pull request titles

Use a short, imperative title that describes what changed, for example "Add a Bifrost collector"
or "Reject credentials embedded in collector URLs". In the description, list the concrete changes
and the commands you ran to test them.

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).

## Security issues

Please do not open a public issue for a suspected vulnerability. See [SECURITY.md](SECURITY.md)
for how to report it.
