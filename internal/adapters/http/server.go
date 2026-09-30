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
	"github.com/mikeyaustin/jlp/internal/application/apitoken"
	appconversation "github.com/mikeyaustin/jlp/internal/application/conversation"
	"github.com/mikeyaustin/jlp/internal/application/feedback"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	"github.com/mikeyaustin/jlp/internal/application/outcomes"
	"github.com/mikeyaustin/jlp/internal/application/practice"
	appreading "github.com/mikeyaustin/jlp/internal/application/reading"
	"github.com/mikeyaustin/jlp/internal/application/sessions"
	appsettings "github.com/mikeyaustin/jlp/internal/application/settings"
	appspeech "github.com/mikeyaustin/jlp/internal/application/speech"
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
	// APITokens authenticates non-browser clients on /api/v1 and /a2a
	// (see APIAuth). nil when there is no database, in which case those
	// routes accept a session and nothing else.
	APITokens *apitoken.Service
	Sessions  *sessions.Service
	Writing   *appwriting.Service
	Events    storage.LearningEventRepository
	// Feedback drives the workspace's フィードバックを取得 button and
	// correction accept/reject buttons (Task 13): it wraps the Teacher
	// agent with authorization, persistence, and learning events.
	Feedback *feedback.Service
	// Analytics drives the home dashboard's stat tiles and よくある間違い
	// list (Task 14).
	Analytics *analytics.Service
	// Outcomes drives /outcomes (Phase 4 Task 9, PRD §52/§72/§73) — the
	// capstone view: did being corrected lead to better later
	// production? An application-layer analyser rather than a raw
	// repository (unlike Priorities/Observations/Retrieval above)
	// because every classification and every honest "not enough data
	// yet" lives in that package, and an HTTP handler must never
	// re-derive them.
	Outcomes *outcomes.Analyser
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
	// Retrieval backs the /learner page's 復習キュー (review queue) table
	// (Task 7, PRD §54): List(N), the same "raw repository for a GET
	// listing page" split Priorities/Observations above already use —
	// application/retrieval.Scheduler's own mutating side
	// (RecordOutcome) is reached only through the event-bus Consumer
	// (see main.go), never from an HTTP handler.
	Retrieval storage.RetrievalRepository
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
	// Reading drives the 読解 pipeline (/reading pages and
	// /api/v1/reading/...): article submission, study editions, EPUB
	// download and Send to Kindle. A service rather than a raw
	// repository even for the reads, because "which editions exist" is
	// joined with delivery state and known-vocabulary lookups that must
	// not be re-derived in a handler.
	Reading *appreading.Service
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
	// A2AChatURL is where clients/a2a-chat is deployed. Empty renders no
	// nav link — see config.A2A.ChatURL for why that is the default.
	A2AChatURL string
	// PromptNames is every prompt name this process routes, from the
	// composition root — the only place that knows both the structured
	// and tool-calling lists. It drives /settings/agents and bounds what
	// a pin may name.
	PromptNames []string
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
	// Speech drives POST /speech/transcribe (Phase 4 Task 8, PRD §66):
	// always non-nil (main.go constructs it unconditionally), but its
	// own internal recognizer may be nil when config.Speech.STTURL is
	// unset — see application/speech.Service's "recognizer may be nil"
	// doc comment and ErrNotConfigured, which speechTranscribe maps to
	// a 503.
	Speech *appspeech.Service
	// SpeechEnabled gates the conversation pane's 🎙 録音 button, the
	// same way AnkiConnectEnabled gates 「Ankiへ送信」 — the in-tree
	// precedent for "optional integration, so don't render its control
	// until it's configured". True only when main.go was given
	// APP_SPEECH_STTURL and built a real recognizer.
	//
	// Whole-branch review F8/W-2: speech is dormant by DEFAULT, and
	// without this the learner granted microphone permission, recorded a
	// sentence, waited for the upload, and only then read a 503 in the
	// status line. Every other optional integration in this app declines
	// up front instead.
	SpeechEnabled bool
	// AuthRoutes mounts an authenticator's own login endpoints
	// (/auth/login, /auth/callback, /auth/logout) OUTSIDE the
	// authenticated group — they are exactly the requests a learner
	// makes because they have no session yet. nil in static and
	// authelia mode, where nothing here serves a login: static has no
	// login to serve, and in authelia mode the forward-auth proxy owns
	// it. See internal/adapters/oidc.Authenticator.Routes.
	//
	// Passed as a plain http.Handler rather than by importing the oidc
	// adapter here: this package must not learn which authenticator is
	// configured, which is the same reason Auth above is the port type
	// and not a concrete one.
	AuthRoutes http.Handler
	// LogoutPath renders the header's sign-out control when non-empty.
	// Empty in static and authelia mode — a sign-out link that cannot
	// end the session (the proxy would re-authenticate the very next
	// request) is worse than no link.
	LogoutPath string
}

