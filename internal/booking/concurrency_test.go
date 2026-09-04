package booking_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/booking"
	"github.com/calnode/calnode/internal/dbtest"
)

// TestCreate_concurrentPartialOverlap_postgres is the test for the guarantee
// pg_advisory_xact_lock replaces.
//
// The two slots deliberately OVERLAP without SHARING a start time. That is the case
// no index catches: idx_bookings_no_double is UNIQUE(host_id, start_at), so 10:00
// and 10:15 are two distinct keys and both inserts satisfy it. The only thing
// standing between them is hostBusy's read, and on a multi-connection pool two
// transactions can both take that read before either writes. If the lock is not
// held, this test double-books.
//
// It is skipped on SQLite, where the race is not reachable: db.SetMaxOpenConns(1)
// means the second transaction cannot begin until the first has committed.
func TestCreate_concurrentPartialOverlap_postgres(t *testing.T) {
	database := dbtest.RequirePostgres(t)
	svc := booking.New(database)
	hostID := seedHost(t, database)
	etID := seedEventType(t, database, hostID)

	// Enough rounds that a lost race is very unlikely to go unseen. One round is
	// not evidence: two goroutines miss each other's window often enough that an
	// unlocked build passes a single round most of the time.
	const rounds = 40

	for round := 0; round < rounds; round++ {
		// A fresh, non-overlapping window per round, so a round is independent of
		// every earlier round's surviving booking.
		base := slot(0, 0).Add(time.Duration(round) * time.Hour)
		first := [2]time.Time{base, base.Add(30 * time.Minute)}
		second := [2]time.Time{base.Add(15 * time.Minute), base.Add(45 * time.Minute)}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, window := range [2][2]time.Time{first, second} {
			wg.Add(1)
			go func(i int, from, to time.Time) {
				defer wg.Done()
				<-start // line both up so the transactions truly overlap
				_, errs[i] = svc.Create(context.Background(), booking.CreateParams{
					EventTypeID: etID,
					HostIDs:     []string{hostID},
					StartAt:     from,
					EndAt:       to,
					Organizer: booking.Attendee{
						Name:  "Alice",
						Email: "alice@example.com",
					},
				})
			}(i, window[0], window[1])
		}
		close(start)
		wg.Wait()

		var created int
		for i, err := range errs {
			switch {
			case err == nil:
				created++
			case errors.Is(err, booking.ErrDoubleBooked):
				// the expected loser
			default:
				t.Fatalf("round %d: booking %d: unexpected error: %v", round, i, err)
			}
		}
		if created != 1 {
			t.Fatalf("round %d: %d of 2 overlapping bookings were created; want exactly 1 (errors: %v, %v)",
				round, created, errs[0], errs[1])
		}
	}

	// And the database agrees: one booking per round, never two in a window.
	var n int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM bookings WHERE host_id = ? AND status != 'cancelled'`, hostID).Scan(&n); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if n != rounds {
		t.Errorf("bookings for host = %d; want %d (one per round)", n, rounds)
	}
}
