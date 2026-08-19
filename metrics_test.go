package cf_configuration

import "testing"

func TestMetricSamplesNilBeforeSources(t *testing.T) {
	c := New()
	if ms := c.MetricSamples(); ms != nil {
		t.Fatalf("MetricSamples = %+v, want nil before AddSource", ms)
	}
}

func TestMetricSamplesAfterAddSource(t *testing.T) {
	c := New()
	dir := t.TempDir()
	path := writeFile(t, dir, "a.json", `{}`)
	mustAddSource(t, c, Source[mongoConfig]{Name: "alpha", Path: path, Format: FormatJSON})
	path2 := writeFile(t, dir, "b.json", `{}`)
	mustAddSource(t, c, Source[mongoConfig]{Name: "beta", Path: path2, Format: FormatJSON})

	ms := c.MetricSamples()
	if len(ms) != 1 {
		t.Fatalf("MetricSamples = %+v, want one sample", ms)
	}
	if ms[0].Name != "configuration_info" {
		t.Fatalf("Name = %q, want configuration_info", ms[0].Name)
	}
	if ms[0].Value != 2 {
		t.Fatalf("Value = %v, want 2", ms[0].Value)
	}
	if ms[0].Labels["sources"] != "alpha,beta" {
		t.Fatalf("sources label = %q, want alpha,beta", ms[0].Labels["sources"])
	}
}

func TestMetricSamplesNilConfiguration(t *testing.T) {
	var c *Configuration
	if ms := c.MetricSamples(); ms != nil {
		t.Fatalf("nil MetricSamples = %+v, want nil", ms)
	}
}
