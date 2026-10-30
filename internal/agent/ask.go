package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"debbie/internal/prompt"
	"debbie/internal/tools"
)

// Question is one question the model puts to the user with the ask_user
// tool, which works like Claude Code's: answers to pick from, and one the
// user types instead.
type Question struct {
	Header  string // a short label, such as "Database"; "" for none
	Text    string
	Options []Choice // two to four
	Multi   bool     // the user may pick several
}

// Choice is an answer the model offers.
type Choice struct {
	Label, Description string
}

// Answer is the user's answer to one question: the answers picked, by
// label, and what they typed instead or besides.
type Answer struct {
	Picked []string
	Typed  string
}

// Asker is a host that can put questions to the user: the terminal UI. A
// subagent's host and a headless run can't, and the model hears so.
type Asker interface {
	// AskUser asks the questions and returns an answer to each; answered
	// is false when the user chose not to answer.
	AskUser(ctx context.Context, qs []Question) (answers []Answer, answered bool, err error)
}

// AskTool is the ask_user tool.
type AskTool struct{}

func (AskTool) Name() string            { return "ask_user" }
func (AskTool) Kind() tools.Kind        { return tools.Read } // it asks the user, so no one approves it
func (AskTool) Description() string     { return prompt.Tool("ask_user") }
func (AskTool) Params() []tools.Param   { return nil }
func (AskTool) Schema() json.RawMessage { return askSchema }

var askSchema = json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":4,` +
	`"description":"The questions, asked one after another.","items":{"type":"object","properties":{` +
	`"question":{"type":"string","description":"The question, ending in a question mark."},` +
	`"header":{"type":"string","description":"A label of at most 12 characters, such as \"Database\"."},` +
	`"options":{"type":"array","minItems":2,"maxItems":4,"description":"The answers to choose from; the user can always type another.",` +
	`"items":{"type":"object","properties":{"label":{"type":"string","description":"The answer, in a few words."},` +
	`"description":{"type":"string","description":"What choosing it means."}},"required":["label"]}},` +
	`"multi_select":{"type":"boolean","description":"Whether the user may pick more than one answer."}},` +
	`"required":["question","options"]}}},"required":["questions"]}`)

// Prepare checks the questions. Asking needs no approval; the call's Run
// asks the user through the turn's host.
func (AskTool) Prepare(_ context.Context, _ *tools.Env, args tools.Args) (*tools.Call, error) {
	qs, err := parseQuestions(args)
	if err != nil {
		return nil, err
	}
	summary := qs[0].Text
	if len(qs) > 1 {
		summary += fmt.Sprintf(" (and %d more)", len(qs)-1)
	}
	return &tools.Call{Kind: tools.Read, Summary: "ask_user " + summary, Run: func(ctx context.Context) (tools.Result, error) {
		host, _ := callFrom(ctx)
		asker, ok := host.(Asker)
		if !ok {
			return tools.Result{}, errors.New("the user can't be asked here: carry on with your best judgment, and say what you assumed")
		}
		answers, answered, err := asker.AskUser(ctx, qs)
		if err != nil {
			return tools.Result{}, err
		}
		if !answered {
			return tools.Result{Output: "The user chose not to answer. Carry on with your best judgment, or ask in your reply.",
				Display: "not answered"}, nil
		}
		return tools.Result{Output: answerText(qs, answers), Display: answerDisplay(qs, answers)}, nil
	}}, nil
}

// parseQuestions reads and checks the questions, saying what is wrong so
// the model can fix the call.
func parseQuestions(args tools.Args) ([]Question, error) {
	raw, _ := json.Marshal(args["questions"])
	var in []struct {
		Question string `json:"question"`
		Header   string `json:"header"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
		Multi bool `json:"multi_select"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in) == 0 {
		return nil, errors.New("questions must be a list of one to four questions, each with question and options")
	}
	if len(in) > 4 {
		return nil, fmt.Errorf("ask at most 4 questions at once, not %d", len(in))
	}
	var qs []Question
	for i, q := range in {
		text := strings.TrimSpace(q.Question)
		if text == "" {
			return nil, fmt.Errorf("question %d is empty", i+1)
		}
		if len(q.Options) < 2 || len(q.Options) > 4 {
			return nil, fmt.Errorf("question %d needs two to four options, not %d", i+1, len(q.Options))
		}
		out := Question{Header: strings.TrimSpace(q.Header), Text: text, Multi: q.Multi}
		seen := map[string]bool{}
		for j, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			switch {
			case label == "":
				return nil, fmt.Errorf("option %d of question %d has no label", j+1, i+1)
			case seen[strings.ToLower(label)]:
				return nil, fmt.Errorf("question %d offers %q twice", i+1, label)
			case strings.EqualFold(label, "other"):
				return nil, fmt.Errorf("question %d: leave out %q; the user can always type an answer of their own", i+1, label)
			}
			seen[strings.ToLower(label)] = true
			out.Options = append(out.Options, Choice{Label: label, Description: strings.TrimSpace(o.Description)})
		}
		qs = append(qs, out)
	}
	return qs, nil
}

// answerText is the answers for the model.
func answerText(qs []Question, answers []Answer) string {
	var b strings.Builder
	b.WriteString("The user answered:\n")
	for i, q := range qs {
		fmt.Fprintf(&b, "- %q: %s\n", q.Text, answerWords(answers[i]))
	}
	b.WriteString("Carry on with these answers.")
	return b.String()
}

func answerWords(a Answer) string {
	var parts []string
	for _, p := range a.Picked {
		parts = append(parts, fmt.Sprintf("%q", p))
	}
	if a.Typed != "" {
		parts = append(parts, fmt.Sprintf("in their own words: %q", a.Typed))
	}
	if len(parts) == 0 {
		return "(no answer)"
	}
	return strings.Join(parts, ", ")
}

// answerDisplay is the answers in brief, for the UI: with several
// questions, each after its question's header.
func answerDisplay(qs []Question, answers []Answer) string {
	var parts []string
	for i, a := range answers {
		words := append([]string(nil), a.Picked...)
		if a.Typed != "" {
			words = append(words, a.Typed)
		}
		s := strings.Join(words, ", ")
		if s == "" {
			s = "no answer"
		}
		if len(qs) > 1 {
			label := qs[i].Header
			if label == "" {
				label = fmt.Sprintf("question %d", i+1)
			}
			s = label + ": " + s
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}
