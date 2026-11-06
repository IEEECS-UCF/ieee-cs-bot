package aidraft

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
)

// Revisions: an officer reads a draft, says what to change, and Claude
// rewrites it in the same conversation, so it keeps the event's facts, the
// example posts and every earlier piece of feedback. A Thread can start from
// a draft Claude wrote or one the officer typed.

// Thread is the conversation behind a draft. Revise returns a new Thread, so
// a failed call leaves the old one as it was.
type Thread struct {
	System   string
	Limit    int
	Messages []anthropic.BetaMessageParam // the request, then draft, feedback, draft...
	Feedback []string                     // what officers asked for, oldest first
}

// Draft is the latest draft.
func (t *Thread) Draft() string {
	last := t.Messages[len(t.Messages)-1]
	for _, b := range last.Content {
		if b.OfText != nil {
			return b.OfText.Text
		}
	}
	return ""
}

func assistant(text string) anthropic.BetaMessageParam {
	return anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant,
		Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(text)}}
}

func newThread(system string, limit int, request []anthropic.BetaContentBlockParamUnion, draft string) *Thread {
	return &Thread{System: system, Limit: limit, Messages: []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(request...),
		assistant(draft),
	}}
}

// AnnouncementThread is an #announcements draft (Claude's or an officer's)
// and what it was written from: Details for an event, FromNotes for notes.
func AnnouncementThread(ask string, examples []string, draft string) *Thread {
	return newThread(System, 2000, Prompt(examples, ask), draft)
}

// ReviseAsk is the turn that asks for a revision.
func ReviseAsk(feedback string) string {
	return "An officer read the draft and wants changes:\n<feedback>\n" + feedback + "\n</feedback>\n" +
		"Rewrite the whole draft with those changes. Keep everything the feedback doesn't mention, " +
		"follow the same rules as before (only the facts given, nothing invented), " +
		"and reply in exactly the same format as the draft, with nothing before or after it."
}

// Revise has Claude rewrite the draft with an officer's feedback: (the new
// thread, "") or (nil, why it failed).
func Revise(ctx context.Context, t *Thread, feedback string) (*Thread, string) {
	messages := append(append([]anthropic.BetaMessageParam{}, t.Messages...),
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(ReviseAsk(feedback))))
	text, why := send(ctx, t.System, messages, t.Limit)
	if why != "" {
		return nil, why
	}
	return &Thread{System: t.System, Limit: t.Limit,
		Messages: append(messages, assistant(text)),
		Feedback: append(append([]string{}, t.Feedback...), feedback)}, ""
}
