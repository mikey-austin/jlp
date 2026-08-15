package postgres

import (
	"testing"
	"time"
)

// TestIsoWeekStartsMondayAlignedAndOldestFirst pins isoWeekStarts'
// exact output against two hand-computed anchors — a mid-week
// Wednesday and a boundary Sunday — so its Monday-alignment (matching
// SubjectOccurrencesByWeek's `date_trunc('week', ... AT TIME ZONE
// 'UTC')` bucketing) and oldest-first ordering can't silently drift.
func TestIsoWeekStartsMondayAlignedAndOldestFirst(t *testing.T) {
	utc := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}

	tests := []struct {
		name string
		now  time.Time
		n    int
		want []time.Time
	}{
		{
			name: "mid-week Wednesday anchor",
			// 2024-01-10 is a Wednesday; its ISO week's Monday is 2024-01-08.
			now: utc(2024, time.January, 10),
			n:   3,
			want: []time.Time{
				utc(2023, time.December, 25),
				utc(2024, time.January, 1),
				utc(2024, time.January, 8),
			},
		},
		{
			name: "Sunday anchor (last day of its ISO week)",
			// 2024-01-14 is a Sunday, still inside the week starting
			// 2024-01-08 — the ISO week boundary, not the calendar week
			// boundary, must govern.
			now: utc(2024, time.January, 14),
			n:   2,
			want: []time.Time{
				utc(2024, time.January, 1),
				utc(2024, time.January, 8),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isoWeekStarts(tt.now, tt.n)
			if len(got) != len(tt.want) {
				t.Fatalf("isoWeekStarts(%v, %d) = %v, want %v", tt.now, tt.n, got, tt.want)
			}
			for i := range got {
				if !got[i].Equal(tt.want[i]) {
					t.Errorf("isoWeekStarts(%v, %d)[%d] = %v, want %v", tt.now, tt.n, i, got[i], tt.want[i])
				}
				if got[i].Location() != time.UTC {
					t.Errorf("isoWeekStarts(%v, %d)[%d] location = %v, want UTC", tt.now, tt.n, i, got[i].Location())
				}
			}
		})
	}
}
