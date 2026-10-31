package platform

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gamebackend/pkg/pg"
	"gamebackend/services/economy"
	"gamebackend/services/progress"
	"gamebackend/services/rank"
	"gamebackend/services/session"
)

// Reward rules live here, in one auditable place, until they outgrow a switch
// statement. Every sink call is idempotent by a key derived from result_id, so a
// retried battle report cannot pay a player twice.
//
// Atomicity (ADR 0007): when a database is configured, the whole fan-out runs in
// one transaction - the report, every player's xp/gold/mmr and the session state
// change commit together or not at all. In memory mode that is impossible (each
// store owns its own maps), so the writes stay idempotent and a retry converges.
type matchSink struct {
	log      *slog.Logger
	pool     *pgxpool.Pool
	progress *progress.Service
	economy  *economy.Service
	rank     *rank.Service
}

func (m *matchSink) MatchFinished(ctx context.Context, match session.FinishedMatch) error {
	if m.pool == nil {
		if err := m.apply(ctx, m.progress, m.economy, m.rank, match); err != nil {
			m.log.ErrorContext(ctx, "match fan-out incomplete", "session_id", match.SessionID,
				"result_id", match.ResultID, "errors", err)
			return err
		}
		m.log.InfoContext(ctx, "match fan-out complete (memory stores: idempotent, not atomic)",
			"session_id", match.SessionID, "result_id", match.ResultID, "players", len(match.Players))
		return nil
	}

	// Services are stateless wrappers around a Store, so a transaction-scoped set
	// of them is cheap: the same reward code runs against a tx instead of a pool.
	err := pg.InTx(ctx, m.pool, func(tx pgx.Tx) error {
		return m.apply(ctx,
			progress.NewWithStore(m.log, progress.NewWithDB(tx)),
			economy.NewWithStore(m.log, economy.NewWithDB(tx)),
			rank.NewWithStore(m.log, rank.NewWithDB(tx)),
			match)
	})
	if err != nil {
		// Nothing landed: the transaction rolled back, so the session stays open
		// and the caller retries with the same result_id.
		m.log.ErrorContext(ctx, "match fan-out failed, nothing was applied",
			"session_id", match.SessionID, "result_id", match.ResultID, "err", err)
		return err
	}
	m.log.InfoContext(ctx, "match fan-out complete", "session_id", match.SessionID,
		"result_id", match.ResultID, "players", len(match.Players))
	return nil
}

// apply runs the reward rules against one set of stores, collecting every error so
// a single failure does not hide the others.
func (m *matchSink) apply(ctx context.Context, pro *progress.Service, eco *economy.Service,
	rk *rank.Service, match session.FinishedMatch) error {
	var errs []error
	for _, player := range match.Players {
		key := match.ResultID + ":" + player.PlayerID

		xp := 50 + player.Score/10
		if _, _, err := pro.Grant(ctx, player.PlayerID, key+":xp", xp, "match_reward"); err != nil {
			errs = append(errs, err)
		}

		gold := int64(25)
		if player.Won {
			gold = 100
		}
		if _, _, err := eco.Grant(ctx, player.PlayerID, key+":gold", economy.CurrencyGold, gold, "match_reward"); err != nil {
			errs = append(errs, err)
		}

		if _, _, _, err := rk.Apply(ctx, rank.DefaultBoard, player.PlayerID, key+":mmr", player.Won); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
