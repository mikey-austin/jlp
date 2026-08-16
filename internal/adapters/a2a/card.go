package a2a

import (
	"net/http"
	"strings"

	"github.com/mikeyaustin/jlp/internal/tools"
)

// cardVersion is the AGENT's version (AgentCard.version) — JLP's own
// release identity as an agent, bumped when what these skills do
// changes. It is NOT the protocol version: that is per-interface, and
// is the `protocolVersion` const in types.go.
const cardVersion = "2.0.0"

// textMode is the one media type this adapter speaks, in either
// direction. Advertised on the card as both defaults (and repeated per
// skill, since a skill's modes override the card's) rather than left
// empty: a client that asks what it may send should get an answer.
const textMode = "text/plain"

// handleAgentCard serves the agent card at
// {path}/.well-known/agent-card.json — a pure read, computed fresh on
// every request (no caching) since it's cheap, always reflects the
// Registry's current allowlists, and has to be built against THIS
// request's own host anyway (see interfaceURL).
//
// One deployment note that costs people an afternoon otherwise: the
// official client resolves the card with
// `new URL(".well-known/agent-card.json", baseUrl)`, and that path is
// RELATIVE. Given a baseUrl of "http://host/a2a" — no trailing slash —
// URL resolution drops the last segment and fetches
// "http://host/.well-known/agent-card.json", which is not where this
// card lives. Callers must pass either a trailing slash
// ("http://host/a2a/") or the full card path explicitly. docs/api/a2a.md
// spells this out with both working forms.
func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	skills := make([]AgentSkill, 0, len(skillOrder))
	for _, id := range skillOrder {
		def := skillDefs[id]
		skills = append(skills, AgentSkill{
			ID:          id,
			Name:        def.Name,
			Description: s.descriptionFor(def),
			Tags:        append(append([]string(nil), def.Tags...), toolTags(s.reg, def.Agent)...),
			Examples:    def.Examples,
			InputModes:  []string{textMode},
			OutputModes: []string{textMode},
		})
	}
	writeJSON(w, http.StatusOK, AgentCard{
		Name:        "JLP",
		Description: "Japanese Learning Platform — exposes selected agents as A2A skills, each running through the exact same tool-registry permissions its local path uses (PRD §30, Rule 13): a remote caller gets no privilege a local agent lacks.",
		Version:     cardVersion,
		SupportedInterfaces: []AgentInterface{{
			URL:             s.interfaceURL(r),
			ProtocolBinding: protocolBindingJSONRPC,
			ProtocolVersion: protocolVersion,
		}},
		// Honest, and both stated rather than omitted: this adapter
		// serves neither SendStreamingMessage nor any push-notification
		// method (see jsonrpc.go's dispatch table). An agent card is a
		// contract with strangers — advertising a capability we'd then
		// answer -32601 to is worse than advertising none.
		Capabilities:       AgentCapabilities{Streaming: false, PushNotifications: false},
		DefaultInputModes:  []string{textMode},
		DefaultOutputModes: []string{textMode},
		Skills:             skills,
	})
}

// interfaceURL builds the absolute URL of this adapter's JSON-RPC
// endpoint, for the card's supportedInterfaces entry. It has to be
// absolute — the client feeds it straight to `fetch` as the transport's
// endpoint — and this package has no configured notion of JLP's public
// origin, so it is derived from the request that asked for the card.
//
// r.Host (and X-Forwarded-Proto, honoured so a TLS-terminating proxy
// doesn't produce an http:// card for an https:// deployment) are
// client-influenced values. That is not a privilege boundary here: the
// card is a self-description returned only to the caller that asked for
// it, so the most a spoofed Host achieves is telling that one caller to
// send its own next request somewhere else. Nothing server-side is
// keyed on it.
func (s *Server) interfaceURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		// A proxy chain may append rather than replace; the first entry
		// is the one nearest the client.
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		if fwd = strings.TrimSpace(fwd); fwd != "" {
			scheme = fwd
		}
	}
	return scheme + "://" + r.Host + s.cfg.Path + rpcRoute
}

// toolTags projects the LIVE internal/tools.Registry allowlist for
// agent onto `tool:<name>` skill tags.
//
// This is the Rule 13 (PRD §30) transparency the previous, bespoke card
// carried in a `tools` field — preserved here through the change of
// protocol rather than dropped, but relocated, because v1.0's
// AgentSkill has no free-form field except `tags` (no metadata map, and
// the `input_schema`/`output_schema` pair the old shape used is gone
// from the spec entirely). Tags are defined as "a set of keywords
// describing the skill's capabilities", which is exactly what "this
// skill may call get_learning_priorities" is.
//
// Read straight from the Registry on every request, never a static
// claim in this file that could silently drift from the real permission
// the agent's local path has: a remote caller can see, up front, that
// this skill reaches no more of the learner's state than exactly these
// tools allow — and if there are no tool: tags at all, the skill
// currently runs with no tool access whatsoever (still a valid, if less
// useful, run: see docs/api/a2a.md).
func toolTags(reg *tools.Registry, agent string) []string {
	defs := reg.DefsFor(agent)
	tags := make([]string, 0, len(defs))
	for _, d := range defs {
		tags = append(tags, "tool:"+d.Name)
	}
	return tags
}
