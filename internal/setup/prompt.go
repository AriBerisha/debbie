package setup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Prompter is how the wizard talks to the user; the terminal UI implements
// it. Ask and Secret return ErrCancelled when the user presses Ctrl+C or
// the input ends.
type Prompter interface {
	// Say shows a line. By convention, a line starting with "  ✓ " reports
	// a check that passed, one starting with "  ! " a problem, and one
	// starting with " ❯ " the suggested option; the UI may color them.
	Say(text string)
	// Ask shows prompt and returns the line the user types, trimmed.
	Ask(prompt string) (string, error)
	// Secret is Ask for an API key: the answer doesn't stay on screen.
	Secret(prompt string) (string, error)
}

// ErrCancelled ends a wizard the user left.
var ErrCancelled = errors.New("cancelled")

// askDefault asks until valid accepts the answer; an empty answer means
// def. The question shows def, if any.
func askDefault(p Prompter, question, def string, valid func(string) error) (string, error) {
	prompt := question
	if def != "" {
		prompt += " [" + def + "]"
	}
	for {
		ans, err := p.Ask(prompt + ": ")
		if err != nil {
			return "", err
		}
		if ans == "" {
			ans = def
		}
		if valid == nil {
			return ans, nil
		}
		if err := valid(ans); err != nil {
			p.Say("  " + err.Error())
			continue
		}
		return ans, nil
	}
}

// Option is one answer to a question the wizard asks.
type Option struct {
	Label  string
	Detail string // more about it, shown dimmer; "" for none
	// Key is a word that also picks it where the options are typed rather
	// than picked, such as "env"; its number always works.
	Key string
}

// Choice is a question answered by picking one of its options.
type Choice struct {
	Question string
	Options  []Option
	// Default is the option picked unless the user picks another; -1 for
	// none, where a typed answer must name one.
	Default int
	// Other, when set, labels a last entry where the user types an answer
	// of their own, such as a model the list doesn't have.
	Other string
}

// Chooser is a Prompter that lets the user pick with the arrow keys; the
// terminal UI is one. With another, the wizard numbers the options and
// asks for a number.
type Chooser interface {
	// Choose asks c. It returns the option picked, or -1 and the answer
	// typed into the Other entry.
	Choose(c Choice) (int, string, error)
}

// pick asks c with the arrow keys if p can, else by number.
func pick(p Prompter, c Choice) (int, string, error) {
	if ch, ok := p.(Chooser); ok {
		return ch.Choose(c)
	}
	return AskNumbered(p, c)
}

// AskNumbered asks c by numbering its options and reading a number, or an
// option's Key, for input that isn't a terminal.
func AskNumbered(p Prompter, c Choice) (int, string, error) {
	p.Say(c.Question)
	for i, o := range c.Options {
		mark := "   "
		if i == c.Default {
			mark = " ❯ "
		}
		line := fmt.Sprintf("%s%d. %s", mark, i+1, o.Label)
		if o.Detail != "" {
			line += " (" + o.Detail + ")"
		}
		p.Say(line)
	}
	n := len(c.Options)
	if c.Other != "" {
		p.Say(fmt.Sprintf("   %d. %s", n+1, c.Other))
		n++
	}
	which := func(s string) int {
		if i, err := strconv.Atoi(s); err == nil && i >= 1 && i <= n {
			return i - 1
		}
		for i, o := range c.Options {
			if o.Key != "" && strings.EqualFold(s, o.Key) {
				return i
			}
		}
		return -1
	}
	def := ""
	if c.Default >= 0 {
		def = strconv.Itoa(c.Default + 1)
	}
	ans, err := askDefault(p, "Choose", def, func(s string) error {
		if which(s) < 0 {
			return fmt.Errorf("type a number from 1 to %d", n)
		}
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	i := which(ans)
	if i == len(c.Options) { // Other
		text, err := askDefault(p, c.Other, "", nonEmpty("type it, or press Ctrl+C to stop"))
		return -1, text, err
	}
	return i, "", nil
}

// choose asks the user to pick one of options and returns its index.
func choose(p Prompter, question string, options []string, def int) (int, error) {
	c := Choice{Question: question, Default: def}
	for _, o := range options {
		c.Options = append(c.Options, Option{Label: o})
	}
	i, _, err := pick(p, c)
	return i, err
}

// yesNo asks a yes-or-no question; an empty answer means def. In a
// terminal it is a choice between Yes and No.
func yesNo(p Prompter, question string, def bool) (bool, error) {
	if ch, ok := p.(Chooser); ok {
		d := 1
		if def {
			d = 0
		}
		i, _, err := ch.Choose(Choice{Question: question, Options: []Option{{Label: "Yes", Key: "y"}, {Label: "No", Key: "n"}}, Default: d})
		return i == 0, err
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		ans, err := p.Ask(question + " " + hint + ": ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.Say("  answer y or n")
	}
}
