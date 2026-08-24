package thumbnail

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cryp/internal/crypto"
)

func TestGeneratorStopIsIdempotentAndClosesEnqueueRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Generator{
		jobs:     make(chan thumbJob, 1),
		queued:   make(map[string]struct{}),
		failed:   make(map[string]time.Time),
		scanning: make(map[string]struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	g.wg.Add(1)
	go g.worker()

	g.Stop()
	g.Stop()
	// Enqueue must simply observe stopped, not send on the closed channel.
	g.Enqueue("vault", "/vault", nil, "/video.mp4")
}

func TestEnqueueCopiesKeysForQueuedJob(t *testing.T) {
	g := &Generator{
		vaultDir: t.TempDir(),
		jobs:     make(chan thumbJob, 1),
		queued:   make(map[string]struct{}),
		failed:   make(map[string]time.Time),
		scanning: make(map[string]struct{}),
	}
	keys := &crypto.VaultKeys{MasterKey: []byte{1, 2}, MACKey: []byte{3, 4}}
	g.Enqueue("vault", "/vault", keys, "/video.mp4")
	keys.MasterKey[0] = 99

	job := <-g.jobs
	defer zeroVaultKeys(job.Keys)
	if job.Keys.MasterKey[0] != 1 {
		t.Fatalf("queued job retained caller key slice: %v", job.Keys.MasterKey)
	}
}

func TestFFmpegProbeUsesIndependentTimeoutBudgets(t *testing.T) {
	var calls int
	run := func(ctx context.Context, _ string, _ []string) error {
		calls++
		// Each probe consumes most of its own budget. If the implementation
		// accidentally shares the first context, the second call observes its
		// expired deadline and the probe is rejected.
		time.Sleep(350 * time.Millisecond)
		return ctx.Err()
	}
	if !canUseFFmpegAttemptWithRunner("unused", ffmpegAttempt{hwaccel: "vaapi"}, 500*time.Millisecond, run) {
		t.Fatalf("hardware probe failed with independent budgets after %d calls", calls)
	}
	if calls != 2 {
		t.Fatalf("probe runner calls = %d, want encoder and decoder probes", calls)
	}
}

func TestDeleteThumbnailInvalidatesQueuedGeneration(t *testing.T) {
	g := &Generator{
		vaultDir:    t.TempDir(),
		jobs:        make(chan thumbJob, 1),
		queued:      make(map[string]struct{}),
		failed:      make(map[string]time.Time),
		scanning:    make(map[string]struct{}),
		generations: make(map[string]uint64),
	}
	keys := &crypto.VaultKeys{MasterKey: []byte{1}, MACKey: []byte{2}}
	g.Enqueue("vault", "/vault", keys, "/video.mp4")
	job := <-g.jobs
	g.DeleteThumbnail("vault", "/video.mp4")
	defer zeroVaultKeys(job.Keys)
	if g.thumbnailJobCurrent(job) {
		t.Fatal("deleted thumbnail generation remained current")
	}
}

func TestDeleteThumbnailWithoutOutstandingJobDoesNotRetainGeneration(t *testing.T) {
	g := &Generator{
		vaultDir:       t.TempDir(),
		queued:         make(map[string]struct{}),
		failed:         make(map[string]time.Time),
		generations:    make(map[string]uint64),
		generationRefs: make(map[string]int),
	}

	for i := 0; i < 1000; i++ {
		g.DeleteThumbnail("vault", fmt.Sprintf("/removed-%d.mp4", i))
	}
	if len(g.generations) != 0 {
		t.Fatalf("idle generations retained %d historical paths", len(g.generations))
	}
	if len(g.generationRefs) != 0 {
		t.Fatalf("idle generation refs retained %d historical paths", len(g.generationRefs))
	}
}

func TestGenerationReleasedAfterStaleAndReplacementJobsFinish(t *testing.T) {
	const key = "vault\x00/video.mp4"
	g := &Generator{
		vaultDir:       t.TempDir(),
		jobs:           make(chan thumbJob, 2),
		queued:         make(map[string]struct{}),
		failed:         make(map[string]time.Time),
		scanning:       make(map[string]struct{}),
		generations:    make(map[string]uint64),
		generationRefs: make(map[string]int),
	}
	keys := &crypto.VaultKeys{MasterKey: []byte{1}, MACKey: []byte{2}}

	g.Enqueue("vault", "/vault", keys, "/video.mp4")
	oldJob := <-g.jobs
	g.DeleteThumbnail("vault", "/video.mp4")
	g.Enqueue("vault", "/vault", keys, "/video.mp4")
	newJob := <-g.jobs
	if oldJob.generation == newJob.generation {
		t.Fatal("replacement reused the stale generation")
	}
	if refs := g.generationRefs[key]; refs != 2 {
		t.Fatalf("generation refs = %d, want 2", refs)
	}

	g.finishJob(oldJob, errThumbnailStale)
	if refs := g.generationRefs[key]; refs != 1 {
		t.Fatalf("generation refs after stale finish = %d, want 1", refs)
	}
	if _, ok := g.generations[key]; !ok {
		t.Fatal("stale job released the replacement generation")
	}
	if _, ok := g.queued[key]; !ok {
		t.Fatal("stale job cleared the replacement queue marker")
	}

	g.finishJob(newJob, nil)
	if _, ok := g.generations[key]; ok {
		t.Fatal("completed replacement retained an idle generation")
	}
	if _, ok := g.generationRefs[key]; ok {
		t.Fatal("completed replacement retained a generation reference")
	}
}

func TestStaleJobDoesNotClearNewGenerationQueueMarker(t *testing.T) {
	const key = "vault\x00/video.mp4"
	g := &Generator{
		queued:      map[string]struct{}{key: {}},
		failed:      make(map[string]time.Time),
		generations: map[string]uint64{key: 1},
	}
	job := thumbJob{
		VaultID:    "vault",
		FilePath:   "/video.mp4",
		generation: 0,
		Keys:       &crypto.VaultKeys{MasterKey: []byte{1}, MACKey: []byte{2}},
	}
	g.finishJob(job, errThumbnailStale)
	if _, ok := g.queued[key]; !ok {
		t.Fatal("stale job cleared the newer generation queue marker")
	}
}

func TestSkippedThumbnailEntersFailureCooldown(t *testing.T) {
	const key = "vault\x00/video.mp4"
	g := &Generator{
		queued:      map[string]struct{}{key: {}},
		failed:      make(map[string]time.Time),
		generations: make(map[string]uint64),
	}
	job := thumbJob{
		VaultID:  "vault",
		FilePath: "/video.mp4",
		Keys:     &crypto.VaultKeys{MasterKey: []byte{1}, MACKey: []byte{2}},
	}
	g.finishJob(job, errThumbnailSkipped)
	if until, ok := g.failed[key]; !ok || !until.After(time.Now()) {
		t.Fatal("skipped thumbnail did not enter cooldown")
	}
}

func TestStaleSkippedThumbnailDoesNotCooldownNewGeneration(t *testing.T) {
	const key = "vault\x00/video.mp4"
	g := &Generator{
		queued:      map[string]struct{}{key: {}},
		failed:      make(map[string]time.Time),
		generations: map[string]uint64{key: 2},
	}
	job := thumbJob{
		VaultID:    "vault",
		FilePath:   "/video.mp4",
		generation: 1,
		Keys:       &crypto.VaultKeys{MasterKey: []byte{1}, MACKey: []byte{2}},
	}
	g.finishJob(job, errThumbnailSkipped)
	if _, ok := g.failed[key]; ok {
		t.Fatal("stale skipped job imposed a cooldown on the newer generation")
	}
	if _, ok := g.queued[key]; !ok {
		t.Fatal("stale skipped job cleared the newer generation queue marker")
	}
}

func TestQuiesceVaultCancelsActiveRunAndBlocksNewJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Generator{
		vaultDir:    t.TempDir(),
		jobs:        make(chan thumbJob, 2),
		queued:      make(map[string]struct{}),
		failed:      make(map[string]time.Time),
		scanning:    make(map[string]struct{}),
		generations: make(map[string]uint64),
		ctx:         ctx,
		cancel:      cancel,
	}

	g.mu.Lock()
	state := g.lifecycleLocked("vault")
	state.nextID = 1
	runCtx, runCancel := context.WithCancel(ctx)
	state.active[1] = &thumbnailRun{cancel: runCancel, done: make(chan struct{})}
	g.mu.Unlock()

	jobDone := make(chan struct{})
	go func() {
		<-runCtx.Done()
		g.endJob(thumbJob{VaultID: "vault", runID: 1})
		close(jobDone)
	}()

	if err := g.QuiesceVault(context.Background(), "vault"); err != nil {
		t.Fatalf("QuiesceVault: %v", err)
	}
	select {
	case <-jobDone:
	case <-time.After(time.Second):
		t.Fatal("active thumbnail run did not stop")
	}

	keys := &crypto.VaultKeys{MasterKey: []byte{1}, MACKey: []byte{2}}
	g.Enqueue("vault", t.TempDir(), keys, "/video.mp4")
	if len(g.jobs) != 0 {
		t.Fatal("quiesced vault accepted a new thumbnail job")
	}

	g.ResumeVault("vault")
	g.Enqueue("vault", t.TempDir(), keys, "/video.mp4")
	if len(g.jobs) != 1 {
		t.Fatal("resumed vault did not accept a thumbnail job")
	}
	queued := <-g.jobs
	zeroVaultKeys(queued.Keys)
}

