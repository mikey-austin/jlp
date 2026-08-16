package httpx

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mikeyaustin/jlp/internal/adapters/a2a"
	"github.com/mikeyaustin/jlp/internal/application/analytics"
	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	"github.com/mikeyaustin/jlp/internal/application/feedback"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	"github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appsettings "github.com/mikeyaustin/jlp/internal/application/settings"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	appwriting "github.com/mikeyaustin/jlp/internal/application/writing"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/auth"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Options struct {
	Addr       string
	Auth       auth.Authenticator
	Identities storage.IdentityRepository
	Sessions   *sessions.Service
	Writing    *appwriting.Service
	Events     storage.LearningEventRepository
	// Feedback drives the workspace's フィードバックを取得 button and
	// correction accept/reject buttons (Task 13): it wraps the Teacher
	// agent with authorization, persistence, and learning events.
	Feedback *feedback.Service
	// Analytics drives the home dashboard's stat tiles and よくある間違い
	// list (Task 14).
	Analytics *analytics.Service
	// AI is the always-observed structured generator (fake or Anthropic
	// underneath). No route consumes it directly — the teacher feedback
	// pipeline (Task 12/13) goes through Feedback above — but it's
	// wired through here in case a future task needs it directly.
	AI ai.StructuredGenerator
	// AIRequests backs the /ai observability page's table (Task 15):
	// the same audit-log repository the observability decorator writes
	// through, read back here for display.
	AIRequests storage.AIRequestRepository
	// AIRatings backs the feedback partial's star widget and the /ai
	// page's rating column (Task 15).
	AIRatings storage.AIRatingRepository
	// AIQuality backs the /ai page's プロバイダー比較 and プロンプト品質
	// sections (Task 10, PRD §26): aggregate quality numbers computed
	// server-side over the same ai_requests/ai_ratings tables AIRequests
	// and AIRatings above read individual rows from.
	AIQuality storage.AIQualityRepository
	// Grammar backs the /grammar catalog + per-concept detail pages
	// (Task 3): ConceptStats drives the list, GetConcept and
	// CorrectionsForConcept drive the detail page. The same repository
	// instance Feedback's concept-tagging path uses (see main.go).
	Grammar storage.GrammarRepository
	// Priorities backs the /learner page's priority table and the
	// /api/v1/learner/priorities API (Task 5): Top(N) — the same
	// repository instance the planner writes through and Feedback reads
	// Top(5) from to fill the Teacher prompt's RecentErrors (see
	// main.go).
	Priorities storage.PriorityRepository
	// Observations backs the /learner page's observations list (Task 5):
	// the raw learnermodel.Observation rows the planner scores into
	// Priorities — shown alongside the priority table so a learner can
	// see both the current read AND what it's derived from.
	Observations storage.ObservationRepository
	// Vocabulary backs both the /vocabulary page's List and POST
	// /api/v1/vocabulary/events's Ingest (Task 6, PRD §12) — a single
	// application-layer service rather than a raw repository (unlike
	// Priorities/Observations above), since Ingest carries real
	// business logic (idempotency, event recording) an HTTP handler
	// must never duplicate. The same instance's DetectProduction is
	// also wired into Feedback (see main.go), so a lookup made here and
	// a production detected there share one vocabulary catalog.
	Vocabulary *appvocabulary.Service
	// Practice drives the /practice page's 練習する button and answer
	// forms (Task 9, PRD §17.2/§58): the drill engine's application-layer
	// pipeline (planner-driven concept selection, exercise generation,
	// deterministic-plus-AI evaluation, events).
	Practice *practice.Service
	// Anki drives the /anki review queue's mutations (Phase 3 Task 3,
	// PRD §19): generating a draft from a correction, approve/reject,
	// TSV export, and the optional AnkiConnect push.
	Anki *appanki.Service
	// AnkiCards backs the /anki page's own draft/approved listing —
	// read-only, so it goes straight to the repository rather than
	// through Anki above, the same "raw repository for a GET listing
	// page, service for mutations" split Grammar/Priorities/Observations
	// already use.
	AnkiCards storage.AnkiCardRepository
	// AnkiConnectEnabled gates the /anki page's 「Ankiへ送信」 button:
	// true only when main.go configured APP_ANKI_CONNECT_URL and wired a
	// connector into Anki via SetConnector. False (the default) keeps
	// AnkiConnect fully dormant — the button is never rendered at all,
	// not just disabled.
	AnkiConnectEnabled bool
	// Lessons drives the /lessons pipeline's mutations (Phase 3 Task 4,
	// PRD §18/§60): generating a tutor lesson guide and recording a
	// human tutor's post-lesson observation.
	Lessons *applessons.Service
	// LessonsRepo backs the /lessons list/detail pages' own read-only
	// GET queries — the same "raw repository for a listing page,
	// service for mutations" split AnkiCards/Anki above use.
	LessonsRepo storage.LessonRepository
	// AgentRuns backs the /ai/agents trace viewer (Phase 4 Task 2, PRD
	// §27/§50): the same storage.AgentRunRepository instance
	// application/agentrun.Runner writes through (see main.go), read
	// back here for display — the same "raw repository for a
	// read-only listing/detail page" split every other *Repo field in
	// this struct already uses.
	AgentRuns storage.AgentRunRepository
	// A2A mounts Phase 4 Task 3's protocol adapter
	// (internal/adapters/a2a, PRD §29/§30) at A2APath when non-nil.
	// nil — the default, when APP_A2A_ENABLED=false — means the routes
	// it would otherwise expose are entirely absent (404 from chi's
	// own no-route-matched handling), not merely guarded: see
	// routes()'s own comment on where this is mounted, and
	// config.A2A's doc comment for why main.go leaves this nil by
	// default.
	A2A *a2a.Server
	// A2APath is cfg.A2A.Path, passed through so routes() doesn't need
	// its own copy of config.Config just to read one string; only read
	// when A2A above is non-nil.
	A2APath string
	// Conversation drives the workspace's conversation pane (Phase 4
	// Task 6, PRD §17.4): free-form dialogue turns and the
	// end-of-conversation digest, governed by the session's
	// session.Profile.FeedbackTiming.
	Conversation *appconversation.Service
	// Settings drives the /settings page (Phase 4 Task S, linked from
	// /ai rather than a new top-level nav item — see settings.go's own
	// doc comment): reading each AI provider's current effective
	// model/effort and its source (config or override), and saving or
	// resetting an override. The SAME instance is also the
	// ports/ai.ModelResolver every AI adapter main.go builds holds and
	// re-consults on every call — a save here reaches the next AI
	// request with no restart.
	Settings *appsettings.Service
	// AIProviders is the fixed, ordered set of AI provider names
	// actually constructed in this deployment (Phase 4 Task W item 5) —
	// the workspace's per-request adapter-override dropdown's <option>
	// values, and the set feedbackRequest validates a submitted
	// provider_override against (rejecting anything else with a 400
	// rather than falling through to the default). AIDefaultProvider
	// names which one is pre-selected and labelled default: main.go
	// computes it as the first provider in the "teacher.feedback"
	// APP_AI_ROUTES chain when one exists, else APP_AI_PROVIDER — see
	// cmd/jlp/ai.go's buildAIGenerator, whose second and third return
	// values these are, verbatim.
	AIProviders       []string
	AIDefaultProvider string
}

