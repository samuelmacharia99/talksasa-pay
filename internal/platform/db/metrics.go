package db

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Transaction retry metrics. They are process-wide (WithTx has no handle to
// a registry); RegisterMetrics exposes them on a registry.
var (
	txRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "talksasa_pay", Subsystem: "db", Name: "tx_retries_total",
		Help: "Transactions re-run by WithTx after a transient concurrency failure.",
	}, []string{"reason"})
	txRetryExhausted = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "talksasa_pay", Subsystem: "db", Name: "tx_retries_exhausted_total",
		Help: "Transactions that still failed with a retryable error after MaxTxAttempts.",
	})
)

func init() {
	// Pre-create the label values so dashboards see zeros, not gaps.
	txRetries.WithLabelValues("serialization_failure")
	txRetries.WithLabelValues("deadlock_detected")
}

func retryReason(err error) string {
	if pgCode(err) == codeDeadlockDetected {
		return "deadlock_detected"
	}
	return "serialization_failure"
}

// RegisterMetrics registers the transaction retry counters on r. Registering
// on the same registry twice is a no-op.
func RegisterMetrics(r prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{txRetries, txRetryExhausted} {
		if err := r.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				return err
			}
		}
	}
	return nil
}
