// Package prompt holds the prompt text sent to models, embedded from
// Markdown files, and assembles the system prompt.
package prompt

import (
	_ "embed"
	"fmt"
	"runtime"
	"strings"
	"time"

	"debbie/internal/app"
)

// systemText is an interim prompt until the one from Qwen Code is ported
// (milestone M6).
//
//go:embed system.md
var systemText string

//go:embed plan_mode.md
var planModeText string

// ReasoningBudgetMessage ends a model's thinking when its reasoning budget
// (SPEC §4.3) is used up: llama-server appends it and the end-of-thinking
// tag, so the reply follows from what was thought so far. It is the
// sentence Qwen3's technical report inserts to stop thinking at a budget.
const ReasoningBudgetMessage = "Considering the limited time by the user, I have to give the solution based on the thinking directly now."

// Env describes the environment the agent runs in (SPEC §13).
type Env struct {
	Workspace string
	OS, Arch  string
	Shell     string // the program the shell tool runs, such as "Git Bash (C:\...\bash.exe)"
	Date      time.Time
	Git       string   // short git status, branch first; "" outside a repository
	Listing   []string // the workspace's top-level entries
}

// CurrentEnv returns the environment of this process, working in
// workspace.
func CurrentEnv(workspace string) Env {
	return Env{Workspace: workspace, OS: runtime.GOOS, Arch: runtime.GOARCH, Date: time.Now()}
}

// System returns the system prompt: the base prompt, the environment block
// and the provider's system_prompt_append.
func System(env Env, appendText string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(systemText))
	b.WriteString("\n\n")
	b.WriteString(environment(env))
	if extra := strings.TrimSpace(appendText); extra != "" {
		b.WriteString("\n\n")
		b.WriteString(extra)
	}
	return b.String()
}

//go:embed subagent_full.md
var subagentFullText string

//go:embed subagent_read.md
var subagentReadText string

// Subagent returns a subagent's system prompt (SPEC §10), for one that
// may change files with full, else for one that only reads; then the
// environment block.
func Subagent(env Env, full bool) string {
	text := subagentReadText
	if full {
		text = subagentFullText
	}
	return strings.TrimSpace(text) + "\n\n" + environment(env)
}

// environment is the environment block of the system prompts.
func environment(env Env) string {
	var b strings.Builder
	b.WriteString("<environment>\n")
	fmt.Fprintf(&b, "Program: %s %s, a terminal coding agent (https://debbie.sh); this conversation runs in it\n", app.Name, app.Version)
	fmt.Fprintf(&b, "Workspace: %s\n", env.Workspace)
	fmt.Fprintf(&b, "Operating system: %s/%s\n", env.OS, env.Arch)
	if env.Shell != "" {
		fmt.Fprintf(&b, "Shell tool: %s\n", env.Shell)
	}
	fmt.Fprintf(&b, "Today's date: %s\n", env.Date.Format("2006-01-02"))
	if env.Git != "" {
		fmt.Fprintf(&b, "Git status:\n%s\n", env.Git)
	}
	if len(env.Listing) > 0 {
		fmt.Fprintf(&b, "Top-level entries of the workspace:\n%s\n", strings.Join(env.Listing, "\n"))
	}
	b.WriteString("</environment>")
	return b.String()
}

// PlanMode tells the model that plan mode is on (SPEC §9).
func PlanMode() string { return strings.TrimSpace(planModeText) }

//go:embed editor_mode.md
var editorModeText string

// EditorMode tells the model that editor mode is on (SPEC §9).
func EditorMode() string { return strings.TrimSpace(editorModeText) }

// The compaction prompts, ported from Qwen Code's compression service
// (SPEC §7): the summarizer's system prompt, the request that ends the
// conversation sent to it, the header and trailer around the summary that
// replaces the conversation, and the model's acknowledgement after it.

//go:embed compression.md
var compressionText string

//go:embed compression_request.md
var compressionRequestText string

//go:embed compression_summary.md
var compressionSummaryText string

//go:embed compression_resume.md
var compressionResumeText string

//go:embed compression_ack.md
var compressionAckText string

//go:embed compression_retry.md
var compressionRetryText string

// Compression is the summarizer's system prompt.
func Compression() string { return strings.TrimSpace(compressionText) }

// CompressionRequest asks for the summary; focus, if not empty, is what the
// user wants it to keep in particular.
func CompressionRequest(focus string) string {
	req := strings.TrimSpace(compressionRequestText)
	if focus = strings.TrimSpace(focus); focus != "" {
		req += "\n\nThe user asks the summary to focus on: " + focus
	}
	return req
}

// CompactedSummary is the message that replaces the conversation.
func CompactedSummary(summary string) string {
	return strings.TrimSpace(compressionSummaryText) + "\n\n" + strings.TrimSpace(summary) + "\n\n" + strings.TrimSpace(compressionResumeText)
}

// CompactionAck is the model's reply to the summary, which keeps the
// conversation alternating between the user and the model.
func CompactionAck() string { return strings.TrimSpace(compressionAckText) }

// IsCompactedSummary reports whether a user message is the summary that
// compaction put in place of the conversation before it.
func IsCompactedSummary(text string) bool {
	return strings.HasPrefix(text, strings.TrimSpace(compressionSummaryText))
}

// CompressionRetry asks again for a summary that didn't come, without the
// drafting that filled the reply.
func CompressionRetry() string { return strings.TrimSpace(compressionRetryText) }

// The /goal prompts: the message that starts work on a goal, the one that
// continues it when a check finds it not yet met, and the check itself.

//go:embed goal_start.md
var goalStartText string

//go:embed goal_continue.md
var goalContinueText string

//go:embed goal_check.md
var goalCheckText string

// GoalStart is the message that sets the model to work on a goal.
func GoalStart(condition string) string {
	return strings.ReplaceAll(strings.TrimSpace(goalStartText), "{condition}", condition)
}

// GoalContinue sends the model back to work after a check.
func GoalContinue(condition, reason string) string {
	return strings.NewReplacer("{condition}", condition, "{reason}", reason).Replace(strings.TrimSpace(goalContinueText))
}

// GoalCheck asks the model whether a goal is met.
func GoalCheck(condition string) string {
	return strings.ReplaceAll(strings.TrimSpace(goalCheckText), "{condition}", condition)
}

//go:embed btw.md
var btwText string

// Aside wraps a side question (/btw), which the model answers without
// tools and without it joining the conversation.
func Aside(question string) string {
	return strings.ReplaceAll(strings.TrimSpace(btwText), "{question}", question)
}
