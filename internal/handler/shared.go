package handler

import (
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/livekit"
	"github.com/calnode/calnode/internal/llm"
	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/stripe"
	"github.com/calnode/calnode/internal/zoom"
)

// shared is everything one process has exactly one of: the logger, the mailer,
// the hot-swappable integration clients and the mutexes guarding them, the
// configured hosts, and the demo-mode bookkeeping.
//
// It exists so Handler can be a cheap VALUE. A tenant-scoped request needs a
// handler whose db is bound to its workspace, and the only way to give 314
// methods a bound `h.db` without editing all of them is to hand them a receiver
// that differs in that field — so Handler is copied per request. A struct
// holding six sync.RWMutex cannot be copied (go vet copylocks, and rightly:
// copying a mutex copies its state), so the mutexes live behind this pointer,
// which every copy shares.
//
// Embedding it as *shared rather than naming it keeps `h.logger`, `h.mailer`,
// `h.livekitMu` and the rest compiling unchanged across the package.
type shared struct {
	logger *slog.Logger
	live   *mailer.Live // non-nil in production; nil in tests using a direct stub
	encKey [32]byte     // AES-256 key for encrypting secrets stored in the DB

	// The per-workspace runtime state (D7). Each is built lazily from THAT
	// workspace's server_settings row through the bound handle, and replaced when
	// that workspace saves its settings. One entry keyed "" in single-tenant mode.
	mailerCache  *tenantCache[mailer.Mailer]
	llmCache     *tenantCache[*llm.Client]
	zoomCache    *tenantCache[*zoom.Client]
	stripeCache  *tenantCache[*stripe.Client]
	livekitCache *tenantCache[*livekit.Client]

	// calBase is the PLATFORM-level provider registry: one Service holding the
	// instance's Google/Microsoft OAuth apps and the CalDAV client, per D7. It is
	// never used to read a tenant table directly — calCache holds the per-workspace
	// copies that Service.ForDB produces, and getCal is the only reader.
	calMu    sync.RWMutex
	calBase  *calendar.Service
	calCache *tenantCache[*calendar.Service]
	calNudge chan struct{} // buffered(1): wakes the calendar reconciler after a failed inline op

	baseURL       string
	publicBaseURL string
	dataDir       string

	authMu        sync.RWMutex
	googleAuth    *oauth2.Config
	microsoftAuth *oauth2.Config
	secureCookie  bool

	demoMode          bool // true on the public demo instance: disables calendar/Zoom connect
	demoResetInterval time.Duration
	demoMu            sync.RWMutex
	demoNextResetAt   time.Time

	// mcpServers caches one MCP server per workspace, keyed by workspace id ("" in
	// single-tenant mode). The tools close over their handler, so one shared
	// instance would run every tenant's tool calls on one workspace's handle.
	mcpMu      sync.RWMutex
	mcpServers map[string]*mcp.Server

	// multiTenant mirrors config.MultiTenant. It is what turns host and
	// credential resolution on; unset, every request runs on the default
	// workspace and the handler behaves exactly as it did before.
	multiTenant bool
}
