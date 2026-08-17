package httpx

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// aiRequestsPageLimit caps the /ai observability page at the identity's
// most recent 50 requests — an operator's audit view, not a paginated
// archive.
const aiRequestsPageLimit = 50

// aiRequestView is one ai_requests row as the "ai" page's table renders
// it. Rating is 0 when the identity hasn't rated the request; the
// template renders that as an em dash rather than a bare 0.
type aiRequestView struct {
	ID, Provider, Model, PromptName, PromptVersion string
	LatencyMS, InputTokens, OutputTokens           int
	CostUSD                                        float64
	Success                                        bool
	Rating                                         int
}

// aiSummaryView is the /ai page's top-level stat-grid — the design's AI
// dashboard screen composition (jlp-design-system.html:595-599) shows
// four request-weighted aggregates above the tables. All four are real
// aggregates over ProviderStats (already fetched for プロバイダー比較
// below), not invented numbers: TotalRequests/TotalCostUSD are plain
// sums; SuccessRate is request-weighted
// (sum(Requests*SuccessRate)/sum(Requests)); AvgRating is
// request-weighted too, but only over providers that HAVE a rating
// (AvgRating==0 is this codebase's existing "unrated" sentinel — see
// aiRequestView.Rating/toAIRequestView — so an unrated provider is
// skipped in both the numerator and denominator rather than dragging
// the average toward 0).
//
// SuccessRate/AvgRating are meaningless divide-by-zero candidates when
// their denominator is empty, so the template gates on TotalRequests
// (for SuccessRate) and a nonzero AvgRating (for AvgRating, same
// sentinel convention already used elsewhere) rather than this struct
// pre-formatting a dash — that keeps display formatting in the
// template via the existing percent/printf helpers, consistent with
// every other numeric field on this page. TotalCostUSD has no
// zero-state: summing zero rows is a well-defined 0.0, rendered the
// same way the existing cost column always does.
type aiSummaryView struct {
	TotalRequests int
	SuccessRate   float64 // 0..1, weighted by Requests; meaningless when TotalRequests==0
	AvgRating     float64 // weighted avg over rated providers only; 0 when none rated
	TotalCostUSD  float64
}

func toAISummaryView(stats []storage.ProviderStats) aiSummaryView {
	var totalRequests, ratedRequests int
	var successWeighted, ratingWeighted, totalCost float64
	for _, s := range stats {
		totalRequests += s.Requests
		successWeighted += s.SuccessRate * float64(s.Requests)
		if s.AvgRating > 0 {
			ratingWeighted += s.AvgRating * float64(s.Requests)
			ratedRequests += s.Requests
		}
		totalCost += s.TotalCostUSD
	}

	view := aiSummaryView{TotalRequests: totalRequests, TotalCostUSD: totalCost}
	if totalRequests > 0 {
		view.SuccessRate = successWeighted / float64(totalRequests)
	}
	if ratedRequests > 0 {
		view.AvgRating = ratingWeighted / float64(ratedRequests)
	}
	return view
}

func toAIRequestView(rec storage.AIRequestRecord, rating int) aiRequestView {
	return aiRequestView{
		ID:            rec.ID,
		Provider:      rec.Provider,
		Model:         rec.Model,
		PromptName:    rec.PromptName,
		PromptVersion: rec.PromptVersion,
		LatencyMS:     rec.LatencyMS,
		InputTokens:   rec.InputTokens,
		OutputTokens:  rec.OutputTokens,
		CostUSD:       rec.CostUSD,
		Success:       rec.Success,
		Rating:        rating,
	}
}

// aiRequests handles GET /ai: the observability page (PRD §26), three
// sections. プロバイダー比較 and プロンプト品質 are aggregate quality
// numbers computed server-side (Options.AIQuality, over the identity's
// full ai_requests/ai_ratings history); 最近のリクエスト is the
// pre-existing last-50 table listing the caller's ai_requests alongside
// their own rating of each, if any.
func (s *Server) aiRequests(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())

	recs, err := s.opts.AIRequests.List(r.Context(), ident.ID, aiRequestsPageLimit)
	if err != nil {
		http.Error(w, "could not load ai requests", http.StatusInternalServerError)
		return
	}
	ids := make([]string, len(recs))
	for i, rec := range recs {
		ids[i] = rec.ID
	}
	ratings, err := s.opts.AIRatings.ForRequests(r.Context(), ident.ID, ids)
	if err != nil {
		http.Error(w, "could not load ratings", http.StatusInternalServerError)
		return
	}

	views := make([]aiRequestView, 0, len(recs))
	for _, rec := range recs {
		views = append(views, toAIRequestView(rec, ratings[rec.ID]))
	}

	byProvider, err := s.opts.AIQuality.ByProvider(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load provider stats", http.StatusInternalServerError)
		return
	}
	byPrompt, err := s.opts.AIQuality.ByPrompt(r.Context(), ident.ID)
	if err != nil {
		http.Error(w, "could not load prompt stats", http.StatusInternalServerError)
		return
	}

	s.render(w, r, "ai", map[string]any{
		"Title":         "AI Requests",
		"Identity":      ident,
		"Requests":      views,
		"ProviderStats": byProvider,
		"PromptStats":   byPrompt,
		"Summary":       toAISummaryView(byProvider),
	})
}

// ratingsCreate handles POST /ratings, the feedback partial's star
// widget: form fields ai_request_id/rating. One rating per
// (ai_request_id, identity) — rating the same request again replaces
// the previous value (storage.AIRatingRepository.Upsert). Responds 204
// with no body since the widget updates its own state client-side via
// Alpine rather than expecting a rendered fragment back.
func (s *Server) ratingsCreate(w http.ResponseWriter, r *http.Request) {
	ident, _ := IdentityFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	rating, err := strconv.Atoi(r.FormValue("rating"))
	if err != nil || rating < 1 || rating > 5 {
		http.Error(w, "rating must be an integer between 1 and 5", http.StatusBadRequest)
		return
	}
	// Validated here, not left to surface as a 500 from the repository's
	// UUID parse: a missing/malformed ai_request_id is a client error,
	// the same shape as the rating-range check just above.
	requestID := r.FormValue("ai_request_id")
	if _, err := uuid.Parse(requestID); err != nil {
		http.Error(w, "ai_request_id must be a valid id", http.StatusBadRequest)
		return
	}

	err = s.opts.AIRatings.Upsert(r.Context(), storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: requestID,
		IdentityID:  ident.ID,
		Rating:      rating,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		// Upsert verifies ai_request_id belongs to ident.ID via a join
		// (see postgres.AIRatingRepository.Upsert): a request that
		// doesn't exist, or exists but belongs to someone else, comes
		// back as ErrNotFound — the caller can't tell those apart, by
		// design, same as every other identity-scoped write in this app.
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not save rating", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