type Server struct {
	http.Server
	opts Options
}

func NewServer(opts Options) *Server {
	s := &Server{opts: opts}
	s.Addr = opts.Addr
	s.Handler = s.routes()
	// Guard against slow-client resource exhaustion (Slowloris-style
	// connections that trickle bytes to keep a handler goroutine and its
	// connection pinned indefinitely). None of these routes stream or
	// long-poll, so ordinary request/response timing comfortably fits
	// inside all four.
	s.ReadHeaderTimeout = 5 * time.Second
	s.ReadTimeout = 30 * time.Second
	s.WriteTimeout = 60 * time.Second
	s.IdleTimeout = 120 * time.Second
	return s
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	// Deliberately omit chi's middleware.RealIP: it unconditionally rewrites
	// r.RemoteAddr from the client-supplied X-Forwarded-For/X-Real-IP header,
	// which would let any client spoof the peer address our auth adapters use
	// for their trusted-proxy decision (see internal/adapters/authelia). The
	// authelia adapter must observe the true TCP peer.
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
			slog.Error("write healthz response", "err", err)
		}
	})
	// /offline: the PWA shell's offline fallback page (Task 17). It sits
	// outside the auth group like /healthz — the service worker serves it
	// from cache with no network (and thus no session) available.
	r.Get("/offline", s.offline)
	fs := http.StripPrefix("/static/", http.FileServer(http.Dir("web/static")))
	// /static/sw.js needs its own exact-path route ahead of the wildcard
	// below so we can set Service-Worker-Allowed: / on it — without that
	// response header, a worker served from /static/ cannot register with
	// scope '/' (the browser would otherwise restrict it to /static/*).
	r.Get("/static/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Worker-Allowed", "/")
		fs.ServeHTTP(w, r)
	})
	r.Handle("/static/*", fs)
	r.Group(func(r chi.Router) {
		r.Use(RequireIdentity(s.opts.Auth, s.opts.Identities))
		// CSRFProtect (origin verification, not tokens — see csrf.go)
		// mounts here, after RequireIdentity but before every route
		// handler in this group: every state-changing route below is
		// session-cookie-authenticated, so a cross-site page that
		// tricked a browser into submitting a request would otherwise
		// ride the victim's own cookie. Deliberately scoped to this
		// authenticated group only — every unauthenticated route above
		// (/healthz, /static/*, /offline) is GET-only, so there's
		// nothing state-changing there to protect.
		r.Use(CSRFProtect())
		r.Get("/", s.home)
		r.Get("/sessions", s.sessionsList)
		r.Post("/sessions", s.sessionsCreate)
		r.Get("/sessions/{id}", s.sessionsWorkspace)
		r.Post("/sessions/{id}/feedback", s.feedbackRequest)
		// GET /sessions/{id}/feedback/{feedbackID}: Phase 4 Task W item 2 —
		// clicking a feedback-history row loads that past review into the
		// bottom pane. A GET (not a POST) so it's back/forward-friendly,
		// per the task brief.
		r.Get("/sessions/{id}/feedback/{feedbackID}", s.feedbackShow)
		// Phase 4 Task 6: the conversation tutor (PRD §17.4) — a
		// message-posting form in the session pane, and the
		// end-of-conversation digest button. Listed right after
		// /sessions/{id}/feedback since both are the same workspace
		// page's two review surfaces.
		r.Post("/sessions/{id}/conversation", s.conversationSay)
		r.Post("/sessions/{id}/conversation/summary", s.conversationSummarise)
		r.Post("/corrections/{id}/status", s.correctionStatus)
		// retry/reveal/confidence: Phase 2 Task 8's active recall +
		// confidence tracking (PRD §9/§53) — see feedback.go's handler
		// doc comments and correction_card.html.tmpl's socratic gate.
		r.Post("/corrections/{id}/retry", s.correctionRetry)
		r.Post("/corrections/{id}/reveal", s.correctionReveal)
		r.Post("/corrections/{id}/confidence", s.correctionConfidence)
		r.Post("/documents/{id}", s.documentsSave)
		r.Get("/ai", s.aiRequests)
		// /ai/agents: Phase 4 Task 2's agent-run trace viewer (PRD
		// §27/§50) — linked from /ai rather than a new top-level nav
		// item (see layout.html.tmpl). Listed before /ai's own route
		// only for readability; chi's routing has no literal-vs-wildcard
		// ambiguity here since /ai has no {id} segment of its own.
		r.Get("/ai/agents", s.agentRunsList)
		r.Get("/ai/agents/{id}", s.agentRunsDetail)
		// /settings: Phase 4 Task S — change which AI model/effort each
		// provider uses at runtime, no restart. Linked from /ai (see
		// ai.html.tmpl), deliberately NOT a tenth top-level nav item —
		// the topbar already wraps to its own row on phones (layout.html.tmpl).
		r.Get("/settings", s.settingsPage)
		r.Post("/settings/{provider}/model", s.settingsSetModel)
		r.Post("/settings/{provider}/effort", s.settingsSetEffort)
		r.Post("/settings/{provider}/reset", s.settingsReset)
		r.Post("/ratings", s.ratingsCreate)
		r.Get("/grammar", s.grammarList)
		r.Get("/grammar/{slug}", s.grammarDetail)
		r.Get("/learner", s.learnerPage)
		r.Get("/vocabulary", s.vocabularyPage)
		r.Get("/practice", s.practicePage)
		r.Post("/practice/start", s.practiceStart)
		r.Post("/practice/{id}/answer", s.practiceAnswer)

		// Phase 3 Task 3: Anki card generation + review queue (PRD
		// §19). /anki/export.tsv is a GET despite mutating (marks
		// exported) — see ankiExportTSV's doc comment for why that's
		// safe and deliberate; it's listed before /anki/{id}/status so
		// its literal path always wins chi's routing over the wildcard
		// {id} segment.
		r.Get("/anki", s.ankiPage)
		r.Get("/anki/export.tsv", s.ankiExportTSV)
		r.Post("/anki/push", s.ankiPush)
		r.Post("/anki/{id}/status", s.ankiStatus)
		r.Post("/corrections/{id}/anki", s.correctionAnki)

		// Phase 3 Task 4: tutor lesson guides + post-lesson observations
		// (PRD §18, §60).
		r.Get("/lessons", s.lessonsList)
		r.Post("/lessons", s.lessonsGenerate)
		r.Get("/lessons/{id}", s.lessonsDetail)
		r.Post("/lessons/{id}/complete", s.lessonsComplete)

		// /api/v1: the versioned JSON API (Task 16). It shares the exact
		// same application services as the HTML routes above — no new
		// application-layer code — proving HTMX isn't the domain boundary
		// (PRD §38). It sits inside this same auth group so RequireIdentity
		// covers it too; see middleware.go for how that middleware emits
		// JSON 401s for paths under /api/ instead of the HTML routes'
		// plain-text body.
		r.Route("/api/v1", func(r chi.Router) {
			r.Get("/sessions", s.apiSessionsList)
			r.Post("/sessions", s.apiSessionsCreate)
			r.Get("/sessions/{id}", s.apiSessionsGet)
			r.Post("/sessions/{id}/feedback", s.apiFeedbackRequest)
			r.Post("/corrections/{id}/status", s.apiCorrectionStatus)
			r.Get("/learner/statistics", s.apiLearnerStatistics)
			r.Get("/learner/priorities", s.apiLearnerPriorities)
			r.Post("/ratings", s.apiRatingsCreate)
			r.Post("/vocabulary/events", s.apiVocabularyIngest)

			// Phase 3 Task 8: bulk vocabulary ingestion for external
			// reader apps (Nihongo Daily's contract, implemented verbatim
			// — see words.go's package doc comment). Sits in this same
			// authenticated group like every other learner-owned write;
			// no CSRF special-casing needed — a non-browser caller sends
			// no Origin header and passes Task 1's CSRF middleware
			// unchanged.
			r.Post("/words", s.apiWordsIngest)
		})

		// Phase 4 Task 3: the A2A protocol adapter (PRD §29/§30, Rule
		// 13), mounted INSIDE this authenticated group deliberately —
		// same RequireIdentity + CSRFProtect posture as every route
		// above, not a separate unauthenticated surface. A2A task
		// creation is a POST from a non-browser client, which sends no
		// Origin/Sec-Fetch-Site header at all, so CSRFProtect's
		// same-origin check passes it exactly the same way it already
		// passes curl/API clients hitting /api/v1 above (see csrf.go's
		// csrfReject: no Origin ⇒ never rejected). withA2AIdentity below
		// is what actually hands this request's already-authenticated
		// identity to the adapter — see that function's doc comment for
		// why a2a.Server needs its own bridge rather than importing
		// IdentityFrom itself.
		if s.opts.A2A != nil {
			mountA2A(r, s.opts.A2APath, withA2AIdentity(s.opts.A2A.Routes()))
		}
	})
	return r
}

