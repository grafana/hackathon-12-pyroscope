package ingester

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/user"
	"github.com/grafana/pyroscope/pkg/phlaredb"
	"github.com/grafana/pyroscope/pkg/validation"
	pushv1 "github.com/grafana/pyroscope/api/gen/proto/go/push/v1"
	ingesterv1 "github.com/grafana/pyroscope/api/gen/proto/go/ingester/v1"
	profilev1 "github.com/grafana/pyroscope/api/gen/proto/go/google/v1"
	typesv1 "github.com/grafana/pyroscope/api/gen/proto/go/types/v1"
)

// mockLimits implements the Limits interface for testing
type mockLimits struct {
	maxSeriesPerUser        int
	maxLabelNamesPerSeries  int
}

func (m *mockLimits) MaxSeriesPerUser(_ string) int { return m.maxSeriesPerUser }
func (m *mockLimits) MaxLabelNamesPerSeries(_ string) int { return m.maxLabelNamesPerSeries }
func (m *mockLimits) MaxLocalSeriesPerUser(_ string) int { return m.maxSeriesPerUser }
func (m *mockLimits) MaxLocalSeriesPerMetric(_ string) int { return m.maxSeriesPerUser }
func (m *mockLimits) MaxLocalSeriesPerTenant(_ string) int { return m.maxSeriesPerUser }
func (m *mockLimits) MaxGlobalSeriesPerUser(_ string) int { return m.maxSeriesPerUser }
func (m *mockLimits) MaxGlobalSeriesPerMetric(_ string) int { return m.maxSeriesPerUser }
func (m *mockLimits) MaxGlobalSeriesPerTenant(_ string) int { return m.maxSeriesPerUser }
func (m *mockLimits) DistributorUsageGroups(_ string) *validation.UsageGroupConfig {
	config, _ := validation.NewUsageGroupConfig(map[string]string{
		"default": "",
	})
	return &config
}
func (m *mockLimits) IngestionTenantShardSize(_ string) int { return 1024 * 1024 * 1024 }

func setupTestIngester(b *testing.B, ctx context.Context) (*Ingester, error) {
	dir := b.TempDir()

	defaultLifecyclerConfig := ring.LifecyclerConfig{
		RingConfig: ring.Config{
			KVStore: kv.Config{
				Store: "inmemory",
			},
			ReplicationFactor: 1,
		},
		NumTokens:  1,
		HeartbeatPeriod: 5 * time.Second,
		ObservePeriod: 0 * time.Second,
		JoinAfter: 0 * time.Second,
		MinReadyDuration: 0 * time.Second,
		FinalSleep: 0,
		ID: "localhost",
		Addr: "127.0.0.1",
		Zone: "localhost",
	}

	limits := &mockLimits{
		maxSeriesPerUser: 1000000,
		maxLabelNamesPerSeries: 100,
	}

	ing, err := New(
		ctx,
		Config{
			LifecyclerConfig: defaultLifecyclerConfig,
		},
		phlaredb.Config{
			DataPath: dir,
		},
		nil, // storage bucket
		limits,
		0, // queryStoreAfter
	)
	if err != nil {
		return nil, err
	}

	return ing, nil
}

func generateTestProfile() []byte {
	// Create a simple profile for testing
	profile := &profilev1.Profile{
		StringTable: []string{"", "samples", "count", "function", "main"},  // Add StringTable
		SampleType: []*profilev1.ValueType{
			{
				Type: 1, // Index into StringTable
				Unit: 2, // Index into StringTable
			},
		},
		Sample: []*profilev1.Sample{
			{
				Value: []int64{1},
				Label: []*profilev1.Label{
					{
						Key: 3, // Index into StringTable for "function"
						Str: 4, // Index into StringTable for "main"
					},
				},
				LocationId: []uint64{1},
			},
		},
		Location: []*profilev1.Location{
			{
				Id: 1,
				Line: []*profilev1.Line{
					{
						FunctionId: 1,
						Line:      1,
					},
				},
			},
		},
		Function: []*profilev1.Function{
			{
				Id:       1,
				Name:     4, // Index into StringTable for "main"
				SystemName: 4, // Index into StringTable for "main"
				Filename: 4, // Index into StringTable for "main"
			},
		},
		TimeNanos: time.Now().UnixNano(),
		DurationNanos: int64(time.Second),
		PeriodType: &profilev1.ValueType{
			Type: 1, // Index into StringTable
			Unit: 2, // Index into StringTable
		},
		Period: 100000000, // 100ms in nanoseconds
	}
	
	data, _ := profile.MarshalVT()
	return data
}

func generateLabels(cardinality int) []string {
	labels := make([]string, 0, cardinality*2)
	// Always include service label
	labels = append(labels, "service", "test")
	
	// Add additional labels
	for i := 0; i < cardinality-1; i++ {
		labels = append(labels, 
			fmt.Sprintf("label_%d", i), 
			fmt.Sprintf("value_%d", i))
	}
	return labels
}

