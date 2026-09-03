package observability

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type sampleKey struct {
	name   string
	labels string
}

type sample struct {
	kind  string
	value float64
}

// Registry is the process-local P0 metric registry shared by one binary role.
type Registry struct {
	mu      sync.RWMutex
	samples map[sampleKey]sample
}

var Default = NewRegistry()

func NewRegistry() *Registry {
	return &Registry{samples: make(map[sampleKey]sample)}
}

func (r *Registry) AddCounter(name string, delta float64, labels ...string) {
	if r == nil || delta < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := sampleKey{name: name, labels: formatLabels(labels)}
	current := r.samples[key]
	current.kind = "counter"
	current.value += delta
	r.samples[key] = current
}

func (r *Registry) SetGauge(name string, value float64, labels ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples[sampleKey{name: name, labels: formatLabels(labels)}] = sample{kind: "gauge", value: value}
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	r.mu.RLock()
	keys := make([]sampleKey, 0, len(r.samples))
	values := make(map[sampleKey]sample, len(r.samples))
	for key, value := range r.samples {
		keys = append(keys, key)
		values[key] = value
	}
	r.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name == keys[j].name {
			return keys[i].labels < keys[j].labels
		}
		return keys[i].name < keys[j].name
	})
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	seen := make(map[string]struct{})
	for _, key := range keys {
		value := values[key]
		if _, ok := seen[key.name]; !ok {
			_, _ = fmt.Fprintf(w, "# TYPE %s %s\n", key.name, value.kind)
			seen[key.name] = struct{}{}
		}
		_, _ = fmt.Fprintf(w, "%s%s %g\n", key.name, key.labels, value.value)
	}
}

func formatLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		value := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(labels[i+1])
		pairs = append(pairs, labels[i]+"=\""+value+"\"")
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ",") + "}"
}
