package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/anthropic"
	"github.com/mikeyaustin/jlp/internal/adapters/authelia"
	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	httpx "github.com/mikeyaustin/jlp/internal/adapters/http"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	"github.com/mikeyaustin/jlp/internal/adapters/staticauth"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	"github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	"github.com/mikeyaustin/jlp/internal/application/vocabulary"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/observability"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
)

// aiPricing is the USD-per-million-token rate card the observability
// decorator costs every AI call against. It lives here rather than in
// package config because it's not deployment configuration a operator
// tunes per environment — it's a fixed fact about what providers
// charge, reviewed and updated in code alongside the provider list
// itself.
func aiPricing() map[string]observability.ModelPricing {
	return map[string]observability.ModelPricing{
		"claude-sonnet-5": {InPerMTok: 3, OutPerMTok: 15},
		"fake-1":          {InPerMTok: 0, OutPerMTok: 0},
	}
}

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	switch cmd {
	case "serve":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		pool, err := postgres.NewPool(context.Background(), cfg.Database.URL)
		if err != nil {
			slog.Error("database", "err", err)
			os.Exit(1)
		}
		identities := postgres.NewIdentityRepository(pool)
		sessionsSvc := sessions.NewService(postgres.NewSessionRepository(pool))
		eventRepo := postgres.NewLearningEventRepository(pool)
		bus := inprocbus.New()
		recorder := learning.NewRecorder(eventRepo, bus)
		writingSvc := appwriting.NewService(postgres.NewDocumentRepository(pool), recorder)

		// grammarRepo is shared between the planner's JLPT-weight lookups
		// (below), the feedback pipeline's concept-tagging, and the
		// /grammar pages: one stateless repository instance wrapping the
		// same pool, not several separate constructions of the same thing.
		grammarRepo := postgres.NewGrammarRepository(pool)
		obsRepo := postgres.NewObservationRepository(pool)
		prioRepo := postgres.NewPriorityRepository(pool)
		// vocabRepo is constructed here (ahead of its usual spot further
		// down, next to vocabSvc) because the teaching planner below now
		// needs it too — see teachingPlanner's own comment.
		vocabRepo := postgres.NewVocabularyRepository(pool)
		// The heuristic teaching planner (Task 5, PRD §16) turns the
		// learner model's observations into the ranked, explainable
		// priority list feedback.Service.RequestFeedback reads back via
		// Top(5) to fill the Teacher prompt's RecentErrors — closing the
		// adapt loop. Task 7 (PRD §55/§17.5) added a second
		// responsibility, ActivationCandidates, which is why it now also
		// takes vocabRepo.
		teachingPlanner := planner.NewPlanner(obsRepo, eventRepo, grammarRepo, prioRepo, vocabRepo, time.Now)

		// The learner model (Task 4, PRD §13/§14/§44) reacts to every
		// correction.presented and grammar.concept.encountered event as
		// it's published — the same two event types Rebuild (see
		// cmd/jlp/rebuild.go) replays wholesale through the exact same
		// Updater.HandleEvent. Subscribe absorbs handler errors (see
		// learning.Recorder.Record's doc comment on publish absorption),
		// so a learner-model failure never fails the request that
		// produced the event. SetPlanner wires the planner in so every
		// observation change also recomputes priorities (see
		// consumers.go's SetPlanner doc comment).
		obsUpdater := learnermodel.NewUpdater(eventRepo, obsRepo, time.Now)
		obsUpdater.SetPlanner(teachingPlanner)
		bus.Subscribe(event.TypeCorrectionPresented, obsUpdater.HandleEvent)
		bus.Subscribe(event.TypeGrammarConceptEncountered, obsUpdater.HandleEvent)

		var innerGen ai.StructuredGenerator
		switch cfg.AI.Provider {
		case "fake":
			innerGen = fakeai.New()
		case "anthropic":
			innerGen = anthropic.New(cfg.AI.Anthropic)
		default:
			slog.Error("ai", "err", fmt.Sprintf("unknown ai provider %q", cfg.AI.Provider))
			os.Exit(1)
		}
		// Every AI call is observed, whichever provider is behind it: the
		// audit trail (latency, cost, success) must never depend on
		// remembering to wrap a specific adapter. The same repository
		// instance is read back by the /ai page (Task 15) below.
		aiRequestRepo := postgres.NewAIRequestRepository(pool)
		aiGen := observability.NewAIObserver(innerGen, aiRequestRepo, aiPricing())

		// The learner's personal vocabulary (Task 6, PRD §12): vocabSvc's
		// Ingest backs POST /api/v1/vocabulary/events and the /vocabulary
		// page's List (see httpx.Options.Vocabulary below); its
		// DetectProduction is wired into feedback.Service so every review
		// round also notices when a looked-up expression shows up,
		// produced, in the learner's own writing. vocabRepo itself was
		// constructed above, alongside teachingPlanner.
		vocabSvc := vocabulary.NewService(vocabRepo, recorder)

		teacherAgent := teacher.New(aiGen)
		feedbackSvc := feedback.NewService(
			postgres.NewSessionRepository(pool),
			postgres.NewDocumentRepository(pool),
			postgres.NewFeedbackRepository(pool),
			grammarRepo,
			prioRepo,
			teachingPlanner,
			vocabSvc,
			teacherAgent,
			recorder,
		)
		analyticsSvc := analytics.NewService(postgres.NewAnalyticsRepository(pool))
		aiRatingRepo := postgres.NewAIRatingRepository(pool)

		var authn auth.Authenticator
		switch cfg.Auth.Mode {
		case "static":
			authn = staticauth.New(cfg.Auth.Static.ID, cfg.Auth.Static.DisplayName)
		case "authelia":
			authn, err = authelia.New(cfg.Auth.TrustedProxies)
			if err != nil {
				slog.Error("auth", "err", err)
				os.Exit(1)
			}
		default:
			slog.Error("auth", "err", fmt.Sprintf("unknown auth mode %q", cfg.Auth.Mode))
			os.Exit(1)
		}
		srv := httpx.NewServer(httpx.Options{
			Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
			Auth:         authn,
			Identities:   identities,
			Sessions:     sessionsSvc,
			Writing:      writingSvc,
			Events:       eventRepo,
			Feedback:     feedbackSvc,
			Analytics:    analyticsSvc,
			AI:           aiGen,
			AIRequests:   aiRequestRepo,
			AIRatings:    aiRatingRepo,
			Grammar:      grammarRepo,
			Priorities:   prioRepo,
			Observations: obsRepo,
			Vocabulary:   vocabSvc,
		})
		slog.Info("listening", "port", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
	case "migrate":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := postgres.Migrate(context.Background(), cfg.Database.URL); err != nil {
			slog.Error("migrate", "err", err)
			os.Exit(1)
		}
		slog.Info("migrations applied")
	case "seed":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := runSeed(context.Background(), cfg); err != nil {
			slog.Error("seed", "err", err)
			os.Exit(1)
		}
		slog.Info("seed complete")
	case "rebuild-model":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := runRebuildModel(context.Background(), cfg); err != nil {
			slog.Error("rebuild-model", "err", err)
			os.Exit(1)
		}
		slog.Info("rebuild-model complete")
	default:
		slog.Error("unknown command", "cmd", cmd)
		os.Exit(2)
	}
}
