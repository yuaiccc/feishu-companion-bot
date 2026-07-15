package context

import (
	"fmt"
	"log"
	"strings"
	"unicode/utf8"
)

type Budget struct {
	MaxChars int
	Used     int
	Sections []Section
}

type Section struct {
	Name  string
	Chars int
}

func NewBudget(maxChars int) *Budget {
	return &Budget{MaxChars: maxChars}
}

// CanFit reports whether a content of a given size can fit in the remaining budget.
func (b *Budget) CanFit(length int) bool {
	return b.Used+length <= b.MaxChars
}

// Reserve allocates budget for a content. It returns true if it fits, otherwise false.
// If it fits, b.Used is incremented.
func (b *Budget) Reserve(section string, content string) bool {
	l := utf8.RuneCountInString(content)
	if b.Used+l > b.MaxChars {
		return false
	}
	b.Used += l
	b.Sections = append(b.Sections, Section{Name: section, Chars: l})
	return true
}

func (b *Budget) Log() {
	parts := []string{fmt.Sprintf("budget: total=%d/%d", b.Used, b.MaxChars)}
	for _, s := range b.Sections {
		parts = append(parts, fmt.Sprintf("%s=%d", s.Name, s.Chars))
	}
	log.Printf("[上下文] %s", strings.Join(parts, " "))
}
