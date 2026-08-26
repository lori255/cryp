package task

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cryp/internal/crypto"
	"cryp/internal/fileindex"
	"cryp/internal/filemeta"
	"cryp/internal/pathguard"
	"cryp/internal/storage"
	"cryp/internal/thumbnail"
)

const rebuildHashCacheThreshold = 256 * 1024 * 1024

// ThumbEnqueuer is called after each file is encrypted to generate thumbnails
type ThumbEnqueuer interface {
	Enqueue(vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string)
}

// MediaDeriver extracts metadata from a completed encrypted file. The API
// layer supplies the implementation because it owns authenticated content
// serving; task workers only orchestrate when derived data is written.
type MediaDeriver func(ctx context.Context, vaultID, vaultPath, virtualPath string, keys *crypto.VaultKeys) (*filemeta.Record, error)

type thumbInvalidator interface {
	DeleteThumbnail(vaultID, virtualPath string)
}

type thumbContextEnqueuer interface {
	EnqueueContext(ctx context.Context, vaultID, vaultPath string, keys *crypto.VaultKeys, virtualPath string) error
}

type thumbVaultWaiter interface {
	WaitVaultIdle(ctx context.Context, vaultID string) error
}

type thumbRebuildPreparer interface {
	PrepareVaultRebuild(ctx context.Context, vaultID string) error
}

type thumbRebuildResumer interface {
	ResumeVault(vaultID string)
}

// ReplaceGuard runs immediately before an imported file is written to its
// destination. Implementations can stop readers/transcoders that still use
// the old bytes and return an error when it is unsafe to replace the file.
type ReplaceGuard func(vaultID, virtualPath string) error

// ReplaceLeaseGuard is the lifecycle-safe form of ReplaceGuard. It returns a
// release function that remains held until the encrypted replacement has been
// written, preventing a new reader from starting in the stop/write window.
type ReplaceLeaseGuard func(vaultID, virtualPath string) (release func(), err error)

// ErrTaskManagerClosed is returned when a task is submitted during shutdown.
var ErrTaskManagerClosed = errors.New("task manager is shutting down")

// ErrTaskManagerNotClosed prevents callers from using Wait as a generic drain
// operation while new starts are still allowed. WaitGroup permits Add/Wait
// concurrency only while its counter is non-zero; requiring Shutdown first
// gives Wait a stable admission boundary and avoids an Add-after-zero race.
var ErrTaskManagerNotClosed = errors.New("task manager must be shut down before waiting")

// ErrVaultQuiescing is returned while a vault is being deleted. Quiescing
// prevents a new import/index task from starting between cancellation and the
// destructive operation.
var ErrVaultQuiescing = errors.New("vault is being removed")

type runningTask struct {
	cancel  context.CancelFunc
	done    chan struct{}
	vaultID string
}

type pendingStart struct {
	vaultID string
	done    chan struct{}
}

// Manager handles background task execution
type Manager struct {
	db *storage.DB
	mu sync.RWMutex

	// admissionMu closes the gap between a task start reserving its exclusive
	// slot and registering its worker in running/wg.  Shutdown and vault
	// quiesce take the write side before changing lifecycle state; a starter
	// takes the read side only for the short durable-create/admission phase.
	// Expensive source validation/counting remains outside this barrier, so a
	// slow filesystem walk cannot indefinitely block unrelated shutdown work.
	admissionMu        sync.RWMutex
	running            map[string]*runningTask // taskID -> lifecycle handle
	runningByVaultType map[string]string       // vaultID:type -> taskID
	taskRunKeys        map[string]string       // taskID -> vaultID:type
	thumbs             ThumbEnqueuer
	mediaDeriver       MediaDeriver
	replaceGuard       ReplaceGuard
	replaceLeaseGuard  ReplaceLeaseGuard
	importGuard        *pathguard.Guard
	quiescingVaults    map[string]struct{}
	pendingStarts      map[string]*pendingStart
	resumeVaults       map[string]struct{}
	startWg            sync.WaitGroup
	wg                 sync.WaitGroup
	closed             bool
}

var errTaskCancelled = errors.New("task cancelled")

// NewManager creates a new task manager and recovers interrupted tasks
func NewManager(db *storage.DB) *Manager {
	m := &Manager{
		db:                 db,
		running:            make(map[string]*runningTask),
		runningByVaultType: make(map[string]string),
		taskRunKeys:        make(map[string]string),
		quiescingVaults:    make(map[string]struct{}),
		pendingStarts:      make(map[string]*pendingStart),
		resumeVaults:       make(map[string]struct{}),
	}
	// Mark previously running tasks as interrupted
	m.recoverTasks()
	return m
}

