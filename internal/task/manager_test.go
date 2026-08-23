package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cryp/internal/crypto"
)

func bareManager() *Manager {
	return &Manager{
		running:            make(map[string]*runningTask),
		runningByVaultType: make(map[string]string),
		taskRunKeys:        make(map[string]string),
		quiescingVaults:    make(map[string]struct{}),
		pendingStarts:      make(map[string]*pendingStart),
		resumeVaults:       make(map[string]struct{}),
	}
}

func TestCancelKeepsExclusiveSlotUntilWorkerExits(t *testing.T) {
	m := bareManager()
	ctx, cancel := context.WithCancel(context.Background())
	run := &runningTask{cancel: cancel, done: make(chan struct{}), vaultID: "vault"}
	m.running["task"] = run
	m.runningByVaultType["vault:import"] = "task"
	m.taskRunKeys["task"] = "vault:import"
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(run.done)
		<-ctx.Done()
		m.finishRun("task")
	}()

	if !m.CancelTask("task") {
		t.Fatal("CancelTask returned false")
	}
	if err := m.reserveExclusiveTask("next", "vault", "import"); err == nil {
		t.Fatal("new task acquired the exclusive slot before the old worker exited")
	}
	select {
	case <-run.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after cancellation")
	}
	if _, ok := m.running["task"]; ok {
		t.Fatal("finished task remained registered")
	}
	if err := m.reserveExclusiveTask("next", "vault", "import"); err != nil {
		t.Fatalf("exclusive slot was not released after worker exit: %v", err)
	}
}

func TestOptionalGuardsAreNilReceiverSafe(t *testing.T) {
	var m *Manager
	m.SetThumbEnqueuer(nil)
	m.SetReplaceGuard(nil)
	m.SetReplaceLeaseGuard(nil)
	m.SetImportSourceGuard(nil)
	if m.getThumbEnqueuer() != nil || m.getReplaceGuard() != nil || m.getReplaceLeaseGuard() != nil || m.getImportSourceGuard() != nil {
		t.Fatal("nil manager guard access returned a non-nil value")
	}
}

