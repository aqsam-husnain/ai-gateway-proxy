// Command ai-cli-proxy-go is a Go/Gin port of the Node.js AI API proxy.
//
// It exposes a unified set of endpoints that transparently proxy requests to
// multiple upstream AI providers (Gemini, OpenAI, Claude, Antigravity) while
// presenting a consistent API surface to downstream clients.
//
// # Endpoints
//
//	POST   /v1/chat/completions                      – OpenAI chat format
//	POST   /v1/messages                              – Claude messages format
//	POST   /v1beta/models/:model/generateContent      – Gemini generate format
//	POST   /v1beta/models/:model/streamGenerateContent – Gemini stream format
//	GET    /v1/models                                – OpenAI model list
//	GET    /v1beta/models                            – Gemini model list
//	POST   /v1/responses                             – OpenAI Responses API format
//	GET    /api/tags                                 – Ollama tags
//	POST   /api/chat                                 – Ollama chat
//	POST   /api/generate                             – Ollama generate
//	GET    /api/version                              – Ollama version
//	GET    /health                                   – Liveness probe
//	GET    /provider_health                          – Per-provider health status
//
// # CLI flags
//
//	--port                        <number>
//	--host                        <address>
//	--api-key                     <key>
//	--model-provider              <provider[,provider...]>
//	--openai-api-key              <key>
//	--openai-base-url             <url>
//	--claude-api-key              <key>
//	--claude-base-url             <url>
//	--gemini-base-url             <url>
//	--gemini-oauth-creds-base64   <base64>
//	--gemini-oauth-creds-file     <path>
//	--project-id                  <id>
//	--system-prompt-file          <path>
//	--system-prompt-mode          overwrite|append
//	--log-prompts                 console|file|none
//	--prompt-log-base-name        <name>
//	--provider-pools-file         <path>
//	--cron-near-minutes           <number>
//	--cron-refresh-token          true|false
//	--max-error-count             <number>
//	--health-check-interval-hours <float>
//	--request-max-retries         <number>
//	--request-base-delay          <number>
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ai-cli-proxy-go/config"
	"ai-cli-proxy-go/handlers"
	"ai-cli-proxy-go/providers"
	"ai-cli-proxy-go/router"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

func main() {
	// Load .env file if present (non-fatal if absent).
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("[Startup] .env file not loaded: %v", err)
	}

	// Parse CLI args and build the configuration.
	cfg, err := config.LoadConfig("configs/config.json", os.Args[1:])
	if err != nil {
		log.Fatalf("[Startup] Failed to load configuration: %v", err)
	}

	// Optional: initialise Redis.  Failure is non-fatal; the proxy continues
	// without caching.
	redisClient := initRedis(cfg.RedisURL)
	if redisClient != nil {
		log.Println("[Startup] Redis connection established.")
	} else {
		log.Println("[Startup] Redis not available – running without cache.")
	}

	// Set Gin mode based on environment.
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Resolve static directory to an absolute path so file serving works
	// regardless of how the binary is invoked (go run, built binary, etc.).
	staticDir, err := resolveStaticDir()
	if err != nil {
		log.Printf("[Startup] Warning: could not resolve static dir: %v — UI may not serve correctly", err)
		staticDir = "static"
	}

	// Build the provider pool manager.
	poolManager := providers.NewProviderPoolManager(cfg.ProviderPools, cfg)

	// Build the Gin router with all routes registered.
	r := router.SetupRouter(cfg, poolManager, staticDir)

	// Hook Go's log package into the SSE broadcaster so every log line
	// appears in the dashboard's real-time log view.
	handlers.InitLogBroadcaster()

	addr := cfg.Addr()
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      5 * time.Minute, // generous for streaming responses
		IdleTimeout:       120 * time.Second,
	}

	// Print startup banner.
	printBanner(cfg)

	// Run the server in a goroutine so the main goroutine can listen for signals.
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("[Server] Listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// Block until a signal or a fatal server error arrives.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		log.Printf("[Server] Received signal %s — shutting down gracefully…", sig)
	case err := <-serverErr:
		log.Printf("[Server] Fatal error: %v — shutting down…", err)
	}

	// Give in-flight requests up to 30 seconds to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if shutdownErr := srv.Shutdown(ctx); shutdownErr != nil {
		log.Printf("[Server] Graceful shutdown incomplete: %v", shutdownErr)
		os.Exit(1)
	}

	// Close Redis connection if we have one.
	if redisClient != nil {
		if closeErr := redisClient.Close(); closeErr != nil {
			log.Printf("[Startup] Redis close error: %v", closeErr)
		}
	}

	log.Println("[Server] Shutdown complete.")
}

