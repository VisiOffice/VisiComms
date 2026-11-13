// Package untrusted fences text a contact wrote and flags attempts to
// instruct an agent with it.
//
// No model can be relied on to ignore instructions hidden in a message, so
// these are not the defence: policy is (an agent cannot exceed its
// permissions even when misled). Fencing makes contact text read as quoted
// data; a flag holds the thread's sends for a human, so a false positive
// costs one review.
package untrusted

import (
	"regexp"

	"github.com/VisiOffice/VisiComms/internal/email"
)

const (
	openTag  = "<contact_message>"
	closeTag = "</contact_message>"
)

var fenceTags = regexp.MustCompile(`(?i)</?contact_message>`)

// Wrap fences contact-written text for a model. Any fence tags inside the
// text are removed, so it cannot close the fence early.
func Wrap(text string) string {
	return openTag + "\n" + fenceTags.ReplaceAllString(text, "") + "\n" + closeTag
}

// Not "agent": "Dear Agent" is how people write to a rental agent.
const agentWords = `(?:ai|a\.i\.|assistant|chatbot|llm|language model|gpt|chatgpt|claude|gemini)`

type pattern struct {
	reason string
	re     *regexp.Regexp
}

var patterns = []pattern{
	// "ignore my previous instructions" is a customer correcting themselves;
	// only the agent's own instructions count.
	{"asks to ignore instructions", regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override|bypass)\s+(?:all\s+|any\s+)?(?:of\s+)?(?:your|the|all|any|previous|prior|above|earlier|system)\b(?:\s+\w+){0,2}?\s+(?:instructions?|prompts?|rules|guidelines)\b`)},
	{"mentions a system prompt", regexp.MustCompile(`(?i)\b(?:system|developer)\s+(?:prompt|instructions)\b`)},
	{"addresses the AI directly", regexp.MustCompile(`(?i)\b(?:if you are|you are now|as) an? ` + agentWords + `\b|\b(?:dear|attention|note to( the)?|hey) ` + agentWords + `\b|\b` + agentWords + `s? (?:reading|processing|handling|summari[sz]ing) this\b`)},
	{"gives the AI new instructions", regexp.MustCompile(`(?i)\b(?:new|updated|additional|real|actual|hidden|secret) instructions?\s+(?:for|to)\s+(?:the\s+|this\s+|any\s+|all\s+)?` + agentWords + `\b`)},
	{"contains model control tokens", regexp.MustCompile(`(?i)<\|(?:im_start|im_end|system|user|assistant|endoftext)\|>|\[/?INST\]|<</?SYS>>|</?(?:system|tool_call|function_call)>`)},
	// VisiComms tool names.
	{"names an agent tool", regexp.MustCompile(`(?i)\b(?:threads\.(?:claim|resolve|handoff)|contacts\.(?:update|merge|remember)|proposals\.(?:list|get)|rules\.propose|calls\.place|mail\.send|bookings\.create|connectors\.status)\b`)},
}

// SuspiciousReasons lists why text or HTML looks like an attempt to instruct
// an agent, or nil. Hidden HTML text is checked on its own as well, since a
// reader never sees it.
func SuspiciousReasons(text, html string) []string {
	visible, hidden := email.SplitHidden(html)
	var reasons []string
	hiddenHit := false
	for _, p := range patterns {
		if p.re.MatchString(hidden) {
			hiddenHit = true
		}
		if p.re.MatchString(text) || p.re.MatchString(visible) || p.re.MatchString(hidden) {
			reasons = append(reasons, p.reason)
		}
	}
	if hiddenHit {
		reasons = append(reasons, "instructions hidden in the email's HTML")
	}
	return reasons
}