type Server struct {
	http.Server
	opts Options
	// prefetch holds at most one drill built ahead per learner — see
	// practiceprefetch.go. Process state, not a cache of anything
	// authoritative: losing it costs one synchronous build.
	prefetch *drillPrefetcher
}

func NewServer(opts Options) *Server {
	s := &Server{opts: opts, prefetch: newDrillPrefetcher()}
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
	fs := http.StripPrefix("/static/", http.FileServer(http.Dir(assetRoot)))
	// /static/sw.js needs its own exact-path route ahead of the wildcard
	// below: it is rendered from a template rather than read off disk, so
	// that the fingerprinted asset URLs it precaches — and the cache name
	// derived from them — change whenever an asset does. See assets.go.
	r.Get("/static/sw.js", serviceWorker)
	// The extension's shared capture files, for the phone pages. Exact
	// route for the same reason: they come from the binary, not disk.
	r.Get("/static/ext/{name}", s.extAsset)
	// assetHandler translates /static/css/app.<hash>.css back to the file
	// on disk and picks the cache policy: a year and immutable when the
	// hash matches the bytes, revalidate otherwise.
	r.Handle("/static/*", assetHandler(fs))
	// The login endpoints sit here, with /healthz and /static/*, for
	// the obvious reason: a learner arriving at /auth/login has no
	// session, and putting them inside the group below would mean
	// RequireIdentity bouncing them to a login they can never reach.
	// They are also outside CSRFProtect, which only guards the
	// authenticated group — the flow's own state cookie plus the
	// `state` parameter it must match is what protects the callback,
	// and it protects it against an attacker who can't read cookies at
	// all, which an Origin check does not.
	if s.opts.AuthRoutes != nil {
		r.Handle("/auth/*", s.opts.AuthRoutes)
	}
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
		// Phase 4 Task D: soft delete. POST rather than DELETE because
		// the control is a plain <form> inside the confirmation
		// <dialog> and HTML forms cannot issue DELETE — the same
		// verb-suffix shape /lessons/{id}/complete and /anki/{id}/status
		// already use. Both are inside this CSRF-protected group.
		r.Post("/sessions/{id}/delete", s.sessionsDelete)
		r.Post("/sessions/{id}/restore", s.sessionsRestore)
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
		// Phase 4 Task 8: speech recognition feeding the SAME
		// conversation pipeline above — deliberately flat (no {id}
		// session scoping), matching the brief's literal route.
		// record.js posts here, drops the transcript into the
		// conversation form's own input, and the learner submits it
		// through the unchanged /sessions/{id}/conversation route just
		// above — see speech.go's package doc comment for why.
		r.Post("/speech/transcribe", s.speechTranscribe)
		r.Post("/speech/say", s.speechSay)
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
		// /settings is an index of configuration areas; each area owns
		// its own page below. Adding a settings page means adding a card
		// to the index, not hiding a link inside whichever page happened
		// to exist first — which is how the API tokens page ended up
		// buried in the AI model settings.
		r.Get("/settings", s.settingsPage)
		r.Get("/settings/models", s.settingsModelsPage)
		r.Post("/settings/models/{provider}/model", s.settingsSetModel)
		r.Post("/settings/models/{provider}/effort", s.settingsSetEffort)
		r.Post("/settings/models/{provider}/reset", s.settingsReset)
		// API tokens for non-browser clients. These are the credentials
		// APIAuth checks on /api/v1 and /a2a; minting them is a browser
		// action by the learner, so it lives in this session-authenticated
		// group and NOT in the token-authenticated one — a token must
		// never be able to mint another token.
		r.Get("/settings/agents", s.agentsPage)
		r.Post("/settings/agents/{prompt}", s.agentsSet)
		r.Get("/settings/phone", s.settingsPhonePage)
		r.Get("/settings/tokens", s.apiTokensPage)
		r.Post("/settings/tokens", s.apiTokensCreate)
		r.Post("/settings/tokens/{id}/revoke", s.apiTokensRevoke)
		r.Post("/ratings", s.ratingsCreate)
		r.Get("/grammar", s.grammarList)
		r.Get("/grammar/{slug}", s.grammarDetail)
		r.Get("/learner", s.learnerPage)
		// /outcomes: Phase 4 Task 9's capstone (PRD §52/§72/§73) —
		// listed right after /learner because the two are the same
		// question at different depths: /learner shows what the model
		// currently believes, /outcomes shows whether acting on those
		// beliefs changed anything.
		r.Get("/outcomes", s.outcomesPage)
		r.Get("/vocabulary", s.vocabularyPage)
		// Phase 4 Task D: soft delete — see /sessions/{id}/delete above.
		r.Post("/vocabulary/{id}/delete", s.vocabularyDelete)
		r.Post("/vocabulary/{id}/restore", s.vocabularyRestore)
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
		// Phase 4 Task D: soft delete — see /sessions/{id}/delete above.
		r.Post("/lessons/{id}/delete", s.lessonsDelete)
		r.Post("/lessons/{id}/restore", s.lessonsRestore)

		// 読解: article → AI study edition → EPUB / Send to Kindle. The
		// JSON surface for the extension is in the API group below; these
		// are the learner's own pages. Deletes are per ARTICLE (an
		// article and every edition of it), hence the /articles/ prefix,
		// while everything else is per edition.
		if s.opts.Reading != nil {
			r.Get("/reading", s.readingList)
			r.Post("/reading", s.readingCreate)
			// Before /reading/{id}, which would otherwise read "capture" as an id.
			r.Get("/reading/capture", s.readingCapturePage)
			r.Get("/reading/share", s.readingSharePage)
			r.Post("/reading/share", s.readingSharePage)
			r.Get("/reading/{id}", s.readingDetail)
			r.Get("/reading/{id}/epub", s.readingEpub)
			r.Post("/reading/{id}/deliver", s.readingAction(s.readingDeliver))
			r.Post("/reading/{id}/regenerate", s.readingAction(s.readingRegenerate))
			r.Post("/reading/{id}/vocabulary", s.readingAction(s.readingAddVocabulary))
			r.Post("/reading/{id}/anki", s.readingAction(s.readingAnki))
			r.Get("/reading/articles/{id}/figures/{n}", s.readingFigure)
			r.Post("/reading/articles/{id}/delete", s.readingDelete)
			r.Post("/reading/articles/{id}/restore", s.readingRestore)
		}
	})

	// The non-browser surfaces. These used to sit in the group above, on
	// RequireIdentity, which worked while a session cookie was the only
	// way in. Under OIDC it stopped working for the callers these routes
	// exist for: a reader app has no browser to run a login flow in, so
	// it can only ever be told 401.
	//
	// APIAuth accepts either — the learner's session, exactly as before,
	// or an API token scoped to what that client was minted to do. It is
	// a separate group precisely so token access CANNOT reach the HTML
	// routes above: a token for a reader app must not be able to read a
	// learner's correction history just because it can add words.
	//
	// CSRFProtect stays for the same reason it was here before: these are
	// also reachable by a browser riding its cookie. A non-browser caller
	// sends no Origin header and passes it unchanged (see csrf.go).
	r.Group(func(r chi.Router) {
		r.Use(APIAuth(s.opts.APITokens, s.opts.Auth, s.opts.Identities))
		r.Use(CSRFProtect())

		// /api/v1: the versioned JSON API (Task 16). It shares the exact
		// same application services as the HTML routes above — no new
		// application-layer code — proving HTMX isn't the domain boundary
		// (PRD §38).
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

			// 読解 for the Chrome extension (reading:write — see
			// apitoken.ScopeReadingWrite and requiredScope).
			if s.opts.Reading != nil {
				r.Post("/reading/articles", s.apiReadingSubmit)
				r.Get("/reading/editions/{id}", s.apiReadingEdition)
				r.Post("/reading/editions/{id}/deliver", s.apiReadingDeliver)
			}
		})

		// Phase 4 Task 3: the A2A protocol adapter (PRD §29/§30, Rule
		// 13), mounted INSIDE this authenticated group deliberately —
		// same authenticated + CSRFProtect posture as every route
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
			mountA2A(r, s.opts.A2APath, withLongWriteDeadline(withA2AIdentity(s.opts.A2A.Routes())))
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
	s.render(w, r, "offline", map[string]any{
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

	s.render(w, r, "home", map[string]any{
		"Title":          "JLP",
		"Identity":       ident,
		"Stats":          stats,
		"RecentSessions": recent,
		"Funnel":         funnel,
		"WeaknessTrends": weaknessTrendVMs(trends),
		"Calibration":    calibration,
	})
}

// a2aWriteDeadline bounds one blocking A2A request.
//
// An agent run is minutes, not seconds: a coordinate request that
// consults a specialist is two full agent runs, each up to ten turns of
// model calls. The server's 60s WriteTimeout closed the connection while
// the run continued, so the caller lost an answer it had already paid
// for.
const a2aWriteDeadline = 10 * time.Minute

// withLongWriteDeadline extends the write deadline for the routes it
// wraps, leaving the server's own 60s in place everywhere else — that
// timeout is a real protection for the HTML app, and it should not be
// relaxed globally to accommodate one long-running surface.
func withLongWriteDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(a2aWriteDeadline)); err != nil {
			// Not fatal: a ResponseWriter that cannot carry a deadline
			// (a wrapper that does not implement it) still serves the
			// request — it just keeps the server default, which is the
			// behaviour this replaced.
			slog.Warn("a2a: could not extend the write deadline; a long run may be cut off", "err", err)
		}
		next.ServeHTTP(w, r)
	})
}
