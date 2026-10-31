package platform

import (
	"context"
	"errors"
	"log/slog"

	"gamebackend/services/economy"
	"gamebackend/services/progress"
	"gamebackend/services/rank"
	"gamebackend/services/session"
)

// Reward rules live here, in one auditable place, until they outgrow a switch
// statement. Every sink call is idempotent by a key derived from result_id, so
// a retried battle report cannot pay a player twice.
type matchSink struct {
	log      *slog.Logger
	progress *progress.Service
	economy  *economy.Service
	rank     *rank.Service
}

func (m *matchSink) MatchFinished(ctx context.Context, match session.FinishedMatch) error {
	var errs []error
	for _, player := range match.Players {
		key := match.ResultID + ":" + player.PlayerID

		xp := 50 + player.Score/10
		if _, _, err := m.progress.Grant(player.PlayerID, key+":xp", xp, "match_reward"); err != nil {
			errs = append(errs, err)
		}

		gold := int64(25)
		if player.Won {
			gold = 100
		}
		if _, _, err := m.economy.Grant(player.PlayerID, key+":gold", economy.CurrencyGold, gold, "match_reward"); err != nil {
			errs = append(errs, err)
		}

		if _, _, _, err := m.rank.Apply(rank.DefaultBoard, player.PlayerID, key+":mmr", player.Won); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		m.log.ErrorContext(ctx, "match fan-out incomplete", "session_id", match.SessionID, "result_id", match.ResultID, "errors", len(errs))
		return errors.Join(errs...)
	}
	m.log.InfoContext(ctx, "match fan-out complete", "session_id", match.SessionID,
		"result_id", match.ResultID, "players", len(match.Players))
	return nil
}
