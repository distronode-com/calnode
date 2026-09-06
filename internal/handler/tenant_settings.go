package handler

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/calnode/calnode/internal/db"
)

// tenantSettings holds the two server_settings columns that exist only for
// multi-tenant deployments (migration 00062): the origins allowed to embed this
// workspace's booking page, and the speech-to-text host its notetaker uses.
//
// Both were written by the platform API from the day the columns existed and read
// by nothing, so every tenant shared the process-wide EMBED_ALLOWED_ORIGINS and
// STT_BASE_URL. That is a real isolation gap for the CORS allowlist (tenant A's
// embed origins were honoured on tenant B's booking page) and a routing gap for
// STT (an EU tenant's recordings went to whichever host the process was booted
// with). These readers close both.
//
// Single-tenant mode does not consult either column: the process-wide values keep
// their meaning exactly, and the columns are ” there anyway because only the
// platform API writes them.
type tenantSettings struct {
	embedOrigins []string
	sttBaseURL   string
}

// loadTenantSettings reads the two columns through a handle bound to the
// workspace, so the row is that workspace's own. A missing row (a workspace the
// platform API did not create, or a test that seeded no settings) is the same as
// empty values, not an error: empty is the documented default for both.
func loadTenantSettings(d *db.DB) (tenantSettings, error) {
	var origins, stt string
	err := d.QueryRow(`SELECT embed_allowed_origins, stt_base_url FROM server_settings WHERE id = 1`).
		Scan(&origins, &stt)
	if errors.Is(err, sql.ErrNoRows) {
		return tenantSettings{}, nil
	}
	if err != nil {
		return tenantSettings{}, err
	}
	var list []string
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			list = append(list, o)
		}
	}
	return tenantSettings{embedOrigins: list, sttBaseURL: strings.TrimSpace(stt)}, nil
}

// tenantSettings returns this workspace's per-tenant settings, cached per
// workspace like the vendor clients. A read error is logged and answers as
// empty, because both consumers have a safe empty: the CORS check falls back to
// "no origin is allowed" (see EmbedOriginsFor) and the notetaker falls back to
// the process default host.
func (h *Handler) tenantSettings() tenantSettings {
	return h.settingsCache.get(h.cacheKey(), func() tenantSettings {
		s, err := loadTenantSettings(h.db)
		if err != nil {
			h.logger.Warn("settings: could not load per-tenant settings", "workspace", h.cacheKey(), "error", err)
			return tenantSettings{}
		}
		return s
	})
}

// EmbedOriginsFor is the multi-tenant origin resolver for the public CORS
// middleware: the workspace is the one whose public host the request names, and
// the allowlist is that workspace's own.
//
// The middleware runs BEFORE the route's own host resolution, so this is a second
// lookup of the same host on the platform handle; it is one indexed read and the
// route's 404 for an unknown host is unchanged. For an unknown host the resolver
// answers known=false, which the middleware turns into NO Access-Control-Allow-Origin
// at all — never `*`, because "no workspace" must not read as "any origin".
//
// An EMPTY per-tenant list keeps the single-tenant meaning: any origin. That is
// what an operator who never set the field expects, and it is what the platform
// API stores when the caller passes none.
func (h *Handler) EmbedOriginsFor(r *http.Request) ([]string, bool) {
	ws, err := h.workspaceByHost(r.Context(), r.Host)
	if err != nil {
		return nil, false
	}
	return h.forWorkspace(ws).tenantSettings().embedOrigins, true
}
