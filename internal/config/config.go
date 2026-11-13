// Package config reads settings from the environment. Secrets come from the
// deploy (Kamal secrets), never from files in the repo.
package config

import (
	"errors"
	"os"
	"strings"
)

type Config struct {
	// Env is development, test or production.
	Env         string
	DatabaseURL string
	// Addr is the HTTP listen address, e.g. ":8080".
	Addr string
	// InboundEmailSecret verifies inbound email webhooks; without it every inbound
	// email is rejected.
	InboundEmailSecret string
	// AppURL is where people open the console; invite links point here.
	AppURL string
	// RPID and Origins bind passkeys to the site: RPID is the registrable
	// domain, Origins the exact page origins (comma-separated in env).
	RPID    string
	Origins []string
	// The platform's Amazon SES account. Without keys, sends through SES stay
	// queued.
	SESRegion, SESKeyID, SESSecret, SESConfigurationSet string
	// SESEventsTopic is the SNS topic ARN whose deliveries /webhooks/ses accepts.
	SESEventsTopic string
	// EmailProvider chooses the outbound adapter; inbound remains independent.
	EmailProvider       string
	ResendKey           string
	ResendWebhookSecret string
	InboundDomain       string
	// NotifyFrom is the address notifications are emailed from; it must be
	// verified with the email provider. Unset, notifications are not emailed.
	NotifyFrom           string
	SignalWireSpace      string
	SignalWireProject    string
	SignalWireToken      string
	SignalWireSigningKey string
	SignalWirePublicURL  string
	// PublicURL is the API's own public address (https://api.<domain>); chat
	// widgets load from it. Defaults to SIGNALWIRE_PUBLIC_URL, the same place.
	PublicURL string
	// MasterKey seals each workspace's data key, which seals connector
	// credentials. Without it, workspaces can only use the installation's
	// SIGNALWIRE_* project.
	MasterKey string
	// The hosted parent project, through which 10DLC brands and campaigns are
	// read for every subproject. Self-hosted installations leave it unset.
	SignalWireParentSpace, SignalWireParentProject, SignalWireParentToken string
	// The OpenAI project that answers calls in AI mode. Its webhook must
	// point at /webhooks/openai/realtime, subscribed to
	// live.transport.incoming for GPT-Live or realtime.call.incoming for the
	// realtime API.
	OpenAIKey, OpenAIProject, OpenAIWebhookSecret string
	// OpenAIVoiceAPI picks the API that answers: "live" (GPT-Live) or
	// "realtime", the default.
	OpenAIVoiceAPI string
	// The bucket call recordings are kept in, over the S3 API (Backblaze B2
	// in production). Without a bucket and key, recordings stay at SignalWire
	// and are only deleted there after a year.
	RecordingsEndpoint, RecordingsRegion, RecordingsBucket, RecordingsKeyID, RecordingsSecret string
}

func Load() (Config, error) {
	c := Config{
		SignalWireSpace:      os.Getenv("SIGNALWIRE_SPACE_URL"),
		SignalWireProject:    os.Getenv("SIGNALWIRE_PROJECT_ID"),
		SignalWireToken:      os.Getenv("SIGNALWIRE_API_TOKEN"),
		SignalWireSigningKey: os.Getenv("SIGNALWIRE_SIGNING_KEY"),
		SignalWirePublicURL:  os.Getenv("SIGNALWIRE_PUBLIC_URL"),
		PublicURL:            strings.TrimRight(getenv("PUBLIC_URL", os.Getenv("SIGNALWIRE_PUBLIC_URL")), "/"),
		MasterKey:            os.Getenv("VISICOMMS_MASTER_KEY"),

		SignalWireParentSpace:   os.Getenv("SIGNALWIRE_PARENT_SPACE_URL"),
		SignalWireParentProject: os.Getenv("SIGNALWIRE_PARENT_PROJECT_ID"),
		SignalWireParentToken:   os.Getenv("SIGNALWIRE_PARENT_API_TOKEN"),
		OpenAIKey:               os.Getenv("OPENAI_API_KEY"),
		OpenAIProject:           os.Getenv("OPENAI_PROJECT_ID"),
		OpenAIWebhookSecret:     os.Getenv("OPENAI_WEBHOOK_SECRET"),
		OpenAIVoiceAPI:          getenv("OPENAI_VOICE_API", "realtime"),
		RecordingsEndpoint:      getenv("RECORDINGS_S3_ENDPOINT", "https://s3.us-west-004.backblazeb2.com"),
		RecordingsRegion:        getenv("RECORDINGS_S3_REGION", "us-west-004"),
		RecordingsBucket:        trimmed("RECORDINGS_BUCKET"),
		RecordingsKeyID:         trimmed("RECORDINGS_S3_KEY_ID"),
		RecordingsSecret:        trimmed("RECORDINGS_S3_SECRET"),
		Env:                     getenv("VISICOMMS_ENV", "development"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		Addr:                    ":" + getenv("PORT", "8080"),

		InboundEmailSecret: os.Getenv("INBOUND_EMAIL_SECRET"),
		AppURL:             strings.TrimRight(getenv("APP_URL", "http://localhost:5173"), "/"),
		RPID:               getenv("WEBAUTHN_RP_ID", "localhost"),

		NotifyFrom:          os.Getenv("NOTIFY_FROM"),
		SESRegion:           getenv("AWS_SES_REGION", "us-west-2"),
		SESKeyID:            os.Getenv("AWS_SES_ACCESS_KEY_ID"),
		SESSecret:           os.Getenv("AWS_SES_SECRET_ACCESS_KEY"),
		SESConfigurationSet: os.Getenv("SES_CONFIGURATION_SET"),
		SESEventsTopic:      os.Getenv("SES_EVENTS_TOPIC_ARN"),
		EmailProvider:       os.Getenv("EMAIL_PROVIDER"),
		ResendKey:           os.Getenv("RESEND_API_KEY"),
		ResendWebhookSecret: os.Getenv("RESEND_WEBHOOK_SECRET"),
		InboundDomain:       strings.ToLower(strings.TrimSpace(os.Getenv("INBOUND_EMAIL_DOMAIN"))),
	}
	if c.EmailProvider == "" {
		c.EmailProvider = "ses"
		if c.ResendKey != "" && c.SESKeyID == "" {
			c.EmailProvider = "resend"
		}
	}
	if c.EmailProvider != "ses" && c.EmailProvider != "resend" {
		return c, errors.New("EMAIL_PROVIDER must be ses or resend")
	}
	if c.InboundDomain == "" && c.EmailProvider == "ses" {
		c.InboundDomain = "in.visicomms.com"
	}
	for _, o := range strings.Split(getenv("WEBAUTHN_ORIGINS", c.AppURL), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.Origins = append(c.Origins, o)
		}
	}
	if c.DatabaseURL == "" {
		if c.Env != "development" {
			return c, errors.New("DATABASE_URL is required")
		}
		c.DatabaseURL = "postgres://localhost:5432/visicomms_development?sslmode=disable"
	}
	return c, nil
}

func (c Config) Production() bool { return c.Env == "production" }

func getenv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// trimmed reads a pasted credential. A trailing space in a stored key makes
// B2 reject every request signature, so whitespace around it is dropped.
func trimmed(key string) string { return strings.TrimSpace(os.Getenv(key)) }
