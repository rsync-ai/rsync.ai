package notifier

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Slack and email are the two places alert text leaves rsync.ai. Everything the
// alert carries from the event — the error message and the pipeline name — is
// free text written by a connector, a database, or a user, so it is treated as
// untrusted on the way out: secrets are scrubbed, Slack's control sequences
// are escaped, and the length is capped. Pins KI-ALERT-TEXT-UNSCRUBBED,
// KI-SLACK-ALERT-MRKDWN-UNESCAPED and KI-ALERT-TEXT-UNCAPPED.

// slackStrings returns every string value in a Slack payload, so an
// assertion covers the fallback text, attachment fields and Block Kit alike
// without depending on where each one sits.
func slackStrings(v interface{}) []string {
	var out []string
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case map[string]interface{}:
		for _, e := range x {
			out = append(out, slackStrings(e)...)
		}
	case []map[string]interface{}:
		for _, e := range x {
			out = append(out, slackStrings(e)...)
		}
	case []interface{}:
		for _, e := range x {
			out = append(out, slackStrings(e)...)
		}
	}
	return out
}

// bothSlackShapes renders one alert as the legacy attachment and as Block Kit.
func bothSlackShapes(p notificationPayload, r Rendered) map[string]map[string]interface{} {
	n := &Notifier{appBaseURL: "https://app.rsync.ai", interactiveApprovals: true}
	return map[string]map[string]interface{}{
		"attachment": n.slackPayload(p, r, false),
		"block kit":  n.slackPayload(p, r, true),
	}
}

func alertFixture(message string) (notificationPayload, Rendered) {
	p := notificationPayload{
		Type:       "structured_error_notification",
		PipelineID: "pl-7",
		Message:    message,
		ActionURL:  "/pipelines/pl-7",
	}
	r := resolve("PIPELINE_RUN_FAILED", p.Type, notifyTopic, "error", map[string]string{"pipeline": "orders-sync"})
	return p, r
}

const leakyError = "pg_dump: connection to postgres://rsync:S3cretPass@db.internal:5432/app failed; " +
	"retry with password=hunter2; Failing row contains (42, alice@example.com, 555-12-3456)"

var leakedSecrets = []string{"S3cretPass", "hunter2", "alice@example.com", "555-12-3456"}

func TestSlackAlertTextIsScrubbed(t *testing.T) {
	p, r := alertFixture(leakyError)
	for shape, payload := range bothSlackShapes(p, r) {
		for _, s := range slackStrings(payload) {
			for _, secret := range leakedSecrets {
				if strings.Contains(s, secret) {
					t.Errorf("%s: %q reached Slack: %s", shape, secret, s)
				}
			}
		}
	}
}

func TestEmailAlertTextIsScrubbed(t *testing.T) {
	p, r := alertFixture(leakyError)
	m := (&Notifier{appBaseURL: "https://app.rsync.ai"}).alertEmail(p, r, "footer")
	for _, secret := range leakedSecrets {
		if strings.Contains(m.Body, secret) || strings.Contains(m.Subject, secret) {
			t.Errorf("%q reached the email:\n%s", secret, m.Body)
		}
	}
}

// A message is rendered as Slack mrkdwn, where <url|text> is a link, <!channel>
// pings everyone and <@U…> mentions a person. Text from an error or a pipeline
// name must show those characters, never act on them.
func TestSlackAlertTextCannotInjectControlSequences(t *testing.T) {
	const injected = "see <https://evil.example/login|Open rsync-ai> <!channel> <@U024BE7LH> & more"
	p, r := alertFixture(injected)
	r.Title = "<!here> orders-sync failed"
	r.PipelineName = "<!everyone> orders"
	for shape, payload := range bothSlackShapes(p, r) {
		for _, s := range slackStrings(payload) {
			for _, seq := range []string{"<https://evil", "<!channel>", "<!here>", "<!everyone>", "<@U024BE7LH>"} {
				if strings.Contains(s, seq) {
					t.Errorf("%s: control sequence %q reached Slack unescaped: %s", shape, seq, s)
				}
			}
		}
		all := strings.Join(slackStrings(payload), "\n")
		// Escaped, not deleted: the reader still sees what the error said.
		if !strings.Contains(all, "&lt;!channel&gt;") || !strings.Contains(all, "&amp; more") {
			t.Errorf("%s: the text was not escaped the way Slack reads it:\n%s", shape, all)
		}
	}
}

