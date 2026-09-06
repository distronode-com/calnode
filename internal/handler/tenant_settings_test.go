package handler_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/calnode/calnode/internal/dbtest"
	"github.com/calnode/calnode/internal/handler"
	"github.com/calnode/calnode/internal/stt"
)

// The two multi-tenant-only settings columns were written by the platform API and
// read by nothing (D7 follow-up). These pin the readers: each workspace gets its
// OWN embed allowlist and STT host, an empty column falls through exactly as the
// single-tenant ladder does, and an unknown host resolves to no tenant at all.
func TestTenantSettings_perWorkspaceReaders(t *testing.T) {
	app, _ := dbtest.RequireTenantPair(t)

	h := handler.New(app, slog.New(slog.DiscardHandler))
	h.SetMultiTenant(true)
	h.SetBaseURL("https://cal.example.test")
	h.SetPlatformToken(platformToken)
	h.SetEncKey(platformTestEncKey)
	h.SetSTTBaseURL("https://stt-process.example")
	create := h.Platform((*handler.Handler).CreateWorkspace)

	const hostA, hostB = "book.acme.example", "book.globex.example"

	bodyA := platformCreateBody("acme", hostA)
	bodyA["defaults"].(map[string]any)["embed_allowed_origins"] = []string{"https://embed-acme.example", "https://www.acme.example/"}
	bodyA["defaults"].(map[string]any)["stt_base_url"] = "https://stt-eu.example"
	if rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces", bodyA, platformToken); rec.Code != http.StatusCreated {
		t.Fatalf("create acme: %d %s", rec.Code, rec.Body.String())
	}
	bodyB := platformCreateBody("globex", hostB)
	bodyB["defaults"].(map[string]any)["embed_allowed_origins"] = []string{}
	delete(bodyB["defaults"].(map[string]any), "stt_base_url")
	if rec := doPlatform(t, create, http.MethodPost, "/v1/platform/workspaces", bodyB, platformToken); rec.Code != http.StatusCreated {
		t.Fatalf("create globex: %d %s", rec.Code, rec.Body.String())
	}

	// STT: A's own host wins; B, with the column empty, falls through to the
	// process value, which is the rung single-tenant mode has always had.
	if got := handler.STTBaseURLForWorkspaceForTest(h, "acme"); got != "https://stt-eu.example" {
		t.Errorf("acme stt = %q; want its own column", got)
	}
	if got := handler.STTBaseURLForWorkspaceForTest(h, "globex"); got != "https://stt-process.example" {
		t.Errorf("globex stt = %q; want the process value", got)
	}
	h.SetSTTBaseURL("")
	// The cache holds the column, not the resolution, so clearing the process
	// value is visible without a restart: B now lands on the provider default.
	if got := handler.STTBaseURLForWorkspaceForTest(h, "globex"); got != stt.DefaultBaseURL {
		t.Errorf("globex stt with no process value = %q; want %q", got, stt.DefaultBaseURL)
	}
	if got := handler.STTBaseURLForWorkspaceForTest(h, "acme"); got != "https://stt-eu.example" {
		t.Errorf("acme stt after clearing the process value = %q; want its own column still", got)
	}

	// Embed origins: resolved from the request HOST, and an unknown host is
	// "no tenant", never "any origin".
	originsFor := func(host string) ([]string, bool) {
		r := httptest.NewRequest(http.MethodGet, "/v1/event-types/intro/public", nil)
		r.Host = host
		return h.EmbedOriginsFor(r)
	}
	if got, known := originsFor(hostA); !known || !reflect.DeepEqual(got, []string{"https://embed-acme.example", "https://www.acme.example/"}) {
		t.Errorf("acme origins = %v, known=%v; want its own two", got, known)
	}
	if got, known := originsFor(hostB); !known || len(got) != 0 {
		t.Errorf("globex origins = %v, known=%v; want known and empty (any origin)", got, known)
	}
	if got, known := originsFor("nobody.example"); known || got != nil {
		t.Errorf("unknown host origins = %v, known=%v; want unknown", got, known)
	}
}
