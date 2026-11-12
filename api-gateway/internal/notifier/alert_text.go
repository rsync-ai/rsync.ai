package notifier

import (
	"unicode/utf8"

	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
)

// Slack and email are where alert text leaves rsync.ai. The event's message is
// free text from a connector, a database or a sink — a DSN with its password,
// a failing row's values — and the pipeline name is typed by a user, so both
// are treated as untrusted on the way out (KI-ALERT-TEXT-UNSCRUBBED,
// KI-SLACK-ALERT-MRKDWN-UNESCAPED, KI-ALERT-TEXT-UNCAPPED).
const (
	// slackTextMax is under both Slack limits that bit: a Block Kit section
	// rejects text over 3,000 characters, and a message over about 4,000 is
	// split into two posts.
	slackTextMax = 3000
	// alertNameMax keeps a long title or pipeline name from eating the
	// message's room.
	alertNameMax = 250
	// emailBodyMax bounds the email body; the link to the full error is kept.
	emailBodyMax = 4000
)

// alertMessage is the event's message as it may leave rsync.ai: secrets and row
// values masked by the same scrubber that guards LLM prompts.
func alertMessage(message string) string {
	return llmscrub.Scrub(message)
}

// slackEscape escapes the three characters Slack reads as markup, so
// <url|text> cannot become a link, <!channel> cannot ping and <@U…> cannot
// mention anyone. The reader still sees what the text said.
func slackEscape(s string) string {
	return slackEscapeMax(s, 0)
}

// slackEscapeMax escapes s and caps the result at max characters (0: no cap).
// The cap is counted after escaping — one "<" is four characters on the wire —
// and never cuts an entity in half. A cut text ends in "…".
func slackEscapeMax(s string, max int) string {
	out := make([]rune, 0, len(s))
	budget := max - 1 // room for the "…"
	for i, r := range s {
		var piece string
		switch r {
		case '&':
			piece = "&amp;"
		case '<':
			piece = "&lt;"
		case '>':
			piece = "&gt;"
		default:
			piece = string(r)
		}
		if max > 0 && len(out)+utf8.RuneCountInString(piece) > budget {
			// Only cut when what is left does not fit either.
			if len(out)+escapedLen(s[i:]) <= max {
				return string(out) + slackEscape(s[i:])
			}
			return string(out) + "…"
		}
		out = append(out, []rune(piece)...)
	}
	return string(out)
}

// escapedLen is s's length in characters once escaped for Slack.
func escapedLen(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case '&':
			n += 5
		case '<', '>':
			n += 4
		default:
			n++
		}
	}
	return n
}

// capRunes cuts s to at most max characters, the last of them "…".
func capRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}