func TestCreateUploadTaskValidatesMetadataBeforeAdmission(t *testing.T) {
	m := bareManager()
	tests := []struct {
		name       string
		taskID     string
		vaultID    string
		totalFiles int
		totalBytes int64
		want       string
	}{
		{name: "empty task id", taskID: "", vaultID: "vault", want: "task id is empty"},
		{name: "empty vault id", taskID: "task", vaultID: "", want: "vault id is empty"},
		{name: "negative files", taskID: "task", vaultID: "vault", totalFiles: -1, want: "total files cannot be negative"},
		{name: "negative bytes", taskID: "task", vaultID: "vault", totalBytes: -1, want: "total bytes cannot be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := m.CreateUploadTask(tt.taskID, tt.vaultID, tt.totalFiles, tt.totalBytes)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("CreateUploadTask error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCancelTaskToleratesNilLifecycleHandle(t *testing.T) {
	m := bareManager()
	m.running["task"] = nil
	if !m.CancelTask("task") {
		t.Fatal("CancelTask returned false for a registered nil handle")
	}
}

func TestShutdownWaitsForWorkersAndRejectsNewTasks(t *testing.T) {
	m := bareManager()
	ctx, cancel := context.WithCancel(context.Background())
	run := &runningTask{cancel: cancel, done: make(chan struct{}), vaultID: "vault"}
	m.running["task"] = run
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(run.done)
		<-ctx.Done()
		m.finishRun("task")
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := m.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	if err := m.reserveExclusiveTask("next", "vault", "import"); !errors.Is(err, ErrTaskManagerClosed) {
		t.Fatalf("reserve after shutdown = %v, want ErrTaskManagerClosed", err)
	}
}

func TestShutdownTimeoutCanBeFollowedByWait(t *testing.T) {
	m := bareManager()
	release := make(chan struct{})
	run := &runningTask{cancel: func() {}, done: make(chan struct{}), vaultID: "vault"}
	m.running["task"] = run
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(run.done)
		<-release
		m.finishRun("task")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want context deadline", err)
	}

	close(release)
	if err := m.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after timeout = %v", err)
	}
	if _, ok := m.running["task"]; ok {
		t.Fatal("worker remained registered after Wait")
	}
}

func TestWaitRequiresShutdownAdmissionBoundary(t *testing.T) {
	m := bareManager()
	if err := m.Wait(context.Background()); !errors.Is(err, ErrTaskManagerNotClosed) {
		t.Fatalf("Wait on an open manager = %v, want ErrTaskManagerNotClosed", err)
	}
}

func TestStartImportRequiresSourceGuard(t *testing.T) {
	m := bareManager()
	err := m.StartImport("task", "vault", "/vault", &crypto.VaultKeys{MasterKey: make([]byte, 32), MACKey: make([]byte, 32)}, "/tmp", "/", false)
	if err == nil || !strings.Contains(err.Error(), "source guard") {
		t.Fatalf("StartImport error = %v, want source guard configuration error", err)
	}
}

func TestResumeVaultKeepsTombstoneWhileWorkerIsStillExiting(t *testing.T) {
	m := bareManager()
	run := &runningTask{done: make(chan struct{}), vaultID: "vault"}
	m.running["task"] = run
	m.quiescingVaults["vault"] = struct{}{}

	m.ResumeVault("vault")
	if _, blocked := m.quiescingVaults["vault"]; !blocked {
		t.Fatal("ResumeVault reopened a vault with an active worker")
	}

	delete(m.running, "task")
	m.ResumeVault("vault")
	if _, blocked := m.quiescingVaults["vault"]; blocked {
		t.Fatal("ResumeVault did not reopen a settled vault")
	}
}

func TestForgetVaultRemovesSettledTombstone(t *testing.T) {
	m := bareManager()
	m.quiescingVaults["vault"] = struct{}{}
	m.ForgetVault("vault")
	if _, blocked := m.quiescingVaults["vault"]; blocked {
		t.Fatal("ForgetVault retained a settled tombstone")
	}
}

func TestQuiesceVaultWaitsForPendingStart(t *testing.T) {
	m := bareManager()
	if err := m.beginTaskStart("pending", "vault"); err != nil {
		t.Fatalf("beginTaskStart: %v", err)
	}

	quiesced := make(chan error, 1)
	go func() {
		quiesced <- m.QuiesceVault(context.Background(), "vault")
	}()

	// Wait until QuiesceVault has published the tombstone, then prove that it
	// does not report success while the public Start* call is still unwinding.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.RLock()
		_, blocked := m.quiescingVaults["vault"]
		m.mu.RUnlock()
		if blocked {
			break
		}
		time.Sleep(time.Millisecond)
	}
	m.mu.RLock()
	_, blocked := m.quiescingVaults["vault"]
	m.mu.RUnlock()
	if !blocked {
		t.Fatal("QuiesceVault did not publish a vault tombstone")
	}
	select {
	case err := <-quiesced:
		t.Fatalf("QuiesceVault returned before pending start finished: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	m.finishTaskStart("pending")
	select {
	case err := <-quiesced:
		if err != nil {
			t.Fatalf("QuiesceVault returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("QuiesceVault did not observe pending start completion")
	}
}

func TestResumeVaultDefersUntilPendingStartFinishes(t *testing.T) {
	m := bareManager()
	if err := m.beginTaskStart("pending", "vault"); err != nil {
		t.Fatalf("beginTaskStart: %v", err)
	}
	m.mu.Lock()
	m.quiescingVaults["vault"] = struct{}{}
	m.mu.Unlock()

	m.ResumeVault("vault")
	m.mu.RLock()
	_, blocked := m.quiescingVaults["vault"]
	m.mu.RUnlock()
	if !blocked {
		t.Fatal("ResumeVault reopened a vault while a pending start was active")
	}

	m.finishTaskStart("pending")
	m.mu.RLock()
	_, blocked = m.quiescingVaults["vault"]
	m.mu.RUnlock()
	if blocked {
		t.Fatal("pending start completion did not apply deferred ResumeVault")
	}
}

func TestShutdownWaitsForPendingStart(t *testing.T) {
	m := bareManager()
	if err := m.beginTaskStart("pending", "vault"); err != nil {
		t.Fatalf("beginTaskStart: %v", err)
	}

	shutdown := make(chan error, 1)
	go func() {
		shutdown <- m.Shutdown(context.Background())
	}()
	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown returned before pending start finished: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	m.finishTaskStart("pending")
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("Shutdown returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not wait for pending start completion")
	}
}

func TestForgetVaultRetainsPendingTombstoneUntilRetried(t *testing.T) {
	m := bareManager()
	if err := m.beginTaskStart("pending", "vault"); err != nil {
		t.Fatalf("beginTaskStart: %v", err)
	}
	m.mu.Lock()
	m.quiescingVaults["vault"] = struct{}{}
	m.mu.Unlock()

	m.ForgetVault("vault")
	m.mu.RLock()
	_, blocked := m.quiescingVaults["vault"]
	m.mu.RUnlock()
	if !blocked {
		t.Fatal("ForgetVault removed tombstone while pending start was active")
	}

	m.finishTaskStart("pending")
	m.ForgetVault("vault")
	m.mu.RLock()
	_, blocked = m.quiescingVaults["vault"]
	m.mu.RUnlock()
	if blocked {
		t.Fatal("ForgetVault did not clear settled tombstone on retry")
	}
}
