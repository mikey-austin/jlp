package main

import (
	"context"
	"fmt"

	slackadapter "github.com/mikeyaustin/jlp/internal/adapters/slack"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/channels"
)

// runSlackSmoke is `make slack-smoke`'s implementation: post ONE fixed
// message to cfg.Slack.SmokeChannel via the same production
// slackadapter.Adapter.Send path the running app itself would use to
// reply to a learner — confirming APP_SLACK_BOTTOKEN actually works
// without going through the full Socket Mode event loop (Start), and
// without needing a database at all (unlike `make send-summary`, this
// needs no postgres.NewPool — chat.postMessage is a bare Web API call).
//
// This command requires real Slack credentials the person running it
// must supply; it is never exercised by `go test` or `make test`/
// `make test-integration` — see README.md's Slack section for the
// exact setup.
func runSlackSmoke(ctx context.Context, cfg config.Config) error {
	if cfg.Slack.BotToken == "" {
		return fmt.Errorf("slack-smoke: APP_SLACK_BOTTOKEN is required (see README.md's Slack section)")
	}
	if cfg.Slack.SmokeChannel == "" {
		return fmt.Errorf("slack-smoke: APP_SLACK_SMOKECHANNEL is required — a Slack channel or user ID to post the test message to")
	}

	adapter := slackadapter.New(cfg.Slack.AppToken, cfg.Slack.BotToken)
	out := channels.Outbound{
		ThreadID: cfg.Slack.SmokeChannel,
		Text:     "✅ JLP slack-smoke: this bot token can post messages.",
	}
	if err := adapter.Send(ctx, out); err != nil {
		return fmt.Errorf("slack-smoke: %w", err)
	}
	return nil
}
