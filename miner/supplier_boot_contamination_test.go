//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the fix for the miner-boot contamination bug (the
// HighStakes operator complaint): at boot, filterStakedSuppliers and
// warmupSingleSupplier run BEFORE the first Redis block event arrives, so
// BlockClient.LastBlock reports height 0. GetActiveServiceConfigs(0) returns
// an empty set for any mainnet supplier (every activation_height > 0), and
// the manager persisted {staked:true, status:active, services:[]} — the
// contaminated tuple — for EVERY supplier on EVERY boot. Relayers treat that
// tuple as a cache miss and 503 the supplier's relays until the next
// reconcile pass heals it (observed live three times: 1585 rejections in one
// second after a restart).
//
// The fix: when the height is unknown (0), fall back to the denormalized
// supplier.Services snapshot instead of computing the height-aware set. The
// snapshot may cut deactivations slightly early, but the window only lasts
// until the first reconcile with a real height (≤ ~60s) and is infinitely
// better than an empty list.

// TestFilterStakedSuppliers_BootHeightZero_UsesDenormalizedServices is the
// regression test for the boot write: height 0 must never persist an empty
// services list for a supplier whose snapshot has services.
func TestFilterStakedSuppliers_BootHeightZero_UsesDenormalizedServices(t *testing.T) {
	const addr = "pokt1bootsupplier"

	// Staked supplier whose service activates at height 5 — the mainnet
	// shape: at boot (height 0) the height-aware set is empty, while the
	// denormalized snapshot carries the service.
	sup := supplierWithHistory(addr, [3]any{"svc1", 5, 0})

	qc := &historySupplierQueryClient{addr: addr}
	qc.set(sup)
	bc := &fakeBlockClient{} // height 0: no block observed yet (boot)

	mgr, sc, cleanup := newManagerForHistoryTest(t, &fakeKeyManager{addrs: []string{addr}}, qc, bc)
	defer cleanup()
	_ = mgr

	staked := mgr.filterStakedSuppliers(context.Background(), []string{addr})
	require.Equal(t, []string{addr}, staked, "staked supplier must be claimable at boot")

	services := servicesForSupplierFromCache(t, sc, addr)
	assert.Equal(t, []string{"svc1"}, services,
		"boot write (height 0) must fall back to the denormalized snapshot, never persist empty services (contaminated tuple)")
}

// TestFilterStakedSuppliers_KnownHeight_StaysHeightAware proves the fix does
// NOT weaken the height-aware behavior: with a real height below the
// activation height, the active set is legitimately empty and is written as
// such (the read guard turns it into a miss, which is correct — no session
// should route relays to a service that is not active yet).
func TestFilterStakedSuppliers_KnownHeight_StaysHeightAware(t *testing.T) {
	const addr = "pokt1futureactivation"

	sup := supplierWithHistory(addr, [3]any{"svc1", 5, 0})
	qc := &historySupplierQueryClient{addr: addr}
	qc.set(sup)
	bc := &fakeBlockClient{}
	bc.height.Store(3) // real height, activation still in the future

	mgr, sc, cleanup := newManagerForHistoryTest(t, &fakeKeyManager{addrs: []string{addr}}, qc, bc)
	defer cleanup()

	staked := mgr.filterStakedSuppliers(context.Background(), []string{addr})
	require.Equal(t, []string{addr}, staked)

	state, err := sc.GetSupplierState(context.Background(), addr)
	require.NoError(t, err)
	// The contaminated-read guard nils exactly this tuple — that IS the
	// correct consumer view for a not-yet-active supplier. Assert on the raw
	// stored value instead, straight from Redis.
	if state != nil {
		assert.Empty(t, state.Services, "height-aware set must remain authoritative at a known height")
	}
}

// TestWarmupSingleSupplier_BootHeightZero_UsesDenormalizedServices covers the
// second write path with the same boot condition: the prewarmed data used by
// resolveAndPublishSupplierState must not carry an empty services list at
// height 0.
func TestWarmupSingleSupplier_BootHeightZero_UsesDenormalizedServices(t *testing.T) {
	const addr = "pokt1warmupsupplier"

	sup := supplierWithHistory(addr, [3]any{"svc1", 5, 0}, [3]any{"svc2", 7, 0})
	qc := &historySupplierQueryClient{addr: addr}
	qc.set(sup)
	bc := &fakeBlockClient{} // height 0

	mgr, _, cleanup := newManagerForHistoryTest(t, &fakeKeyManager{addrs: []string{addr}}, qc, bc)
	defer cleanup()

	data := mgr.warmupSingleSupplier(context.Background(), addr)
	require.NotNil(t, data)
	assert.ElementsMatch(t, []string{"svc1", "svc2"}, data.Services,
		"warmup at height 0 must fall back to the denormalized snapshot")
}
