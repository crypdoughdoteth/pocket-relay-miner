//go:build test

package redis

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cache"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// countingAppQueryClient is the L3 (chain) stub for the real application cache.
// It returns a fixed valid application and counts GetApplication calls so the
// test can prove a post-cleanup cold read regenerates the entry from chain
// (i.e. cleanup left no stale/broken L2 behind, only a regenerable gap).
type countingAppQueryClient struct {
	getCalls atomic.Int64
}

func (q *countingAppQueryClient) GetApplication(_ context.Context, address string) (*apptypes.Application, error) {
	q.getCalls.Add(1)
	return &apptypes.Application{Address: address}, nil
}

func (q *countingAppQueryClient) InvalidateApplication(_ string) {}

// TestCleanupAll_LiveReadersUnaffected proves the `--type all` hot-safe cleanup
// (invalidateAllTypes in cache_all.go) is safe to run while live relayer-side
// caches are being read concurrently. It wires the REAL cache.SupplierCache and
// cache.ApplicationCache exactly as cmd_relayer.go does (shared Redis client,
// FailOpen supplier cache, ha:supplier prefix) against miniredis, hammers them
// from 8 reader goroutines, fires the cleanup once mid-flight, and asserts the
// observable safety contract holds under the race detector:
//
//   - healthy suppliers never read back nil/empty (any nil is the 503 window),
//   - the contaminated supplier entry is deleted,
//   - healthy supplier entries survive (their L2 is preserved, so reads keep
//     serving even if L1 were cleared),
//   - the application L2 entry is deleted and is regenerable from L3 afterwards.
//
// Determinism note: the cleanup publishes the `{}` clear-all payload to each
// invalidation channel, but that payload does NOT clear L1 for the supplier or
// application caches — `json.Unmarshal([]byte("{}"))` succeeds (no error), so the
// `if payload == "{}"` clear-all branch inside each handleInvalidation's error
// path is dead code. The safety contract therefore does NOT rely on L1 being
// cleared: it relies on L2 preservation (healthy entries keep serving) and on
// L2 deletion of regenerable entries. The test asserts that observable contract,
// so it is deterministic regardless of async pub/sub delivery timing.
func TestCleanupAll_LiveReadersUnaffected(t *testing.T) {
	client, mr := newTestCacheClient(t)
	logger := logging.NewLoggerFromConfig(logging.Config{Level: "error", Format: "text", Async: false})
	ctx := context.Background()

	// --- REAL supplier cache: same wiring as cmd_relayer.go (FailOpen, ha:supplier). ---
	supplierCache := cache.NewSupplierCache(logger, client.Client, cache.SupplierCacheConfig{
		KeyPrefix: "ha:supplier",
		FailOpen:  true,
	})
	require.NoError(t, supplierCache.Start(ctx))
	t.Cleanup(func() { _ = supplierCache.Close() })

	healthy := []string{"pokt1healthy1", "pokt1healthy2", "pokt1healthy3"}
	for _, addr := range healthy {
		require.NoError(t, supplierCache.SetSupplierState(ctx, &cache.SupplierState{
			OperatorAddress: addr,
			Staked:          true,
			Status:          cache.SupplierStatusActive,
			Services:        []string{"svc1"},
		}))
	}

	// Contaminated entry written DIRECTLY to Redis (bypasses SetSupplierState):
	// staked+active with empty services — exactly the artifact cleanup targets.
	const contamAddr = "pokt1contam"
	contamKey := "ha:supplier:" + contamAddr
	contamJSON, err := json.Marshal(cache.SupplierState{
		OperatorAddress: contamAddr,
		Staked:          true,
		Status:          cache.SupplierStatusActive,
		Services:        []string{},
	})
	require.NoError(t, err)
	require.NoError(t, client.Set(ctx, contamKey, contamJSON, 0).Err())
	require.True(t, mr.Exists(contamKey), "contaminated entry must be present before cleanup")

	// --- REAL application cache with a counting L3 stub. ---
	appStub := &countingAppQueryClient{}
	appCache := cache.NewApplicationCache(logger, client.Client, appStub)
	require.NoError(t, appCache.Start(ctx))
	t.Cleanup(func() { _ = appCache.Close() })

	const appAddr = "pokt1app"
	appL2Key := client.KB().CacheKey("application", appAddr)

	// Warm the app cache: one Get populates L2 + L1 via exactly one L3 query.
	warm, err := appCache.Get(ctx, appAddr)
	require.NoError(t, err)
	require.Equal(t, appAddr, warm.GetAddress())
	require.Equal(t, int64(1), appStub.getCalls.Load(), "warmup should hit L3 exactly once")
	require.True(t, mr.Exists(appL2Key), "warmup should populate application L2")

	// --- Concurrent readers + one cleanup, overlapping. ---
	const readers = 8
	const iters = 200

	// A supplier read "fails" (would 503 in production) if it returns nil, an
	// error, or an empty services list. An app read "fails" if it errors.
	var supplierReadFailures atomic.Int64
	var appReadFailures atomic.Int64

	var ready sync.WaitGroup // released once every reader has done one iteration
	var done sync.WaitGroup  // released when every reader has finished
	ready.Add(readers)
	done.Add(readers)

	for i := 0; i < readers; i++ {
		supplierReader := i%2 == 0
		go func(supplierReader bool) {
			defer done.Done()
			for j := 0; j < iters; j++ {
				if supplierReader {
					addr := healthy[j%len(healthy)]
					state, gErr := supplierCache.GetSupplierState(ctx, addr)
					if gErr != nil || state == nil || len(state.Services) == 0 {
						supplierReadFailures.Add(1)
					}
				} else {
					if _, gErr := appCache.Get(ctx, appAddr); gErr != nil {
						appReadFailures.Add(1)
					}
				}
				if j == 0 {
					ready.Done()
				}
			}
		}(supplierReader)
	}

	// Fire the cleanup once while all readers are actively looping (ready.Wait
	// returns only after every reader is past its first iteration, so cleanup
	// genuinely overlaps concurrent reads for the race detector).
	ready.Wait()
	require.NoError(t, invalidateAllTypes(ctx, client, false /*dryRun*/, true /*yes*/))
	done.Wait()

	// (a) No 503 window: healthy suppliers never read back nil/empty, app reads
	//     never error, throughout the concurrent cleanup.
	assert.Equal(t, int64(0), supplierReadFailures.Load(),
		"healthy suppliers must never read back nil/empty while cleanup runs")
	assert.Equal(t, int64(0), appReadFailures.Load(),
		"application reads must never error while cleanup runs")

	// (b) Contaminated supplier entry deleted.
	assert.False(t, mr.Exists(contamKey), "contaminated supplier entry must be deleted by cleanup")

	// (c) Healthy supplier keys preserved in Redis.
	for _, addr := range healthy {
		assert.True(t, mr.Exists("ha:supplier:"+addr), "healthy supplier key must survive cleanup: "+addr)
	}

	// (e) Healthy supplier reads still serve after cleanup. L2 is untouched, so a
	//     read re-hydrates from L2 even if L1 had been cleared.
	for _, addr := range healthy {
		state, gErr := supplierCache.GetSupplierState(ctx, addr)
		require.NoError(t, gErr)
		require.NotNil(t, state, "healthy supplier must still resolve after cleanup: "+addr)
		assert.Equal(t, []string{"svc1"}, state.Services, "preserved supplier must keep its services: "+addr)
	}

	// (d) Application L2 entry deleted by cleanup, and still regenerable from L3.
	assert.False(t, mr.Exists(appL2Key), "application L2 key must be deleted by cleanup")

	l3Before := appStub.getCalls.Load()
	// Evict L1 to model the application L1 aging out. The app L1 TTL (60s) is
	// unexported in the cache package and cannot be shrunk from here; production
	// relies on that TTL to age L1 out because — as documented above — the `{}`
	// clear-all does not clear the application L1. With L1 evicted and L2 already
	// deleted by cleanup, a Get must fall through to the L3 stub.
	require.NoError(t, appCache.Invalidate(ctx, appAddr))
	regen, err := appCache.Get(ctx, appAddr)
	require.NoError(t, err)
	require.Equal(t, appAddr, regen.GetAddress())
	assert.Equal(t, l3Before+1, appStub.getCalls.Load(),
		"cold read after cleanup must regenerate the application from L3")
	assert.True(t, mr.Exists(appL2Key), "regeneration should repopulate application L2")
}