// SetThumbEnqueuer sets the thumbnail enqueuer (avoids circular init)
func (m *Manager) SetThumbEnqueuer(t ThumbEnqueuer) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.thumbs = t
	m.mu.Unlock()
}

func (m *Manager) SetMediaDeriver(deriver MediaDeriver) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.mediaDeriver = deriver
	m.mu.Unlock()
}

func (m *Manager) getMediaDeriver() MediaDeriver {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	deriver := m.mediaDeriver
	m.mu.RUnlock()
	return deriver
}

func (m *Manager) getThumbEnqueuer() ThumbEnqueuer {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	thumbs := m.thumbs
	m.mu.RUnlock()
	return thumbs
}

// SetReplaceGuard installs an optional lifecycle guard for imported file
// replacements. It is a callback to keep task and API packages decoupled.
func (m *Manager) SetReplaceGuard(guard ReplaceGuard) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.replaceGuard = guard
	m.mu.Unlock()
}

// SetReplaceLeaseGuard installs a guard that can hold a replacement barrier
// across the actual encryption write. It is optional for compatibility with
// callers that only need the pre-replacement callback.
func (m *Manager) SetReplaceLeaseGuard(guard ReplaceLeaseGuard) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.replaceLeaseGuard = guard
	m.mu.Unlock()
}

// SetImportSourceGuard installs the same host-path policy used by the HTTP
// directory browser. The manager repeats validation defensively because task
// creation may later gain non-HTTP callers.
func (m *Manager) SetImportSourceGuard(guard *pathguard.Guard) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.importGuard = guard
	m.mu.Unlock()
}

func (m *Manager) getImportSourceGuard() *pathguard.Guard {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	guard := m.importGuard
	m.mu.RUnlock()
	return guard
}

func (m *Manager) getReplaceGuard() ReplaceGuard {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	guard := m.replaceGuard
	m.mu.RUnlock()
	return guard
}

func (m *Manager) getReplaceLeaseGuard() ReplaceLeaseGuard {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	guard := m.replaceLeaseGuard
	m.mu.RUnlock()
	return guard
}

// recoverTasks marks any tasks that were "running" when server restarted as "error"
func (m *Manager) recoverTasks() {
	if m == nil || m.db == nil {
		return
	}
	tasks, err := m.db.ListRunningTasks()
	if err != nil {
		log.Printf("Failed to recover tasks: %v", err)
		return
	}
	for _, t := range tasks {
		t.Status = "error"
		t.ErrorMsg = "server restarted, task interrupted"
		m.updateTask(&t)
	}
}

func (m *Manager) updateTask(t *storage.TaskRecord) {
	if m == nil || m.db == nil || t == nil {
		return
	}
	if err := m.db.UpdateTask(t); err != nil {
		log.Printf("task %s: persist state: %v", t.ID, err)
	}
}

// beginTaskStart registers the whole public Start* call, including source
// validation and file counting.  These steps happen before a durable task row
// exists, so tracking them separately is what lets vault deletion and
// shutdown wait without guessing whether a starter is still in flight.
func (m *Manager) beginTaskStart(taskID, vaultID string) error {
	if m == nil {
		return ErrTaskManagerClosed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrTaskManagerClosed
	}
	if _, blocked := m.quiescingVaults[vaultID]; blocked {
		return ErrVaultQuiescing
	}
	if taskID == "" {
		return errors.New("task id is empty")
	}
	if m.pendingStarts == nil {
		m.pendingStarts = make(map[string]*pendingStart)
	}
	if _, exists := m.pendingStarts[taskID]; exists {
		return fmt.Errorf("task start already in progress: %s", taskID)
	}
	m.pendingStarts[taskID] = &pendingStart{vaultID: vaultID, done: make(chan struct{})}
	m.startWg.Add(1)
	return nil
}

func (m *Manager) finishTaskStart(taskID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	start := m.pendingStarts[taskID]
	if start != nil {
		delete(m.pendingStarts, taskID)
		close(start.done)
		m.startWg.Done()
		m.maybeResumeVaultLocked(start.vaultID)
	}
	m.mu.Unlock()
}

