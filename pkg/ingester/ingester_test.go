package ingester

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-kit/log"
	"github.com/google/uuid"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/oklog/ulid"

	pushv1 "github.com/grafana/pyroscope/api/gen/proto/go/push/v1"
	typesv1 "github.com/grafana/pyroscope/api/gen/proto/go/types/v1"
	phlaremodel "github.com/grafana/pyroscope/pkg/model"
	"github.com/grafana/pyroscope/pkg/objstore/client"
	"github.com/grafana/pyroscope/pkg/objstore/providers/filesystem"
	phlarecontext "github.com/grafana/pyroscope/pkg/phlare/context"
	"github.com/grafana/pyroscope/pkg/phlaredb"
	"github.com/grafana/pyroscope/pkg/tenant"
)

func defaultIngesterTestConfig(t testing.TB) Config {
	kvClient, err := kv.NewClient(kv.Config{Store: "inmemory"}, ring.GetCodec(), nil, log.NewNopLogger())
	require.NoError(t, err)
	cfg := Config{}
	flagext.DefaultValues(&cfg)
	cfg.LifecyclerConfig.RingConfig.KVStore.Mock = kvClient
	cfg.LifecyclerConfig.NumTokens = 1
	cfg.LifecyclerConfig.ListenPort = 0
	cfg.LifecyclerConfig.Addr = "localhost"
	cfg.LifecyclerConfig.ID = "localhost"
	cfg.LifecyclerConfig.FinalSleep = 0
	cfg.LifecyclerConfig.MinReadyDuration = 0
	return cfg
}

func testProfile(t *testing.T) []byte {
	t.Helper()

	buf := bytes.NewBuffer(nil)
	require.NoError(t, pprof.WriteHeapProfile(buf))
	return buf.Bytes()
}

func Test_MultitenantReadWrite(t *testing.T) {
	dbPath := t.TempDir()
	logger := log.NewJSONLogger(os.Stdout)
	reg := prometheus.NewRegistry()
	ctx := phlarecontext.WithLogger(context.Background(), logger)
	ctx = phlarecontext.WithRegistry(ctx, reg)
	cfg := client.Config{
		StorageBackendConfig: client.StorageBackendConfig{
			Backend: client.Filesystem,
			Filesystem: filesystem.Config{
				Directory: dbPath,
			},
		},
	}

	fs, err := client.NewBucket(ctx, cfg, "storage")
	require.NoError(t, err)

	ing, err := New(ctx, defaultIngesterTestConfig(t), phlaredb.Config{
		DataPath:         dbPath,
		MaxBlockDuration: 30 * time.Hour,
	}, fs, &fakeLimits{}, 0)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing))

	req := &connect.Request[pushv1.PushRequest]{
		Msg: &pushv1.PushRequest{
			Series: []*pushv1.RawProfileSeries{
				{
					Samples: []*pushv1.RawSample{
						{
							ID:         uuid.NewString(),
							RawProfile: testProfile(t),
						},
					},
				},
			},
		},
	}
	req.Msg.Series[0].Labels = phlaremodel.LabelsFromStrings("foo", "bar")
	_, err = ing.Push(tenant.InjectTenantID(context.Background(), "foo"), req)
	require.NoError(t, err)

	req.Msg.Series[0].Labels = phlaremodel.LabelsFromStrings("buzz", "bazz")
	_, err = ing.Push(tenant.InjectTenantID(context.Background(), "buzz"), req)
	require.NoError(t, err)

	labelNames, err := ing.LabelNames(tenant.InjectTenantID(context.Background(), "foo"), connect.NewRequest(&typesv1.LabelNamesRequest{}))
	require.NoError(t, err)
	require.Equal(t, []string{"__period_type__", "__period_unit__", "__profile_type__", "__type__", "__unit__", "foo"}, labelNames.Msg.Names)

	labelNames, err = ing.LabelNames(tenant.InjectTenantID(context.Background(), "buzz"), connect.NewRequest(&typesv1.LabelNamesRequest{}))
	require.NoError(t, err)
	require.Equal(t, []string{"__period_type__", "__period_unit__", "__profile_type__", "__type__", "__unit__", "buzz"}, labelNames.Msg.Names)

	labelsValues, err := ing.LabelValues(tenant.InjectTenantID(context.Background(), "foo"), connect.NewRequest(&typesv1.LabelValuesRequest{Name: "foo"}))
	require.NoError(t, err)
	require.Equal(t, []string{"bar"}, labelsValues.Msg.Names)

	labelsValues, err = ing.LabelValues(tenant.InjectTenantID(context.Background(), "buzz"), connect.NewRequest(&typesv1.LabelValuesRequest{Name: "buzz"}))
	require.NoError(t, err)
	require.Equal(t, []string{"bazz"}, labelsValues.Msg.Names)

	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ing))
}

