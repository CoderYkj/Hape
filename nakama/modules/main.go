// Package main is a Nakama Go runtime plugin.
//
// Nakama owns authentication, friends, guilds, chat and the device/Steam/Apple
// sign-in flows. It does NOT own economy, progression or match session state:
// those live in the Go services in this repo (see ADR 0001).
//
// This plugin's whole job is to bridge the two worlds:
//
//	client --(Nakama session)--> Nakama --(this RPC)--> backend POST /v1/account/sessions
//
// Build (requires network access to fetch nakama-common):
//
//	go mod tidy
//	go build -buildmode=plugin -o build/gamemodule.so .
//
// Then copy the .so into Nakama's data/modules directory and restart Nakama.
//
// NOT covered by `scripts/verify.ps1`: this file is a separate Go module whose
// dependency graph needs network access, so it is excluded from the root
// module build on purpose.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// backendClient calls the Go services with the service token.
type backendClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newBackendClient() *backendClient {
	base := os.Getenv("BACKEND_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	token := os.Getenv("BACKEND_SERVICE_TOKEN")
	if token == "" {
		token = "dev-service"
	}
	return &backendClient{baseURL: base, token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// login exchanges a Nakama user id for a backend player session.
func (c *backendClient) login(ctx context.Context, externalID, displayName string) (map[string]any, error) {
	payload, err := json.Marshal(map[string]any{
		"external_id":  externalID,
		"display_name": displayName,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/account/sessions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend login: status %d: %s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("backend login: invalid JSON: %w", err)
	}
	return out, nil
}

// rpcLogin is callable from the client as `login` after the Nakama session is
// established. It returns the backend player_id and session token.
func rpcLogin(ctx context.Context, logger runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, payload string) (string, error) {
	userID, ok := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if !ok || userID == "" {
		return "", fmt.Errorf("login: no Nakama user in context")
	}
	username, _ := ctx.Value(runtime.RUNTIME_CTX_USERNAME).(string)

	result, err := newBackendClient().login(ctx, userID, username)
	if err != nil {
		logger.Error("backend login failed: %v", err)
		return "", fmt.Errorf("login: backend unavailable")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// InitModule registers the bridge RPCs.
func InitModule(_ context.Context, logger runtime.Logger, _ *sql.DB, _ runtime.NakamaModule, initializer runtime.Initializer) error {
	if err := initializer.RegisterRpc("login", rpcLogin); err != nil {
		return err
	}
	logger.Info("gamemodule loaded: rpc=login")
	return nil
}