func (m *Manager) maybeResumeVaultLocked(vaultID string) {
	if _, requested := m.resumeVaults[vaultID]; !requested || m.closed {
		return
	}
	for _, run := range m.running {
		if run != nil && run.vaultID == vaultID {
			return
		}
	}
	for _, start := range m.pendingStarts {
		if start != nil && start.vaultID == vaultID {
			return
		}
	}
	delete(m.quiescingVaults, vaultID)
	delete(m.resumeVaults, vaultID)
}

// StartImport starts a background directory import+encrypt task
func (m *Manager) StartImport(taskID, vaultID, vaultPath string, keys *crypto.VaultKeys, sourcePath, destPath string, deleteSource bool) error {
	if m == nil {
		return ErrTaskManagerClosed
	}
	if keys == nil {
		return errors.New("missing vault keys")
	}
	if err := m.beginTaskStart(taskID, vaultID); err != nil {
		return err
	}
	defer m.finishTaskStart(taskID)
	guard := m.getImportSourceGuard()
	if guard == nil {
		return errors.New("import source guard is not configured")
	}
	validatedPath, err := guard.ValidateImport(sourcePath, deleteSource)
	if err != nil {
		return fmt.Errorf("validate import source: %w", err)
	}
	sourcePath = validatedPath
	destPath = crypto.NormalizeVirtualPath(destPath)
	if err := m.reserveExclusiveTask(taskID, vaultID, "import"); err != nil {
		return err
	}
	reserved := true
	defer func() {
		if reserved {
			m.releaseExclusiveTask(taskID)
		}
	}()

	// Count files first
	totalFiles, totalBytes, err := countFiles(sourcePath)
	if err != nil {
		return fmt.Errorf("count files: %w", err)
	}

	t := &storage.TaskRecord{
		ID:           taskID,
		VaultID:      vaultID,
		Type:         "import",
		Status:       "running",
		TotalFiles:   totalFiles,
		TotalBytes:   totalBytes,
		SourcePath:   sourcePath,
		DestPath:     destPath,
		DeleteSource: deleteSource,
		StartedAt:    time.Now().Unix(),
	}

	ownedKeys := keys.Clone()
	if ownedKeys == nil {
		return errors.New("missing vault keys")
	}
	if err := m.admitBackgroundTask(taskID, vaultID, t, ownedKeys, func(ctx context.Context) {
		m.runImport(t, vaultPath, ownedKeys, ctx)
	}); err != nil {
		return err
	}
	reserved = false
	return nil
}

// StartRebuildIndex starts a background task that rebuilds the vault file index.
func (m *Manager) StartRebuildIndex(taskID, vaultID, vaultPath string, keys *crypto.VaultKeys) error {
	if m == nil {
		return ErrTaskManagerClosed
	}
	if keys == nil {
		return errors.New("missing vault keys")
	}
	if err := m.beginTaskStart(taskID, vaultID); err != nil {
		return err
	}
	defer m.finishTaskStart(taskID)
	if err := m.reserveExclusiveTask(taskID, vaultID, "index"); err != nil {
		return err
	}
	reserved := true
	defer func() {
		if reserved {
			m.releaseExclusiveTask(taskID)
		}
	}()

	vault := &crypto.Vault{
		ID:   vaultID,
		Path: vaultPath,
		Keys: keys,
	}

	t := &storage.TaskRecord{
		ID:        taskID,
		VaultID:   vaultID,
		Type:      "index",
		Status:    "running",
		StartedAt: time.Now().Unix(),
	}

	ownedKeys := keys.Clone()
	if ownedKeys == nil {
		return errors.New("missing vault keys")
	}
	vault.Keys = ownedKeys
	if err := m.admitBackgroundTask(taskID, vaultID, t, ownedKeys, func(ctx context.Context) {
		m.runRebuildIndex(t, vault, ctx)
	}); err != nil {
		return err
	}
	reserved = false
	return nil
}

