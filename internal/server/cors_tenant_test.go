package server_test

import (
	"context"
	"net/http"
	"testing"
)

// End to end through the real mux: the public booking endpoints honour the
// workspace's OWN embed allowlist, chosen by the request's host, and an unknown
// host gets no CORS header at all. Before this, EMBED_ALLOWED_ORIGINS was one
// process-wide list, so a tenant's embed origin could call every tenant's
// booking endpoints.
func TestTenancy_embedOriginsArePerWorkspace(t *testing.T) {
	f := newTenancyFixture(t)

	// The fixture seeds an empty settings row; give acme an allowlist through the
	// platform handle, naming the workspace, before the first request fills the
	// per-workspace cache.
	if _, err := f.plat.ExecContext(context.Background(),
		`UPDATE server_settings SET embed_allowed_origins = ? WHERE workspace_id = ? AND id = 1`,
		"https://embed-acme.example", "acme"); err != nil {
		t.Fatalf("set acme embed origins: %v", err)
	}

	get := func(host, slug, origin string) (int, string) {
		r := newRequest(t, http.MethodGet, host, "/v1/event-types/"+slug+"/public", nil)
		r.Header.Set("Origin", origin)
		rec := f.serve(r)
		return rec.Code, rec.Header().Get("Access-Control-Allow-Origin")
	}

	if code, acao := get(hostA, f.a.eventSlug, "https://embed-acme.example"); code != http.StatusOK || acao != "https://embed-acme.example" {
		t.Errorf("acme's own embed origin: %d %q; want 200 and the origin echoed", code, acao)
	}
	if code, acao := get(hostA, f.a.eventSlug, "https://embed-globex.example"); code != http.StatusOK || acao != "" {
		t.Errorf("a foreign origin on acme: %d %q; want 200 with NO allow-origin", code, acao)
	}
	if code, acao := get(hostB, f.b.eventSlug, "https://embed-globex.example"); code != http.StatusOK || acao != "*" {
		t.Errorf("globex with an empty allowlist: %d %q; want 200 and *", code, acao)
	}
	if code, acao := get("nobody.example", f.a.eventSlug, "https://embed-acme.example"); code != http.StatusNotFound || acao != "" {
		t.Errorf("unknown host: %d %q; want 404 with NO allow-origin (never *)", code, acao)
	}
}
