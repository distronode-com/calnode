package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/stt"
)

func notetakerSettings(t *testing.T, h *handler.Handler, apiKey string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.RequireAuth(h.GetNotetakerSettings)(rec, authReq(http.MethodGet, "/v1/settings/notetaker", "", apiKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// An admin should be able to read which endpoint recording audio is sent to without
// shelling into the container to look at the environment.
func TestNotetakerSettings_reportsSTTBaseURL(t *testing.T) {
	h, _, apiKey, _ := setupWorkspaceWithDB(t)

	if got := notetakerSettings(t, h, apiKey)["stt_base_url"]; got != stt.DefaultBaseURL {
		t.Errorf("stt_base_url = %v; want the provider default %q", got, stt.DefaultBaseURL)
	}

	h.SetSTTBaseURL("https://api.eu.deepgram.com")
	if got := notetakerSettings(t, h, apiKey)["stt_base_url"]; got != "https://api.eu.deepgram.com" {
		t.Errorf("stt_base_url = %v; want the configured host", got)
	}
}

// Read-only: PATCH has no field for it, so a value posted under that name is ignored
// rather than stored. The endpoint answers with GetNotetakerSettings, so the response
// still reports the env-configured host.
func TestNotetakerSettings_sttBaseURLIsNotWritable(t *testing.T) {
	h, _, apiKey, _ := setupWorkspaceWithDB(t)
	h.SetSTTBaseURL("https://api.eu.deepgram.com")

	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchNotetakerSettings)(rec,
		authReq(http.MethodPatch, "/v1/settings/notetaker",
			`{"enabled":true,"stt_base_url":"https://attacker.example.test"}`, apiKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 — %s", rec.Code, rec.Body.String())
	}

	if got := notetakerSettings(t, h, apiKey)["stt_base_url"]; got != "https://api.eu.deepgram.com" {
		t.Errorf("stt_base_url = %v; a PATCH must not be able to repoint it", got)
	}
}