// admitBackgroundTask performs the short, irreversible part of task startup
// under the admission read barrier: the durable task row is created and the
// worker is registered in running/wg before a shutdown or vault quiesce can
// proceed.  Callers may do expensive validation/counting before this helper;
// those callers will simply observe closed/quiescing state and abort here.
// Ownership of ownedKeys transfers to the worker on success and is wiped on
// every failed admission path.
func (m *Manager) admitBackgroundTask(taskID, vaultID string, t *storage.TaskRecord, ownedKeys *crypto.VaultKeys, runner func(context.Context)) error {
	if m == nil {
		if ownedKeys != nil {
			ownedKeys.Zero()
		}
		return ErrTaskManagerClosed
	}
	if t == nil || ownedKeys == nil || runner == nil {
		if ownedKeys != nil {
			ownedKeys.Zero()
		}
		return errors.New("invalid task admission")
	}

	m.admissionMu.RLock()
	defer m.admissionMu.RUnlock()

	ctx, cancel := context.WithCancel(context.Background())
	run := &runningTask{cancel: cancel, done: make(chan struct{}), vaultID: vaultID}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		ownedKeys.Zero()
		return ErrTaskManagerClosed
	}
	if _, blocked := m.quiescingVaults[vaultID]; blocked {
		m.mu.Unlock()
		cancel()
		ownedKeys.Zero()
		return ErrVaultQuiescing
	}
	if m.db == nil {
		m.mu.Unlock()
		cancel()
		ownedKeys.Zero()
		return errors.New("task database is unavailable")
	}
	if err := m.db.CreateTask(t); err != nil {
		m.mu.Unlock()
		cancel()
		ownedKeys.Zero()
		return fmt.Errorf("create task: %w", err)
	}
	if m.running == nil {
		m.running = make(map[string]*runningTask)
	}
	m.running[taskID] = run
	// Add while admissionMu.RLock is held. Shutdown takes the write side before
	// setting closed and waiting, so it cannot observe a zero counter and then
	// race this Add/worker with DB.Close.
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		defer close(run.done)
		runner(ctx)
	}()
	return nil
}

func (m *Manager) reserveExclusiveTask(taskID, vaultID, taskType string) error {
	key := vaultID + ":" + taskType
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrTaskManagerClosed
	}
	if _, blocked := m.quiescingVaults[vaultID]; blocked {
		return ErrVaultQuiescing
	}
	if m.runningByVaultType == nil {
		m.runningByVaultType = make(map[string]string)
	}
	if m.taskRunKeys == nil {
		m.taskRunKeys = make(map[string]string)
	}
	if runningID, ok := m.runningByVaultType[key]; ok {
		return fmt.Errorf("%s task already running: %s", taskType, runningID)
	}
	m.runningByVaultType[key] = taskID
	m.taskRunKeys[taskID] = key
	return nil
}

func (m *Manager) releaseExclusiveTask(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseExclusiveTaskLocked(taskID)
}

func (m *Manager) releaseExclusiveTaskLocked(taskID string) {
	if key, ok := m.taskRunKeys[taskID]; ok {
		delete(m.runningByVaultType, key)
		delete(m.taskRunKeys, taskID)
	}
}

// CreateUploadTask creates a task record for tracking uploads
func (m *Manager) CreateUploadTask(taskID, vaultID string, totalFiles int, totalBytes int64) error {
	if m == nil {
		return ErrTaskManagerClosed
	}
	if taskID == "" {
		return errors.New("task id is empty")
	}
	if vaultID == "" {
		return errors.New("vault id is empty")
	}
	if totalFiles < 0 {
		return errors.New("total files cannot be negative")
	}
	if totalBytes < 0 {
		return errors.New("total bytes cannot be negative")
	}
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return ErrTaskManagerClosed
	}
	if _, quiescing := m.quiescingVaults[vaultID]; quiescing {
		m.mu.RUnlock()
		return ErrVaultQuiescing
	}
	if m.db == nil {
		m.mu.RUnlock()
		return errors.New("task database is unavailable")
	}
	t := &storage.TaskRecord{
		ID:         taskID,
		VaultID:    vaultID,
		Type:       "upload",
		Status:     "running",
		TotalFiles: totalFiles,
		TotalBytes: totalBytes,
		StartedAt:  time.Now().Unix(),
	}
	// Keep the read lock through the insert. QuiesceVault takes the write lock,
	// so it cannot delete the vault's task rows between this admission check and
	// the durable record creation.
	err := m.db.CreateTask(t)
	m.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

// UpdateUploadProgress updates the progress of an upload task
func (m *Manager) UpdateUploadProgress(taskID string, processedFiles int, processedBytes int64, currentFile string) {
	m.updateUploadTask(taskID, func(t *storage.TaskRecord) {
		t.ProcessedFiles = processedFiles
		t.ProcessedBytes = processedBytes
		t.CurrentFile = currentFile
		if processedFiles >= t.TotalFiles {
			t.Status = "done"
		}
	})
}

// CompleteUploadTask marks an upload task as done
func (m *Manager) CompleteUploadTask(taskID string) {
	m.updateUploadTask(taskID, func(t *storage.TaskRecord) {
		t.Status = "done"
		t.ProcessedFiles = t.TotalFiles
		t.ProcessedBytes = t.TotalBytes
	})
}

