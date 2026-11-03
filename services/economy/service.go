// Package economy owns wallets and the immutable ledger.
//
// Non-negotiables:
//   - integer money only, per currency allowlist;
//   - every movement is idempotent and lands in the ledger;
//   - a balance can never go negative;
//   - clients never state their own balance or price.
package economy

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
	"gamebackend/pkg/httpx"
)

// Service is the economy domain service.
type Service struct {
	log   *slog.Logger
	store Store
}

// New builds the service with the in-memory store.
func New(log *slog.Logger) *Service { return &Service{log: log, store: NewMemory()} }

// NewWithStore builds the service on a caller-supplied store.
func NewWithStore(log *slog.Logger, store Store) *Service {
	return &Service{log: log, store: store}
}

// GetWalletResponse is the wallet read model.
type GetWalletResponse struct {
	Wallet Wallet `json:"wallet"`
}

// GrantRequest is the body of POST /v1/economy/players/{player_id}/grants.
type GrantRequest struct {
	Currency string `json:"currency" doc:"gold or gems."`
	Amount   int64  `json:"amount" doc:"Integer amount greater than zero."`
	Reason   string `json:"reason" doc:"Audit trail, e.g. match_reward."`
}

// GrantResponse reports the wallet after the grant.
type GrantResponse struct {
	Wallet  Wallet `json:"wallet"`
	Applied bool   `json:"applied" doc:"false when the idempotency key was already used."`
}

// PurchaseRequest is the body of
// POST /v1/economy/players/{player_id}/purchases.
type PurchaseRequest struct {
	Currency string `json:"currency" doc:"Currency to spend."`
	Amount   int64  `json:"amount" doc:"Price in integer minor units; the server owns pricing."`
	ItemID   string `json:"item_id" doc:"Item being bought."`
}

// PurchaseResponse reports the wallet after the purchase.
type PurchaseResponse struct {
	Wallet  Wallet `json:"wallet"`
	Applied bool   `json:"applied" doc:"false when the idempotency key was already used."`
}

// LedgerResponse lists a player's movements.
type LedgerResponse struct {
	Entries []LedgerEntry `json:"entries" doc:"Immutable movements, oldest first."`
	// NextAfter is the cursor to pass as ?after= for the next page. Empty means this is the
	// last page.
	NextAfter string `json:"next_after" doc:"Cursor for the next page; empty when there is none."`
}

// Ledger pagination. The default is what a UI can render without thinking; the cap is what
// keeps one request from materialising a whale's whole history.
const (
	defaultLedgerLimit = 50
	maxLedgerLimit     = 200
)

// Register mounts the economy operations on r.
func (s *Service) Register(r *httpx.Router) {
	r.Handle(api.Op{
		ID:       "economy.get_wallet",
		Service:  "economy",
		Method:   http.MethodGet,
		Path:     "/v1/economy/players/{player_id}/wallet",
		Summary:  "Read the caller's wallet",
		Response: api.T(GetWalletResponse{}),
		Auth:     api.AuthPlayer,
	}, s.getWallet)

	r.Handle(api.Op{
		ID:         "economy.grant",
		Service:    "economy",
		Method:     http.MethodPost,
		Path:       "/v1/economy/players/{player_id}/grants",
		Summary:    "Grant currency (server-to-server, idempotent)",
		Request:    api.Body(GrantRequest{}),
		Response:   api.T(GrantResponse{}),
		Auth:       api.AuthService,
		Idempotent: true,
		Note:       "Requires an Idempotency-Key header. Every movement is written to the ledger.",
	}, s.grant)

	r.Handle(api.Op{
		ID:         "economy.purchase",
		Service:    "economy",
		Method:     http.MethodPost,
		Path:       "/v1/economy/players/{player_id}/purchases",
		Summary:    "Spend currency on an item",
		Request:    api.Body(PurchaseRequest{}),
		Response:   api.T(PurchaseResponse{}),
		Auth:       api.AuthPlayer,
		Idempotent: true,
		Note:       "Requires an Idempotency-Key header. Returns failed_precondition when the balance is short.",
	}, s.purchase)

	r.Handle(api.Op{
		ID:       "economy.get_ledger",
		Service:  "economy",
		Method:   http.MethodGet,
		Path:     "/v1/economy/players/{player_id}/ledger",
		Summary:  "Read the caller's ledger, newest pages by cursor",
		Response: api.T(LedgerResponse{}),
		Auth:     api.AuthPlayer,
		Query: []api.QueryParam{
			{Name: "limit", Type: "integer", Doc: "Page size, 1-200 (default 50). Larger values are clamped."},
			{Name: "after", Type: "string", Doc: "Cursor from the previous page's next_after. Omit for the first page."},
		},
	}, s.getLedger)
}

