package redis

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pokt-network/pocket-relay-miner/cache"
)

// Patterns and prefixes for the --type all hot-safe cleanup. Everything
// under ha:cache:* is regenerable from chain via L3; ha:cache:lock:* is
// excluded because deleting a held repopulation lock breaks the mutual
// exclusion that bounds the post-cleanup L3 refill (locks self-expire via
// TTL). Supplier entries (ha:supplier:*) are only deleted when contaminated
// (staked+active with empty services): those are already treated as cache
// misses by the read guard, so deleting them changes no traffic behavior,
// while deleting a healthy entry would 503 that supplier's relays until the
// miner reconcile rewrites it (relayer fail-open covers Redis errors, not
// misses).
const (
	cacheAllPattern      = "ha:cache:*"
	cacheLockPrefix      = "ha:cache:lock:"
	supplierStatePattern = "ha:supplier:*"
	supplierStatePrefix  = "ha:supplier:"

	// delBatchSize bounds each pipelined DEL so a large cleanup never sends
	// one giant multi-key command.
	delBatchSize = 500
)

// invalidationChannelTypes are the cache types with a live
// SubscribeToInvalidations consumer. Publishing the `{}` clear-all payload
// to each channel empties every instance's L1 immediately after the L2
// deletion. session_params/supplier_params have no subscriber; their L2
// keys are deleted via ha:cache:* and their L1 ages out by TTL.
var invalidationChannelTypes = []string{
	"application",
	"service",
	"supplier",
	"account",
	"shared_params",
	"proof_params",
}

// cleanupPlan is the classified result of scanning the cleanup patterns.
type cleanupPlan struct {
	// toDelete is every Redis key the cleanup will DEL.
	toDelete []string
	// deleteCounts breaks toDelete down by display group for reporting.
	deleteCounts map[string]int
	// locksPreserved counts ha:cache:lock:* keys left untouched.
	locksPreserved int
	// suppliersHealthy counts healthy ha:supplier:* entries left untouched.
	suppliersHealthy int
	// suppliersContaminated counts contaminated supplier entries in toDelete.
	suppliersContaminated int
	// supplierReadErrors counts supplier keys skipped because their value
	// could not be read or parsed; they are preserved (fail-safe).
	supplierReadErrors int
}

// cacheKeyGroup maps a full ha:cache:* Redis key to a display group for the
// per-type breakdown.
func cacheKeyGroup(redisKey string) string {
	rest := strings.TrimPrefix(redisKey, "ha:cache:")
	if idx := strings.IndexByte(rest, ':'); idx > 0 {
		return rest[:idx]
	}
	// Singletons: ha:cache:shared_params, ha:cache:session_params, ...
	return rest
}

// buildCleanupPlan scans the cleanup patterns and classifies every key.
// Supplier values are fetched to detect contamination; unreadable or
// unparseable values are preserved and counted (fail-safe: never delete
// what we cannot classify).
func buildCleanupPlan(ctx context.Context, client *DebugRedisClient) (*cleanupPlan, error) {
	plan := &cleanupPlan{deleteCounts: make(map[string]int)}

	cacheKeys, err := scanAllKeys(ctx, client, cacheAllPattern)
	if err != nil {
		return nil, err
	}
	for _, k := range cacheKeys {
		if strings.HasPrefix(k, cacheLockPrefix) {
			plan.locksPreserved++
			continue
		}
		plan.toDelete = append(plan.toDelete, k)
		plan.deleteCounts[cacheKeyGroup(k)]++
	}

	supplierKeys, err := scanAllKeys(ctx, client, supplierStatePattern)
	if err != nil {
		return nil, err
	}
	for _, k := range supplierKeys {
		data, getErr := client.Get(ctx, k).Bytes()
		if getErr != nil {
			plan.supplierReadErrors++
			continue
		}
		var state cache.SupplierState
		if jsonErr := json.Unmarshal(data, &state); jsonErr != nil {
			plan.supplierReadErrors++
			continue
		}
		if state.IsContaminated() {
			plan.toDelete = append(plan.toDelete, k)
			plan.suppliersContaminated++
			continue
		}
		plan.suppliersHealthy++
	}
	plan.deleteCounts["supplier (contaminated)"] = plan.suppliersContaminated

	return plan, nil
}

