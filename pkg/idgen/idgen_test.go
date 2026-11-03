package idgen_test

import (
	"strings"
	"sync"
	"testing"

	"gamebackend/pkg/idgen"
)

// TestIDsAreUniqueUnderConcurrency is the property the load test broke: many goroutines
// minting an id in the same clock tick must not produce the same value.
func TestIDsAreUniqueUnderConcurrency(t *testing.T) {
	const workers, perWorker = 16, 5000
	ids := make(chan string, workers*perWorker)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ids <- idgen.New("s_")
			}
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool, workers*perWorker)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("minted %d unique ids, want %d", len(seen), workers*perWorker)
	}
}

func TestPrefixIsKept(t *testing.T) {
	for _, prefix := range []string{"s_", "t_", "m_", "q_", ""} {
		id := idgen.New(prefix)
		if !strings.HasPrefix(id, prefix) {
			t.Fatalf("id %q does not start with %q", id, prefix)
		}
		// Timestamp + counter + entropy: two dashes separate three non-empty parts.
		if parts := strings.Split(strings.TrimPrefix(id, prefix), "-"); len(parts) != 3 {
			t.Fatalf("id %q has %d parts, want 3", id, len(parts))
		}
	}
}
