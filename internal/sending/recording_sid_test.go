package sending

import "testing"

func TestRecordingSIDAcceptsSignalWireURLShapesForThisProjectOnly(t *testing.T) {
	const project = "11111111-2222-3333-4444-555555555555"
	const rec = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const call = "99999999-8888-7777-6666-555555555555"
	const space = "77777777-7777-7777-7777-777777777777"
	sw := &SignalWire{Space: "example.signalwire.com", Project: project}
	ok := map[string]string{
		"https://example.signalwire.com/api/laml/2010-04-01/Accounts/" + project + "/Calls/" + call + "/Recordings/" + rec: rec,
		"https://example.signalwire.com/api/laml/2010-04-01/Accounts/" + project + "/Recordings/" + rec + ".mp3":           rec,
		"https://example.signalwire.com/api/laml/2010-04-01/Accounts/" + project + "/Recordings/RE1":                       "RE1",
		"https://example.signalwire.com/" + space + "/" + project + "/recordings/" + rec + ".wav":                          rec,
		"https://files.signalwire.com/" + space + "/" + project + "/recordings/" + rec + ".wav":                            rec,
	}
	for raw, want := range ok {
		if got, err := sw.recordingSID(raw); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", raw, got, err, want)
		}
	}
	other := "00000000-0000-0000-0000-000000000000"
	bad := []string{
		"http://example.signalwire.com/api/laml/2010-04-01/Accounts/" + project + "/Recordings/" + rec,
		"https://evil.example/api/laml/2010-04-01/Accounts/" + project + "/Recordings/" + rec,
		"https://example.signalwire.com/api/laml/2010-04-01/Accounts/" + other + "/Recordings/" + rec,
		"https://files.signalwire.com/" + space + "/" + other + "/recordings/" + rec + ".wav",
		"https://files.signalwire.com/" + project + "/" + other + "/recordings/" + rec + ".wav",
		"https://files.signalwire.com/x/" + space + "/" + project + "/recordings/" + rec + ".wav",
		"https://example.signalwire.com/api/laml/2010-04-01/Accounts/" + project + "/Calls/" + call,
		"https://example.signalwire.com/api/laml/2010-04-01/Accounts/" + project + "/Recordings/not-an-id",
		"https://example.signalwire.com/api/laml/2010-04-01/Accounts/" + project + "/Recordings/" + rec + "/../" + rec,
	}
	for _, raw := range bad {
		if sid, err := sw.recordingSID(raw); err == nil {
			t.Errorf("%s: accepted as %q", raw, sid)
		}
	}
}
