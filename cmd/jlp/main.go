package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/a2a"
	agycliadapter "github.com/mikeyaustin/jlp/internal/adapters/agycli"
	"github.com/mikeyaustin/jlp/internal/adapters/ankiconnect"
	anthropicadapter "github.com/mikeyaustin/jlp/internal/adapters/anthropic"
	"github.com/mikeyaustin/jlp/internal/adapters/authelia"
	geminiadapter "github.com/mikeyaustin/jlp/internal/adapters/gemini"
	httpx "github.com/mikeyaustin/jlp/internal/adapters/http"
	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	adaptermqtt "github.com/mikeyaustin/jlp/internal/adapters/mqtt"
	oidcadapter "github.com/mikeyaustin/jlp/internal/adapters/oidc"
	ollamaadapter "github.com/mikeyaustin/jlp/internal/adapters/ollama"
	"github.com/mikeyaustin/jlp/internal/adapters/postgres"
	signaladapter "github.com/mikeyaustin/jlp/internal/adapters/signal"
	slackadapter "github.com/mikeyaustin/jlp/internal/adapters/slack"
	smtpadapter "github.com/mikeyaustin/jlp/internal/adapters/smtp"
	"github.com/mikeyaustin/jlp/internal/adapters/staticauth"
	whisperadapter "github.com/mikeyaustin/jlp/internal/adapters/whisper"
	agentanki "github.com/mikeyaustin/jlp/internal/agent/anki"
	agentconversation "github.com/mikeyaustin/jlp/internal/agent/conversation"
	"github.com/mikeyaustin/jlp/internal/agent/drill"
	agentlesson "github.com/mikeyaustin/jlp/internal/agent/lesson"
	agentsummary "github.com/mikeyaustin/jlp/internal/agent/summary"
	"github.com/mikeyaustin/jlp/internal/agent/teacher"
	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	appchannel "github.com/mikeyaustin/jlp/internal/application/channel"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	"github.com/mikeyaustin/jlp/internal/application/feedback"
	"github.com/mikeyaustin/jlp/internal/application/learnermodel"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	appoutcomes "github.com/mikeyaustin/jlp/internal/application/outcomes"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/application/practice"
	appretrieval "github.com/mikeyaustin/jlp/internal/application/retrieval"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appsettings "github.com/mikeyaustin/jlp/internal/application/settings"
	appspeech "github.com/mikeyaustin/jlp/internal/application/speech"
	appsummary "github.com/mikeyaustin/jlp/internal/application/summary"
	"github.com/mikeyaustin/jlp/internal/application/vocabulary"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
	"github.com/mikeyaustin/jlp/internal/tools"
)

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
		// API tokens for clients that cannot run a browser login:
		// reader apps posting to /api/v1/words, the A2A chat server.
		// See internal/application/apitoken.
		apiTokens := apitoken.New(postgres.NewAPITokenRepository(pool))
		eventRepo := postgres.NewLearningEventRepository(pool)
		bus := inprocbus.New()
		recorder := learning.NewRecorder(eventRepo, bus)
		sessionRepo := postgres.NewSessionRepository(pool)
		sessionsSvc := sessions.NewService(sessionRepo, recorder)
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

		// settingsSvc is the ports/ai.ModelResolver every AI adapter
		// buildAIGenerator/buildToolCaller construct below holds and
		// re-consults on every single call (Phase 4 Task S): it loads
		// whatever operator overrides already exist in app_settings once,
		// here, into an in-memory snapshot (never the other direction —
		// cfg's own APP_AI_* values are never written INTO the database,
		// see that package's doc comment on why), then keeps that
		// snapshot in sync with every subsequent /settings save. This is
		// what lets a model/effort change from /settings take effect on
		// the next AI request with no restart: the SAME aiGen/toolCaller
		// instances below just resolve a different value on their next
		// call.
		// modelListers backs the /settings model dropdowns (Phase 4 Task
		// M): one ports/ai.ModelLister per provider that has a genuine,
		// verifiable source of truth for what it can serve — Ollama's
		// /api/tags, agy's `agy models`, Anthropic's /v1/models, Gemini's
		// /v1beta/models. A provider absent from this map (claudecli,
		// codexcli — neither can enumerate at all) falls back to
		// /settings' free-text input, same as any provider whose lister
		// fails at render time. None of these dial out here: constructing
		// a lister never calls its provider (see each one's own doc
		// comment) — only /settings rendering does, lazily. Gemini's is
		// built unconditionally like the rest; with no API key it returns
		// ErrNoAPIKey without a request, and its /settings row is marked
		// unavailable anyway because buildAIGenerator constructed no
		// generator for it.
		modelListers := map[string]ai.ModelLister{
			"ollama":    ollamaadapter.NewModelLister(cfg.AI.Ollama),
			"anthropic": anthropicadapter.NewModelLister(cfg.AI.Anthropic),
			"gemini":    geminiadapter.NewModelLister(cfg.AI.Gemini),
			"agycli":    agycliadapter.NewModelLister(cfg.AI.AgyCLI),
		}
		settingsRepo := postgres.NewSettingsRepository(pool)
		settingsSvc, err := appsettings.NewService(context.Background(), settingsRepo, cfg.AI, modelListers)
		if err != nil {
			slog.Error("settings", "err", err)
			os.Exit(1)
		}

		// Every AI call is observed, whichever provider is behind it: the
		// audit trail (latency, cost, success) must never depend on
		// remembering to wrap a specific adapter. The same repository
		// instance is read back by the /ai page (Task 15) below.
		// buildAIGenerator (cmd/jlp/ai.go) does the rest: it builds one
		// observed instance per configured provider (fake/claudecli/
		// codexcli always, plus anthropic/ollama when actually
		// configured), then wraps them in an airouter.New — so
		// APP_AI_ROUTES-directed requests, and the APP_AI_PROVIDER
		// fallback, are both just different chains of already-observed
		// generators (Task 11/12, PRD §23/§24). settingsSvc is threaded
		// through so every one of those adapters can resolve a /settings
		// override at call time (Phase 4 Task S).
		aiRequestRepo := postgres.NewAIRequestRepository(pool)
		aiGen, aiProviders, aiDefaultProvider, err := buildAIGenerator(cfg, aiRequestRepo, settingsSvc, settingsSvc.PinnedProvider)
		if err != nil {
			slog.Error("ai", "err", err)
			os.Exit(1)
		}

		// toolCaller is the ai.ToolCaller counterpart of aiGen (Phase 4
		// Task 2, PRD §27/§29): buildToolCaller (cmd/jlp/ai.go) mirrors
		// buildAIGenerator's per-provider observed construction, just
		// over a narrower provider set (only fake/anthropic/ollama
		// implement ai.ToolCaller — see that function's own doc
		// comment). Always built, whether or not APP_AI_AGENTICTEACHER
		// is set: it's cheap (no network call at construction) and the
		// agent-run loop below needs it regardless of which teacher path
		// ends up calling it.
		toolCaller, err := buildToolCaller(cfg, aiRequestRepo, settingsSvc, settingsSvc.PinnedProvider)
		if err != nil {
			slog.Error("ai", "err", err)
			os.Exit(1)
		}

		// The learner's personal vocabulary (Task 6, PRD §12): vocabSvc's
		// Ingest backs POST /api/v1/vocabulary/events and the /vocabulary
		// page's List (see httpx.Options.Vocabulary below); its
		// DetectProduction is wired into feedback.Service so every review
		// round also notices when a looked-up expression shows up,
		// produced, in the learner's own writing. vocabRepo itself was
		// constructed above, alongside teachingPlanner.
		vocabSvc := vocabulary.NewService(vocabRepo, recorder)

		// MQTT event bridge (Phase 3 Task 6, PRD §31-33/§12/§59):
		// adaptermqtt.NewBridge subscribes every event.AllTypes() entry
		// on bus internally (see that constructor's own doc comment),
		// so it's constructed here, after bus/vocabSvc/identities are
		// all wired above. The whole block is skipped unless
		// cfg.MQTT.URL is set — see config.MQTT's "dormant by default"
		// doc comment: an operator who never sets APP_MQTT_URL gets
		// zero MQTT connections attempted, ever, matching Anki.ConnectURL's
		// and Summary.Enabled's own opt-in contracts above.
		if cfg.MQTT.URL != "" {
			mqttBridge, err := adaptermqtt.NewBridge(cfg.MQTT.URL, bus, vocabSvc, identities)
			if err != nil {
				slog.Error("mqtt", "err", err)
				os.Exit(1)
			}
			if err := mqttBridge.Start(context.Background()); err != nil {
				slog.Error("mqtt", "err", err)
				os.Exit(1)
			}
		}

		teacherAgent := teacher.New(aiGen)
		// feedbackRepo is named (rather than inlined like the other
		// one-off repository args above) because the Anki review queue
		// below also needs it: GetCorrection reads a correction's
		// original/replacement/explanation back for the Anki agent to
		// write a card from — the same repository instance, not a second
		// construction of it.
		feedbackRepo := postgres.NewFeedbackRepository(pool)

		// The spaced-retrieval scheduler (Phase 4 Task 7, PRD §54):
		// retrievalSched is shared by feedbackSvc (the encourage block's
		// due-expression preference) and practiceSvc (Start's due-concept
		// preference) below, and by retrievalConsumer here, which reacts
		// to the three outcome events that already exist — quiz.answered,
		// correction.retried, vocabulary.produced-correctly — the same
		// "subscribe a consumer's HandleEvent to the events it classifies"
		// wiring obsUpdater below uses. Unlike obsUpdater, it needs no
		// repository of its own beyond retrievalRepo: a correction.retried
		// event's concept slug(s) arrive directly in its own Evidence
		// (feedback.Service.RetryCorrection threads them through — see
		// application/retrieval.Consumer's own doc comment on why, rather
		// than this consumer re-querying feedbackRepo itself).
		retrievalRepo := postgres.NewRetrievalRepository(pool)
		retrievalSched := appretrieval.NewScheduler(retrievalRepo, time.Now)
		retrievalConsumer := appretrieval.NewConsumer(retrievalSched)
		bus.Subscribe(event.TypeQuizAnswered, retrievalConsumer.HandleEvent)
		bus.Subscribe(event.TypeCorrectionRetried, retrievalConsumer.HandleEvent)
		bus.Subscribe(event.TypeVocabularyProducedCorrectly, retrievalConsumer.HandleEvent)

		// The tool registry (Phase 4 Task 1/2, PRD §27-§29/§64): the
		// ONLY route any agent has from a tool-calling conversation to
		// real application state. Read-only tools whose dependencies
		// already exist at this point in main are registered here;
		// LearningTools (record_learning_event/create_exercise/
		// create_lesson_plan/create_anki_card — all MUTATING) is
		// registered further below, once practiceSvc/lessonSvc/ankiSvc
		// exist — Register just adds to the catalog, so registering more
		// tools into the SAME *tools.Registry instance after runner is
		// already constructed is safe (DefsFor/Invoke only ever run at
		// request time, never at construction time).
		toolRegistry := tools.NewRegistry()
		registerTools(toolRegistry, tools.SessionTools(sessionsSvc))
		registerTools(toolRegistry, tools.LearnerTools(identities, obsRepo, feedbackRepo))
		registerTools(toolRegistry, tools.WritingTools(writingSvc))
		registerTools(toolRegistry, tools.VocabularyTools(vocabSvc))
		registerTools(toolRegistry, tools.AnalyticsTools(prioRepo, grammarRepo))
		allowA2AAgents(toolRegistry)

		// agentRunRepo/runner back the Task 2 agent-run loop: every
		// tool-calling conversation any agent drives (today: the
		// agentic teacher, when APP_AI_AGENTICTEACHER=true) goes
		// through this ONE Runner, so /ai/agents (httpx.Options.AgentRuns
		// below) sees every run regardless of which agent produced it.
		agentRunRepo := postgres.NewAgentRunRepository(pool)
		runner := agentrun.NewRunner(toolCaller, toolRegistry, agentRunRepo, time.Now)

		// A2A (Phase 4 Task 3, PRD §29/§30, APP_A2A_ENABLED): dormant
		// unless explicitly enabled — see config.A2A's doc comment.
		// a2aServer stays nil otherwise, which httpx.Options.A2A's own
		// doc comment says means the routes it would expose are simply
		// absent. Built from the SAME runner/toolRegistry every local
		// agent-run path already uses — no repository, no application
		// service, nothing else — so a remote A2A caller gets no
		// privilege a local agent-run lacks (Rule 13).
		var a2aServer *a2a.Server
		if cfg.A2A.Enabled {
			a2aServer = a2a.New(runner, toolRegistry, cfg.A2A)
		}

		feedbackSvc := feedback.NewService(
			postgres.NewSessionRepository(pool),
			postgres.NewDocumentRepository(pool),
			feedbackRepo,
			grammarRepo,
			prioRepo,
			teachingPlanner,
			vocabSvc,
			teacherAgent,
			recorder,
			cfg.AI.AgenticTeacher,
			runner,
			retrievalSched,
		)
		analyticsSvc := analytics.NewService(postgres.NewAnalyticsRepository(pool))
		// The /outcomes capstone (Phase 4 Task 9, PRD §52/§72/§73). It
		// takes time.Now the same way retrievalSched does — every
		// window boundary it judges against comes from the injected
		// clock, never from a call inside the analyser.
		outcomesAnalyser := appoutcomes.NewAnalyser(postgres.NewOutcomeRepository(pool), time.Now)
		aiRatingRepo := postgres.NewAIRatingRepository(pool)
		aiQualityRepo := postgres.NewAIQualityRepository(pool)

		// The drill engine (Task 9, PRD §17.2/§58): drillAgent generates
		// and evaluates exercises through the same always-observed aiGen
		// every other agent uses; practiceSvc reuses teachingPlanner (for
		// TopConcept) and grammarRepo (for the random-catalog fallback and
		// TopConcept's own concept resolution) — the same shared
		// instances feedbackSvc's construction above already established.
		drillAgent := drill.New(aiGen)
		practiceSvc := practice.NewService(postgres.NewExerciseRepository(pool), drillAgent, teachingPlanner, grammarRepo, recorder, retrievalSched)

		// The conversation tutor (Phase 4 Task 6, PRD §17.4): a free-form
		// dialogue alternative to the writing/feedback pane, in the same
		// session workspace. conversationAgent goes through the same
		// always-observed aiGen every other agent uses; conversationSvc
		// reuses vocabSvc (DetectProduction — a conversation turn feeds
		// the learner model exactly like a writing review does) and
		// recorder, the same shared instances feedbackSvc's/practiceSvc's
		// construction above already established. It takes its own
		// postgres.NewSessionRepository(pool) rather than sessionsSvc
		// (the application-layer service) — the same "a storage
		// repository, not the sibling application service" choice
		// feedbackSvc's own first two constructor args already make.
		conversationAgent := agentconversation.New(aiGen)
		conversationSvc := appconversation.NewService(postgres.NewConversationRepository(pool), postgres.NewSessionRepository(pool), conversationAgent, vocabSvc, recorder, grammarRepo, eventRepo)

		// Speech recognition (Phase 4 Task 8, PRD §66): recognizer stays
		// nil — and appspeech.Service.Transcribe always returns
		// ErrNotConfigured — unless APP_SPEECH_STTURL is set, matching
		// every other optional integration's dormant-unless-configured
		// contract (config.Speech's own doc comment). whisperadapter.New
		// never dials out at construction (same never-fails contract as
		// ollamaadapter.New/ankiconnect.New), so it's safe to construct
		// unconditionally the moment the URL is non-empty, before the
		// sidecar (docker-compose.yml's "speech" profile) has necessarily
		// even started. speechSvc itself is always constructed — a nil
		// recognizer inside it IS the dormant state — mirroring
		// channelSvc's own "always build the service, only conditionally
		// drive it" shape just below.
		var recognizer ai.SpeechRecognizer
		if cfg.Speech.STTURL != "" {
			recognizer = whisperadapter.New(cfg.Speech.STTURL)
		}
		// postgres.NewSessionRepository(pool) here (not sessionsSvc) is
		// the same "a storage repository, not the sibling application
		// service" choice conversationSvc's own construction above makes
		// — Transcribe only needs Get for authorization (code review
		// Important I1), never the rest of sessions.Service's surface.
		speechSvc := appspeech.NewService(recognizer, postgres.NewSessionRepository(pool), recorder)
		// Deliberately NOT constructing an internal/adapters/tts.
		// Synthesizer here even when APP_SPEECH_TTSURL is set: nothing in
		// this task's HTTP surface calls ai.SpeechSynthesizer.Speak yet
		// (only POST /speech/transcribe is wired — see server.go's
		// routes()), so building one now would be main.go wiring dead
		// code, not "dormant unless configured" like recognizer above.
		// The adapter itself is real and tested (internal/adapters/tts,
		// against a genuine local VOICEVOX Engine — see that package's
		// doc comment for why this differs from a stub), and
		// ttsadapter.New(cfg.Speech.TTSURL, speaker) is exactly what a
		// future task wires in the moment it adds a consumer.

		// Channel port + Slack Socket Mode adapter (Phase 4 Task 4, PRD
		// §20/§20.1): channelSvc composes the SAME sessions/feedback/
		// practice services every other JLP surface already uses — a
		// channel gets no privilege, and no different correction/gating
		// behaviour, an HTTP request wouldn't also get. Constructed
		// unconditionally (cheap — no network call happens until an
		// adapter's Start actually dials out), but only ever driven by an
		// adapter when that adapter's own tokens are configured; with no
		// channel adapter enabled at all, channelSvc simply has no caller.
		channelSvc := appchannel.NewService(sessionsSvc, feedbackSvc, practiceSvc, postgres.NewDocumentRepository(pool), identities, cfg.Channels)

		// Slack is dormant unless BOTH APP_SLACK_APPTOKEN and
		// APP_SLACK_BOTTOKEN are set (config.Slack's own doc comment;
		// validate() already rejects a lone token at boot). Socket Mode
		// dials OUT to Slack, so Start is run on its own goroutine — see
		// that method's doc comment: a genuinely fatal Start error (an
		// invalid token, say) is logged, not os.Exit'd, exactly like a
		// transient reconnect is logged and never fatal. One channel
		// going down — or never starting at all, on a boot-time token
		// problem — must never take the rest of the app with it.
		if cfg.Slack.AppToken != "" && cfg.Slack.BotToken != "" {
			slackAdapter := slackadapter.New(cfg.Slack.AppToken, cfg.Slack.BotToken)
			go func() {
				// Belt-and-suspenders alongside slack.Adapter's own
				// handleEvent/dispatch recovers: Start itself blocks on
				// the underlying transport, so a panic here would be a
				// genuinely unexpected setup-path failure, not a
				// per-message one — but per this same "a channel failure
				// must never take the app down" rule, even that must not
				// reach this goroutine's top unrecovered (see
				// cmd/jlp/summary.go's summaryJob for the same pattern).
				defer func() {
					if r := recover(); r != nil {
						slog.Error("slack: adapter goroutine panicked", "panic", r, "stack", string(debug.Stack()))
					}
				}()
				if err := slackAdapter.Start(context.Background(), channelSvc.Handle); err != nil {
					slog.Error("slack: adapter stopped", "err", err)
				}
			}()
			slog.Info("slack: adapter starting")
		}

		// Signal is dormant unless BOTH APP_SIGNAL_RPCURL and
		// APP_SIGNAL_NUMBER are set (config.Signal's own doc comment;
		// validate() already rejects a lone value at boot). Same
		// never-fatal, own-goroutine posture as Slack immediately
		// above: a bad RPCURL, or a sidecar that's down, only ends this
		// one channel, never the process — see
		// internal/adapters/signal's own package doc comment for the
		// full "why" (identical to Slack's, over a different
		// transport).
		if cfg.Signal.RPCURL != "" && cfg.Signal.Number != "" {
			signalAdapter := signaladapter.New(cfg.Signal.RPCURL, cfg.Signal.Number)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("signal: adapter goroutine panicked", "panic", r, "stack", string(debug.Stack()))
					}
				}()
				if err := signalAdapter.Start(context.Background(), channelSvc.Handle); err != nil {
					slog.Error("signal: adapter stopped", "err", err)
				}
			}()
			slog.Info("signal: adapter starting")
		}

		// The Anki review queue (Phase 3 Task 3, PRD §19): ankiAgent
		// writes one flashcard per accepted correction through the same
		// always-observed aiGen every other agent uses; ankiSvc reuses
		// feedbackRepo (GetCorrection) to read a correction's context back
		// without a second repository construction. PushToAnkiConnect
		// stays dormant (SetConnector never called) unless
		// cfg.Anki.ConnectURL is set — see config.Anki's doc comment.
		ankiAgent := agentanki.New(aiGen)
		ankiCardRepo := postgres.NewAnkiCardRepository(pool)
		ankiSvc := appanki.NewService(ankiCardRepo, feedbackRepo, ankiAgent, recorder)
		if cfg.Anki.ConnectURL != "" {
			ankiSvc.SetConnector(ankiconnect.New(cfg.Anki.ConnectURL))
		}

		// Tutor lesson guides + post-lesson observations (Phase 3 Task 4,
		// PRD §18/§60): lessonAgent writes one lesson_plan.v1 guide per
		// call through the same always-observed aiGen every other agent
		// uses; lessonSvc reuses feedbackRepo (RecentCorrections),
		// teachingPlanner (ActivationCandidates), prioRepo (Top), and
		// obsRepo (List) — the same shared instances feedbackSvc's/
		// practiceSvc's construction above already established, not a
		// second construction of any of them.
		lessonAgent := agentlesson.New(aiGen)
		lessonRepo := postgres.NewLessonRepository(pool)
		lessonSvc := applessons.NewService(lessonRepo, prioRepo, teachingPlanner, feedbackRepo, obsRepo, lessonAgent, recorder)

		// LearningTools (record_learning_event/create_exercise/
		// create_lesson_plan/create_anki_card) is registered here, now
		// that its dependencies (practiceSvc/lessonSvc/ankiSvc) all
		// exist — into the SAME toolRegistry constructed above, before
		// any request can reach it. No agent is Allow()ed to call these
		// yet: they're MUTATING tools reserved for a future agent (a
		// conversation tutor, Phase 4 Task 6) that acts on the
		// learner's behalf rather than merely reviewing writing.
		registerTools(toolRegistry, tools.LearningTools(recorder, practiceSvc, lessonSvc, ankiSvc))

		// Weekly email summary (Phase 3 Task 5, PRD §21/§65): summarySvc
		// is always constructed (cheap — no network call happens until
		// SendWeekly is actually invoked, matching smtpadapter.New's
		// never-fails-at-construction contract), but the cron scheduler
		// that could actually TRIGGER a send is only started when
		// cfg.Summary.Enabled is true — see maybeStartSummaryScheduler's
		// own doc comment for the full PRD §65 opt-in reasoning. An
		// operator who never sets APP_SUMMARY_ENABLED gets a nil
		// scheduler and this line logs nothing.
		summaryAgent := agentsummary.New(aiGen)
		summarySvc := appsummary.NewService(analyticsSvc, prioRepo, vocabRepo, summaryAgent, smtpadapter.New(cfg.SMTP))
		if _, err := maybeStartSummaryScheduler(cfg, summarySvc); err != nil {
			slog.Error("summary", "err", err)
			os.Exit(1)
		}

		// Authentication (see config.Auth's doc comment for what the
		// three modes actually prove). authRoutes and logoutPath stay
		// zero unless the chosen adapter owns a browser session of its
		// own — only oidc does — so static and authelia mode build
		// exactly the server they always did.
		var authn auth.Authenticator
		var authRoutes http.Handler
		var logoutPath string
		switch cfg.Auth.Mode {
		case "static":
			authn = staticauth.New(cfg.Auth.Static.ID, cfg.Auth.Static.DisplayName)
		case "authelia":
			authn, err = authelia.New(cfg.Auth.TrustedProxies)
			if err != nil {
				slog.Error("auth", "err", err)
				os.Exit(1)
			}
		case "oidc":
			oidcAuth, oerr := oidcadapter.New(oidcadapter.Config{
				IssuerURL:    cfg.Auth.OIDC.IssuerURL,
				ClientID:     cfg.Auth.OIDC.ClientID,
				ClientSecret: cfg.Auth.OIDC.ClientSecret,
				RedirectURL:  cfg.OIDCRedirectURL(),
				CookieKey:    []byte(cfg.Auth.OIDC.CookieKey),
				// The session cookie is Secure whenever the app is
				// actually served over TLS. Derived from the base URL
				// rather than configured separately so the two can
				// never disagree — and so a plain-http dev run of oidc
				// mode still works instead of silently dropping every
				// cookie.
				CookieSecure: strings.HasPrefix(cfg.Server.BaseURL, "https://"),
				SessionTTL:   cfg.Auth.OIDC.SessionTTL,
				IdleTimeout:  cfg.Auth.OIDC.IdleTimeout,
				LogoutURL:    cfg.Auth.OIDC.LogoutURL,
			})
			if oerr != nil {
				// oidcadapter.New's errors never contain the client
				// secret — see its own doc comments.
				slog.Error("auth", "err", oerr)
				os.Exit(1)
			}
			authn = oidcAuth
			authRoutes = oidcAuth.Routes()
			logoutPath = oidcadapter.LogoutPath
			slog.Info("auth", "mode", "oidc", "issuer", cfg.Auth.OIDC.IssuerURL, "redirect_uri", cfg.OIDCRedirectURL())
		default:
			slog.Error("auth", "err", fmt.Sprintf("unknown auth mode %q", cfg.Auth.Mode))
			os.Exit(1)
		}
		srv := httpx.NewServer(httpx.Options{
			Addr:       fmt.Sprintf(":%d", cfg.Server.Port),
			Auth:       authn,
			Identities: identities,
			APITokens:  apiTokens,
			A2AChatURL: cfg.A2A.ChatURL,
			// Both routing lists, so /settings/agents can offer a pin for
			// every prompt this process actually routes.
			PromptNames:        allRoutablePromptNames(),
			Sessions:           sessionsSvc,
			Writing:            writingSvc,
			Events:             eventRepo,
			Feedback:           feedbackSvc,
			Analytics:          analyticsSvc,
			Outcomes:           outcomesAnalyser,
			AI:                 aiGen,
			AIRequests:         aiRequestRepo,
			AIRatings:          aiRatingRepo,
			AIQuality:          aiQualityRepo,
			Grammar:            grammarRepo,
			Priorities:         prioRepo,
			Observations:       obsRepo,
			Retrieval:          retrievalRepo,
			Vocabulary:         vocabSvc,
			Practice:           practiceSvc,
			Anki:               ankiSvc,
			AnkiCards:          ankiCardRepo,
			AnkiConnectEnabled: cfg.Anki.ConnectURL != "",
			Lessons:            lessonSvc,
			LessonsRepo:        lessonRepo,
			AgentRuns:          agentRunRepo,
			A2A:                a2aServer,
			A2APath:            cfg.A2A.Path,
			Conversation:       conversationSvc,
			Speech:             speechSvc,
			// Same source of truth the recognizer is built from above, so
			// the button and the route can never disagree about whether
			// speech is configured (whole-branch review F8) — the
			// AnkiConnectEnabled line above is the same shape.
			SpeechEnabled:     cfg.Speech.STTURL != "",
			Settings:          settingsSvc,
			AIProviders:       aiProviders,
			AIDefaultProvider: aiDefaultProvider,
			AuthRoutes:        authRoutes,
			LogoutPath:        logoutPath,
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
	case "eval":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := runEvalCommand(context.Background(), cfg); err != nil {
			slog.Error("eval", "err", err)
			os.Exit(1)
		}
		slog.Info("eval complete")
	case "send-summary":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := runSendSummary(context.Background(), cfg); err != nil {
			slog.Error("send-summary", "err", err)
			os.Exit(1)
		}
	case "slack-smoke":
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := runSlackSmoke(context.Background(), cfg); err != nil {
			slog.Error("slack-smoke", "err", err)
			os.Exit(1)
		}
		slog.Info("slack-smoke: message sent")
	case "restore":
		// Soft delete's durable way back (Phase 4 Task D): `jlp restore
		// <session|word|lesson> <identity> <id>`. The list's undo button
		// covers the misclick; this covers noticing a week later. See
		// restore.go.
		cfg, err := config.Load()
		if err != nil {
			slog.Error("config", "err", err)
			os.Exit(1)
		}
		if err := runRestore(context.Background(), cfg, os.Args[2:]); err != nil {
			slog.Error("restore", "err", err)
			os.Exit(1)
		}
		slog.Info("restore complete")
	default:
		slog.Error("unknown command", "cmd", cmd)
		os.Exit(2)
	}
}

