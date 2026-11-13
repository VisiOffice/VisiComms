package email

import (
	"regexp"
	"strings"
)

// Kind says whether an inbound email can need an answer. Only KindMessage
// can wake an agent; the others are recorded and nothing else happens.
type Kind string

const (
	KindMessage   Kind = "message"
	KindBounce    Kind = "bounce"
	KindAutoReply Kind = "auto_reply"
	KindAutomated Kind = "automated"
)

// Classification is the cheap, rule-based triage that runs before any model.
type Classification struct {
	Kind   Kind   `json:"kind"`
	Reason string `json:"reason"`
	// OptOut is set when the sender asked not to be emailed again.
	OptOut bool `json:"opt_out"`
}

var (
	bounceFrom       = regexp.MustCompile(`(?i)^(postmaster|mailer-daemon|maildeliverysubsystem)@`)
	bounceSubject    = regexp.MustCompile(`(?i)delivery status notification|undeliver(able|ed)|mail delivery failed|delivery has failed|returned mail|failure notice`)
	bounceBody       = regexp.MustCompile(`(?i)an error occurred while trying to deliver the mail|delivery has failed to these recipients|couldn't be delivered|message blocked|failed recipient`)
	autoReplySubject = regexp.MustCompile(`(?i)automatic reply|auto(matic)? response|out of office|away from (the )?office|^ooo\b`)
	noReplyFrom      = regexp.MustCompile(`(?i)^(noreply|no-reply|no_reply|notifications|alerts?|bounces?)[+@]`)
	// A short message whose whole point is "stop emailing me". Matched on the
	// subject or on the first line written above any quoted reply.
	optOutLine = regexp.MustCompile(`(?i)^\W*(unsubscribe|stop|opt[ -]?out|remove me( from (your|this) (list|emails?))?|please remove me|do not (email|contact) me( again)?)\W*$`)
)

// Classify triages an inbound email. Bounces are checked before opt-outs so a
// returned message quoting "unsubscribe" does not opt anyone out.
func Classify(in Inbound) Classification {
	body := in.Text + "\n" + in.HTML
	switch {
	case bounceFrom.MatchString(in.From.Email):
		return Classification{Kind: KindBounce, Reason: "sender is a mail server"}
	case in.Headers["x-failed-recipients"] != "" || strings.Contains(in.Headers["content-type"], "message/delivery-status"):
		return Classification{Kind: KindBounce, Reason: "delivery status headers"}
	case bounceSubject.MatchString(in.Subject) || bounceBody.MatchString(body):
		return Classification{Kind: KindBounce, Reason: "delivery failure wording"}
	}
	if autoReply(in) {
		return Classification{Kind: KindAutoReply, Reason: "automatic reply header or subject"}
	}
	if isOptOut(in) {
		return Classification{Kind: KindMessage, Reason: "asks not to be emailed", OptOut: true}
	}
	if noReplyFrom.MatchString(in.Contact.Email) {
		return Classification{Kind: KindAutomated, Reason: "no-reply sender"}
	}
	return Classification{Kind: KindMessage, Reason: "default"}
}

func autoReply(in Inbound) bool {
	if v := strings.TrimSpace(in.Headers["auto-submitted"]); v != "" && !strings.EqualFold(v, "no") {
		return true
	}
	if in.Headers["x-autoreply"] != "" || in.Headers["x-autorespond"] != "" {
		return true
	}
	if p := strings.ToLower(strings.TrimSpace(in.Headers["precedence"])); p == "bulk" || p == "auto_reply" {
		return true
	}
	return autoReplySubject.MatchString(strings.TrimSpace(in.Subject))
}

func isOptOut(in Inbound) bool {
	subject := strings.TrimSpace(in.Subject)
	for _, prefix := range []string{"re:", "fwd:", "fw:"} {
		if len(subject) >= len(prefix) && strings.EqualFold(subject[:len(prefix)], prefix) {
			subject = strings.TrimSpace(subject[len(prefix):])
		}
	}
	if optOutLine.MatchString(subject) {
		return true
	}
	return optOutLine.MatchString(firstLine(in.Text))
}

// firstLine is the first non-empty line the sender wrote, before any quoted
// reply ("> ..." or "On <date>, <name> wrote:").
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ">") || (strings.HasPrefix(line, "On ") && strings.HasSuffix(line, "wrote:")) {
			return ""
		}
		return line
	}
	return ""
}
