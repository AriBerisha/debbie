package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"debbie/internal/agent"
	"debbie/internal/prompt"
)

// /goal, like Claude Code's: the user sets a condition, and the session
// keeps working toward it. After each turn a separate check judges from the
// conversation whether it is met; if not, the model gets another turn with
// the check's reason. The goal ends when it is met, when the check finds it
// impossible, or with /goal clear. An interruption, an error or turns
// without any tool call pause it until the user sends a message.

// goal is the condition the session works toward.
type goal struct {
	condition string
	started   time.Time
	checks    int    // turns checked
	reason    string // what the last check said
	quiet     int    // checked turns in a row without a tool call
	paused    bool   // waiting for the user's next message
	ended     time.Duration
}

// goalStallTurns is how many turns in a row without a tool call pause a
// goal: the model is talking, not working.
const goalStallTurns = 3

// goalMaxTurns is how many checked turns pause a goal, so one set and left
// alone can't spend without end.
const goalMaxTurns = 25

// goalHold says why the goal must pause after a checked turn, "" when it
// may go on.
func goalHold(g *goal, v agent.GoalVerdict) string {
	switch {
	case v.Verdict == agent.GoalUnclear:
		return "The goal check gave no verdict, so the goal is paused. Send a message to continue, or /goal clear."
	case g.quiet >= goalStallTurns:
		return fmt.Sprintf("No tool calls in the last %d turns; the goal is paused. Send a message to continue, or /goal clear.", goalStallTurns)
	case g.checks >= goalMaxTurns:
		return fmt.Sprintf("The goal has run for %d turns and is paused. Send a message to continue, or /goal clear.", g.checks)
	}
	return ""
}

// goalOff are the words that clear a goal.
var goalOff = map[string]bool{"clear": true, "stop": true, "off": true, "reset": true, "none": true, "cancel": true}

// goalStatus describes the goal for /goal without a condition.
func goalStatus(active, last *goal) string {
	switch {
	case active != nil:
		var b strings.Builder
		fmt.Fprintf(&b, "Goal: %s\nRunning for %s", active.condition, seconds(time.Since(active.started)))
		if active.checks > 0 {
			fmt.Fprintf(&b, " · %d turn%s checked · last check: %s", active.checks, pluralS(active.checks), active.reason)
		}
		if active.paused {
			b.WriteString("\nPaused: send a message to continue.")
		}
		return b.String()
	case last != nil:
		return fmt.Sprintf("No goal set. The last one, %q, ended after %d turn%s in %s.", last.condition, last.checks, pluralS(last.checks), seconds(last.ended))
	}
	return "No goal set. /goal <condition> sets one; /goal clear ends it."
}

// goalEvent is what a goal line in the conversation reports.
type goalEvent uint8

const (
	goalSet goalEvent = iota
	goalNotYet
	goalMetEvent
	goalFailed
)

// goalBlock is a line about the goal in the conversation.
type goalBlock struct {
	event goalEvent
	text  string
}

func (b *goalBlock) draw(width int, t Terminal) [][]span {
	mark := t.sym().goal + " "
	var first []span
	switch b.event {
	case goalSet:
		first = []span{bolded(mark+"Goal set: ", inkCyan), plain(b.text)}
	case goalNotYet:
		first = []span{grayed(mark + "Not met yet · " + b.text)}
	case goalMetEvent:
		first = []span{bolded(mark+"Goal met: ", inkGreen), plain(b.text)}
	case goalFailed:
		first = []span{bolded(mark+"Goal can't be met: ", inkRed), plain(b.text)}
	}
	return wrap(first, width, nil, []span{plain("  ")})
}

// The fullscreen UI's goal loop.

func (f *fullscreen) goalCommand(arg string) {
	switch {
	case arg == "":
		f.say(goalStatus(f.goal, f.lastGoal), ink{})
	case goalOff[strings.ToLower(arg)]:
		if f.goal == nil {
			f.say("No goal set.", inkGray)
			return
		}
		f.say("Goal cleared: "+f.goal.condition, ink{})
		f.goal = nil
	case f.busy:
		f.flash("wait for the turn to end, or press esc")
	default:
		f.goal = &goal{condition: arg, started: time.Now()}
		f.tr.add(&goalBlock{event: goalSet, text: arg})
		f.sendAs("", prompt.GoalStart(arg))
	}
}