// FailUploadTask marks an upload task as error
func (m *Manager) FailUploadTask(taskID, errMsg string) {
	m.updateUploadTask(taskID, func(t *storage.TaskRecord) {
		t.Status = "error"
		t.ErrorMsg = errMsg
	})
}

// updateUploadTask serializes progress writes with vault quiescing. Uploads
// are driven by HTTP requests rather than Manager worker goroutines, so the
// read lock is the lifecycle barrier that prevents a vault delete from
// removing its record concurrently with a final progress update.
func (m *Manager) updateUploadTask(taskID string, update func(*storage.TaskRecord)) {
	if m == nil || m.db == nil || taskID == "" || update == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return
	}
	t, err := m.db.GetTask(taskID)
	if err != nil {
		return
	}
	if _, blocked := m.quiescingVaults[t.VaultID]; blocked {
		return
	}
	update(t)
	m.updateTask(t)
}

// CancelTask cancels a running task
func (m *Manager) CancelTask(taskID string) bool {
	m.mu.Lock()
	run, ok := m.running[taskID]
	m.mu.Unlock()

	if ok && run != nil && run.cancel != nil {
		// Keep the lifecycle handle registered until the goroutine has actually
		// exited. This preserves exclusivity and lets shutdown wait for all
		// filesystem/DB work to finish.
		run.cancel()
	}
	return ok
}

