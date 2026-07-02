package context

import (
	"fmt"
	"log"
)

type Budget struct {
	MaxChars int
	Used     int
	Sections []Section
}

type Section struct {
	Name   string
	Chars  int
}

func NewBudget(maxChars int) *Budget {
	return &Budget{MaxChars: maxChars}
}

func (b *Budget) Add(section, content string) string {
	if b.Used >= b.MaxChars {
		return ""
	}
	remaining := b.MaxChars - b.Used
	if len(content) > remaining {
		content = content[:remaining]
	}
	b.Used += len(content)
	b.Sections = append(b.Sections, Section{Name: section, Chars: len(content)})
	return content
}

func (b *Budget) Log() {
	parts := []string{fmt.Sprintf("budget: total=%d/%d", b.Used, b.MaxChars)}
	for _, s := range b.Sections {
		parts = append(parts, fmt.Sprintf("%s=%d", s.Name, s.Chars))
	}
	log.Printf("[上下文] %s", joinStrings(parts, " "))
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
