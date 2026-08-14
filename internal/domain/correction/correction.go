package correction

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Type represents the correction type (grammar, vocabulary, etc)
type Type string

// Severity represents the severity level of a correction
type Severity string

// Explanation holds explanations in Japanese and English
type Explanation struct {
	JA string
	EN string
}

// Correction represents a single correction suggestion
type Correction struct {
	ID          string
	Original    string
	Replacement string
	Type        Type
	Severity    Severity
	Explanation Explanation
	Concepts    []string
}

// Result represents the result of applying corrections to a selection
type Result struct {
	Original    string
	Corrected   string
	Corrections []Correction
}

// ErrNoMatch is returned when no corrections were found in the selection
var ErrNoMatch = errors.New("correction original not found in selection")

// NewResult applies the given corrections to the selection string in order.
// Corrections are applied left-to-right using substring matching on the remaining text.
// Corrections whose Original text is not found are skipped.
// Each applied correction is assigned a UUID.
// Returns ErrNoMatch-wrapped error only if corrections were provided but none applied.
func NewResult(selection string, corrections []Correction) (Result, error) {
	result := Result{
		Original:    selection,
		Corrected:   selection,
		Corrections: []Correction{},
	}

	// Handle empty corrections list - no error, just return unchanged
	if len(corrections) == 0 {
		return result, nil
	}

	// Process corrections in order
	applied := 0
	remaining := selection
	cursor := 0 // byte position in original selection

	for _, corr := range corrections {
		// Find the original text in the remaining part
		idx := strings.Index(remaining, corr.Original)
		if idx == -1 {
			// Correction not found, skip it
			continue
		}

		// Apply the correction
		before := remaining[:idx]
		after := remaining[idx+len(corr.Original):]

		// Build the corrected version
		correctedPart := before + corr.Replacement + after

		// Assign a UUID to this correction
		corrID := uuid.New().String()

		// Add to applied corrections
		appliedCorr := corr
		appliedCorr.ID = corrID
		result.Corrections = append(result.Corrections, appliedCorr)

		// Update for next iteration
		remaining = correctedPart
		cursor += len(before) + len(corr.Replacement)
		applied++
	}

	// Update the final corrected result
	result.Corrected = remaining

	// Return error if no corrections were applied but some were provided
	if applied == 0 && len(corrections) > 0 {
		return result, fmt.Errorf("%w", ErrNoMatch)
	}

	return result, nil
}