func Test_EvictBlock(t *testing.T) {
	dbPath := t.TempDir()
	logger := log.NewJSONLogger(os.Stdout)
	reg := prometheus.NewRegistry()
	ctx := phlarecontext.WithLogger(context.Background(), logger)
	ctx = phlarecontext.WithRegistry(ctx, reg)

	fs, err := client.NewBucket(ctx, client.Config{
		StorageBackendConfig: client.StorageBackendConfig{
			Backend: client.Filesystem,
			Filesystem: filesystem.Config{
				Directory: dbPath,
			},
		},
	}, "storage")
	require.NoError(t, err)

	ing, err := New(ctx, defaultIngesterTestConfig(t), phlaredb.Config{
		DataPath:         dbPath,
		MaxBlockDuration: 30 * time.Hour,
	}, fs, &fakeLimits{}, 0)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing))
	defer func() {
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ing))
	}()

	// Create a test block ID
	blockID := ulid.MustNew(ulid.Now(), nil)
	tenantID := "test-tenant"

	// Test evicting non-existent block (should call callback)
	callbackCalled := false
	err = ing.evictBlock(tenantID, blockID, func() error {
		callbackCalled = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, callbackCalled)

	// Test error propagation from callback
	expectedErr := errors.New("test error")
	err = ing.evictBlock(tenantID, blockID, func() error {
		return expectedErr
	})
	require.ErrorIs(t, err, expectedErr)
}

func Test_ServiceLifecycle(t *testing.T) {
	dbPath := t.TempDir()
	logger := log.NewJSONLogger(os.Stdout)
	reg := prometheus.NewRegistry()
	ctx := phlarecontext.WithLogger(context.Background(), logger)
	ctx = phlarecontext.WithRegistry(ctx, reg)

	fs, err := client.NewBucket(ctx, client.Config{
		StorageBackendConfig: client.StorageBackendConfig{
			Backend: client.Filesystem,
			Filesystem: filesystem.Config{
				Directory: dbPath,
			},
		},
	}, "storage")
	require.NoError(t, err)

	ing, err := New(ctx, defaultIngesterTestConfig(t), phlaredb.Config{
		DataPath:         dbPath,
		MaxBlockDuration: 30 * time.Hour,
	}, fs, &fakeLimits{}, 0)
	require.NoError(t, err)

	// Test starting
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing))

	// Test running state
	require.Equal(t, services.Running, ing.State())

	// Test stopping
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ing))
	require.Equal(t, services.Terminated, ing.State())
}

func Test_GetOrCreateInstanceErrors(t *testing.T) {
	dbPath := t.TempDir()
	logger := log.NewJSONLogger(os.Stdout)
	reg := prometheus.NewRegistry()
	ctx := phlarecontext.WithLogger(context.Background(), logger)
	ctx = phlarecontext.WithRegistry(ctx, reg)

	fs, err := client.NewBucket(ctx, client.Config{
		StorageBackendConfig: client.StorageBackendConfig{
			Backend: client.Filesystem,
			Filesystem: filesystem.Config{
				Directory: dbPath,
			},
		},
	}, "storage")
	require.NoError(t, err)

	ing, err := New(ctx, defaultIngesterTestConfig(t), phlaredb.Config{
		DataPath:         dbPath,
		MaxBlockDuration: 30 * time.Hour,
	}, fs, &fakeLimits{}, 0)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing))
	defer func() {
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ing))
	}()

	// Test concurrent access
	const numGoroutines = 10
	tenantID := "test-tenant"
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			instance, err := ing.GetOrCreateInstance(tenantID)
			require.NoError(t, err)
			require.NotNil(t, instance)
		}()
	}
	wg.Wait()

	// Verify only one instance was created
	ing.instancesMtx.RLock()
	numInstances := len(ing.instances)
	ing.instancesMtx.RUnlock()
	require.Equal(t, 1, numInstances)
}

func Test_ForInstanceHelpers(t *testing.T) {
	dbPath := t.TempDir()
	logger := log.NewJSONLogger(os.Stdout)
	reg := prometheus.NewRegistry()
	ctx := phlarecontext.WithLogger(context.Background(), logger)
	ctx = phlarecontext.WithRegistry(ctx, reg)

	fs, err := client.NewBucket(ctx, client.Config{
		StorageBackendConfig: client.StorageBackendConfig{
			Backend: client.Filesystem,
			Filesystem: filesystem.Config{
				Directory: dbPath,
			},
		},
	}, "storage")
	require.NoError(t, err)

	ing, err := New(ctx, defaultIngesterTestConfig(t), phlaredb.Config{
		DataPath:         dbPath,
		MaxBlockDuration: 30 * time.Hour,
	}, fs, &fakeLimits{}, 0)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ing))
	defer func() {
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ing))
	}()

	// Test forInstanceUnary with valid tenant
	ctx = tenant.InjectTenantID(context.Background(), "test-tenant")
	result, err := forInstanceUnary(ctx, ing, func(inst *instance) (string, error) {
		return "test-result", nil
	})
	require.NoError(t, err)
	require.Equal(t, "test-result", result)

	// Test forInstanceUnary with error
	expectedErr := errors.New("test error")
	_, err = forInstanceUnary(ctx, ing, func(inst *instance) (string, error) {
		return "", expectedErr
	})
	require.ErrorIs(t, err, expectedErr)

	// Test forInstance with invalid tenant context
	err = ing.forInstance(context.Background(), func(inst *instance) error {
		return nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid_argument: no org id")
}
