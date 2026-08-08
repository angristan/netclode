// Package metrics provides OpenTelemetry-backed helpers for custom
// application metrics. Instruments are created lazily and cached by name;
// all helpers are no-ops until a meter provider is installed (see the
// observability package).
package metrics

import (
	"context"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	meterName = "github.com/angristan/netclode/services/control-plane"
	// prefix preserves the metric namespace used by the previous
	// DogStatsD client so existing metric names stay stable.
	prefix = "netclode."
)

var (
	counters   sync.Map // name -> metric.Int64Counter
	gauges     sync.Map // name -> metric.Float64Gauge
	histograms sync.Map // name -> metric.Float64Histogram
)

// attrs converts DogStatsD-style "key:value" tags into OTel attributes.
func attrs(tags []string) metric.MeasurementOption {
	kvs := make([]attribute.KeyValue, 0, len(tags))
	for _, tag := range tags {
		k, v, ok := strings.Cut(tag, ":")
		if !ok {
			k, v = tag, "true"
		}
		kvs = append(kvs, attribute.String(k, v))
	}
	return metric.WithAttributes(kvs...)
}

// Incr increments a counter.
func Incr(name string, tags []string) {
	c, ok := counters.Load(name)
	if !ok {
		inst, err := otel.Meter(meterName).Int64Counter(prefix + name)
		if err != nil {
			return
		}
		c, _ = counters.LoadOrStore(name, inst)
	}
	c.(metric.Int64Counter).Add(context.Background(), 1, attrs(tags))
}

// Gauge sets a gauge value.
func Gauge(name string, value float64, tags []string) {
	g, ok := gauges.Load(name)
	if !ok {
		inst, err := otel.Meter(meterName).Float64Gauge(prefix + name)
		if err != nil {
			return
		}
		g, _ = gauges.LoadOrStore(name, inst)
	}
	g.(metric.Float64Gauge).Record(context.Background(), value, attrs(tags))
}

// Distribution records a value in a histogram.
func Distribution(name string, value float64, tags []string) {
	h, ok := histograms.Load(name)
	if !ok {
		inst, err := otel.Meter(meterName).Float64Histogram(prefix + name)
		if err != nil {
			return
		}
		h, _ = histograms.LoadOrStore(name, inst)
	}
	h.(metric.Float64Histogram).Record(context.Background(), value, attrs(tags))
}
