package cleaner

type RunReport struct {
	DryRun               bool       `json:"dry_run"`
	SyncTrackerLabel     bool       `json:"sync_tracker_label"`
	TrackerLabelsPlanned int        `json:"tracker_labels_planned"`
	TrackerLabelsChanged int        `json:"tracker_labels_changed"`
	RunID                string     `json:"run_id"`
	Mode                 string     `json:"mode"`
	Version              string     `json:"app_version"`
	Status               string     `json:"status"`
	Scanned              int        `json:"scanned"`
	Planned              int        `json:"planned"`
	Attempted            int        `json:"attempted"`
	Skipped              int        `json:"skipped"`
	WouldDelete          int        `json:"would_delete"`
	Removed              int        `json:"removed"`
	Failed               int        `json:"failed"`
	Unknown              int        `json:"unknown"`
	NotAttempted         int        `json:"not_attempted"`
	Errors               int        `json:"errors"`
	DurationSeconds      float64    `json:"duration_seconds"`
	Decisions            []Decision `json:"decisions"`
}
