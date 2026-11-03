package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoginSendsTheServiceToken is the regression guard for a bug that only a real load could
// find: the client stored the service token but never put it on the request, so the backend
// answered 401. Compiling a plugin proves nothing about whether it works.
func TestLoginSendsTheServiceToken(t *testing.T) {
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"player":{"id":"p_1"},"session_token":"tok"}`))
	}))
	defer server.Close()

	client := &backendClient{baseURL: server.URL, token: "dev-service", http: server.Client()}
	out, err := client.login(context.Background(), "nakama-user-1", "Alice")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if gotAuth != "Bearer dev-service" {
		t.Fatalf("Authorization = %q, want \"Bearer dev-service\"", gotAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if sent["external_id"] != "nakama-user-1" || sent["display_name"] != "Alice" {
		t.Fatalf("request body = %v", sent)
	}
	if out["session_token"] != "tok" {
		t.Fatalf("response was not passed through: %v", out)
	}
}

// TestLoginSurfacesBackendRejection keeps a 401 from being reported as success.
func TestLoginSurfacesBackendRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
	}))
	defer server.Close()

	client := &backendClient{baseURL: server.URL, token: "wrong", http: server.Client()}
	if _, err := client.login(context.Background(), "u", "n"); err == nil {
		t.Fatal("a rejected login must return an error")
	}
}
