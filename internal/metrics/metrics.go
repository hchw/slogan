// Package metrics exposes Prometheus text metrics derived from the operational
// sources of truth (PostgreSQL and Redis) plus a small set of in-process
// counters. Labels are deliberately low-cardinality and never carry prompts,
// API keys, provider secrets or user identifiers.
package metrics

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Sample is one metric point.
type Sample struct {
	Name   string
	Help   string
	Type   string // counter | gauge
	Labels map[string]string
	Value  float64
}

// Collector produces samples on demand.
type Collector interface {
	Collect(ctx context.Context) ([]Sample, error)
}

// FuncCollector adapts a function to a Collector.
type FuncCollector func(ctx context.Context) ([]Sample, error)

// Collect implements Collector.
func (f FuncCollector) Collect(ctx context.Context) ([]Sample, error) { return f(ctx) }

// Counters holds monotonic in-process counters keyed by metric name and labels.
type Counters struct {
	mu     sync.Mutex
	values map[string]counted
	help   map[string]string
}

type counted struct {
	labels map[string]string
	value  float64
}

// NewCounters returns an empty counter set.
func NewCounters() *Counters {
	return &Counters{values: map[string]counted{}, help: map[string]string{}}
}

// Help registers the help text for a metric name.
func (c *Counters) Help(name, help string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.help[name] = help
	c.mu.Unlock()
}

// Add increments a counter by delta.
func (c *Counters) Add(name string, labels map[string]string, delta float64) {
	if c == nil {
		return
	}
	key := name + "\x00" + canonical(labels)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.values[key]
	entry.labels = labels
	entry.value += delta
	c.values[key] = entry
}

// Inc increments a counter by one.
func (c *Counters) Inc(name string, labels map[string]string) { c.Add(name, labels, 1) }

// Snapshot returns the current counters sorted by name and label order.
func (c *Counters) Snapshot() []Sample {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Sample, 0, len(c.values))
	for key, entry := range c.values {
		name := key[:strings.Index(key, "\x00")]
		out = append(out, Sample{Name: name, Help: c.help[name], Type: "counter", Labels: entry.labels, Value: entry.value})
	}
	sortSamples(out)
	return out
}

func canonical(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(labels[k])
		b.WriteString(",")
	}
	return b.String()
}

func sortSamples(samples []Sample) {
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Name != samples[j].Name {
			return samples[i].Name < samples[j].Name
		}
		return canonical(samples[i].Labels) < canonical(samples[j].Labels)
	})
}

// Render writes samples in Prometheus text exposition format.
func Render(samples []Sample) string {
	sortSamples(samples)
	var b strings.Builder
	emittedHelp := map[string]bool{}
	emittedType := map[string]bool{}
	for _, s := range samples {
		if !emittedHelp[s.Name] && s.Help != "" {
			b.WriteString("# HELP " + s.Name + " " + s.Help + "\n")
			emittedHelp[s.Name] = true
		}
		if !emittedType[s.Name] {
			kind := s.Type
			if kind == "" {
				kind = "gauge"
			}
			b.WriteString("# TYPE " + s.Name + " " + kind + "\n")
			emittedType[s.Name] = true
		}
		b.WriteString(s.Name)
		b.WriteString(renderLabels(s.Labels))
		b.WriteString(" ")
		b.WriteString(strconv.FormatFloat(s.Value, 'f', -1, 64))
		b.WriteString("\n")
	}
	return b.String()
}

func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+`="`+escapeLabel(labels[k])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return strings.ReplaceAll(value, "\n", `\n`)
}
