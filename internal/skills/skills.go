// Package skills holds the built-in skill prompts that enhance the agent in
// specific domains. The user enables them with /skill enable <name> and they
// are appended to the system prompt for the session. A build may have none:
// builtin.go, which defines them, is left out of the public repository.
package skills

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Skill is a built-in skill prompt.
type Skill struct {
	// ID is the short name the user types, such as "golang".
	ID string
	// Name is the display name.
	Name string
	// Summary is a one-line description for /skills.
	Summary string
	// Description is the longer description for /skill info.
	Description string
	// content is the prompt text loaded from the embedded file.
	content string
}

// Content returns the skill's prompt text.
func (s *Skill) Content() string { return s.content }

// builtins are the skills that ship with the program, in display order:
// builtin.go adds them when the build has it, and otherwise there are none.
var builtins []*Skill

// Set tracks which skills are enabled for a session. It is safe for
// concurrent use.
type Set struct {
	mu      sync.Mutex
	enabled map[string]bool
}

// NewSet returns an empty skill set with nothing enabled.
func NewSet() *Set {
	return &Set{enabled: map[string]bool{}}
}

// List returns all built-in skills, in display order.
func List() []*Skill { return builtins }

// Lookup finds a built-in skill by its ID (case-insensitive).
func Lookup(id string) *Skill {
	lower := strings.ToLower(id)
	for _, s := range builtins {
		if s.ID == lower {
			return s
		}
	}
	return nil
}

// IDs returns the IDs of all built-in skills, in display order.
func IDs() []string {
	ids := make([]string, len(builtins))
	for i, s := range builtins {
		ids[i] = s.ID
	}
	return ids
}

// Enable turns on a skill. It returns an error if the skill is not known.
func (s *Set) Enable(id string) error {
	sk := Lookup(id)
	if sk == nil {
		return fmt.Errorf("unknown skill %q; /skills lists them", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled[sk.ID] = true
	return nil
}

// Disable turns off a skill. It returns an error if the skill is not known.
func (s *Set) Disable(id string) error {
	sk := Lookup(id)
	if sk == nil {
		return fmt.Errorf("unknown skill %q; /skills lists them", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.enabled, sk.ID)
	return nil
}

// IsEnabled reports whether a skill is on.
func (s *Set) IsEnabled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled[id]
}

// Enabled returns the IDs of the enabled skills, sorted.
func (s *Set) Enabled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.enabled {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Count returns how many skills are enabled.
func (s *Set) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.enabled)
}

// Text returns the combined prompt text of all enabled skills, separated by
// blank lines. It returns "" when nothing is enabled.
func (s *Set) Text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.enabled) == 0 {
		return ""
	}
	var parts []string
	for _, sk := range builtins {
		if s.enabled[sk.ID] {
			parts = append(parts, sk.content)
		}
	}
	return strings.Join(parts, "\n\n")
}