// resolveStaticDir returns an absolute path to the static/ directory.
// It first tries relative to the working directory, then relative to the
// executable location (for installed binaries).
func resolveStaticDir() (string, error) {
	// First: check if static/ exists relative to the current working directory.
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	cwdStatic := filepath.Join(cwd, "static")
	if info, statErr := os.Stat(cwdStatic); statErr == nil && info.IsDir() {
		return cwdStatic, nil
	}

	// Second: check relative to the executable (for built binaries run from another dir).
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(cwd, "static"), nil
	}
	exeDir := filepath.Dir(exe)
	exeStatic := filepath.Join(exeDir, "static")
	if info, statErr := os.Stat(exeStatic); statErr == nil && info.IsDir() {
		return exeStatic, nil
	}

	// Fall back to cwd-relative (will be correct when running via `go run` from project root).
	return cwdStatic, nil
}

// initRedis attempts to connect to Redis using the provided URL.
// Returns nil (without error) when the URL is empty or the server is
// unreachable, so the caller can continue without caching.
func initRedis(redisURL string) *redis.Client {
	if redisURL == "" {
		return nil
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Printf("[Redis] Could not parse REDIS_URL %q: %v", redisURL, err)
		return nil
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, pingErr := client.Ping(ctx).Result(); pingErr != nil {
		log.Printf("[Redis] Ping failed (%v) — proceeding without Redis.", pingErr)
		_ = client.Close()
		return nil
	}

	return client
}

// printBanner writes a startup summary to stdout, matching the JS original.
func printBanner(cfg *config.Config) {
	fmt.Println("--- Unified API Server Configuration ---")
	fmt.Printf("  Primary Model Provider : %s\n", cfg.ModelProvider)
	if len(cfg.DefaultModelProviders) > 1 {
		fmt.Printf("  Additional Providers   : %s\n",
			joinStrings(cfg.DefaultModelProviders[1:], ", "))
	}
	fmt.Printf("  System Prompt File     : %s\n", orDefault(cfg.SystemPromptFilePath, "None"))
	fmt.Printf("  System Prompt Mode     : %s\n", cfg.SystemPromptMode)
	fmt.Printf("  Host                   : %s\n", cfg.Host)
	fmt.Printf("  Port                   : %d\n", cfg.ServerPort)
	fmt.Printf("  Required API Key       : %s\n", cfg.RequiredAPIKey)
	fmt.Printf("  Prompt Logging         : %s\n", cfg.PromptLogMode)
	fmt.Println("------------------------------------------")
	fmt.Printf("\nUnified API Proxy running on http://%s\n", cfg.Addr())
	fmt.Println("Supported API formats:")
	fmt.Println("  * OpenAI-compatible : /v1/chat/completions, /v1/responses, /v1/models")
	fmt.Println("  * Gemini-compatible : /v1beta/models, /v1beta/models/{model}:generateContent")
	fmt.Println("  * Claude-compatible : /v1/messages")
	fmt.Println("  * Ollama-compatible : /api/tags, /api/chat, /api/generate, /api/version")
	fmt.Println("  * Health check      : /health, /provider_health")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func joinStrings(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
