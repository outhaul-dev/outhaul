package core

import "time"

// RestoreMarker is written by restore-state and consumed by the first start,
// which re-creates what it can and shows what is left.
type RestoreMarker struct {
	RestoredAt   time.Time `json:"restored_at"`
	From         string    `json:"from"`
	Pending      bool      `json:"pending"`
	Databases    []string  `json:"databases"`
	Queued       []string  `json:"queued"`
	Failed       []string  `json:"failed"`
	WaitingPush  []string  `json:"waiting_push"`
	PrunedImages int       `json:"pruned_images"`
}
