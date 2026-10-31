package rank

import (
	"sort"
	"sync"
	"time"

	"gamebackend/pkg/apierr"
)

// Rating constants. The ladder is Elo-shaped but deliberately simple: a
// dedicated rating service is a later problem, not a v0 one.
const (
	// DefaultBoard is the ladder matchmaking reads from.
	DefaultBoard = "ranked_1v1"
	// DefaultRating is where every new player starts.
	DefaultRating int64 = 1000
	// WinDelta and LossDelta are the point swing of one match.
	WinDelta  int64 = 24
	LossDelta int64 = -16
	// MinRating is the ladder floor; nobody falls below it.
	MinRating int64 = 100
)

// Rating is one player's standing on one board.
type Rating struct {
	PlayerID  string    `json:"player_id" doc:"Player id."`
	Board     string    `json:"board" doc:"Board name, e.g. ranked_1v1."`
	Rating    int64     `json:"rating" doc:"Current MMR."`
	Wins      int64     `json:"wins" doc:"Matches won."`
	Losses    int64     `json:"losses" doc:"Matches lost."`
	UpdatedAt time.Time `json:"updated_at" doc:"RFC3339 time of the last change."`
}

// Entry is one leaderboard row.
type Entry struct {
	PlayerID string `json:"player_id" doc:"Player id."`
	Rating   int64  `json:"rating" doc:"Current MMR."`
	Rank     int32  `json:"rank" doc:"1-based position on this board."`
	Wins     int64  `json:"wins" doc:"Matches won."`
	Losses   int64  `json:"losses" doc:"Matches lost."`
}

// Leaderboard is a sorted snapshot.
type Leaderboard struct {
	Board     string    `json:"board" doc:"Board name."`
	Season    string    `json:"season" doc:"Season identifier; ratings reset between seasons."`
	Entries   []Entry   `json:"entries" doc:"Top entries, best first."`
	UpdatedAt time.Time `json:"updated_at" doc:"RFC3339 snapshot time."`
}

// Store is the persistence seam for ratings.
type Store interface {
	RatingOf(board, playerID string) int64
	Apply(board, playerID, idempotencyKey string, won bool) (rating Rating, delta int64, applied bool, err error)
	Top(board string, limit int) (Leaderboard, error)
}

// Memory is the in-memory Store.
type Memory struct {
	mu      sync.Mutex
	byKey   map[string]Rating
	applied map[string]Rating
	now     func() time.Time
}

// NewMemory builds an empty rating store.
func NewMemory() *Memory {
	return &Memory{byKey: map[string]Rating{}, applied: map[string]Rating{}, now: time.Now}
}

func key(board, playerID string) string { return board + "|" + playerID }

func (m *Memory) RatingOf(board, playerID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rating, ok := m.byKey[key(board, playerID)]; ok {
		return rating.Rating
	}
	return DefaultRating
}

func (m *Memory) Apply(board, playerID, idempotencyKey string, won bool) (Rating, int64, bool, error) {
	if idempotencyKey == "" {
		return Rating{}, 0, false, apierr.Invalid("idempotency key is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if previous, ok := m.applied[board+"|"+playerID+"|"+idempotencyKey]; ok {
		return previous, 0, false, nil
	}
	rating, ok := m.byKey[key(board, playerID)]
	if !ok {
		rating = Rating{PlayerID: playerID, Board: board, Rating: DefaultRating}
	}

	delta := WinDelta
	if won {
		rating.Wins++
	} else {
		delta = LossDelta
		rating.Losses++
	}
	rating.Rating += delta
	if rating.Rating < MinRating {
		rating.Rating = MinRating
	}
	rating.UpdatedAt = m.now().UTC()

	m.byKey[key(board, playerID)] = rating
	m.applied[board+"|"+playerID+"|"+idempotencyKey] = rating
	return rating, delta, true, nil
}

func (m *Memory) Top(board string, limit int) (Leaderboard, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries := make([]Entry, 0, len(m.byKey))
	for k, rating := range m.byKey {
		if rating.Board != board {
			continue
		}
		entries = append(entries, Entry{
			PlayerID: rating.PlayerID,
			Rating:   rating.Rating,
			Wins:     rating.Wins,
			Losses:   rating.Losses,
			Rank:     0,
		})
		_ = k
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Rating != entries[j].Rating {
			return entries[i].Rating > entries[j].Rating
		}
		return entries[i].PlayerID < entries[j].PlayerID
	})
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	for i := range entries {
		entries[i].Rank = int32(i + 1)
	}
	return Leaderboard{
		Board:     board,
		Season:    "s0",
		Entries:   entries,
		UpdatedAt: m.now().UTC(),
	}, nil
}
