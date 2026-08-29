package metrics_test

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ratecap/core/metrics"
)

func TestRecordGRPCRequest_IncrementsCounterWithMethodAndCode(t *testing.T) {
	metrics.RecordGRPCRequest("CheckRateLimit", "OK", 10*time.Millisecond)

	got := testutil.ToFloat64(metrics.GRPCRequestsTotal.WithLabelValues("CheckRateLimit", "OK"))
	if got != 1 {
		t.Errorf("expected counter=1 for method=CheckRateLimit code=OK, got %v", got)
	}
}

func TestRecordGRPCRequest_ObservesDuration(t *testing.T) {
	before := testutil.CollectAndCount(metrics.GRPCRequestDuration)
	metrics.RecordGRPCRequest("ReleaseConcurrency", "OK", 5*time.Millisecond)
	after := testutil.CollectAndCount(metrics.GRPCRequestDuration)

	if after <= before {
		t.Errorf("expected GRPCRequestDuration observation count to increase, before=%d after=%d", before, after)
	}
}

func TestRecordRedisCall_NoErrorOnlyObservesDuration(t *testing.T) {
	before := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("check_and_decrement"))
	metrics.RecordRedisCall("check_and_decrement", time.Millisecond, nil)
	after := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("check_and_decrement"))

	if after != before {
		t.Errorf("expected RedisErrorsTotal unchanged on a nil error, before=%v after=%v", before, after)
	}
}

func TestRecordRedisCall_ErrorIncrementsErrorCounter(t *testing.T) {
	before := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("incr_concurrent"))
	metrics.RecordRedisCall("incr_concurrent", time.Millisecond, errors.New("dial tcp: connection refused"))
	after := testutil.ToFloat64(metrics.RedisErrorsTotal.WithLabelValues("incr_concurrent"))

	if after != before+1 {
		t.Errorf("expected RedisErrorsTotal to increment by 1, before=%v after=%v", before, after)
	}
}

func TestRecordConfigReload_IncrementsByResult(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConfigReloadTotal.WithLabelValues("success"))
	metrics.RecordConfigReload("success")
	after := testutil.ToFloat64(metrics.ConfigReloadTotal.WithLabelValues("success"))

	if after != before+1 {
		t.Errorf("expected ConfigReloadTotal{result=success} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestRecordFailOpen_IncrementsByTierAndReason(t *testing.T) {
	before := testutil.ToFloat64(metrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))
	metrics.RecordFailOpen("rate_limiter", "store_error")
	after := testutil.ToFloat64(metrics.FailOpenTotal.WithLabelValues("rate_limiter", "store_error"))

	if after != before+1 {
		t.Errorf("expected FailOpenTotal{tier=rate_limiter,reason=store_error} to increment by 1, before=%v after=%v", before, after)
	}
}

func TestRecordConfigVersion_ClearsPreviousHashSeries(t *testing.T) {
	metrics.RecordConfigVersion("hash-aaa")
	if got := testutil.ToFloat64(metrics.ConfigVersionInfo.WithLabelValues("hash-aaa")); got != 1 {
		t.Errorf("expected hash-aaa series set to 1, got %v", got)
	}

	metrics.RecordConfigVersion("hash-bbb")
	if got := testutil.ToFloat64(metrics.ConfigVersionInfo.WithLabelValues("hash-bbb")); got != 1 {
		t.Errorf("expected hash-bbb series set to 1, got %v", got)
	}
	count := testutil.CollectAndCount(metrics.ConfigVersionInfo)
	if count != 1 {
		t.Errorf("expected exactly one active series after switching hashes, got %d", count)
	}
}

func TestRecordConnectionSecurity_IncrementsByTransportAndClientCert(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "present"))
	metrics.RecordConnectionSecurity("tls", "present")
	after := testutil.ToFloat64(metrics.ConnectionSecurityTotal.WithLabelValues("tls", "present"))

	if after != before+1 {
		t.Errorf("expected ConnectionSecurityTotal{transport=tls,client_cert=present} to increment by 1, before=%v after=%v", before, after)
	}
}