func TestQuiescedQueuedJobIsDiscardedAfterVaultForget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Generator{
		vaultDir:    t.TempDir(),
		jobs:        make(chan thumbJob, 1),
		queued:      make(map[string]struct{}),
		failed:      make(map[string]time.Time),
		scanning:    make(map[string]struct{}),
		generations: make(map[string]uint64),
		ctx:         ctx,
		cancel:      cancel,
	}
	keys := &crypto.VaultKeys{MasterKey: []byte{1}, MACKey: []byte{2}}
	g.Enqueue("vault", filepath.Join(g.vaultDir, "vault"), keys, "/video.mp4")
	// Seed all per-file maps to verify retirement releases bookkeeping even
	// when the queued job belongs to an older generation.
	key := thumbJobKey("vault", "/video.mp4")
	g.mu.Lock()
	g.generations[key] = 1
	g.failed[key] = time.Now().Add(time.Minute)
	g.mu.Unlock()
	if err := g.QuiesceVault(context.Background(), "vault"); err != nil {
		t.Fatalf("QuiesceVault: %v", err)
	}
	g.ForgetVault("vault")
	g.wg.Add(1)
	go g.worker()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		_, exists := g.vaults["vault"]
		g.mu.Unlock()
		if !exists {
			break
		}
		time.Sleep(time.Millisecond)
	}
	g.mu.Lock()
	_, exists := g.vaults["vault"]
	g.mu.Unlock()
	if exists {
		t.Fatal("retired vault lifecycle was not released after queued job discard")
	}
	g.mu.Lock()
	_, generationExists := g.generations[key]
	_, failureExists := g.failed[key]
	g.mu.Unlock()
	if generationExists {
		t.Fatal("retired vault generation bookkeeping was not purged")
	}
	if failureExists {
		t.Fatal("retired vault failure cooldown was not purged")
	}
	g.Stop()
}
