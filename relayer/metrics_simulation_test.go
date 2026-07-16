//go:build test

package relayer

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestSimulatedRelaysMetric_IsolatedAndLabeled proves the simulated-relay
// counter increments independently under its four bounded labels and that
// incrementing it does NOT touch any real-relay counter. This is the metric
// half of the "simulation never pollutes real metrics" guarantee.
func TestSimulatedRelaysMetric_IsolatedAndLabeled(t *testing.T) {
	realBefore := testutil.ToFloat64(relaysServed.WithLabelValues("svc", "3", "200"))

	simulatedRelaysTotal.WithLabelValues("jsonrpc", "svc", "pokt1sup", "success").Inc()
	simulatedRelaysTotal.WithLabelValues("jsonrpc", "svc", "pokt1sup", "success").Inc()
	simulatedRelaysTotal.WithLabelValues("grpc", "svc", "pokt1sup", "verify_failed").Inc()

	require.Equal(t, float64(2),
		testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("jsonrpc", "svc", "pokt1sup", "success")))
	require.Equal(t, float64(1),
		testutil.ToFloat64(simulatedRelaysTotal.WithLabelValues("grpc", "svc", "pokt1sup", "verify_failed")))

	// The real counter is untouched by simulated activity.
	require.Equal(t, realBefore,
		testutil.ToFloat64(relaysServed.WithLabelValues("svc", "3", "200")),
		"simulated relays must not increment real relay counters")
}

// TestSimulatedRelayDuration_Observes proves the histogram accepts observations
// under its two bounded labels without panicking.
func TestSimulatedRelayDuration_Observes(t *testing.T) {
	simulatedRelayDuration.WithLabelValues("websocket", "svc").Observe(0.012)
	require.GreaterOrEqual(t,
		testutil.CollectAndCount(simulatedRelayDuration), 1,
		"histogram must expose at least the observed series")
}
