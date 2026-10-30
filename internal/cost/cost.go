// Package cost prices what a session spends, from a table of prices per
// million tokens: Anthropic's published list prices for Claude, built in,
// and whatever the user sets in the config or with /cost. Everything is
// computed here, from the usage the model's replies report; nothing is
// ever looked up outside (CLAUDE.md, hard rule 2).
package cost

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"debbie/internal/config"
	"debbie/internal/provider"
)

// PricesAsOf is the date the built-in prices were taken from
// https://platform.claude.com/docs/en/about-claude/pricing.
const PricesAsOf = "2026-10-30"

// mtok is a million tokens.
const mtok = 1_000_000

// Defaults are the built-in prices, in USD per million tokens, by model
// ID or the start of one: a dated ID such as claude-opus-5-5-20261001
// takes its model's price (Lookup). Cache writes are the 5-minute kind,
// which the agent asks for.
func Defaults() map[string]config.Price {
	p := func(in, out, read, write float64) config.Price {
		return config.Price{Input: in, Output: out, CacheRead: read, CacheWrite: write}
	}
	return map[string]config.Price{
		"claude-fable-5-1":  p(10, 50, 0.25, 12.50),
		"claude-mythos-5-1": p(10, 50, 0.25, 12.50),
		"claude-fable-5":    p(10, 50, 1, 12.50),
		"claude-mythos-5":   p(10, 50, 1, 12.50),
		"claude-opus-5-5":   p(4, 20, 0.20, 5),
		"claude-opus-5":     p(5, 25, 0.50, 6.25),
		"claude-opus-4-8":   p(5, 25, 0.50, 6.25),
		"claude-opus-4-7":   p(5, 25, 0.50, 6.25),
		"claude-opus-4-6":   p(5, 25, 0.50, 6.25),
		"claude-opus-4-5":   p(5, 25, 0.50, 6.25),
		"claude-opus-4-1":   p(15, 75, 1.50, 18.75),
		"claude-opus-4":     p(15, 75, 1.50, 18.75),
		"claude-sonnet-5-5": p(2, 10, 0.10, 2.50),
		"claude-sonnet-5":   p(2, 10, 0.20, 2.50),
		"claude-sonnet-4-6": p(3, 15, 0.30, 3.75),
		"claude-sonnet-4-5": p(3, 15, 0.30, 3.75),
		"claude-sonnet-4":   p(3, 15, 0.30, 3.75),
		// Haiku 5.5 is priced by the prompt's length: a request whose input,
		// cache reads and writes included, is over 100,000 tokens pays the
		// higher prices for all of it.
		"claude-haiku-5-5": {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125,
			LongFrom: 100_000, LongInput: 0.50, LongOutput: 2.50, LongCacheRead: 0.05, LongCacheWrite: 0.625},
		"claude-haiku-4-5": p(1, 5, 0.10, 1.25),
		"claude-3-5-haiku": p(0.80, 4, 0.08, 1),
	}
}

// Table is the prices in force: the defaults, with the user's on top.
type Table struct {
	mu     sync.Mutex
	prices map[string]config.Price
	own    map[string]bool // set by the user, not built in
}

// NewTable makes the table from the user's prices over the defaults.
func NewTable(own map[string]config.Price) *Table {
	t := &Table{prices: Defaults(), own: map[string]bool{}}
	for model, p := range own {
		t.Set(model, p)
	}
	return t
}

// Set puts the user's price for a model, or the start of a model ID.
func (t *Table) Set(model string, p config.Price) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prices[model] = p
	t.own[model] = true
}

// Lookup finds the price of a model: the entry that is the model's ID, or
// else the longest one the ID starts with (before a dash), so a dated ID
// takes its model's price. key is the entry found; own says the user set
// it.
func (t *Table) Lookup(model string) (p config.Price, key string, own, ok bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range t.prices {
		if (model == k || strings.HasPrefix(model, k+"-")) && len(k) > len(key) {
			p, key, ok = v, k, true
		}
	}
	return p, key, ok && t.own[key], ok
}

// Cost prices one request's usage for a model, in USD; priced is false
// when the table has no price for the model.
func (t *Table) Cost(model string, u provider.Usage) (usd float64, priced bool) {
	p, _, _, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	return Of(p, u), true
}

// Of prices one request's usage at p. Input counts every input token,
// cache reads and writes among them, as the providers report it.
func Of(p config.Price, u provider.Usage) float64 {
	in, out, read, write := p.Input, p.Output, p.CacheRead, p.CacheWrite
	if p.LongFrom > 0 && u.Input > p.LongFrom {
		in, out, read, write = p.LongInput, p.LongOutput, p.LongCacheRead, p.LongCacheWrite
	}
	plain := max(0, u.Input-u.CacheRead-u.CacheWrite)
	return (float64(plain)*in + float64(u.CacheRead)*read + float64(u.CacheWrite)*write + float64(u.Output)*out) / mtok
}

// Known lists the models the table prices, the user's first, each with
// its price.
func (t *Table) Known() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Entry
	for k, v := range t.prices {
		out = append(out, Entry{Model: k, Price: v, Own: t.own[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Own != out[j].Own {
			return out[i].Own
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Entry is one line of the table.
type Entry struct {
	Model string
	Price config.Price
	Own   bool
}

// Format writes an amount in USD: cents for a dollar or more, a tenth of
// a cent below that.
func Format(usd float64) string {
	switch {
	case usd == 0:
		return "$0.00"
	case usd < 0.01:
		return fmt.Sprintf("$%.4f", usd)
	case usd < 1:
		return fmt.Sprintf("$%.3f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// Describe writes a price in a line: "$4 in · $20 out · cache $0.20 read,
// $5 write, per million tokens".
func Describe(p config.Price) string {
	s := fmt.Sprintf("%s in · %s out", money(p.Input), money(p.Output))
	if p.CacheRead > 0 || p.CacheWrite > 0 {
		s += fmt.Sprintf(" · cache %s read, %s write", money(p.CacheRead), money(p.CacheWrite))
	}
	s += ", per million tokens"
	if p.LongFrom > 0 {
		s += fmt.Sprintf("; over %dk input tokens %s in · %s out", p.LongFrom/1000, money(p.LongInput), money(p.LongOutput))
	}
	return s
}

func money(v float64) string {
	s := fmt.Sprintf("$%.4f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if strings.HasSuffix(s, "$") {
		return "$0"
	}
	return s
}
