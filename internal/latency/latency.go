package latency

import (
	"fmt"
	"log"
	"time"
)

type Trace struct {
	name   string
	start  time.Time
	spans  map[string]time.Time
	marks  []string
}

func NewTrace(name string) *Trace {
	return &Trace{
		name:  name,
		start: time.Now(),
		spans: make(map[string]time.Time),
	}
}

func (t *Trace) Span(name string) {
	t.spans[name] = time.Now()
}

func (t *Trace) Mark(name string) {
	t.marks = append(t.marks, name)
}

func (t *Trace) Log() {
	total := time.Since(t.start)
	parts := []string{fmt.Sprintf("%s: total=%dms", t.name, total.Milliseconds())}
	for name, ts := range t.spans {
		parts = append(parts, fmt.Sprintf("%s=%dms", name, ts.Sub(t.start).Milliseconds()))
	}
	for _, m := range t.marks {
		parts = append(parts, fmt.Sprintf("%s_at=%dms", m, time.Since(t.start).Milliseconds()))
	}
	log.Printf("[延迟] %s", joinStrings(parts, " "))
}

func joinStrings(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	n := (len(parts) - 1) * len(sep)
	for _, s := range parts {
		n += len(s)
	}
	buf := make([]byte, 0, n)
	buf = append(buf, parts[0]...)
	for _, s := range parts[1:] {
		buf = append(buf, sep...)
		buf = append(buf, s...)
	}
	return string(buf)
}
