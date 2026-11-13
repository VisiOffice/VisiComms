package voice

import (
	"encoding/xml"
	"net/url"
	"strconv"
	"strings"
)

// Callback paths, under the public origin. SignalWire posts each with the
// call's CallSid and AccountSid, signed with the project's key.
const (
	PathIncoming   = "/webhooks/signalwire/voice"
	PathMenu       = "/webhooks/signalwire/voice/menu"
	PathDial       = "/webhooks/signalwire/voice/dial"
	PathRecorded   = "/webhooks/signalwire/voice/recorded"
	PathTranscript = "/webhooks/signalwire/voice/transcript"
	PathStatus     = "/webhooks/signalwire/voice/status"
	// PathAIDial follows the leg handed to the AI; PathMessage takes a
	// message from a call the AI could not answer.
	PathAIDial  = "/webhooks/signalwire/voice/ai-dial"
	PathMessage = "/webhooks/signalwire/voice/message"
	// PathAIRecording receives the recording of a call the AI answered.
	PathAIRecording = "/webhooks/signalwire/voice/ai-recording"
)

// aiRingSeconds is how long OpenAI has to pick up before voicemail.
const aiRingSeconds = 20

// voicemailSeconds is the longest message a caller can leave.
const voicemailSeconds = 120

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return strings.ReplaceAll(b.String(), `"`, "&#34;")
}

func response(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><Response>` + body + `</Response>`
}

// Answer is the markup for a call's first step.
func Answer(publicURL string, c Call) string {
	if c.Flow.Answer.Mode == IVR && c.Flow.Menu != nil {
		return MenuPrompt(publicURL, c.Flow)
	}
	return Act(publicURL, c, RouteAnswer, 0)
}

// dialAction is where a ring or forward reports back, naming the step so an
// unanswered one can fall through to the next.
func dialAction(publicURL, route string, depth int) string {
	return esc(publicURL + PathDial + "?" + url.Values{"route": {route}, "depth": {strconv.Itoa(depth)}}.Encode())
}

// Act is the markup for the step depth fallbacks into route. Anything this
// build cannot run takes a message instead, so a call is never dropped.
func Act(publicURL string, c Call, route string, depth int) string {
	f := c.Flow
	a := f.At(route, depth)
	if a == nil {
		return TakeMessage(publicURL, f)
	}
	switch {
	case a.Mode == Forward && a.ForwardTo != "":
		return response(`<Dial timeout="` + strconv.Itoa(f.RingSeconds) + `" action="` + dialAction(publicURL, route, depth) + `" method="POST"><Number>` + esc(a.ForwardTo) + `</Number></Dial>`)
	case a.Mode == RingApp:
		host := VertoHost(c.Space)
		if len(c.Browsers) == 0 || host == "" {
			// Nobody is online to ring.
			return Act(publicURL, c, route, depth+1)
		}
		var b strings.Builder
		for _, resource := range c.Browsers {
			b.WriteString(`<Verto>` + esc(resource+"@"+host) + `</Verto>`)
		}
		return response(`<Dial timeout="` + strconv.Itoa(f.RingSeconds) + `" answerOnBridge="true" action="` + dialAction(publicURL, route, depth) + `" method="POST">` + b.String() + `</Dial>`)
	case a.Mode == AI && openAI != nil && f.Agent != nil && c.ID != "":
		return response(`<Dial timeout="` + strconv.Itoa(aiRingSeconds) + `" action="` + esc(publicURL+PathAIDial) + `" method="POST" record="record-from-answer-dual" recordingStatusCallback="` + esc(publicURL+PathAIRecording) + `" recordingStatusCallbackEvent="completed" recordingStatusCallbackMethod="POST"><Sip codecs="PCMU,PCMA">` + esc(openAI.SIPAddress(c.ID)) + `</Sip></Dial>`)
	}
	return TakeMessage(publicURL, f)
}

// VertoHost is where a SignalWire space's browser phones are dialled.
func VertoHost(space string) string {
	name, ok := strings.CutSuffix(space, ".signalwire.com")
	if !ok || name == "" {
		return ""
	}
	return name + ".verto.signalwire.com"
}

// MenuPrompt plays the IVR and waits for one key. Pressing nothing comes back
// to the menu callback with no digits, which runs the no-choice action.
func MenuPrompt(publicURL string, f Flow) string {
	action := esc(publicURL + PathMenu)
	return response(`<Gather numDigits="1" timeout="6" action="` + action + `" method="POST"><Say>` + esc(f.Menu.Prompt) + `</Say></Gather><Redirect method="POST">` + action + `</Redirect>`)
}

// Choose runs the option for the key pressed, or the no-choice action.
func Choose(publicURL string, c Call, digits string) string {
	if c.Flow.Menu == nil {
		return TakeMessage(publicURL, c.Flow)
	}
	for _, o := range c.Flow.Menu.Options {
		if o.Digit == digits {
			return Act(publicURL, c, RouteKey(o.Digit), 0)
		}
	}
	return Act(publicURL, c, RouteNoChoice, 0)
}

// TakeMessage plays the greeting and records a voicemail, transcribed.
func TakeMessage(publicURL string, f Flow) string {
	return response(`<Say>` + esc(f.Greeting) + `</Say><Record maxLength="` + strconv.Itoa(voicemailSeconds) + `" playBeep="true" action="` +
		esc(publicURL+PathRecorded) + `" method="POST" transcribe="true" transcribeCallback="` + esc(publicURL+PathTranscript) + `"/>`)
}

// Hangup ends the call.
func Hangup() string { return response(`<Hangup/>`) }

// Empty acknowledges a callback with nothing more to do.
func Empty() string { return response("") }