// mountA2A mounts h at path on r, recovering any panic r.Mount itself
// raises and re-panicking with a single, clearly worded message naming
// APP_A2A_PATH — a last-resort net for a route collision neither
// config.validate()'s structural check (a2aPathShapeError) nor its
// hardcoded prefix denylist (a2aReservedPathPrefixes) anticipated
// (Task 3 code review, Minor 6 follow-up): both live in
// internal/config and are kept in sync with this package's route
// table BY HAND, so a future route added here without a matching
// config-side update would otherwise reach r.Mount and panic with
// chi's own internal message — one that names neither APP_A2A_PATH
// nor gives an operator anything actionable to fix. This recover
// doesn't replace either config-time check (both still run first, at
// `jlp serve` startup, before the HTTP server is even built) — it's
// the safety net for whatever they didn't anticipate.
func mountA2A(r chi.Router, path string, h http.Handler) {
	defer func() {
		if p := recover(); p != nil {
			panic(fmt.Sprintf("config: APP_A2A_PATH %q could not be mounted (%v) — it likely collides with an existing route; choose a different APP_A2A_PATH", path, p))
		}
	}()
	r.Mount(path, h)
}

// withA2AIdentity wraps next (a2a.Server.Routes()) so every request
// reaching it carries this request's already-authenticated identity
// via a2a.WithIdentity — the ONLY way that package's handlers ever see
// an identity (see internal/adapters/a2a's package doc comment: it
// never reads one from a task's JSON body). This one-line bridge is
// what lets internal/adapters/a2a stay entirely ignorant of this
// package's own identityKey/RequireIdentity machinery (importing
// httpx from a2a would risk an import cycle back the other way, since
// this file is what constructs and mounts an a2a.Server in the first
// place) while still only ever using the SAME identity RequireIdentity
// already resolved for this request — never a second, independent
// notion of who's asking.
func withA2AIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ident, _ := IdentityFrom(r.Context())
		next.ServeHTTP(w, r.WithContext(a2a.WithIdentity(r.Context(), ident.ID)))
	})
}

