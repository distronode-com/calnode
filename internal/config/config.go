package config

import (
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port           string
	DatabaseURL    string
	EncryptionKey  string // platform secret (KEK input); vault derives the real AES key
	RecoverySecret string // CALNODE_RECOVERY_SECRET — escrow key stored in keystore
	BaseURL        string // identity host: OAuth callbacks, admin UI, team invites
	PublicBaseURL  string // booker-facing host: booking links, emails; defaults to BaseURL
	LogLevel       slog.Level

	// MultiTenant serves many isolated workspaces from one process: the tenant of
	// a request is resolved from its Host or from the credential it carries, and
	// PostgreSQL row-level security — not the query author — is what keeps one
	// workspace out of another's rows.
	//
	// Unset is the default and changes nothing: one workspace with the literal id
	// "default", row-level security never enabled, SQLite still supported.
	MultiTenant bool

	// DatabaseAdminURL is the PLATFORM role's DSN: the owner of the schema, with
	// BYPASSRLS, which runs migrations, the worker's cross-tenant claim loop, the
	// reconciler's workspace enumeration and the platform API. DatabaseURL is then
	// the APPLICATION role, which must be NOBYPASSRLS and must not own the tables —
	// that is the whole of the isolation guarantee, since a role that owns a table
	// or bypasses RLS is not constrained by its policy.
	//
	// Required when MultiTenant is set, ignored otherwise: in single-tenant mode
	// both handles are the same one.
	DatabaseAdminURL string

	// PlatformToken authenticates the platform API (workspace provisioning,
	// export/import, erasure) on the identity host. Empty means the platform API
	// is not mounted at all — a 404, not a 401, so an instance that does not
	// provision workspaces does not advertise that it could.
	PlatformToken string

	// Email / SMTP
	SMTPHost      string
	SMTPPort      string
	SMTPUser      string
	SMTPPass      string
	SMTPTLS       bool // implicit TLS (port 465)
	SMTPStartTLS  bool // STARTTLS (port 587)
	EmailFrom     string
	EmailFromName string

	// Google OAuth (calendar + sign-in)
	GoogleClientID     string
	GoogleClientSecret string

	// Microsoft 365 / Outlook (calendar) — env-only; tenant defaults to "common".
	MicrosoftClientID     string
	MicrosoftClientSecret string
	MicrosoftTenant       string

	// Zoom OAuth app (per-host meeting links) — DB settings take priority over these.
	ZoomClientID     string
	ZoomClientSecret string

	// CookieSecure sets the Secure flag on session cookies. Defaults to true
	// when BASE_URL starts with https://, but can be overridden explicitly via
	// COOKIE_SECURE=false for HTTPS-terminated-at-proxy setups where the binary
	// itself listens on plain HTTP and BASE_URL is set correctly.
	CookieSecure bool

	// EmbedAllowedOrigins lists the origins permitted to call the public booking
	// endpoints cross-origin (for the embeddable widget). Empty ⇒ allow any origin
	// (`*`). CORS only constrains browsers; it is not an access-control boundary —
	// the public endpoints are rate-limited regardless. Comma-separated.
	EmbedAllowedOrigins []string

	// DemoMode turns this instance into a public, self-resetting demo: seeds sample
	// data on every boot (there's no persistent volume, so every boot is a fresh DB),
	// disables calendar/Zoom connect, serves a disallow-all robots.txt, and exposes
	// demo_mode/next_reset_at via the public auth-status endpoint so the frontend can
	// show the reset banner. Never set this on a real deployment.
	DemoMode bool
	// DemoResetInterval is how often DemoMode wipes and re-seeds the DB. Configurable
	// (not hardcoded to 30m) so local verification doesn't require waiting half an hour.
	DemoResetInterval time.Duration

	// DBMaxOpenConns / DBMaxIdleConns size the PostgreSQL connection pool.
	// PostgreSQL's own max_connections is the thing they have to fit inside, and
	// that is a property of the server a self-hoster runs, not of Calnode — a
	// small instance behind a PgBouncer wants a different number from one talking
	// to a 200-connection server directly.
	//
	// They do NOT apply to SQLite, which is pinned at 1/1 in internal/db because
	// the single writer connection is a correctness guarantee (ARCHITECTURE §17),
	// not a tuning choice.
	DBMaxOpenConns int
	DBMaxIdleConns int
}

