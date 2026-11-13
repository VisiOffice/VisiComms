package voice

import (
	"strings"
	"testing"
)

func TestTheAIKnowsAGateVisitorFromACaller(t *testing.T) {
	gate := AICall{Call: Call{Entrance: "Front gate"}, Agent: Agent{Opening: "Hello", Instructions: "Leave packages at the gate."}, Business: "Cedar Court"}
	caller := gate
	caller.Entrance = ""
	for name, prompt := range map[string]func(AICall) string{"realtime": AICall.Instructions, "live": AICall.LiveInstructions, "backend": AICall.BackendInstructions} {
		g, c := prompt(gate), prompt(caller)
		if !strings.Contains(g, "intercom at Front gate") || strings.Contains(c, "intercom") {
			t.Errorf("%s prompt does not tell a gate visitor from a caller", name)
		}
	}
	// The business's visitor rules come after the gate's, so they win.
	g := gate.Instructions()
	if strings.Index(g, "intercom at Front gate") > strings.Index(g, "Leave packages at the gate.") {
		t.Error("the business's rules come before the gate's")
	}
	if !strings.Contains(g, "You cannot open the door or gate") {
		t.Error("the AI is not told it cannot open the gate")
	}
}

func TestSIPUser(t *testing.T) {
	for from, want := range map[string]string{
		"sip:gate-abc@example.sip.signalwire.com":          "gate-abc",
		"sip:gate-abc@example.sip.signalwire.com:5060;x=1": "gate-abc",
		"sips:gate-abc@example.sip.signalwire.com":         "gate-abc",
		"+13105550100":                                      "",
		"sip:@example.sip.signalwire.com":                   "",
		"sip:gate abc@example.sip.signalwire.com":           "",
		"<sip:gate-abc@example.sip.signalwire.com>;tag=bad": "",
	} {
		if got, _ := sipUser(from); got != want {
			t.Errorf("sipUser(%q) = %q, want %q", from, got, want)
		}
	}
}