// goalStep runs after a turn and any compaction: it checks the goal, or
// pauses it after an interruption or error, then sends what comes next.
func (f *fullscreen) goalStep(turnErr error, stopped string) {
	g := f.goal
	switch {
	case g == nil || g.paused:
		f.sendQueued()
		return
	case stopped != "": // the agent loop ended the turn: the user decides
		g.paused = true
		f.say("Goal paused; send a message to continue, or /goal clear.", inkYellow)
		f.sendQueued()
		return
	case errors.Is(turnErr, context.Canceled):
		g.paused = true
		f.say("Goal paused; send a message to continue, or /goal clear.", inkYellow)
		f.sendQueued()
		return
	case turnErr != nil:
		g.paused = true
		f.say("Goal paused after the error; send a message to continue, or /goal clear.", inkYellow)
		f.sendQueued()
		return
	}
	if f.turnTools == 0 {
		g.quiet++
	} else {
		g.quiet = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.busy, f.cancel, f.turnStart, f.firstDelta, f.deltas, f.activity = true, cancel, time.Now(), time.Time{}, 0, "Checking the goal"
	go func() {
		v, err := f.opts.Session.CheckGoal(ctx, g.condition)
		f.post(func() { f.goalChecked(g, v, err) })
	}()
}

func (f *fullscreen) goalChecked(g *goal, v agent.GoalVerdict, err error) {
	f.busy = false
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	if f.goal != g { // cleared meanwhile
		f.sendQueued()
		return
	}
	if err != nil {
		g.paused = true
		if errors.Is(err, context.Canceled) {
			f.say("Goal paused; send a message to continue, or /goal clear.", inkYellow)
		} else {
			f.say("The goal check failed: "+err.Error()+"; the goal is paused.", inkYellow)
		}
		f.sendQueued()
		return
	}
	g.checks++
	g.reason = v.Reason
	switch v.Verdict {
	case agent.GoalMet, agent.GoalImpossible:
		g.ended = time.Since(g.started)
		event := goalMetEvent
		text := fmt.Sprintf("%s (%d turn%s, %s)", g.condition, g.checks, pluralS(g.checks), seconds(g.ended))
		if v.Verdict == agent.GoalImpossible {
			event, text = goalFailed, v.Reason
		} else if v.Reason != "" {
			text += " · " + v.Reason
		}
		f.tr.add(&goalBlock{event: event, text: text})
		f.lastGoal, f.goal = g, nil
		f.sendQueued()
		return
	}
	f.tr.add(&goalBlock{event: goalNotYet, text: v.Reason})
	hold := goalHold(g, v)
	switch {
	case len(f.queue) > 0:
		f.sendQueued() // the user's message first; the check comes after it
	case hold != "":
		g.paused = true
		f.say(hold, inkYellow)
		f.sendQueued()
	default:
		f.sendAs("", prompt.GoalContinue(g.condition, v.Reason))
	}
}

// goalIndicator is the goal's part of the status line.
func (f *fullscreen) goalIndicator() []span {
	if f.goal == nil {
		return nil
	}
	text := f.term.sym().goal + " goal " + seconds(time.Since(f.goal.started))
	if f.goal.paused {
		text += " (paused)"
	}
	return []span{grayed(" · "), inked(text, inkCyan)}
}

// The line-mode goal loop.

// goalCommand sets, shows or clears the goal in line mode; a new goal runs
// turns until it ends or pauses. It reports whether the user quit.
func (c *chat) goalCommand(arg string) (quit bool) {
	switch {
	case arg == "":
		c.r.println(goalStatus(c.goal, c.lastGoal))
		return false
	case goalOff[strings.ToLower(arg)]:
		if c.goal == nil {
			c.r.println(c.st.gray("No goal set."))
		} else {
			c.r.println("Goal cleared: " + c.goal.condition)
			c.goal = nil
		}
		return false
	}
	c.goal = &goal{condition: arg, started: time.Now()}
	c.r.println(c.st.cyan(c.term.sym().goal+" Goal set: ") + arg)
	return c.runGoal(prompt.GoalStart(arg))
}

// runGoal sends text and keeps the goal's turns and checks going until the
// goal ends or pauses.
func (c *chat) runGoal(text string) (quit bool) {
	for g := c.goal; g != nil && c.goal == g; {
		if c.turn(text) {
			return true
		}
		switch {
		case errors.Is(c.turnErr, context.Canceled):
			g.paused = true
			c.r.println(c.st.yellow("Goal paused; send a message to continue, or /goal clear."))
			return false
		case c.turnErr != nil:
			g.paused = true
			c.r.println(c.st.yellow("Goal paused after the error; send a message to continue, or /goal clear."))
			return false
		case c.turnStopped != "": // the agent loop ended the turn: the user decides
			g.paused = true
			c.r.println(c.st.yellow("Goal paused; send a message to continue, or /goal clear."))
			return false
		}
		if c.turnTools == 0 {
			g.quiet++
		} else {
			g.quiet = 0
		}
		ctx, stop := c.Interruptible()
		c.r.startSpinner("checking the goal")
		v, err := c.opts.Session.CheckGoal(ctx, g.condition)
		c.r.stopSpinner()
		stop()
		if err != nil {
			g.paused = true
			c.r.println(c.st.yellow("Goal paused: the check didn't finish (" + err.Error() + ")."))
			return false
		}
		g.checks++
		g.reason = v.Reason
		mark := c.term.sym().goal + " "
		switch v.Verdict {
		case agent.GoalMet:
			g.ended = time.Since(g.started)
			c.r.println(c.st.green(mark+"Goal met: ") + fmt.Sprintf("%s (%d turn%s, %s)", g.condition, g.checks, pluralS(g.checks), seconds(g.ended)))
			c.lastGoal, c.goal = g, nil
			return false
		case agent.GoalImpossible:
			g.ended = time.Since(g.started)
			c.r.println(c.st.red(mark+"Goal can't be met: ") + v.Reason)
			c.lastGoal, c.goal = g, nil
			return false
		}
		c.r.println(c.st.gray(mark + "Not met yet · " + v.Reason))
		if hold := goalHold(g, v); hold != "" {
			g.paused = true
			c.r.println(c.st.yellow(hold))
			return false
		}
		text = prompt.GoalContinue(g.condition, v.Reason)
	}
	return false
}
