package main

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	apiRCS "github.com/zagnu-labs/rcs-messenger/internal/api/rcs"
	"github.com/zagnu-labs/rcs-messenger/internal/db"
	"github.com/zagnu-labs/rcs-messenger/internal/logg"
	"github.com/zagnu-labs/rcs-messenger/internal/providers/rcsandroid"
	"github.com/zagnu-labs/rcs-messenger/internal/providers/rcsdirect"
	"github.com/zagnu-labs/rcs-messenger/internal/providers/rcsphone"
	"github.com/zagnu-labs/rcs-messenger/internal/rcs"
)

const maxSendWait = 20 * time.Second

type config struct {
	Addr           string
	DatabaseURL    string
	Debug          bool
	Enabled        bool
	DryRun         bool
	WaitSeconds    int
	MonitorSeconds int
	Allowlist      []string

	// Transport picks how messages reach the phone: "web" (Messages for web,
	// paired once) or "android" (drive the Google Messages app over adb).
	Transport string
	Android   androidConfig
}

type androidConfig struct {
	ADB          string
	Serial       string
	Addr         string
	PIN          string
	Selectors    string
	MinGap       time.Duration
	Jitter       time.Duration
	SettleBudget time.Duration
}

func loadConfig() config {
	return config{
		Addr:           env("ADDR", "127.0.0.1:8080"),
		DatabaseURL:    env("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/rcs_messenger?sslmode=disable"),
		Debug:          envBool("DEBUG", false),
		Enabled:        envBool("RCS_ENABLED", true),
		DryRun:         envBool("RCS_DRY_RUN", false),
		WaitSeconds:    envInt("RCS_WAIT_SECONDS", 20),
		MonitorSeconds: envInt("RCS_MONITOR_SECONDS", 60),
		Allowlist:      envList("RCS_ALLOWLIST_PHONES"),
		Transport:      strings.ToLower(env("RCS_TRANSPORT", "web")),
		Android: androidConfig{
			ADB:          env("RCS_ANDROID_ADB", "adb"),
			Serial:       env("RCS_ANDROID_SERIAL", ""),
			Addr:         env("RCS_ANDROID_ADDR", ""),
			PIN:          env("RCS_ANDROID_PIN", ""),
			Selectors:    env("RCS_ANDROID_SELECTORS", ""),
			MinGap:       time.Duration(envInt("RCS_ANDROID_MIN_GAP_SECONDS", 45)) * time.Second,
			Jitter:       time.Duration(envInt("RCS_ANDROID_JITTER_SECONDS", 15)) * time.Second,
			SettleBudget: time.Duration(envInt("RCS_ANDROID_SETTLE_SECONDS", 60)) * time.Second,
		},
	}
}

func main() {
	cfg := loadConfig()
	logg.Setup(cfg.Debug)

	// Encryption used to be optional here. It no longer is, and an old
	// deployment that still sets this to false must not start silently
	// believing it can send unconfirmed messages.
	if v, err := strconv.ParseBool(env("RCS_REQUIRE_E2EE", "")); err == nil && !v {
		logg.Fatal().Msg("RCS_REQUIRE_E2EE=false is no longer supported: messages are only ever sent to chats confirmed end-to-end encrypted. Remove the variable.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		logg.Fatal().Err(err).Msg("Failed to open the database")
	}
	defer conn.Close()

	stack := setupRCS(cfg, conn)
	if cfg.Enabled {
		if stack.runner != nil {
			go stack.runner.Start(ctx, nil)
		}
		go rcs.NewMonitor(stack.service, nil, time.Duration(cfg.MonitorSeconds)*time.Second).Start(ctx, nil)
	}

	srv := setupRouter(cfg, stack)
	go func() {
		logg.Info().Str("addr", cfg.Addr).Msg("Starting HTTP server")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logg.Fatal().Err(err).Msg("Failed to start HTTP server")
		}
	}()

	<-ctx.Done()
	logg.Info().Msg("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logg.Error().Err(err).Msg("HTTP server forced to shutdown")
	}
}

// rcsSender is what both transports provide to the service.
type rcsSender interface {
	Enabled() bool
	Send(ctx context.Context, to, message string, opts rcsdirect.SendOptions) (*rcsdirect.SendResult, error)
	Check(ctx context.Context, to string) (*rcsdirect.Check, error)
	Health(ctx context.Context) (*rcsdirect.Health, error)
	Thread(ctx context.Context, to string, limit int) (*rcsdirect.Thread, error)
}

type rcsStack struct {
	service *rcs.Service
	runner  *rcsphone.Runner
	pairing *rcsphone.Pairing
}

