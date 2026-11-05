package plugins

// Unit tests for the P8-④ update rollback orchestration (update_rollback.go).
// The plan is fully injected, so these tests drive it with fakes and never
// start a real plugin process (no data dir, no executable, no network).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRollback records every injected step it was asked to run and lets each
// step fail on demand. healthErrs is consumed one entry per startAndWait call;
// an empty slice means "health check passes".
type fakeRollback struct {
	calls      []string
	backupErr  error
	applyErr   error
	restoreErr error
	healthErrs []error
}

func (f *fakeRollback) plan() *updateRollbackPlan {
	return &updateRollbackPlan{
		backupDir: func() error {
			f.calls = append(f.calls, "backup")
			return f.backupErr
		},
		apply: func() error {
			f.calls = append(f.calls, "apply")
			return f.applyErr
		},
		stop: func() error {
			f.calls = append(f.calls, "stop")
			return nil
		},
		startAndWait: func(ctx context.Context) error {
			f.calls = append(f.calls, "health")
			if len(f.healthErrs) > 0 {
				e := f.healthErrs[0]
				f.healthErrs = f.healthErrs[1:]
				return e
			}
			return nil
		},
		restoreOld: func() error {
			f.calls = append(f.calls, "restore")
			return f.restoreErr
		},
		prune: func() { f.calls = append(f.calls, "prune") },
		logInfo: func(msg string, _ map[string]any) {
			f.calls = append(f.calls, "info:"+msg)
		},
		logWarn: func(msg string, _ map[string]any) {
			f.calls = append(f.calls, "warn:"+msg)
		},
	}
}

