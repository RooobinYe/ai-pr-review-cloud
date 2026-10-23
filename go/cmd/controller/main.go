// Command controller drives review runs through their state machine: it claims
// queued runs, creates runner Jobs, receives results, and performs every GitHub
// write. See docs/architecture.md §6.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/controller"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/kube"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/litellm"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/metrics"
	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("controller stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Serve the probes right away: /healthz (liveness) passes while the
	// database is still starting, and /readyz (readiness) only once the runner
	// API is registered, so the Service never routes a result to a controller
	// that would answer 404.
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	// CONNECT requests carry no path for the mux to route on; they are fetch's
	// egress to GitHub once the controller is up.
	var egress atomic.Pointer[controller.Controller]
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			mux.ServeHTTP(w, r)
		} else if ctl := egress.Load(); ctl != nil {
			ctl.EgressProxy(w, r)
		} else {
			http.Error(w, "controller is not ready", http.StatusServiceUnavailable)
		}
	})
	srv := &http.Server{Addr: envOr("LISTEN_ADDR", ":8080"), Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		log.Info("controller listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	db, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(ctx, log, db); err != nil {
		return err
	}
	// Metrics come from the database, so they work without a GitHub App too.
	mux.Handle("GET /metrics", metrics.Handler(db.Stats, log))
	ctl, err := newController(ctx, log, db)
	if err != nil {
		return err
	}
	if ctl != nil {
		ctl.Routes(mux)
		egress.Store(ctl)
		go ctl.Run(ctx, 5*time.Second)
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				ctl.CleanPreviews(ctx)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
		redeliver, err := time.ParseDuration(envOr("REDELIVER_INTERVAL", "5m"))
		if err != nil {
			return fmt.Errorf("REDELIVER_INTERVAL: %w", err)
		}
		if redeliver > 0 {
			go ctl.RedeliverEvery(ctx, redeliver)
		}
	} else {
		log.Warn("GITHUB_APP_ID is not set; runs stay queued until a GitHub App is configured")
	}
	ready.Store(true)

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		return err
	}
}

// migrate applies the schema, waiting for the database to come up.
func migrate(ctx context.Context, log *slog.Logger, db *store.Store) error {
	for {
		err := db.Migrate(ctx)
		if err == nil {
			log.Info("database migrated")
			return nil
		}
		log.Warn("migrate database; retrying", "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(3 * time.Second):
		}
	}
}

// newController wires the controller from the environment. It returns nil
// when no GitHub App is configured, so the stack can start without one.
func newController(ctx context.Context, log *slog.Logger, db *store.Store) (*controller.Controller, error) {
	appID := os.Getenv("GITHUB_APP_ID")
	if appID == "" {
		return nil, nil
	}
	app, err := githubapp.NewApp(appID, []byte(os.Getenv("GITHUB_APP_PRIVATE_KEY")), os.Getenv("GITHUB_API_URL"))
	if err != nil {
		return nil, err
	}
	slug, err := app.Slug(ctx)
	if err != nil {
		return nil, fmt.Errorf("look up GitHub App: %w", err)
	}
	k, err := kube.InCluster(os.Getenv("POD_NAMESPACE"))
	if err != nil {
		return nil, err
	}

	maxRunning, err := strconv.Atoi(envOr("MAX_RUNNING", "4"))
	if err != nil {
		return nil, fmt.Errorf("MAX_RUNNING: %w", err)
	}
	maxAttempts, err := strconv.Atoi(envOr("MAX_ATTEMPTS", "3"))
	if err != nil {
		return nil, fmt.Errorf("MAX_ATTEMPTS: %w", err)
	}
	deadline, err := strconv.ParseInt(envOr("RUNNER_DEADLINE_SECONDS", "1800"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("RUNNER_DEADLINE_SECONDS: %w", err)
	}
	resources, err := controller.ParseResources(os.Getenv("RUNNER_RESOURCES"))
	if err != nil {
		return nil, err
	}
	llmEnv := map[string]string{}
	for _, name := range []string{"LLM_PROVIDER", "LLM_MODEL", "LLM_BASE_URL", "LLM_THINKING"} {
		llmEnv[name] = os.Getenv(name)
	}

	keys := &litellm.Client{BaseURL: os.Getenv("LITELLM_URL"), MasterKey: os.Getenv("LITELLM_MASTER_KEY")}
	bot := slug + "[bot]"
	cfg := controller.Config{
		MaxRunning:  maxRunning,
		MaxAttempts: maxAttempts,
		BotLogin:    bot,
		EgressHosts: strings.Split(envOr("FETCH_EGRESS_HOSTS", "github.com:443,api.github.com:443"), ","),
		Job: controller.JobConfig{
			FetchImage:            os.Getenv("FETCH_IMAGE"),
			RunnerImage:           os.Getenv("RUNNER_IMAGE"),
			ImagePullPolicy:       envOr("IMAGE_PULL_POLICY", "IfNotPresent"),
			BotLogin:              bot,
			ControllerURL:         os.Getenv("CONTROLLER_URL"),
			LLMEnv:                llmEnv,
			Language:              envOr("REVIEW_LANGUAGE", "English"),
			ActiveDeadlineSeconds: deadline,
			Resources:             resources,
		},
	}
	for name, v := range map[string]string{"FETCH_IMAGE": cfg.Job.FetchImage, "RUNNER_IMAGE": cfg.Job.RunnerImage,
		"CONTROLLER_URL": cfg.Job.ControllerURL, "POD_NAMESPACE": k.Namespace,
		"LITELLM_URL": keys.BaseURL, "LITELLM_MASTER_KEY": keys.MasterKey, "LLM_BASE_URL": llmEnv["LLM_BASE_URL"]} {
		if v == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	if os.Getenv("PREVIEW_ENABLED") == "true" {
		repositories := map[string]string{}
		if err := json.Unmarshal([]byte(os.Getenv("PREVIEW_IMAGE_REPOSITORIES")), &repositories); err != nil {
			return nil, fmt.Errorf("PREVIEW_IMAGE_REPOSITORIES must be a JSON object: %w", err)
		}
		cfg.Preview = &controller.PreviewConfig{Kube: k, ImageRepositories: repositories}
	}
	log.Info("GitHub App ready", "bot", cfg.BotLogin, "max_running", maxRunning, "max_attempts", maxAttempts)
	return controller.New(cfg, db, controller.AppGitHub{App: app}, k, keys, log), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
