package workload

import "time"

type Job struct {
	JobID       string
	Type        string // light/medium/heavy
	DurationSec int    // Seconds
	ArrivedAt   time.Time
}
