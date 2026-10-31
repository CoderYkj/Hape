module gamemodule

go 1.23.3

// The nakama-common version must match the Nakama server you actually run: check
// `nakama --version` (or the image tag in deploy/docker-compose.yml) and pin the
// matching release.
//
// v1.35.0 is the version this module type-checks against today. The CI job
// `nakama-module` builds it with -buildmode=plugin, which is the only build that
// proves it still loads into Nakama: a plain `go build` demands func main, and a
// plugin deliberately has none.
require github.com/heroiclabs/nakama-common v1.35.0

require google.golang.org/protobuf v1.35.2 // indirect
