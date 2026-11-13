package sending

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestRoutingReadsAndMovesANumber(t *testing.T) {
	var posted string
	sw, err := NewSignalWire("example.signalwire.com", "proj-1", "token", "key", "https://api.visicomms.com")
	if err != nil {
		t.Fatal(err)
	}
	sw.HTTP = &http.Client{Transport: resendTransport(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/IncomingPhoneNumbers.json"):
			return resendResponse(200, `{"incoming_phone_numbers":[
				{"sid":"PN-other","account_sid":"someone-else","phone_number":"+14155550199","voice_url":"https://evil.example"},
				{"sid":"PN1","account_sid":"proj-1","phone_number":"+14155550199","voice_url":"https://old.example.com/signalwire/voice","voice_method":"POST","sms_url":"https://old.example.com/signalwire/sms","voice_receive_mode":"voice"}]}`), nil
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/Accounts/proj-1/IncomingPhoneNumbers/PN1.json"):
			r.ParseForm()
			posted = r.PostForm.Encode()
			return resendResponse(200, `{}`), nil
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL)
		return nil, nil
	})}
	ctx := context.Background()
	cur, err := sw.Routing(ctx, "+14155550199")
	if err != nil || cur.SID != "PN1" || cur.VoiceURL != "https://old.example.com/signalwire/voice" {
		t.Fatalf("routing = %+v %v", cur, err)
	}
	if cur.TakesFaxes() {
		t.Fatal("a voice number reads as taking faxes")
	}
	if _, err := sw.Routing(ctx, "+13105550000"); err != ErrNotInProject {
		t.Fatalf("unknown number: %v", err)
	}
	// A fax number keeps its calls; only texts move.
	if err := sw.SetRouting(ctx, Routing{SID: "PN1", SMSURL: sw.InboundURL()}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(posted, "VoiceUrl") || !strings.Contains(posted, "SmsUrl=https%3A%2F%2Fapi.visicomms.com%2Fwebhooks%2Fsignalwire%2Fsms") {
		t.Fatalf("posted %s", posted)
	}
	if err := sw.SetRouting(ctx, Routing{SID: "PN1", VoiceURL: "https://api.visicomms.com/webhooks/signalwire/voice", StatusCallback: "https://api.visicomms.com/webhooks/signalwire/voice/status"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"VoiceUrl=", "VoiceMethod=POST", "StatusCallback=", "StatusCallbackMethod=POST"} {
		if !strings.Contains(posted, want) {
			t.Fatalf("posted %s, want %s", posted, want)
		}
	}
}