// printCleanupBreakdown prints the per-type breakdown of a cleanup plan.
func printCleanupBreakdown(plan *cleanupPlan) {
	groups := make([]string, 0, len(plan.deleteCounts))
	for g := range plan.deleteCounts {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		fmt.Printf("  %-28s %d keys\n", g, plan.deleteCounts[g])
	}
	fmt.Printf("Preserved: %d repopulation locks (%s*), %d healthy supplier entries",
		plan.locksPreserved, cacheLockPrefix, plan.suppliersHealthy)
	if plan.supplierReadErrors > 0 {
		fmt.Printf(", %d unreadable supplier entries (fail-safe)", plan.supplierReadErrors)
	}
	fmt.Printf("\n")
}

// invalidateAllTypes implements `redis cache --type all --invalidate --all`:
// a hot-safe cleanup of every regenerable cache key, executable while
// relayers and miners are serving traffic. See the const block above for
// the safety rationale.
func invalidateAllTypes(ctx context.Context, client *DebugRedisClient, dryRun, yes bool) error {
	plan, err := buildCleanupPlan(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to build cleanup plan: %w", err)
	}

	total := len(plan.toDelete)

	if dryRun {
		fmt.Printf("[dry-run] would invalidate %d cache keys:\n", total)
		printCleanupBreakdown(plan)
		fmt.Printf("[dry-run] no keys were deleted\n")
		return nil
	}

	if total == 0 {
		fmt.Printf("No regenerable cache keys found\n")
		printCleanupBreakdown(plan)
		fmt.Printf("invalidated 0 entries total\n")
		return nil
	}

	if !yes {
		fmt.Printf("About to invalidate %d regenerable cache keys:\n", total)
		printCleanupBreakdown(plan)
		fmt.Printf("State keys (sessions, SMST, WAL, registry) are never touched.\n")
		fmt.Printf("First read per entity after cleanup pays an L3 chain query (~100ms) until repopulated.\n")
		fmt.Printf("Type 'y' to proceed (or use --yes to bypass): ")
		reader := bufio.NewReader(os.Stdin)
		resp, _ := reader.ReadString('\n')
		if strings.TrimSpace(resp) != "y" {
			fmt.Printf("Aborted. No keys were invalidated.\n")
			return nil
		}
	}

	// Delete first, publish after: publishing the clear-all before deletion
	// would let a subscriber's L1 repopulate from a half-deleted L2.
	deleted := 0
	for start := 0; start < total; start += delBatchSize {
		end := start + delBatchSize
		if end > total {
			end = total
		}
		batch := plan.toDelete[start:end]
		if err := client.Del(ctx, batch...).Err(); err != nil {
			return fmt.Errorf("bulk delete failed at batch %d-%d (deleted %d/%d): %w",
				start, end, deleted, total, err)
		}
		deleted += len(batch)
		if total > delBatchSize {
			fmt.Printf("deleted %d/%d...\n", deleted, total)
		}
	}

	// Clear the known-entity tracking sets' members implicitly deleted above
	// is unnecessary: the sets themselves live under ha:cache:known:* and
	// were deleted with the rest of ha:cache:*.

	// Publish the `{}` clear-all payload so every subscribed instance drops
	// its L1 immediately instead of waiting for the L1 TTL.
	published := 0
	for _, cacheType := range invalidationChannelTypes {
		channel := fmt.Sprintf("ha:events:cache:%s:invalidate", cacheType)
		if err := client.Publish(ctx, channel, "{}").Err(); err != nil {
			fmt.Printf("Warning: failed to publish clear-all to %s: %v\n", channel, err)
			continue
		}
		published++
	}

	fmt.Printf("invalidated %d entries total\n", deleted)
	printCleanupBreakdown(plan)
	fmt.Printf("Published clear-all to %d/%d invalidation channels\n", published, len(invalidationChannelTypes))
	return nil
}