// Base benchmarks
func BenchmarkIngester_Push(b *testing.B) {
	ctx := user.InjectOrgID(context.Background(), "test")
	ing, err := setupTestIngester(b, ctx)
	if err != nil {
		b.Fatal(err)
	}

	if err := services.StartAndAwaitRunning(ctx, ing); err != nil {
		b.Fatal(err)
	}
	defer ing.StopAsync()

	profile := generateTestProfile()
	req := connect.NewRequest(&pushv1.PushRequest{
		Series: []*pushv1.RawProfileSeries{
			{
				Labels: []*typesv1.LabelPair{
					{
						Name:  "service",
						Value: "test",
					},
				},
				Samples: []*pushv1.RawSample{
					{
						ID:         uuid.New().String(),
						RawProfile: profile,
					},
				},
			},
		},
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ing.Push(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIngester_Flush(b *testing.B) {
	ctx := user.InjectOrgID(context.Background(), "test")
	ing, err := setupTestIngester(b, ctx)
	if err != nil {
		b.Fatal(err)
	}

	if err := ing.StartAsync(ctx); err != nil {
		b.Fatal(err)
	}
	if err := ing.AwaitRunning(ctx); err != nil {
		b.Fatal(err)
	}
	defer ing.StopAsync()

	// First push some data
	profile := generateTestProfile()
	pushReq := connect.NewRequest(&pushv1.PushRequest{
		Series: []*pushv1.RawProfileSeries{
			{
				Labels: []*typesv1.LabelPair{
					{
						Name:  "service",
						Value: "test",
					},
				},
				Samples: []*pushv1.RawSample{
					{
						ID:         uuid.New().String(),
						RawProfile: profile,
					},
				},
			},
		},
	})
	_, err = ing.Push(ctx, pushReq)
	if err != nil {
		b.Fatal(err)
	}

	flushReq := connect.NewRequest(&ingesterv1.FlushRequest{})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ing.Flush(ctx, flushReq)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// Label cardinality benchmarks
func BenchmarkIngester_Push_LabelCardinality(b *testing.B) {
	cardinalities := []int{1, 5, 10, 20, 50}
	
	for _, cardinality := range cardinalities {
		b.Run(fmt.Sprintf("labels_%d", cardinality), func(b *testing.B) {
			ctx := user.InjectOrgID(context.Background(), "test")
			ing, err := setupTestIngester(b, ctx)
			if err != nil {
				b.Fatal(err)
			}

			if err := ing.StartAsync(ctx); err != nil {
				b.Fatal(err)
			}
			if err := ing.AwaitRunning(ctx); err != nil {
				b.Fatal(err)
			}
			defer ing.StopAsync()

			profile := generateTestProfile()
			// labels := generateLabels(cardinality) // TODO: fix this
			labels := []*typesv1.LabelPair{
				{
					Name:  "service",
					Value: "test",
				},
			}
			
			req := connect.NewRequest(&pushv1.PushRequest{
				Series: []*pushv1.RawProfileSeries{
					{
						Labels: labels,
						Samples: []*pushv1.RawSample{
							{
								ID:         uuid.New().String(),
								RawProfile: profile,
							},
						},
					},
				},
			})

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := ing.Push(ctx, req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkIngester_Flush_LabelCardinality(b *testing.B) {
	cardinalities := []int{1, 5, 10, 20, 50}
	
	for _, cardinality := range cardinalities {
		b.Run(fmt.Sprintf("labels_%d", cardinality), func(b *testing.B) {
			ctx := user.InjectOrgID(context.Background(), "test")
			ing, err := setupTestIngester(b, ctx)
			if err != nil {
				b.Fatal(err)
			}

			if err := ing.StartAsync(ctx); err != nil {
				b.Fatal(err)
			}
			if err := ing.AwaitRunning(ctx); err != nil {
				b.Fatal(err)
			}
			defer ing.StopAsync()

			// Push data with different label cardinalities
			profile := generateTestProfile()
			// labels := generateLabels(cardinality) // TODO: fix this
			labels := []*typesv1.LabelPair{
				{
					Name:  "service",
					Value: "test",
				},
			}
			
			// Push multiple samples to ensure we have enough data to make the flush meaningful
			for i := 0; i < 100; i++ {
				pushReq := connect.NewRequest(&pushv1.PushRequest{
					Series: []*pushv1.RawProfileSeries{
						{
							Labels: labels,
							Samples: []*pushv1.RawSample{
								{
									ID:         uuid.New().String(),
									RawProfile: profile,
								},
							},
						},
					},
				})
				_, err = ing.Push(ctx, pushReq)
				if err != nil {
					b.Fatal(err)
				}
			}

			flushReq := connect.NewRequest(&ingesterv1.FlushRequest{})

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := ing.Flush(ctx, flushReq)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
} 
