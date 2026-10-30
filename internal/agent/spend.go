package agent

import (
	"sync"

	"debbie/internal/provider"
)

// Pricer prices one request's usage for a model (cost.Table).
type Pricer interface {
	Cost(model string, u provider.Usage) (usd float64, priced bool)
}

// Spend is what a session has spent on one model: every request to it,
// the conversation's own, its compactions, side questions, goal checks
// and subagents alike.
type Spend struct {
	Model                                string
	Requests                             int
	Input, CacheRead, CacheWrite, Output int     // tokens; Input includes the cache reads and writes
	Cost                                 float64 // USD, at the prices in force when each request was made
	Priced                               bool    // the model has a price
}

// Ledger adds up a session's spend, by model.
type Ledger struct {
	pricer Pricer
	mu     sync.Mutex
	spends []*Spend // in order of first use
}

// NewLedger makes a ledger that prices requests with pricer; nil prices
// nothing, and the ledger only counts tokens.
func NewLedger(pricer Pricer) *Ledger { return &Ledger{pricer: pricer} }

// Add records one request's usage for a model.
func (l *Ledger) Add(model string, u provider.Usage) {
	if l == nil {
		return
	}
	usd, priced := 0.0, false
	if l.pricer != nil {
		usd, priced = l.pricer.Cost(model, u)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var s *Spend
	for _, x := range l.spends {
		if x.Model == model {
			s = x
		}
	}
	if s == nil {
		s = &Spend{Model: model}
		l.spends = append(l.spends, s)
	}
	s.Requests++
	s.Input += u.Input
	s.CacheRead += u.CacheRead
	s.CacheWrite += u.CacheWrite
	s.Output += u.Output
	s.Cost += usd
	s.Priced = priced // the latest word on whether the model has a price
}

// Spends returns what was spent, by model, in order of first use.
func (l *Ledger) Spends() []Spend {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Spend, len(l.spends))
	for i, s := range l.spends {
		out[i] = *s
	}
	return out
}

// Total is the session's cost so far, and whether every model used has a
// price; a total with unpriced models in it is a lower bound.
func (l *Ledger) Total() (usd float64, allPriced bool) {
	allPriced = true
	for _, s := range l.Spends() {
		usd += s.Cost
		if !s.Priced {
			allPriced = false
		}
	}
	return usd, allPriced
}

// SetLedger sets what adds up the session's spend; a subagent's session
// shares its parent's.
func (s *Session) SetLedger(l *Ledger) { s.ledger = l }

// Ledger returns what adds up the session's spend; nil when nothing does.
func (s *Session) Ledger() *Ledger { return s.ledger }

// spent records one request's usage, once its stream has ended, for the
// model the provider asked.
func (s *Session) spent(p provider.Provider, u *provider.Usage) {
	if s.ledger == nil || u == nil {
		return
	}
	model := ""
	if m, ok := p.(provider.Modeler); ok {
		model = m.Model()
	}
	s.ledger.Add(model, *u)
}
