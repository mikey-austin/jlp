// Package notifications defines the port through which application
// code sends a learner an out-of-band notification — today, the weekly
// email summary (Phase 3 Task 5, PRD §21, §65); a later channel
// (Slack, SMS) would satisfy the same Notifier interface without
// application/summary.Service ever depending on how delivery actually
// happens. This is deliberately the smallest possible shape: a
// recipient, a subject, and a plain-text body — no headers, no
// transport-specific metadata (From, MIME type, attachments), so any
// future implementation stays free to decide those for itself. See
// internal/adapters/smtp for the one implementation this task ships.
package notifications

import "context"

// Notification is one message to deliver: To identifies the recipient
// in whatever form the concrete Notifier expects (an email address for
// internal/adapters/smtp); Subject and TextBody are both already fully
// composed by the caller — application/summary.Service builds both
// from a validated weekly_summary.v1 response before calling Send, so
// a Notifier never needs to know anything about summaries, schemas, or
// AI at all.
type Notification struct {
	To, Subject, TextBody string
}

// Notifier sends one Notification. Send's error is a real delivery
// failure (the caller has nothing durable to fall back on the way a
// persisted DB row would be) — implementations must not swallow a
// failed send.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
}
