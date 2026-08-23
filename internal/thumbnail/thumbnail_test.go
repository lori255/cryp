package thumbnail

import (
	"context"
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
