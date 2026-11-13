// Package metrics builds the Prometheus registry served on /metrics.
package metrics

import (
	"net/http"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Info is the build metadata exposed as labels on the build_info metric.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// NewRegistry returns a registry with the Go runtime and process collectors
// plus unifi_power_controller_build_info.
//
// build_info is a constant-1 gauge whose labels carry the version, so it can be
// joined onto other series, e.g.
//
//	up{job="unifi-power-controller"} * on(instance) group_left(version) unifi_power_controller_build_info
func NewRegistry(info Info) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "unifi_power_controller_build_info",
			Help: "A metric with a constant '1' value labeled with the version, commit and build date " +
				"the binary was built from, and the Go version it was compiled with.",
			ConstLabels: prometheus.Labels{
				"version":   info.Version,
				"commit":    info.Commit,
				"date":      info.Date,
				"goversion": runtime.Version(),
			},
		}, func() float64 { return 1 }),
	)
	return reg
}

// Handler serves reg in the Prometheus exposition format.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
