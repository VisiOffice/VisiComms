package phone

import "testing"

func TestInternationalNormalization(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"+1 (415) 555-0123", "+14155550123"}, {"+44 20 7946 0958", "+442079460958"},
		{"4155550123", ""}, {"+0123456789", ""}, {"+14155550123 ext 2", ""}, {"+1415555012345678", ""},
	} {
		got, err := Normalize(tc.input)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("Normalize(%q)=%q,%v", tc.input, got, err)
		}
	}
}