// callsContain reports whether want appears in the call log in this order,
// with no steps of the "never on this path" set in between it matters not.
func callsContain(calls []string, want string) bool {
	for _, c := range calls {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// TestUpdateRollbackSuccessNoRollback: apply passes, health passes -> no
// rollback, no stop, old backups pruned, nil error.
func TestUpdateRollbackSuccessNoRollback(t *testing.T) {
	f := &fakeRollback{}
	rolledBack, err := f.plan().run(context.Background(), "demo")

	if err != nil {
		t.Fatalf("successful update must not error, got %v", err)
	}
	if rolledBack {
		t.Fatal("successful update must not report a rollback")
	}
	joined := strings.Join(f.calls, "|")
	for _, want := range []string{"backup", "apply", "health", "prune"} {
		if !callsContain(f.calls, want) {
			t.Fatalf("missing step %q in %s", want, joined)
		}
	}
	for _, forbidden := range []string{"restore", "stop", "rollback"} {
		if callsContain(f.calls, forbidden) {
			t.Fatalf("rollback path ran on a healthy update (%s)", joined)
		}
	}
}

// TestUpdateRollbackNewVersionUnhealthyRollsBack: the new version fails its
// health check; the old tree is restored and its health check passes -> old
// version serving, rolledBack=true, nil error.
func TestUpdateRollbackNewVersionUnhealthyRollsBack(t *testing.T) {
	f := &fakeRollback{healthErrs: []error{errors.New("new version crash-loop")}}
	rolledBack, err := f.plan().run(context.Background(), "demo")

	if err != nil {
		t.Fatalf("rollback that restores service must not error, got %v", err)
	}
	if !rolledBack {
		t.Fatal("unhealthy new version must report rolledBack=true")
	}
	joined := strings.Join(f.calls, "|")
	for _, want := range []string{"backup", "apply", "health", "stop", "restore", "health"} {
		if !callsContain(f.calls, want) {
			t.Fatalf("missing step %q in %s", want, joined)
		}
	}
	if callsContain(f.calls, "prune") {
		t.Fatalf("prune must not run on a rolled-back update (%s)", joined)
	}
	if !callsContain(f.calls, "rolled back") {
		t.Fatalf("expected a rollback info log, got %s", joined)
	}
}

// TestUpdateRollbackRollbackFailsReportsBoth: new version unhealthy AND the
// restore fails -> error must name both the new-version failure and the
// rollback failure, and rolledBack must be false (nothing is serving).
func TestUpdateRollbackRollbackFailsReportsBoth(t *testing.T) {
	newErr := errors.New("new version segfault on boot")
	f := &fakeRollback{
		healthErrs: []error{newErr},
		restoreErr: errors.New("cannot remove running new-tree exe"),
	}
	rolledBack, err := f.plan().run(context.Background(), "demo")

	if err == nil {
		t.Fatal("rollback failure must produce an error")
	}
	if rolledBack {
		t.Fatal("when rollback fails, nothing is rolled back")
	}
	msg := err.Error()
	if !strings.Contains(msg, "new version failed health check") {
		t.Fatalf("error must name the new-version failure, got: %s", msg)
	}
	if !strings.Contains(msg, "rollback ALSO failed") {
		t.Fatalf("error must clearly mark the rollback failure, got: %s", msg)
	}
	if !strings.Contains(msg, "cannot remove running new-tree exe") {
		t.Fatalf("error must carry the rollback root cause, got: %s", msg)
	}
	if !strings.Contains(msg, "DOWN") {
		t.Fatalf("error must state the plugin is down, got: %s", msg)
	}
	restoreCount, healthCount := 0, 0
	for _, c := range f.calls {
		switch c {
		case "restore":
			restoreCount++
		case "health":
			healthCount++
		}
	}
	if restoreCount != 1 || healthCount != 1 {
		t.Fatalf("after a failed restore the plan must stop (exactly one restore and one health check), calls=%v", f.calls)
	}

	// 顺序断言：backup -> apply -> health -> stop -> restore，且只回滚一次。
	seq := []string{"backup", "apply", "health", "stop", "restore"}
	idx := 0
	for _, c := range f.calls {
		if idx < len(seq) && c == seq[idx] {
			idx++
		}
	}
	if idx != len(seq) {
		t.Fatalf("step order violated: want %v, got %v", seq, f.calls)
	}
}

// TestUpdateRollbackApplyFailsRestoresFallsBack pins the pre-swap failure
// path: a rejected apply still restores the previous tree and reports the
// original error, so a bad update can never leave <dest> empty.
func TestUpdateRollbackApplyFailsRestoresFallsBack(t *testing.T) {
	applyErr := errors.New("signature verification rejected package")
	f := &fakeRollback{
		applyErr: applyErr,
	}
	rolledBack, err := f.plan().run(context.Background(), "demo")

	if err == nil {
		t.Fatal("apply failure must surface an error")
	}
	if !strings.Contains(err.Error(), "signature verification rejected package") {
		t.Fatalf("error must carry the apply root cause, got: %v", err)
	}
	if !rolledBack {
		t.Fatal("after a failed apply the previous copy must be restored (rolledBack=true)")
	}
	if !callsContain(f.calls, "restore") {
		t.Fatalf("failed apply must restore the backup, calls=%v", f.calls)
	}
	if callsContain(f.calls, "health") {
		t.Fatalf("a pre-swap failure must not reach the health check, calls=%v", f.calls)
	}
}

// TestUpdateRollbackApplyFailsAndRestoreFailsDead pins the worst case for the
// apply path: both apply AND its restore fail -> single error naming both.
func TestUpdateRollbackApplyFailsAndRestoreFailsDead(t *testing.T) {
	f := &fakeRollback{
		applyErr:   errors.New("copy failed"),
		restoreErr: errors.New("backup already gone"),
	}
	rolledBack, err := f.plan().run(context.Background(), "demo")
	if err == nil || rolledBack {
		t.Fatalf("double failure must report error and no rollback, got rolledBack=%v err=%v", rolledBack, err)
	}
	if !strings.Contains(err.Error(), "copy failed") || !strings.Contains(err.Error(), "rollback ALSO failed") || !strings.Contains(err.Error(), "backup already gone") {
		t.Fatalf("error must name both failures, got: %v", err)
	}
}
