package cf_configuration

import "strings"

// MetricSample is one bootstrap metric sample for observability to scrape.
// Configuration does not implement cf_observability.MetricsProvider — that
// would create an import cycle — so observability registers an internal
// collector that calls MetricSamples.
type MetricSample struct {
	Name   string
	Help   string
	Value  float64
	Labels map[string]string
}

// MetricSamples returns the configuration_info sample when at least one source
// is registered. Returns nil before any source is registered (lazy pickup on
// /metrics).
func (c *Configuration) MetricSamples() []MetricSample {
	if c == nil {
		return nil
	}
	sources := c.Sources()
	if len(sources) == 0 {
		return nil
	}
	return []MetricSample{{
		Name:  "configuration_info",
		Help:  "Registered configuration sources.",
		Value: float64(len(sources)),
		Labels: map[string]string{
			"sources": strings.Join(sources, ","),
		},
	}}
}