// QuiesceVault cancels and waits for all import/index tasks belonging to a
// vault. Once quiesced, new background tasks for the vault are rejected until
// ResumeVault is called. It is used as a barrier before vault deletion.
func (m *Manager) QuiesceVault(ctx context.Context, vaultID string) error {
	if m == nil {
		return ErrTaskManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Block the short durable-create/admission phase of a starter before
	// publishing the vault tombstone. A starter that is still doing source
	// validation will observe the tombstone when it reaches admission and will
	// never create a task row or worker after deletion proceeds.
	m.admissionMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.admissionMu.Unlock()
		return ErrTaskManagerClosed
	}
	if m.quiescingVaults == nil {
		m.quiescingVaults = make(map[string]struct{})
	}
	m.quiescingVaults[vaultID] = struct{}{}
	targets := make([]*runningTask, 0)
	for _, run := range m.running {
		if run != nil && run.vaultID == vaultID {
			targets = append(targets, run)
		}
	}
	pending := make([]*pendingStart, 0)
	for _, start := range m.pendingStarts {
		if start != nil && start.vaultID == vaultID {
			pending = append(pending, start)
		}
	}
	m.mu.Unlock()
	m.admissionMu.Unlock()

	for _, run := range targets {
		run.cancel()
	}
	for _, run := range targets {
		select {
		case <-run.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, start := range pending {
		select {
		case <-start.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ResumeVault allows new tasks after a failed/deferred destructive operation.
func (m *Manager) ResumeVault(vaultID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	for _, run := range m.running {
		if run != nil && run.vaultID == vaultID {
			// A timed-out quiesce may still be waiting for a filesystem call to
			// return. Keep the tombstone until the same owner has actually
			// exited; reopening the vault here would reintroduce the delete race.
			if m.resumeVaults == nil {
				m.resumeVaults = make(map[string]struct{})
			}
			m.resumeVaults[vaultID] = struct{}{}
			m.mu.Unlock()
			return
		}
	}
	for _, start := range m.pendingStarts {
		if start != nil && start.vaultID == vaultID {
			// Source validation/counting happens before durable admission. Defer
			// reopening until the public Start* call has completely returned.
			if m.resumeVaults == nil {
				m.resumeVaults = make(map[string]struct{})
			}
			m.resumeVaults[vaultID] = struct{}{}
			m.mu.Unlock()
			return
		}
	}
	delete(m.quiescingVaults, vaultID)
	delete(m.resumeVaults, vaultID)
	m.mu.Unlock()
}

// ForgetVault removes a completed deletion tombstone once the database record
// is gone. It intentionally refuses to forget a vault with a late-running
// owner; callers can retry after that owner has exited.
func (m *Manager) ForgetVault(vaultID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, run := range m.running {
		if run != nil && run.vaultID == vaultID {
			return
		}
	}
	for _, start := range m.pendingStarts {
		if start != nil && start.vaultID == vaultID {
			// Do not remove the tombstone while a pre-admission starter could
			// still create a task after the destructive operation.
			return
		}
	}
	delete(m.quiescingVaults, vaultID)
	delete(m.resumeVaults, vaultID)
}

// Shutdown cancels all background import/index tasks and waits for both
// pre-admission starters and worker goroutines before dependent resources
// (especially the database) are closed.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Close the same admission barrier used by task starters before marking the
	// manager closed. This makes the subsequent WaitGroup wait a complete
	// lifecycle boundary: no starter can still be between DB work and wg.Add.
	m.admissionMu.Lock()
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for _, run := range m.running {
			if run != nil {
				run.cancel()
			}
		}
	}
	m.mu.Unlock()
	m.admissionMu.Unlock()
	return m.Wait(ctx)
}

// Wait waits for all task starters and workers admitted before Shutdown to
// exit. It is useful after a bounded Shutdown call reports a timeout: callers
// must wait for this barrier before closing the database or session store that
// workers use.
// New work is rejected once Shutdown has marked the manager closed.
func (m *Manager) Wait(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if !closed {
		return ErrTaskManagerNotClosed
	}
	done := make(chan struct{})
	go func() {
		m.startWg.Wait()
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) finishRun(taskID string) {
	m.mu.Lock()
	vaultID := ""
	if run := m.running[taskID]; run != nil {
		vaultID = run.vaultID
	}
	delete(m.running, taskID)
	m.releaseExclusiveTaskLocked(taskID)
	if vaultID != "" {
		m.maybeResumeVaultLocked(vaultID)
	}
	m.mu.Unlock()
}

func (m *Manager) runImport(t *storage.TaskRecord, vaultPath string, keys *crypto.VaultKeys, ctx context.Context) {
	defer func() {
		keys.Zero()
		m.finishRun(t.ID)
	}()

	vault := &crypto.Vault{
		ID:   t.VaultID,
		Path: vaultPath,
		Keys: keys,
	}

	err := m.encryptDirRecursive(ctx, vault, keys, t, t.SourcePath, t.DestPath)
	if err != nil {
		if errors.Is(err, errTaskCancelled) || errors.Is(err, context.Canceled) {
			t.Status = "cancelled"
			t.ErrorMsg = ""
		} else {
			t.Status = "error"
			t.ErrorMsg = err.Error()
		}
	} else {
		t.Status = "done"
	}

	// Clean up empty directories left after per-file deletion
	if t.DeleteSource {
		removeEmptyDirs(ctx, t.SourcePath)
	}

	m.updateTask(t)

	crypto.ReleaseMemoryAfterLargeFile()
}

func (m *Manager) runRebuildIndex(t *storage.TaskRecord, vault *crypto.Vault, ctx context.Context) {
	defer func() {
		vault.Keys.Zero()
		m.finishRun(t.ID)
	}()

	thumbs := m.getThumbEnqueuer()
	if preparer, ok := thumbs.(thumbRebuildPreparer); ok {
		if resumer, ok := thumbs.(thumbRebuildResumer); ok {
			defer resumer.ResumeVault(t.VaultID)
		}
		if err := preparer.PrepareVaultRebuild(ctx, t.VaultID); err != nil {
			t.Status = "error"
			t.ErrorMsg = err.Error()
			m.updateTask(t)
			return
		}
	}

	if err := m.db.ClearEntries(t.VaultID); err != nil {
		t.Status = "error"
		t.ErrorMsg = err.Error()
		m.updateTask(t)
		return
	}

	err := m.rebuildEntryIndex(ctx, vault, t, "/")
	if err == nil {
		if waiter, ok := m.getThumbEnqueuer().(thumbVaultWaiter); ok {
			err = waiter.WaitVaultIdle(ctx, t.VaultID)
		}
	}
	if err == nil {
		err = m.db.PruneFileMetadata(t.VaultID)
	}
	if err == nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if cleanupErr := vault.DropVaultFileCache(cleanupCtx); cleanupErr != nil {
			log.Printf("rebuild: drop vault page cache %s: %v", t.VaultID, cleanupErr)
		}
		cancel()
	}

	if err != nil {
		if errors.Is(err, errTaskCancelled) || errors.Is(err, context.Canceled) {
			t.Status = "cancelled"
			t.ErrorMsg = ""
		} else {
			t.Status = "error"
			t.ErrorMsg = err.Error()
		}
	} else {
		t.Status = "done"
		t.TotalFiles = t.ProcessedFiles
		t.TotalBytes = t.ProcessedBytes
		t.CurrentFile = ""
	}

	m.updateTask(t)
}

func (m *Manager) encryptDirRecursive(ctx context.Context, vault *crypto.Vault, keys *crypto.VaultKeys, t *storage.TaskRecord, srcPath, destPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcPath)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", srcPath, err)
	}

	for _, entry := range entries {
		// Check cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		entryPath := filepath.Join(srcPath, entry.Name())
		virtualPath := filepath.Join(destPath, entry.Name())
		if guard := m.getImportSourceGuard(); guard != nil {
			if err := guard.ValidateEntry(entryPath); err != nil {
				return fmt.Errorf("validate import entry %s: %w", entry.Name(), err)
			}
		}

		if entry.IsDir() {
			if err := vault.CreateEncryptedDirectory(virtualPath); err != nil {
				return fmt.Errorf("create dir %s: %w", virtualPath, err)
			}
			if err := m.upsertEntry(keys.MACKey, vault.ID, virtualPath, true, false, 0, 0, ""); err != nil {
				return fmt.Errorf("index dir %s: %w", virtualPath, err)
			}
			if err := m.encryptDirRecursive(ctx, vault, keys, t, entryPath, virtualPath); err != nil {
				return err
			}
			if err := m.upsertEntry(keys.MACKey, vault.ID, virtualPath, true, true, 0, 0, ""); err != nil {
				return fmt.Errorf("index dir complete %s: %w", virtualPath, err)
			}
		} else {
			t.CurrentFile = entry.Name()
			m.updateTask(t)

			info, _ := entry.Info()
			var release func()
			if guard := m.getReplaceLeaseGuard(); guard != nil {
				var guardErr error
				release, guardErr = guard(vault.ID, virtualPath)
				if guardErr != nil {
					if release != nil {
						release()
					}
					return fmt.Errorf("prepare replacement %s: %w", virtualPath, guardErr)
				}
			} else if guard := m.getReplaceGuard(); guard != nil {
				if guardErr := guard(vault.ID, virtualPath); guardErr != nil {
					return fmt.Errorf("prepare replacement %s: %w", virtualPath, guardErr)
				}
			}
			result, err := func() (crypto.EncryptResult, error) {
				if release != nil {
					defer release()
				}
				return crypto.EncryptSingleFileContext(ctx, vault, keys, entryPath, virtualPath)
			}()
			if err != nil {
				// EncryptSingleFile writes to a private temporary file and only
				// renames after fsync, so a failed replacement leaves the previous
				// destination intact and cleans its own partial output.
				return fmt.Errorf("encrypt %s: %w", virtualPath, err)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := crypto.ValidateSourceIdentity(entryPath, result.Source); err != nil {
				return fmt.Errorf("source changed after encryption %s: %w", entry.Name(), err)
			}

			protectedHash := crypto.ProtectContentHash(keys.MACKey, vault.ID, result.ContentHash)
			if err := m.indexFileWithArtifacts(ctx, vault, virtualPath, protectedHash, result.PlaintextSize, result.ModTime, false); err != nil {
				return fmt.Errorf("derive artifacts %s: %w", virtualPath, err)
			}

			// Drop page cache for source file immediately after encryption.
			crypto.DropPathCache(entryPath)

			// Delete source file only after encryption is fully synced to disk.
			// EncryptSingleFile now fsync's before returning, so this is safe.
			if t.DeleteSource {
				if err := ctx.Err(); err != nil {
					return err
				}
				if guard := m.getImportSourceGuard(); guard != nil {
					// Re-check immediately before unlinking. The source may have
					// been replaced while encryption was running; failing closed is
					// safer than deleting an unexpected path.
					if err := guard.ValidateEntry(entryPath); err != nil {
						return fmt.Errorf("validate source before removal %s: %w", entry.Name(), err)
					}
				}
				if err := crypto.ValidateSourceIdentity(entryPath, result.Source); err != nil {
					return fmt.Errorf("source changed before removal %s: %w", entry.Name(), err)
				}
				if err := crypto.RemoveSourceFile(entryPath, result.Source); err != nil {
					return fmt.Errorf("remove source %s: %w", entryPath, err)
				}
			}

			t.ProcessedFiles++
			if info != nil && info.Size() > 0 {
				t.ProcessedBytes += info.Size()
			} else {
				t.ProcessedBytes += result.PlaintextSize
			}
			m.updateTask(t)
		}
	}
	return nil
}

func (m *Manager) rebuildEntryIndex(ctx context.Context, vault *crypto.Vault, t *storage.TaskRecord, dirPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entries, err := vault.ListDirectory(dirPath)
	if err != nil {
		return err
	}
	if err := m.upsertEntry(vault.Keys.MACKey, vault.ID, dirPath, true, true, 0, 0, ""); err != nil {
		return fmt.Errorf("index dir %s: %w", dirPath, err)
	}

	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		virtualPath := joinVirtualPath(dirPath, entry.Name)
		if entry.IsDir {
			if err := m.rebuildEntryIndex(ctx, vault, t, virtualPath); err != nil {
				return err
			}
			continue
		}

		t.CurrentFile = virtualPath
		if err := m.db.UpdateTask(t); err != nil {
			return err
		}

		hash, err := vault.HashVirtualFileContext(ctx, virtualPath, entry.Size > rebuildHashCacheThreshold)
		if err != nil {
			return fmt.Errorf("hash %s: %w", virtualPath, err)
		}
		protectedHash := crypto.ProtectContentHash(vault.Keys.MACKey, t.VaultID, hash)

		if err := m.indexFileWithArtifacts(ctx, vault, virtualPath, protectedHash, entry.Size, entry.ModTime, true); err != nil {
			return fmt.Errorf("derive artifacts %s: %w", virtualPath, err)
		}

		t.ProcessedFiles++
		t.ProcessedBytes += entry.Size
	}

	return nil
}

func (m *Manager) indexFileWithArtifacts(ctx context.Context, vault *crypto.Vault, virtualPath, protectedHash string, size, modTime int64, reliableThumbnail bool) error {
	if m == nil || vault == nil || vault.Keys == nil {
		return errors.New("media derivation is unavailable")
	}
	var record *filemeta.Record
	if filemeta.IsMediaPath(virtualPath) {
		record = &filemeta.Record{}
		deriver := m.getMediaDeriver()
		if deriver == nil {
			return errors.New("media metadata deriver is not configured")
		}
		derived, err := deriver(ctx, vault.ID, vault.Path, virtualPath, vault.Keys)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("metadata: probe %s: %v", virtualPath, err)
		} else if derived != nil {
			record = derived
		}
	}
	if _, err := fileindex.StoreFile(m.db, vault.Keys, fileindex.FileInput{VaultID: vault.ID, VirtualPath: virtualPath, Size: size, ModTime: modTime, ProtectedHash: protectedHash, Media: record}); err != nil {
		return fmt.Errorf("store file index %s: %w", virtualPath, err)
	}

	thumbs := m.getThumbEnqueuer()
	if thumbs == nil || size <= 0 || (!thumbnail.IsVideo(virtualPath) && !thumbnail.IsHEIF(virtualPath)) {
		return nil
	}
	if invalidator, ok := thumbs.(thumbInvalidator); ok {
		invalidator.DeleteThumbnail(vault.ID, virtualPath)
	}
	if reliableThumbnail {
		if reliable, ok := thumbs.(thumbContextEnqueuer); ok {
			return reliable.EnqueueContext(ctx, vault.ID, vault.Path, vault.Keys, virtualPath)
		}
		return errors.New("thumbnail enqueuer does not support reliable admission")
	}
	thumbs.Enqueue(vault.ID, vault.Path, vault.Keys, virtualPath)
	return nil
}

