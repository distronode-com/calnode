package handler

import (
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtime"
	"github.com/calnode/calnode/internal/livekit"
	"github.com/calnode/calnode/internal/llm"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/stripe"
	"github.com/calnode/calnode/internal/stt"
	"github.com/calnode/calnode/internal/webhook"
	"github.com/calnode/calnode/internal/zoom"
)

// Handler is a per-request value: the process-wide state behind *shared, plus the
// database handle and workspace this particular request is scoped to.
//
// In single-tenant mode there is one Handler and one workspace ("default"), and
// nothing copies it. In multi-tenant mode Scoped makes a copy per request whose
// db is bound to the resolved workspace, so a method body that reads h.db is
// tenant-scoped without being edited. The copy is a value with no locks in it,
// which is what keeps go vet copylocks clean — and it pins no connection, so a
// fire-and-forget goroutine may keep the copy it was started with (see
// db.DB.ForWorkspace).
type Handler struct {
	*shared

	db *db.DB
	ws *Workspace

	// bookingSvc and webhookSvc wrap a *db.DB, so they are per-request too:
	// forWorkspace rebuilds each from the scoped handle. Both are cheap structs
	// over a pool, not connections.
	bookingSvc *booking.Service
	webhookSvc *webhook.Service
}

// SetLiveKit swaps the active LiveKit client (nil disables built-in video rooms).
// Hot-reloadable from the LiveKit settings page.
func (h *Handler) SetLiveKit(c *livekit.Client) {
	h.livekitCache.set(h.cacheKey(), c)
	// Self-heal: any recording still 'active' is an orphan from before this restart (its egress
	// is no longer tracked), and would otherwise block the idempotent guard on its room forever.
	if c != nil {
		if _, err := h.db.Exec(
			`UPDATE recordings SET status = 'complete', updated_at = ? WHERE status = 'active'`,
			dbtime.Now()); err != nil {
			h.logger.Warn("livekit: sweep stale recordings", "error", err)
		}
	}
}

// getLiveKit returns this workspace's LiveKit client, or nil when video is
// unconfigured for it. Built lazily from the workspace's own settings row.
func (h *Handler) getLiveKit() *livekit.Client {
	return h.livekitCache.get(h.cacheKey(), func() *livekit.Client {
		cfg, err := LoadLiveKitSettingsFromDB(h.db, h.encKey)
		if err != nil {
			h.logger.Warn("livekit: could not load settings", "workspace", h.cacheKey(), "error", err)
			return nil
		}
		if cfg == nil {
			return nil
		}
		return livekit.New(cfg.URL, cfg.APIKey, cfg.APISecret, h.encKey)
	})
}

// SetStripe swaps the active Stripe client (nil disables paid bookings). Hot-reloadable
// from the Payments settings page.
func (h *Handler) SetStripe(c *stripe.Client) {
	h.stripeCache.set(h.cacheKey(), c)
}

// getStripe returns this workspace's Stripe client, or nil when payments are
// unconfigured for it.
func (h *Handler) getStripe() *stripe.Client {
	return h.stripeCache.get(h.cacheKey(), func() *stripe.Client {
		cfg, err := LoadStripeSettingsFromDB(h.db, h.encKey)
		if err != nil {
			h.logger.Warn("stripe: could not load settings", "workspace", h.cacheKey(), "error", err)
			return nil
		}
		if cfg == nil {
			return nil
		}
		sc, err := stripe.New(cfg.SecretKey, cfg.PublishableKey, cfg.WebhookSecret)
		if err != nil {
			h.logger.Warn("stripe: init failed", "workspace", h.cacheKey(), "error", err)
			return nil
		}
		return sc
	})
}

// SetZoom swaps the active Zoom client (nil disables Zoom auto-minting). Hot-reloadable
// from the Zoom settings page.
func (h *Handler) SetZoom(c *zoom.Client) {
	h.zoomCache.set(h.cacheKey(), c)
}

// getZoom returns this workspace's Zoom client, or nil when it has no Zoom app.
//
// zoom.New captures the handle it is given, so it gets h.db — the BOUND one —
// and the per-host tokens it later reads are this workspace's.
func (h *Handler) getZoom() *zoom.Client {
	return h.zoomCache.get(h.cacheKey(), func() *zoom.Client {
		cfg, err := LoadZoomSettingsFromDB(h.db, h.encKey)
		if err != nil {
			h.logger.Warn("zoom: could not load settings", "workspace", h.cacheKey(), "error", err)
			return nil
		}
		if cfg == nil || cfg.ClientID == "" || cfg.ClientSecret == "" {
			return nil
		}
		zc, err := zoom.New(h.db, cfg.ClientID, cfg.ClientSecret, h.baseURL+"/v1/zoom/callback", hex.EncodeToString(h.encKey[:]))
		if err != nil {
			h.logger.Warn("zoom: init failed", "workspace", h.cacheKey(), "error", err)
			return nil
		}
		return zc
	})
}

