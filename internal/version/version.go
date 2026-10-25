// Package version carries the build's release tag so the agent can report it to Plutus.
package version

// Version is the release tag the binary was built from ("v1.3.0"), stamped at link time with
// -ldflags "-X github.com/plutus-cloud/plutus-collector/internal/version.Version=<tag>". A build
// without the flag (go run, go test, a local docker build) reports "dev", which is also what
// Plutus should read as "not a released build".
var Version = "dev"
