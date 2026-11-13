package config

import "testing"

func TestEmailProviderSelection(t *testing.T) {
	for _, tc := range []struct {
		name, provider, resend, ses, want string
		invalid                           bool
	}{
		{name: "existing installation", want: "ses"},
		{name: "Resend key", resend: "re_test", want: "resend"},
		{name: "both keys preserve SES", resend: "re_test", ses: "ses_test", want: "ses"},
		{name: "explicit Resend", provider: "resend", resend: "re_test", ses: "ses_test", want: "resend"},
		{name: "invalid", provider: "typo", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISICOMMS_ENV", "development")
			t.Setenv("DATABASE_URL", "")
			t.Setenv("EMAIL_PROVIDER", tc.provider)
			t.Setenv("RESEND_API_KEY", tc.resend)
			t.Setenv("AWS_SES_ACCESS_KEY_ID", tc.ses)
			t.Setenv("INBOUND_EMAIL_DOMAIN", "")
			cfg, err := Load()
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid provider accepted")
				}
				return
			}
			if err != nil || cfg.EmailProvider != tc.want {
				t.Fatalf("provider=%s, error=%v", cfg.EmailProvider, err)
			}
			if tc.want == "resend" && cfg.InboundDomain != "" {
				t.Fatal("Resend must not default to the hosted inbound domain")
			}
		})
	}
}