func Load() *Config {
	cfg := &Config{
		Port:        getEnv("PORT", "3000"),
		DatabaseURL: getEnv("DATABASE_URL", "sqlite://./data/calnode.db"),
		BaseURL:     getEnv("BASE_URL", "http://localhost:3000"),

		SMTPHost:      getEnv("EMAIL_SMTP_HOST", ""),
		SMTPPort:      getEnv("EMAIL_SMTP_PORT", "587"),
		SMTPUser:      getEnv("EMAIL_SMTP_USER", ""),
		SMTPPass:      getEnv("EMAIL_SMTP_PASS", ""),
		SMTPTLS:       getBool("EMAIL_SMTP_TLS", false),
		SMTPStartTLS:  getBool("EMAIL_SMTP_STARTTLS", false),
		EmailFrom:     getEnv("EMAIL_FROM_ADDRESS", "bookings@localhost"),
		EmailFromName: getEnv("EMAIL_FROM_NAME", "Calnode"),

		GoogleClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),

		MicrosoftClientID:     getEnv("MICROSOFT_CLIENT_ID", ""),
		MicrosoftClientSecret: getEnv("MICROSOFT_CLIENT_SECRET", ""),
		MicrosoftTenant:       getEnv("MICROSOFT_TENANT", "common"),

		ZoomClientID:     getEnv("ZOOM_CLIENT_ID", ""),
		ZoomClientSecret: getEnv("ZOOM_CLIENT_SECRET", ""),

		EmbedAllowedOrigins: splitCSV(getEnv("EMBED_ALLOWED_ORIGINS", "")),
	}

	cfg.EncryptionKey = os.Getenv("CALNODE_ENCRYPTION_KEY")
	cfg.RecoverySecret = os.Getenv("CALNODE_RECOVERY_SECRET")
	// PUBLIC_BASE_URL overrides the booker-facing host (custom/vanity domain).
	// Unset → inherits BASE_URL, so single-domain deploys need only set BASE_URL.
	cfg.PublicBaseURL = getEnv("PUBLIC_BASE_URL", cfg.BaseURL)
	cfg.LogLevel = parseLogLevel(getEnv("LOG_LEVEL", "info"))
	cfg.CookieSecure = getBool("COOKIE_SECURE", strings.HasPrefix(cfg.BaseURL, "https://"))
	cfg.DemoMode = getBool("DEMO_MODE", false)
	cfg.DemoResetInterval = getDuration("DEMO_RESET_INTERVAL", 30*time.Minute)
	cfg.DBMaxOpenConns, cfg.DBMaxIdleConns = PoolFromEnv()

	cfg.MultiTenant = getBool("MULTI_TENANT", false)
	cfg.DatabaseAdminURL = os.Getenv("DATABASE_ADMIN_URL")
	cfg.PlatformToken = os.Getenv("CALNODE_PLATFORM_TOKEN")

	return cfg
}

// Validate reports a configuration that cannot work, rather than letting it fail
// later as something that reads like a bug in the code.
//
// It is separate from Load because Load has no error return and every optional
// knob in it deliberately falls back on a typo rather than refusing to boot
// (see PoolFromEnv). These are not typos: each one is a combination whose only
// possible outcome is silent data loss or silent cross-tenant exposure.
func (c *Config) Validate() error {
	if !c.MultiTenant {
		return nil
	}

	// SQLite has no row-level security, so there is nothing to enforce isolation
	// with. Refusing is the only honest answer: the alternative is an instance
	// that looks multi-tenant and separates nothing.
	if !isPostgresURL(c.DatabaseURL) {
		return errors.New("MULTI_TENANT requires a postgres:// DATABASE_URL — " +
			"tenant isolation is PostgreSQL row-level security, which SQLite has no equivalent of")
	}
	if c.DatabaseAdminURL == "" {
		return errors.New("MULTI_TENANT requires DATABASE_ADMIN_URL — " +
			"the platform role that owns the schema and runs migrations, distinct from the application role in DATABASE_URL")
	}
	if !isPostgresURL(c.DatabaseAdminURL) {
		return errors.New("DATABASE_ADMIN_URL must be a postgres:// DSN")
	}
	// Same DSN means one role, which means the application role owns the schema
	// and every policy is inert against it. This is the misconfiguration that
	// would be hardest to notice, because everything works — including reading
	// other tenants' rows.
	if c.DatabaseAdminURL == c.DatabaseURL {
		return errors.New("DATABASE_ADMIN_URL must differ from DATABASE_URL — " +
			"the application role must not own the tables or its row-level-security policies do not apply to it")
	}
	// Demo mode wipes and re-seeds the whole database every DemoResetInterval.
	// Against a multi-tenant database that is every tenant's data.
	if c.DemoMode {
		return errors.New("DEMO_MODE and MULTI_TENANT are mutually exclusive — " +
			"demo mode periodically wipes the entire database")
	}
	return nil
}

// isPostgresURL mirrors the classification internal/db does on the same string.
// Duplicated rather than imported because db imports config, and one three-line
// prefix check is a smaller price than a cycle.
func isPostgresURL(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "postgres://") || strings.HasPrefix(l, "postgresql://")
}

// Pool defaults. Deliberately modest: one Calnode instance is one small process,
// and a self-hoster's PostgreSQL is usually sized to match.
const (
	DefaultDBMaxOpenConns = 10
	DefaultDBMaxIdleConns = 5
)

// PoolFromEnv reads DB_MAX_OPEN_CONNS and DB_MAX_IDLE_CONNS.
//
// It is exported separately from Load because internal/db calls it directly:
// OpenDB has to know the pool size, every entry point that opens a database
// would otherwise have to remember to pass it, and forgetting would silently
// give that entry point the defaults. config imports nothing from the app, so
// db → config is not a cycle.
//
// Validation, rather than handing database/sql whatever the environment said:
//
//   - unset, unparsable or not positive → the default, with a warning. This
//     matches getBool/getDuration above, which also fall back rather than
//     failing a boot over a typo in an optional knob.
//   - idle > open → idle is clamped to open. database/sql silently reduces the
//     idle limit to the open limit in that case, so the pair is the honest
//     description of what the pool will do.
func PoolFromEnv() (maxOpen, maxIdle int) {
	maxOpen = getPositiveInt("DB_MAX_OPEN_CONNS", DefaultDBMaxOpenConns)
	maxIdle = getPositiveInt("DB_MAX_IDLE_CONNS", DefaultDBMaxIdleConns)
	if maxIdle > maxOpen {
		slog.Warn("DB_MAX_IDLE_CONNS exceeds DB_MAX_OPEN_CONNS; clamping",
			"idle", maxIdle, "open", maxOpen)
		maxIdle = maxOpen
	}
	return maxOpen, maxIdle
}

func getPositiveInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("ignoring unparsable integer environment variable", "key", key, "value", v, "default", def)
		return def
	}
	if n < 1 {
		slog.Warn("ignoring non-positive environment variable", "key", key, "value", n, "default", def)
		return def
	}
	return n
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitCSV parses a comma-separated value into trimmed, non-empty entries.
// Returns nil for an empty string.
func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