func setupRCS(cfg config, conn *sql.DB) *rcsStack {
	var (
		sender  rcsSender
		runner  *rcsphone.Runner
		pairing *rcsphone.Pairing
	)
	switch cfg.Transport {
	case "android":
		sel, err := rcsandroid.LoadSelectors(cfg.Android.Selectors)
		if err != nil {
			logg.Fatal().Err(err).Msg("Could not load the Android selectors")
		}
		a, err := rcsandroid.New(rcsandroid.Config{
			Runner:       rcsandroid.ExecRunner{Bin: cfg.Android.ADB, Serial: cfg.Android.Serial},
			Selectors:    sel,
			PIN:          cfg.Android.PIN,
			Addr:         cfg.Android.Addr,
			MinGap:       cfg.Android.MinGap,
			Jitter:       cfg.Android.Jitter,
			SettleBudget: cfg.Android.SettleBudget,
		})
		if err != nil {
			logg.Fatal().Err(err).Msg("Could not set up the Android sender")
		}
		sender = a
		logg.Info().Msg("RCS transport: Android phone over adb (Google Messages app)")
	case "web", "":
		store := rcsphone.NewStore(conn)
		web := rcsphone.NewSender()
		runner = rcsphone.NewRunner(store, web, logg.Logger())
		pairing = rcsphone.NewPairing(store, runner, logg.Logger())
		sender = web
	default:
		logg.Fatal().Str("RCS_TRANSPORT", cfg.Transport).Msg(`RCS_TRANSPORT must be "web" or "android"`)
	}

	wait := time.Duration(cfg.WaitSeconds) * time.Second
	if wait <= 0 {
		wait = 20 * time.Second
	}
	if wait > maxSendWait {
		logg.Warn().
			Dur("configured", wait).
			Dur("capped", maxSendWait).
			Msg("RCS_WAIT_SECONDS exceeds the HTTP write timeout, capping")
		wait = maxSendWait
	}

	svc := rcs.NewService(sender, rcs.Config{
		Enabled:   cfg.Enabled,
		Wait:      wait,
		DryRun:    cfg.DryRun,
		Allowlist: cfg.Allowlist,
	})
	// One store holds send outcomes and opt-outs: a STOP reply is recorded
	// here and refuses every later send to that number.
	results := rcs.NewResultStore(conn)
	svc.SetResults(results).SetBlocker(results)

	if runner != nil {
		runner.OnInbound(func(from, text, messageID string) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := svc.HandleInbound(ctx, rcs.Inbound{From: from, Text: text, MessageID: messageID}); err != nil {
				logg.Error().Err(err).Str("from", from).Msg("RCS inbound could not be handled")
			}
		})
	}

	switch {
	case !cfg.Enabled:
		logg.Info().Msg("RCS sending disabled")
	case cfg.DryRun:
		logg.Info().Msg("RCS enabled in dry-run, nothing reaches the phone")
	default:
		logg.Info().
			Int("allowlisted", len(cfg.Allowlist)).
			Msg("RCS sending enabled")
	}
	return &rcsStack{service: svc, runner: runner, pairing: pairing}
}

func setupRouter(cfg config, stack *rcsStack) *http.Server {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	engine.Use(gin.Recovery())

	e := apiRCS.NewEndpoint(stack.service)
	if stack.pairing != nil { // a nil *Pairing inside the interface would not read as nil
		e.SetPairing(stack.pairing)
	}
	r := engine.Group("/internal/rcs")

	r.GET("/health", e.Health)
	r.POST("/check", e.Check)
	r.POST("/send", e.Send)
	r.GET("/thread", e.Thread)
	r.POST("/inbound", e.Inbound)
	r.POST("/nudge", e.Nudge)
	r.GET("/results", e.Results)

	r.GET("/pair", e.PairPage)
	r.POST("/pair/start", e.PairStart)
	r.GET("/pair/qr", e.PairQR)
	r.GET("/pair/status", e.PairStatus)
	r.POST("/pair/cancel", e.PairCancel)
	r.POST("/pair/cookies", e.PairCookies)
	r.POST("/pair/refresh", e.PairRefresh)

	return &http.Server{
		Addr:         cfg.Addr,
		Handler:      engine,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: writeTimeout(cfg),
		IdleTimeout:  60 * time.Second,
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return fallback
}

func envList(key string) []string {
	var out []string
	for _, part := range strings.Split(env(key, ""), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// writeTimeout is how long a request may take to answer. A send through the
// phone's screen waits for a new chat to settle, for the spacing between
// sends, and for the delivery status, so it can run for minutes.
func writeTimeout(cfg config) time.Duration {
	if cfg.Transport == "android" {
		return cfg.Android.SettleBudget + cfg.Android.MinGap + cfg.Android.Jitter + 3*time.Minute
	}
	return 30 * time.Second
}
