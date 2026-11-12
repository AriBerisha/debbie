package ui

import (
	"fmt"
	"strconv"
	"strings"

	"debbie/internal/config"
	"debbie/internal/cost"
)

// /cost says what the session spent, by model, and sets a model's prices;
// /resume rename names the conversation. Both UIs share the answers below.

// costPart is the session's cost for the status line, once it is above
// zero; a "+" after it means a model in use has no price, so the amount
// is a lower bound.
func (c *chat) costPart() []string {
	l := c.opts.Session.Ledger()
	if l == nil {
		return nil
	}
	usd, all := l.Total()
	if usd == 0 {
		return nil
	}
	s := cost.Format(usd)
	if !all {
		s += "+"
	}
	return []string{s}
}

// costReport answers /cost: alone, the session's spend and the prices in
// force; with a model, its price; with a model and prices, it sets them.
func (c *chat) costReport(arg string) (lines []string, warn bool) {
	fields := strings.Fields(arg)
	switch len(fields) {
	case 0:
		return c.spendLines(), false
	case 1:
		return []string{c.priceLine(fields[0])}, false
	}
	model := strings.ToLower(fields[0])
	var p config.Price
	if len(fields) == 2 && strings.EqualFold(fields[1], "free") {
		p = config.Price{}
	} else {
		var nums []float64
		for _, f := range fields[1:] {
			v, err := strconv.ParseFloat(strings.TrimPrefix(f, "$"), 64)
			if err != nil || v < 0 || len(fields) > 5 {
				return []string{costUsage}, true
			}
			nums = append(nums, v)
		}
		p = config.Price{Input: nums[0], Output: nums[1], CacheRead: nums[0] * 0.1, CacheWrite: nums[0] * 1.25}
		if len(nums) >= 3 {
			p.CacheRead = nums[2]
		}
		if len(nums) == 4 {
			p.CacheWrite = nums[3]
		}
	}
	if c.opts.Prices == nil {
		return []string{"This session can't set prices."}, true
	}
	c.opts.Prices.Set(model, p)
	saved := " (for this session; there is no config file to save it in)"
	if c.opts.Providers != nil {
		if err := c.opts.Providers.SetPrice(model, p); err != nil {
			saved = " (for this session; not saved: " + err.Error() + ")"
		} else {
			saved = ", saved"
		}
	}
	return []string{model + ": " + cost.Describe(p) + saved + "."}, false
}

const costUsage = "Give the prices in USD per million tokens: /cost <model> <input> <output> [cache read] [cache write], or /cost <model> free."

// spendLines is the session's spend by model, then the price of the model
// in use and where prices come from.
func (c *chat) spendLines() []string {
	var lines []string
	l := c.opts.Session.Ledger()
	spends := l.Spends()
	if len(spends) == 0 {
		lines = append(lines, "Nothing spent in this session yet.")
	} else {
		total, all := l.Total()
		head := "This session: " + cost.Format(total)
		if !all {
			head += " at least; a model used has no price"
		}
		lines = append(lines, head)
		for _, s := range spends {
			model := s.Model
			if model == "" {
				model = "(unnamed model)"
			}
			in := kTokens(s.Input) + " in"
			if s.CacheRead > 0 || s.CacheWrite > 0 {
				in += fmt.Sprintf(" (%s cached, %s written)", kTokens(s.CacheRead), kTokens(s.CacheWrite))
			}
			amount := "no price known"
			if s.Priced {
				amount = cost.Format(s.Cost)
			}
			lines = append(lines, fmt.Sprintf("  %s · %s · %s · %s out · %s", model, count(s.Requests, "request"), in, kTokens(s.Output), amount))
		}
	}
	if m := c.opts.Status.Model; m != "" {
		lines = append(lines, c.priceLine(m))
	}
	return append(lines, "Prices are USD per million tokens: Claude's list prices as of "+cost.PricesAsOf+" are built in, and "+
		"/cost <model> <input> <output> [cache read] [cache write] sets a model's own (free for a local one). Nothing is looked up anywhere.")
}

// priceLine is a model's price, and where it came from.
func (c *chat) priceLine(model string) string {
	if c.opts.Prices == nil {
		return model + ": prices aren't set up in this session."
	}
	p, key, own, ok := c.opts.Prices.Lookup(model)
	switch {
	case !ok:
		return model + ": no price known; /cost " + model + " <input> <output> sets one, /cost " + model + " free marks it free."
	case own:
		return model + ": " + cost.Describe(p) + " (set by you, as " + key + ")."
	}
	return model + ": " + cost.Describe(p) + " (built in, as " + key + ")."
}

// renameReport answers /resume rename: with a title it names the conversation,
// alone it says what it is called.
func (c *chat) renameReport(arg string) (string, bool) {
	if c.opts.Sessions == nil {
		return "Conversations aren't saved in this session, so there is nothing to name.", true
	}
	if strings.TrimSpace(arg) == "" {
		if t := c.opts.Sessions.CurrentTitle(); t != "" {
			return fmt.Sprintf("This conversation is called %q; /resume rename <name> changes it.", t), false
		}
		return "This conversation has no name yet: it takes its first message's, or /resume rename <name> gives it one.", false
	}
	if err := c.opts.Sessions.Rename(arg); err != nil {
		return "Can't name it: " + err.Error(), true
	}
	return fmt.Sprintf("This conversation is now called %q; /resume lists it so.", c.opts.Sessions.CurrentTitle()), false
}

// The fullscreen UI.

func (f *fullscreen) costCommand(arg string) {
	lines, warn := f.chat.costReport(arg)
	k := ink{}
	if warn {
		k = inkYellow
	}
	f.say(strings.Join(lines, "\n"), k)
}

func (f *fullscreen) renameCommand(arg string) {
	text, warn := f.chat.renameReport(arg)
	k := inkGray
	if warn {
		k = inkYellow
	}
	f.say(text, k)
}

// Line mode.

func (c *chat) costCommand(arg string) {
	lines, warn := c.costReport(arg)
	for _, l := range lines {
		if warn {
			l = c.st.yellow(l)
		}
		c.r.println(l)
	}
}

func (c *chat) renameCommand(arg string) {
	text, warn := c.renameReport(arg)
	if warn {
		c.r.println(c.st.yellow(text))
	} else {
		c.r.println(c.st.gray(text))
	}
}
