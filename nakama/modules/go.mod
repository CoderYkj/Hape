module gamemodule

go 1.23.0

// The nakama-common version must match the Nakama server you actually run.
// Check it with `nakama --version` (or the image tag) and pin accordingly;
// the value below is a placeholder and has NOT been resolved here because this
// environment has no network access to the Go module proxy.
require github.com/heroiclabs/nakama-common v1.35.0