// SetLLM swaps the active LLM client (nil disables AI features). Hot-reloadable from
// the settings page.
func (h *Handler) SetLLM(c *llm.Client) {
	h.llmCache.set(h.cacheKey(), c)
}

// getLLM returns this workspace's LLM client, or nil when AI is off for it —
// callers MUST nil-check and fall back to the deterministic path.
func (h *Handler) getLLM() *llm.Client {
	return h.llmCache.get(h.cacheKey(), func() *llm.Client {
		cfg, err := LoadLLMSettingsFromDB(h.db, h.encKey)
		if err != nil {
			h.logger.Warn("llm: could not load settings", "workspace", h.cacheKey(), "error", err)
			return nil
		}
		if cfg == nil || !cfg.Enabled || cfg.Endpoint == "" {
			return nil
		}
		return llm.New(llm.Config{Endpoint: cfg.Endpoint, Model: cfg.Model, APIKey: cfg.APIKey})
	})
}

// getMailer returns this workspace's mailer.
//
// In single-tenant mode the entry is the process-wide *mailer.Live that boot
// installed, so nothing changes. In multi-tenant mode each workspace's transport
// is chosen by BuildMailer from its OWN settings row — which is what keeps one
// tenant's SMTP credentials and From address out of another's email.
func (h *Handler) getMailer() mailer.Mailer {
	return h.mailerCache.get(h.cacheKey(), func() mailer.Mailer {
		cfg, err := LoadEmailSettingsFromDB(h.db, h.encKey)
		if err != nil {
			h.logger.Warn("mailer: could not load settings", "workspace", h.cacheKey(), "error", err)
			return &mailer.Noop{}
		}
		if cfg == nil {
			return &mailer.Noop{}
		}
		m, _ := BuildMailer(*cfg)
		return m
	})
}

func New(database *db.DB, logger *slog.Logger) *Handler {
	whs, _ := webhook.New(database, "") // ephemeral key when no encryption key configured
	return &Handler{
		shared: &shared{
			logger:        logger,
			calNudge:      make(chan struct{}, 1),
			mailerCache:   newTenantCache[mailer.Mailer](),
			calCache:      newTenantCache[*calendar.Service](),
			llmCache:      newTenantCache[*llm.Client](),
			zoomCache:     newTenantCache[*zoom.Client](),
			stripeCache:   newTenantCache[*stripe.Client](),
			livekitCache:  newTenantCache[*livekit.Client](),
			settingsCache: newTenantCache[tenantSettings](),
			appDB:         database,
		},
		db:         database,
		ws:         DefaultWorkspace,
		bookingSvc: booking.New(database),
		webhookSvc: whs,
	}
}

// SetMailer configures the email sender and the base URL used in email links.
// If m is a *mailer.Live, it is also stored as h.live for hot-swap support.
func (h *Handler) SetMailer(m mailer.Mailer, baseURL string) {
	h.mailerCache.set(h.cacheKey(), m)
	h.baseURL = baseURL
	if l, ok := m.(*mailer.Live); ok {
		h.live = l
	}
}

// SetEncKey stores the AES-256 encryption key used for secrets in the DB.
func (h *Handler) SetEncKey(hexKey string) {
	if b, err := hex.DecodeString(hexKey); err == nil && len(b) == 32 {
		copy(h.encKey[:], b)
	}
	// If empty or invalid, encKey stays zero — suitable for dev/test.
}

// SetBaseURL sets the identity host used for OAuth redirects, admin UI links,
// and team invites.
func (h *Handler) SetBaseURL(url string) {
	h.baseURL = url
}

// SetPublicBaseURL sets the booker-facing host used for booking-page links and
// outbound email links. When empty, publicURL falls back to baseURL.
func (h *Handler) SetPublicBaseURL(url string) {
	h.publicBaseURL = url
}

// publicURL returns the booker-facing base URL.
//
// In multi-tenant mode each workspace has its own public host and that replaces
// PUBLIC_BASE_URL entirely (D11): booking links, emails, embed snippets and the
// admin UI all live there, while BASE_URL stays the identity host of the whole
// process for OAuth callbacks, /.well-known/*, /oauth/*, /mcp and the platform
// API. Single-tenant behaviour is unchanged: PUBLIC_BASE_URL if set, else
// BASE_URL.
func (h *Handler) publicURL() string {
	if h.multiTenant && h.ws != nil && h.ws.PublicHost != "" {
		return "https://" + h.ws.PublicHost
	}
	if h.publicBaseURL != "" {
		return h.publicBaseURL
	}
	return h.baseURL
}

// SetSTTBaseURL sets the speech-to-text endpoint host used by the notetaker. Empty keeps
// the provider default.
func (h *Handler) SetSTTBaseURL(url string) {
	h.sttBaseURLCfg = url
}

