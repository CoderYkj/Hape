module gamemodule

go 1.21

// The nakama-common version must match the Nakama server you actually run: check
// `nakama --version` (or the image tag in deploy/docker-compose.yml) and pin the
// matching release.
//
// v1.32.0 is the nakama-common version Nakama v3.22.0 itself was built against (see
// the server's go.mod). A Go plugin shares its type information with the hosting
// binary, so a different nakama-common - or a different Go toolchain - makes
// plugin.Open fail and Nakama refuse to start. The CI job `nakama` detects the
// server's toolchain from the image and builds this module with GOTOOLCHAIN set to
// it, rather than guessing.
require github.com/heroiclabs/nakama-common v1.32.0

require google.golang.org/protobuf v1.34.1 // indirect