func (s *Server) HandlerForTest() http.Handler { return s.Handler }

// offline renders the PWA shell's offline fallback (Task 17). It's
// reached two ways: directly, as an ordinary page; and by the service
// worker's fetch handler, which serves this precached response in
// place of a failed navigation. Either way there's no authenticated
// identity available, so the data passed to Render omits Identity —
// the layout's {{with .Identity}} already tolerates that (see the
// topnav, which renders without the "who" span for unauthenticated
// requests).
func (s *Server) offline(w http.ResponseWriter, r *http.Request) {
	Render(w, r, "offline", map[string]any{
		"Title": "オフライン",
	})
}

// recentSessionsLimit is how many of the caller's most-recently-updated
// sessions the dashboard links to — Sessions.List already returns them
// newest-first, so this just truncates.
const recentSessionsLimit = 5

// home renders the Task 14 dashboard: aggregate learner statistics
// (stat tiles + よくある間違い) plus a short list of recent sessions to
// jump back into. Task 7 (PRD §15/§51) adds three richer sections:
// 語彙ファネル (VocabFunnel), 弱点トレンド (WeaknessTrends, rendered as
// per-subject sparklines via weaknessTrendVMs), and 自信の較正
// (ConfidenceCalibration).
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	stats, err := s.opts.Analytics.Statistics(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load statistics", http.StatusInternalServerError)
		return
	}
	recent, err := s.opts.Sessions.List(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load sessions", http.StatusInternalServerError)
		return
	}
	if len(recent) > recentSessionsLimit {
		recent = recent[:recentSessionsLimit]
	}
	funnel, err := s.opts.Analytics.VocabFunnel(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load vocabulary funnel", http.StatusInternalServerError)
		return
	}
	trends, err := s.opts.Analytics.WeaknessTrends(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load weakness trends", http.StatusInternalServerError)
		return
	}
	calibration, err := s.opts.Analytics.ConfidenceCalibration(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load confidence calibration", http.StatusInternalServerError)
		return
	}

	Render(w, r, "home", map[string]any{
		"Title":          "JLP",
		"Identity":       ident,
		"Stats":          stats,
		"RecentSessions": recent,
		"Funnel":         funnel,
		"WeaknessTrends": weaknessTrendVMs(trends),
		"Calibration":    calibration,
	})
}
