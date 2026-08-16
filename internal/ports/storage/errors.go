package storage

import "errors"

var ErrNotFound = errors.New("not found")

// ErrDuplicate is returned by a repository write that violated a
// uniqueness constraint the repository itself enforces at the
// database level — e.g. postgres.LearningEventRepository.Append
// translating a learning_events_speech_event_id_idx (a partial unique
// index on evidence->>'speech_event_id') violation. It exists so
// application code can distinguish "this write lost a genuine race"
// from any other storage failure and react accordingly (see
// application/conversation.Service.Say's post-I1 downgrade-to-typed
// handling) without depending on a specific driver's error type.
var ErrDuplicate = errors.New("duplicate")
