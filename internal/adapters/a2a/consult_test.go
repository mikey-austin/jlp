package a2a

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/agents"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// The delegated run must execute as the SPECIALIST, not as whoever
// consulted it. This is the property that made delegating through the
// A2A dispatch worth doing rather than calling the runner directly: a
// caller that could name the agent could name a more privileged one.
func TestConsultRunsAsTheSpecialistNotTheCaller(t *testing.T) {
	srv, spy := serverWithSpyRunner(t)

	if _, err := srv.Consult(context.Background(), "mikey", "review_writing", "これを見て"); err != nil {
		t.Fatalf("consult: %v", err)
	}
	if spy.lastInput.Agent != skillDefs["review_writing"].Agent {
		t.Errorf("ran as agent %q, want %q — a delegated run must take the specialist's permissions, not the coordinator's",
			spy.lastInput.Agent, skillDefs["review_writing"].Agent)
	}
	if spy.lastInput.Agent == coordinatorAgent {
		t.Error("the delegated run kept the coordinator's agent, so it would inherit the coordinator's allowlist")
	}
}

// The learner is passed explicitly and must survive the hop. Taking it
// from anywhere else — a token, a default, the process — is how one
// learner's history reaches another's answer.
func TestConsultCarriesTheRequestingLearner(t *testing.T) {
	srv, spy := serverWithSpyRunner(t)

	if _, err := srv.Consult(context.Background(), "learner-b", "analyse_learner", "私の弱点は"); err != nil {
		t.Fatalf("consult: %v", err)
	}
	if got := string(spy.lastInput.Identity); got != "learner-b" {
		t.Errorf("delegated run acted as %q, want learner-b", got)
	}
}

// Depth is normally bounded by the allowlist (only the coordinator holds
// the tool). This is the backstop for the day that grant is widened
// without anyone thinking about recursion.
func TestAConsultedRunMayNotConsultAgain(t *testing.T) {
	srv, _ := serverWithSpyRunner(t)

	_, err := srv.Consult(withDelegationDepth(context.Background()), "mikey", "analyse_learner", "again")
	if !errors.Is(err, agents.ErrDelegationDepth) {
		t.Fatalf("err = %v, want ErrDelegationDepth", err)
	}
}

// The coordinator answers by consulting others; consulting it would be
// a loop the one-level depth guard does not close on its own.
func TestTheCoordinatorCannotBeConsulted(t *testing.T) {
	srv, _ := serverWithSpyRunner(t)

	_, err := srv.Consult(context.Background(), "mikey", "coordinate", "help")
	if !errors.Is(err, agents.ErrUnknownSkill) {
		t.Fatalf("err = %v, want ErrUnknownSkill", err)
	}
}

func TestConsultRejectsUnknownSkillsAndEmptyQuestions(t *testing.T) {
	srv, _ := serverWithSpyRunner(t)

	if _, err := srv.Consult(context.Background(), "mikey", "not_a_skill", "x"); !errors.Is(err, agents.ErrUnknownSkill) {
		t.Errorf("unknown skill: err = %v, want ErrUnknownSkill", err)
	}
	if _, err := srv.Consult(context.Background(), "mikey", "analyse_learner", "   "); err == nil {
		t.Error("an empty question started a full agent run")
	}
}

// A delegate answers one question, not an open brief — and it is a
// second run the learner did not directly ask to pay for.
func TestDelegatedRunsGetASmallerTurnBudget(t *testing.T) {
	srv, spy := serverWithSpyRunner(t)

	if _, err := srv.Consult(context.Background(), "mikey", "plan_lesson", "次は？"); err != nil {
		t.Fatalf("consult: %v", err)
	}
	if spy.lastInput.MaxTurns != consultMaxTurns {
		t.Errorf("MaxTurns = %d, want %d", spy.lastInput.MaxTurns, consultMaxTurns)
	}
	if spy.lastInput.MaxTurns == 0 {
		t.Error("a delegated run inherited the default turn budget, so a consultation costs as much as a top-level question")
	}
}

// A delegate's answer is read by a model, not a browser. Telling it that
// cards will be rendered would have it write "as shown above" into text
// nothing renders.
func TestDelegatedRunsGetNoRenderingNote(t *testing.T) {
	srv, spy := serverWithSpyRunner(t)

	if _, err := srv.Consult(context.Background(), "mikey", "analyse_learner", "分析して"); err != nil {
		t.Fatalf("consult: %v", err)
	}
	if strings.Contains(spy.lastInput.System, "DISPLAY:") {
		t.Errorf("the delegate was told its output would be rendered:\n%s", spy.lastInput.System)
	}
}

// spyRunner records what the adapter asked to run without running it.
// What matters here is the REQUEST — which agent, whose identity, what
// budget — not what a model would have replied.
type spyRunner struct{ lastInput agentrun.RunInput }

func (s *spyRunner) Run(_ context.Context, in agentrun.RunInput) (agentrun.RunOutput, error) {
	s.lastInput = in
	return agentrun.RunOutput{RunID: "run-child", Text: "specialist says so"}, nil
}

func serverWithSpyRunner(t *testing.T) (*Server, *spyRunner) {
	t.Helper()
	spy := &spyRunner{}
	return New(spy, tools.NewRegistry(), config.A2A{Enabled: true, Path: "/a2a"}), spy
}

// A model that asks the same specialist twice is looping, not
// investigating — observed doing exactly that, spawning a fresh agent
// run each time while its own run never finished. Each repeat is a full
// second run, so this is the difference between a feature and a bill.
func TestASpecialistIsConsultedAtMostOncePerRun(t *testing.T) {
	srv, spy := serverWithSpyRunner(t)
	ctx := agentrun.WithRunIDForTest(context.Background(), "run-parent")

	if _, err := srv.Consult(ctx, "mikey", "review_writing", "一回目"); err != nil {
		t.Fatalf("first consultation failed: %v", err)
	}
	first := spy.lastInput.Messages[0].Text

	if _, err := srv.Consult(ctx, "mikey", "review_writing", "二回目"); err == nil {
		t.Fatal("the same specialist was consulted twice in one run")
	}
	if spy.lastInput.Messages[0].Text != first {
		t.Error("a second agent run was started for the repeat consultation")
	}

	// A DIFFERENT specialist is still allowed — the rule is one each,
	// not one total.
	if _, err := srv.Consult(ctx, "mikey", "analyse_learner", "別の専門家"); err != nil {
		t.Errorf("consulting a different specialist was refused: %v", err)
	}
}

// The record must not outlive the run it describes, or a long-lived
// process accumulates one entry per question ever asked.
func TestConsultationRecordIsClearedWhenTheRunEnds(t *testing.T) {
	srv, _ := serverWithSpyRunner(t)
	ctx := agentrun.WithRunIDForTest(context.Background(), "run-parent")

	if _, err := srv.Consult(ctx, "mikey", "review_writing", "一回目"); err != nil {
		t.Fatalf("consult: %v", err)
	}
	srv.endRun("run-parent")

	if _, err := srv.Consult(ctx, "mikey", "review_writing", "次の質問"); err != nil {
		t.Errorf("a new run could not consult a specialist the previous run had used: %v", err)
	}
}