func (m *Manager) upsertEntry(macKey []byte, vaultID, virtualPath string, isDir bool, childrenIndexed bool, size, modTime int64, protectedHash string) error {
	record, err := fileindex.BuildEntryRecord(macKey, fileindex.EntryInput{VaultID: vaultID, VirtualPath: virtualPath, IsDir: isDir, ChildrenIndexed: childrenIndexed, Size: size, ModTime: modTime, ProtectedHash: protectedHash})
	if err != nil {
		return err
	}
	return m.db.UpsertEntry(record)
}

func joinVirtualPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func countFiles(dir string) (int, int64, error) {
	count := 0
	var totalBytes int64

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
			if info, err := d.Info(); err == nil {
				totalBytes += info.Size()
			}
		}
		return nil
	})
	return count, totalBytes, err
}

// GenerateID generates a random hex ID (128-bit entropy)
func GenerateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// removeEmptyDirs walks bottom-up and removes empty directories.
// Leaves non-empty directories (containing failed files) intact.
func removeEmptyDirs(ctx context.Context, root string) {
	if ctx == nil {
		ctx = context.Background()
	}
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Reverse order = deepest first, os.Remove only succeeds on empty dirs
	for i := len(dirs) - 1; i >= 0; i-- {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := os.Remove(dirs[i]); err != nil && !os.IsNotExist(err) {
			log.Printf("task: remove empty source directory %s: %v", dirs[i], err)
		}
	}
}