// sttBaseURL returns the effective endpoint host. Resolved on read rather than at set
// time so a Handler built without SetSTTBaseURL (every test) still reports the real
// default rather than an empty string.
//
// In multi-tenant mode a workspace's own `stt_base_url` (written by the platform
// API, see tenant_settings.go) comes first, so an EU tenant's recordings go to the
// EU speech-to-text host regardless of what the process was booted with. Empty
// falls through to the process value, then the provider default — the same ladder
// single-tenant mode has always had, with one rung added on top of it.
func (h *Handler) sttBaseURL() string {
	if h.multiTenant && h.ws != nil {
		if v := h.tenantSettings().sttBaseURL; v != "" {
			return v
		}
	}
	if h.sttBaseURLCfg != "" {
		return h.sttBaseURLCfg
	}
	return stt.DefaultBaseURL
}

// SetDataDir sets the directory used for file uploads (avatars, etc.).
func (h *Handler) SetDataDir(dir string) {
	h.dataDir = dir
}

// SetDemoMode marks this instance as the public, self-resetting demo, which
// disables calendar/Zoom connect and is surfaced to the frontend via
// GET /v1/auth/status. Never set this on a real deployment.
func (h *Handler) SetDemoMode(v bool) {
	h.demoMode = v
}

// SetDemoResetInterval records how often the demo wipes and re-seeds, purely
// so DemoReset (an on-demand reset) can recompute the same "next reset at"
// estimate the periodic ticker uses.
func (h *Handler) SetDemoResetInterval(d time.Duration) {
	h.demoResetInterval = d
}

// SetDemoNextResetAt records when the next scheduled demo reset will fire,
// surfaced to the frontend via GET /v1/auth/status for a countdown.
func (h *Handler) SetDemoNextResetAt(t time.Time) {
	h.demoMu.Lock()
	h.demoNextResetAt = t
	h.demoMu.Unlock()
}

// getDemoNextResetAt returns the next scheduled demo reset time (zero value
// when not in demo mode).
func (h *Handler) getDemoNextResetAt() time.Time {
	h.demoMu.RLock()
	defer h.demoMu.RUnlock()
	return h.demoNextResetAt
}

// SetCalendar installs the platform-level provider registry (nil disables calendar
// integration). Every cached per-workspace copy is dropped, because each was
// derived from the registry being replaced.
func (h *Handler) SetCalendar(c *calendar.Service) {
	h.calMu.Lock()
	h.calBase = c
	h.calMu.Unlock()
	h.calCache.invalidate(h.cacheKey())
}

// getCal returns this workspace's calendar service, or nil when the instance has
// no providers configured.
//
// ⛔ It must not return calBase. Every provider operation reads
// calendar_connections and connection_calendars, which are TENANT tables, and the
// registry holds whichever handle boot gave it — the unbound one on a multi-tenant
// instance, which matches no row. Service.ForDB rebinds the Service and every
// provider in it, keeping the OAuth app configuration (D7).
func (h *Handler) getCal() *calendar.Service {
	return h.calCache.get(h.cacheKey(), func() *calendar.Service {
		h.calMu.RLock()
		base := h.calBase
		h.calMu.RUnlock()
		if base == nil {
			return nil
		}
		if !h.multiTenant {
			// One workspace, one handle: rebinding would allocate a second Service
			// and every provider in it for no behaviour change.
			return base
		}
		return base.ForDB(h.db)
	})
}

// getGoogleAuth returns the current Google OAuth config under a read lock.
func (h *Handler) getGoogleAuth() *oauth2.Config {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	return h.googleAuth
}

// getMicrosoftAuth returns the current Microsoft OAuth config under a read lock.
func (h *Handler) getMicrosoftAuth() *oauth2.Config {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	return h.microsoftAuth
}

// SetWebhookSvc replaces the default ephemeral-key webhook service with one
// backed by the configured encryption key.
func (h *Handler) SetWebhookSvc(svc *webhook.Service) {
	h.webhookSvc = svc
}

// isEmailEnabled reports whether a real SMTP sender is configured.
func (h *Handler) isEmailEnabled() bool {
	if h.live != nil {
		return h.live.IsEnabled()
	}
	// Fallback for tests that inject a direct stub mailer (not wrapped in Live),
	// and the multi-tenant path, where each workspace has its own.
	_, isNoop := h.getMailer().(*mailer.Noop)
	return !isNoop
}

func (h *Handler) writeError(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, map[string]string{"error": msg})
}

// requireAdmin resolves the authenticated caller and writes a 403 (returning
// ok=false, which the caller must check and return on) unless they're an admin.
// Every settings handler (branding/tracking/google/email/llm/storage/stripe/zoom/
// livekit/notetaker) must call this first, not repeat the check inline — copy-paste
// was exactly how GetEmailSettings shipped without it (a real, live gap, not a
// hypothetical one) while its sibling PATCH/test-connection handlers had it.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) (AuthUser, bool) {
	user, ok := userFromContext(r.Context())
	if !ok || !user.IsAdmin {
		h.writeError(w, http.StatusForbidden, "admin access required")
		return AuthUser{}, false
	}
	return user, true
}