func (s *Service) getWallet(r *http.Request) (any, error) {
	playerID := httpx.Param(r, "player_id")
	if err := httpx.RequireSelf(r, playerID); err != nil {
		return nil, err
	}
	wallet, err := s.store.Wallet(r.Context(), playerID)
	if err != nil {
		return nil, err
	}
	return GetWalletResponse{Wallet: wallet}, nil
}

func (s *Service) getLedger(r *http.Request) (any, error) {
	playerID := httpx.Param(r, "player_id")
	if err := httpx.RequireSelf(r, playerID); err != nil {
		return nil, err
	}
	limit, err := httpx.QueryInt(r, "limit", defaultLedgerLimit, maxLedgerLimit)
	if err != nil {
		return nil, err
	}
	after := strings.TrimSpace(r.URL.Query().Get("after"))
	entries, err := s.store.Ledger(r.Context(), playerID, LedgerQuery{Limit: limit, After: after})
	if err != nil {
		return nil, err
	}
	// A full page means there may be more; anything shorter is the end. Saying so on every
	// page is what lets a client stop paging without an extra probe.
	next := ""
	if len(entries) == limit {
		next = entries[len(entries)-1].ID
	}
	return LedgerResponse{Entries: entries, NextAfter: next}, nil
}

func (s *Service) grant(r *http.Request) (any, error) {
	if _, err := httpx.RequireService(r); err != nil {
		return nil, err
	}
	key, err := httpx.IdempotencyKey(r)
	if err != nil {
		return nil, err
	}
	playerID := httpx.Param(r, "player_id")
	var req GrantRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	wallet, applied, err := s.store.Grant(r.Context(), playerID, key, req.Currency, req.Amount, req.Reason)
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(r.Context(), "currency granted", "player_id", playerID,
		"currency", req.Currency, "amount", req.Amount, "applied", applied)
	return GrantResponse{Wallet: wallet, Applied: applied}, nil
}

func (s *Service) purchase(r *http.Request) (any, error) {
	playerID := httpx.Param(r, "player_id")
	if err := httpx.RequireSelf(r, playerID); err != nil {
		return nil, err
	}
	key, err := httpx.IdempotencyKey(r)
	if err != nil {
		return nil, err
	}
	var req PurchaseRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	if !IsCurrency(req.Currency) {
		return nil, apierr.Invalid("unsupported currency").WithDetail("currency", req.Currency)
	}
	wallet, applied, err := s.store.Spend(r.Context(), playerID, key, req.Currency, req.Amount, req.ItemID)
	if err != nil {
		return nil, err
	}
	return PurchaseResponse{Wallet: wallet, Applied: applied}, nil
}

// Grant applies a currency grant at the domain level. Shared by the HTTP
// handler and the in-process match fan-out.
func (s *Service) Grant(ctx context.Context, playerID, idempotencyKey, currency string, amount int64, reason string) (Wallet, bool, error) {
	return s.store.Grant(ctx, playerID, idempotencyKey, currency, amount, reason)
}
