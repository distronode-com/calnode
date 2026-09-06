package handler

import (
	"context"
	"time"
)

// Test-only bridges. This file is a _test.go, so these identifiers exist for
// handler_test and are absent from any real build of the package — the alternative,
// exporting replaceReminderJobs, would widen the production API to serve one test.

// ReplaceReminderJobsForTest calls the unexported reminder replacement, so the external
// test package can drive the exact statement the reschedule path runs rather than a
// copy of it. A test that re-typed the SQL would pass with the production code broken.
func ReplaceReminderJobsForTest(h *Handler, ctx context.Context, bookingID, etID string, newStart time.Time) error {
	return h.replaceReminderJobs(ctx, bookingID, etID, newStart)
}
