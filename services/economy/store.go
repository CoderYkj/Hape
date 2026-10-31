package economy

import (
	"strconv"
	"sync"
	"time"

	"gamebackend/pkg/apierr"
)

// The currency allowlist. Adding a currency is a deliberate act: it needs a
// migration, a client change and a decision about whether it is tradeable.
const (
	CurrencyGold = "gold"
	CurrencyGems = "gems"
)

var allowedCurrencies = map[string]bool{CurrencyGold: true, CurrencyGems: true}

// IsCurrency reports whether c is a supported currency.
func IsCurrency(c string) bool { return allowedCurrencies[c] }

// Wallet is a player's balances.
//
// Money is integer minor units. There is no float anywhere in this package:
// floating point money loses a cent, then a player loses trust.
type Wallet struct {
	PlayerID  string           `json:"player_id" doc:"Player id."`
	Balances  map[string]int64 `json:"balances" doc:"Currency code to integer balance."`
	UpdatedAt time.Time        `json:"updated_at" doc:"RFC3339 time of the last change."`
}

// LedgerEntry is one immutable movement.
type LedgerEntry struct {
	ID        string    `json:"id" doc:"Ledger entry id."`
	PlayerID  string    `json:"player_id" doc:"Player id."`
	Currency  string    `json:"currency" doc:"Currency code."`
	Amount    int64     `json:"amount" doc:"Signed amount; negative for spending."`
	Reason    string    `json:"reason" doc:"Why the movement happened."`
	CreatedAt time.Time `json:"created_at" doc:"RFC3339 time of the movement."`
}

// Store is the persistence seam for the economy.
type Store interface {
	Wallet(playerID string) (Wallet, error)
	Grant(playerID, idempotencyKey, currency string, amount int64, reason string) (Wallet, bool, error)
	Spend(playerID, idempotencyKey, currency string, amount int64, itemID string) (Wallet, bool, error)
	Ledger(playerID string) ([]LedgerEntry, error)
}

// Memory is the in-memory Store.
type Memory struct {
	mu      sync.Mutex
	wallets map[string]Wallet
	ledger  map[string][]LedgerEntry
	applied map[string]Wallet
	serial  int64
	now     func() time.Time
}

// NewMemory builds an empty wallet store.
func NewMemory() *Memory {
	return &Memory{
		wallets: map[string]Wallet{},
		ledger:  map[string][]LedgerEntry{},
		applied: map[string]Wallet{},
		now:     time.Now,
	}
}

func (m *Memory) Wallet(playerID string) (Wallet, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.walletLocked(playerID), nil
}

func (m *Memory) walletLocked(playerID string) Wallet {
	if wallet, ok := m.wallets[playerID]; ok {
		return wallet
	}
	return Wallet{PlayerID: playerID, Balances: map[string]int64{}, UpdatedAt: m.now().UTC()}
}

func (m *Memory) Grant(playerID, idempotencyKey, currency string, amount int64, reason string) (Wallet, bool, error) {
	if err := validateMovement(idempotencyKey, currency, amount); err != nil {
		return Wallet{}, false, err
	}
	if reason == "" {
		return Wallet{}, false, apierr.Invalid("reason is required").WithDetail("field", "reason")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if previous, ok := m.applied[playerID+"|"+idempotencyKey]; ok {
		return previous, false, nil
	}
	wallet := m.walletLocked(playerID)
	wallet.Balances[currency] += amount
	wallet.UpdatedAt = m.now().UTC()
	m.wallets[playerID] = wallet
	m.applied[playerID+"|"+idempotencyKey] = wallet
	m.appendLedgerLocked(playerID, currency, amount, reason)
	return wallet, true, nil
}

func (m *Memory) Spend(playerID, idempotencyKey, currency string, amount int64, itemID string) (Wallet, bool, error) {
	if err := validateMovement(idempotencyKey, currency, amount); err != nil {
		return Wallet{}, false, err
	}
	if itemID == "" {
		return Wallet{}, false, apierr.Invalid("item_id is required").WithDetail("field", "item_id")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if previous, ok := m.applied[playerID+"|"+idempotencyKey]; ok {
		return previous, false, nil
	}
	wallet := m.walletLocked(playerID)
	balance := wallet.Balances[currency]
	if balance < amount {
		return Wallet{}, false, apierr.Precondition("insufficient balance").
			WithDetail("currency", currency).
			WithDetail("balance", strconv.FormatInt(balance, 10)).
			WithDetail("required", strconv.FormatInt(amount, 10))
	}
	wallet.Balances[currency] = balance - amount
	wallet.UpdatedAt = m.now().UTC()
	m.wallets[playerID] = wallet
	m.applied[playerID+"|"+idempotencyKey] = wallet
	m.appendLedgerLocked(playerID, currency, -amount, "purchase:"+itemID)
	return wallet, true, nil
}

func (m *Memory) Ledger(playerID string) ([]LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.ledger[playerID]
	out := make([]LedgerEntry, len(entries))
	copy(out, entries)
	return out, nil
}

func (m *Memory) appendLedgerLocked(playerID, currency string, amount int64, reason string) {
	m.serial++
	m.ledger[playerID] = append(m.ledger[playerID], LedgerEntry{
		ID:        "l_" + strconv.FormatInt(m.serial, 36),
		PlayerID:  playerID,
		Currency:  currency,
		Amount:    amount,
		Reason:    reason,
		CreatedAt: m.now().UTC(),
	})
}

func validateMovement(idempotencyKey, currency string, amount int64) error {
	if idempotencyKey == "" {
		return apierr.Invalid("idempotency key is required")
	}
	if !IsCurrency(currency) {
		return apierr.Invalid("unsupported currency").WithDetail("currency", currency)
	}
	if amount <= 0 {
		return apierr.Invalid("amount must be greater than zero").WithDetail("field", "amount")
	}
	return nil
}