// registerTools registers every tools.Tool in ts into reg — a small
// helper purely because tools.Registry.Register takes one Tool at a
// time while every internal/tools/*.go constructor
// (SessionTools/LearnerTools/... ) returns a []Tool, and main's own
// registration block above would otherwise repeat this same for loop
// six times.
func registerTools(reg *tools.Registry, ts []tools.Tool) {
	for _, t := range ts {
		reg.Register(t)
	}
}

// allowA2AAgents grants every agent behind an A2A skill its tool
// allowlist — the ONE definition of who may call what.
//
// A function rather than inline statements so that cmd/jlp's own tests
// assert against the same grants main actually applies. A test that
// restated this list would pass while the real wiring drifted, which is
// precisely the failure it would exist to catch.
func allowA2AAgents(reg *tools.Registry) {
	// "teacher" (agentName in internal/agent/teacher/teacher.go) may
	// only READ the learner's context to decide what to emphasize —
	// never mutate it (that's what the single-shot path's own
	// RequestFeedback/vocab.DetectProduction calls already do, on
	// the application layer's own authority, not the model's).
	reg.Allow("teacher",
		"get_active_session", "get_session_context",
		"get_learner_profile", "get_recent_errors", "get_correction_history",
		"get_recent_writing",
		"get_vocabulary_history", "search_vocabulary",
		"get_learning_priorities", "get_grammar_history",
	)
	// "summary" and "lesson" are the agents behind A2A's
	// analyse_learner and plan_lesson skills (internal/adapters/a2a's
	// skillDefs). Until now neither was Allow()ed anything, so both
	// answered from the caller's message alone — specialists that
	// could not consult the thing they specialise in. The agent card
	// disclosed that honestly (text/plain only, no widget types), but
	// the honest disclosure of a useless capability is still a
	// useless capability.
	//
	// No session tools for either: a remote A2A caller has no session
	// of ours, and both skills are about the learner over time rather
	// than about one sitting.
	//
	// Read-only, exactly as above. The mutating tools stay allowed to
	// nobody — see where LearningTools is registered below. That
	// means plan_lesson produces a plan in prose and cannot persist
	// one, so A2A never emits the lesson widget; granting a mutation
	// to make a card appear would reverse a deliberate boundary for a
	// cosmetic reason.
	reg.Allow("summary",
		"get_learner_profile", "get_recent_errors", "get_correction_history",
		"get_grammar_history", "get_learning_priorities",
		"get_vocabulary_history", "get_recent_writing",
	)
	reg.Allow("lesson",
		"get_learner_profile", "get_recent_errors", "get_correction_history",
		"get_grammar_history", "get_learning_priorities",
		"get_vocabulary_history", "search_vocabulary",
	)
}
