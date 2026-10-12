package updater

import (
	"context"
	"errors"
	"testing"
)

func TestCoreUnifiedUpdateStrictVersionsAndReservation(t *testing.T) {
	for _, test := range []struct {
		name, current, latest string
		updated, invalid      bool
	}{
		{"newer", "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", true, false},
		{"same", "mihomo-optimized-v1.0.1", "mihomo-optimized-v1.0.1", false, false},
		{"older", "mihomo-optimized-v1.0.2", "mihomo-optimized-v1.0.1", false, false},
		{"unrecognized", "development", "mihomo-optimized-v1.0.1", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCoreAutomaticFixture(t, test.current, test.latest, nil)
			reservation, err := f.u.ReserveCheckAndUpdate()
			if err != nil {
				t.Fatal(err)
			}
			defer reservation.Close()
			if _, err = f.u.ReserveCheckAndUpdate(); !errors.Is(err, ErrCoreUpdateBusy) {
				t.Fatalf("a second task bypassed the core reservation: %v", err)
			}
			if err = f.u.UpdateContext(context.Background(), f.path, ReleaseChannel, false); !errors.Is(err, ErrCoreUpdateBusy) {
				t.Fatalf("legacy installer bypassed the task reservation: %v", err)
			}
			if f.requests.Load() != 0 {
				t.Fatal("reserving the task performed network work")
			}
			var observed CoreCheckResult
			result, err := reservation.CheckAndUpdate(context.Background(), f.path, f.u.restartHandler, func(value CoreCheckResult) { observed = value })
			if test.invalid != errors.Is(err, ErrCoreVersionUnavailable) || !test.invalid && err != nil {
				t.Fatalf("unexpected update error: %v", err)
			}
			if result.Updated != test.updated || observed.CurrentVersion != test.current || observed.LatestVersion != test.latest {
				t.Fatalf("incorrect remote check: %+v observed %+v", result, observed)
			}
			if test.updated {
				if f.archives.Load() != 1 || f.restarts.Load() != 1 {
					t.Fatalf("newer release not installed/restarted exactly once: archives=%d restart=%d", f.archives.Load(), f.restarts.Load())
				}
			} else {
				f.unchanged(t)
				if f.requests.Load() != 1 || f.archives.Load() != 0 {
					t.Fatalf("equal/older/unknown version downloaded an archive: requests=%d archives=%d", f.requests.Load(), f.archives.Load())
				}
			}
			if _, err := reservation.CheckAndUpdate(context.Background(), f.path, f.u.restartHandler, nil); !errors.Is(err, ErrCoreUpdateBusy) {
				t.Fatalf("consumed task reused its reservation: %v", err)
			}
		})
	}
}

func TestCoreUnifiedUpdateCommitCannotBypassCompetingInstaller(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	reservation, err := f.u.ReserveCheckAndUpdate()
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Close()
	result, err := reservation.CheckAndUpdate(context.Background(), f.path, func(_ string, commit func() error) error {
		// Simulate another installer winning the operation gate while the
		// prepared task waits to enter its configuration transaction.
		other, err := f.u.ReserveCheckAndUpdate()
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		return commit()
	}, nil)
	if !errors.Is(err, ErrCoreUpdateBusy) || result.Updated {
		t.Fatalf("commit bypassed a competing installer: %+v %v", result, err)
	}
	f.unchanged(t)
}

func TestCoreUnifiedReservationReleaseAndCancellation(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	reservation, err := f.u.ReserveCheckAndUpdate()
	if err != nil {
		t.Fatal(err)
	}
	reservation.Close()
	reservation.Close()
	reservation, err = f.u.ReserveCheckAndUpdate()
	if err != nil {
		t.Fatal("cleanup did not release the operation reservation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = reservation.CheckAndUpdate(ctx, f.path, f.u.restartHandler, nil)
	if !errors.Is(err, context.Canceled) || !f.settled() || f.requests.Load() != 0 {
		t.Fatalf("canceled task retained its gate or contacted the server: %v", err)
	}
}

func TestCoreUnifiedReservationDefersAutomaticRestartUntilResourcesFinish(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	reservation, err := f.u.ReserveCheckAndUpdate()
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Close()
	f.u.ConfigureAutoUpdate(true, 24)
	job := f.job()
	if job == nil {
		t.Fatal("fixture did not activate its automatic update policy")
	}
	f.u.automaticUpdate(job, false)
	if f.requests.Load() != 0 || f.restarts.Load() != 0 {
		t.Fatal("automatic core interrupted a task's reserved resource phase")
	}
	reservation.Close()
	if err := f.commitResult(t); err != nil || f.restarts.Load() != 1 {
		t.Fatalf("automatic check did not resume after resources finished: %v", err)
	}
}

func TestCoreUnifiedCancellationAtCommitPreservesExecutable(t *testing.T) {
	f := newCoreAutomaticFixture(t, "mihomo-optimized-v1.0.0", "mihomo-optimized-v1.0.1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.u.beforeCommit = cancel
	reservation, err := f.u.ReserveCheckAndUpdate()
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Close()
	result, err := reservation.CheckAndUpdate(ctx, f.path, f.u.restartHandler, nil)
	if !errors.Is(err, context.Canceled) || result.Updated {
		t.Fatalf("cancellation before commit was ignored: %+v %v", result, err)
	}
	f.unchanged(t)
}