// Buttons and the view link are rsync.ai's own, so they keep working.
func TestSlackEscapingLeavesRsyncsOwnLinkAlone(t *testing.T) {
	p, r := alertFixture("plain failure")
	for shape, payload := range bothSlackShapes(p, r) {
		all := strings.Join(slackStrings(payload), "\n")
		if !strings.Contains(all, "https://app.rsync.ai/pipelines/pl-7") {
			t.Errorf("%s: the view link was mangled:\n%s", shape, all)
		}
	}
}

// Slack splits a message whose text runs past 4,000 characters into two posts,
// and rejects a Block Kit section over 3,000.
func TestSlackAlertTextIsCapped(t *testing.T) {
	for name, message := range map[string]string{
		"long prose": strings.Repeat("sink error: destination write timed out; ", 120), // ~4,900
		// Every character escapes to five, so the cap has to hold after escaping
		// and must not cut an entity in half.
		"all escapes": strings.Repeat("<", 2500),
	} {
		t.Run(name, func(t *testing.T) {
			p, r := alertFixture(message)
			for shape, payload := range bothSlackShapes(p, r) {
				for _, s := range slackStrings(payload) {
					if n := utf8.RuneCountInString(s); n > 3000 {
						t.Errorf("%s: a %d-character text reached Slack", shape, n)
					}
					if i := strings.LastIndex(s, "&"); i >= 0 && !strings.Contains(s[i:], ";") {
						t.Errorf("%s: the cap cut an escape in half: …%s", shape, s[max(0, len(s)-12):])
					}
				}
				all := strings.Join(slackStrings(payload), "\n")
				if !strings.Contains(all, "…") {
					t.Errorf("%s: a truncated message does not say it was truncated", shape)
				}
			}
		})
	}
}

func TestEmailAlertTextIsCapped(t *testing.T) {
	p, r := alertFixture(strings.Repeat("sink error: destination write timed out; ", 400)) // ~16,000
	m := (&Notifier{appBaseURL: "https://app.rsync.ai"}).alertEmail(p, r, "footer")
	if n := utf8.RuneCountInString(m.Body); n > 4000 {
		t.Fatalf("a %d-character email body was sent", n)
	}
	// The link to the full error survives the cap.
	if !strings.Contains(m.Body, "https://app.rsync.ai/pipelines/pl-7") || !strings.Contains(m.Body, "footer") {
		t.Fatalf("the cap cut the email's link or footer:\n%s", m.Body)
	}
}

// Control: an ordinary message reaches both channels word for word.
func TestAnOrdinaryAlertIsUnchanged(t *testing.T) {
	const msg = "Table events failed: connection refused by the destination"
	p, r := alertFixture(msg)
	for shape, payload := range bothSlackShapes(p, r) {
		if !strings.Contains(strings.Join(slackStrings(payload), "\n"), msg) {
			t.Errorf("%s: an ordinary message was altered", shape)
		}
	}
	m := (&Notifier{appBaseURL: "https://app.rsync.ai"}).alertEmail(p, r, "footer")
	if !strings.Contains(m.Body, msg) {
		t.Errorf("email: an ordinary message was altered:\n%s", m.Body)
	}
}

// Pins KI-ALERT-FALLBACK-COPY-OVERCLAIMS. An event no catalog entry knows is,
// by definition, one rsync.ai cannot vouch for: its copy may not say the data is
// still moving.
func TestFallbackCopyDoesNotClaimSyncingContinues(t *testing.T) {
	for _, sev := range []string{severityInfo, severityWarning, severityCritical} {
		for _, eventType := range []string{"", "brand_new_event"} {
			impact := strings.ToLower(fallbackEntry(eventType, sev).Impact)
			for _, claim := range []string{"continues", "still syncing", "is syncing", "no action"} {
				if strings.Contains(impact, claim) {
					t.Errorf("fallback (%s, %q) claims %q: %q", sev, eventType, claim, impact)
				}
			}
			if sev != severityInfo && !strings.Contains(impact, "check") {
				t.Errorf("fallback (%s, %q) does not tell the reader to check: %q", sev, eventType, impact)
			}
		}
	}
}
