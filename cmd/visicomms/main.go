// Command visicomms is the single binary: `serve` runs the HTTP API and the
// job workers, `migrate` brings the database current, `invite` lets a person
// into a workspace, and `seed demo` creates a demo workspace.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	// The runtime image has no zoneinfo; workspace time zones need it.
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/chat"
	"github.com/VisiOffice/VisiComms/internal/config"
	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/demo"
	"github.com/VisiOffice/VisiComms/internal/jobs"
	"github.com/VisiOffice/VisiComms/internal/notify"
	"github.com/VisiOffice/VisiComms/internal/oauth"
	"github.com/VisiOffice/VisiComms/internal/recordings"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/secrets"
	"github.com/VisiOffice/VisiComms/internal/sending"
	"github.com/VisiOffice/VisiComms/internal/slack"
	"github.com/VisiOffice/VisiComms/internal/voice"
	"github.com/VisiOffice/VisiComms/internal/web"
)

// Version is set at build time with -ldflags "-X main.Version=<sha>".
var Version = "dev"

const usage = `usage:
  visicomms serve [--migrate]
  visicomms migrate
  visicomms workspace create <name> [timezone]
  visicomms invite <workspace-id> <email> <name>
  visicomms agent create <workspace-id> <name>
  visicomms agent list <workspace-id>
  visicomms agent revoke <workspace-id> <agent-id>
  visicomms seed demo [workspace name]
  visicomms fax connect <workspace-id> <phone-number>
  visicomms sms connect <workspace-id> <phone-number>
  visicomms signalwire connect <workspace-id> <space> <project-id>
      (reads SIGNALWIRE_CONNECT_API_TOKEN and SIGNALWIRE_CONNECT_SIGNING_KEY)
  visicomms signalwire provision <workspace-id>
  visicomms signalwire adopt <workspace-id> <subproject-id> [--rotate]
      (hosted: both read SIGNALWIRE_PARENT_*; adopt without --rotate reads
       SIGNALWIRE_CONNECT_SIGNING_KEY)
  visicomms signalwire brands
  visicomms signalwire registry <workspace-id> <brand-id> [campaign-id...]
  visicomms signalwire repoint <workspace-id> [+number...] [--apply]
      points the workspace's numbers' calls, call status and texts at this
      server. Without --apply it only shows what would change. --apply first
      saves the old settings to a file that restore puts back.
  visicomms signalwire restore <file>
      (hosted: both read SIGNALWIRE_PARENT_*)
  visicomms approved-info draft <workspace-id> <file.json>
      adds the file as a new version of the workspace's approved info, for a
      person to approve under Rules. {"articles": [{"title", "content",
      "channels"}]} is what the AI reads on calls.
  visicomms version`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] == "version" {
		fmt.Println(Version)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := newLogger(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "serve":
		if len(os.Args) > 2 && os.Args[2] == "--migrate" {
			err = migrate(ctx, cfg, logger)
		}
		if err == nil {
			err = serve(ctx, cfg, logger)
		}
	case "approved-info":
		if len(os.Args) != 5 || os.Args[2] != "draft" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		err = draftApprovedInfo(ctx, cfg, os.Args[3], os.Args[4])
	case "fax":
		if len(os.Args) != 5 || os.Args[2] != "connect" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		err = connectFax(ctx, cfg, os.Args[3], os.Args[4])
	case "sms":
		if len(os.Args) != 5 || os.Args[2] != "connect" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		err = connectSMS(ctx, cfg, os.Args[3], os.Args[4])
	case "signalwire":
		switch {
		case len(os.Args) == 6 && os.Args[2] == "connect":
			err = connectSignalWire(ctx, cfg, os.Args[3], os.Args[4], os.Args[5])
		case len(os.Args) == 4 && os.Args[2] == "provision":
			err = provisionSignalWire(ctx, cfg, os.Args[3])
		case (len(os.Args) == 5 || len(os.Args) == 6 && os.Args[5] == "--rotate") && os.Args[2] == "adopt":
			err = adoptSignalWire(ctx, cfg, os.Args[3], os.Args[4], len(os.Args) == 6)
		case len(os.Args) == 3 && os.Args[2] == "brands":
			err = listBrands(ctx, cfg)
		case len(os.Args) >= 4 && os.Args[2] == "repoint":
			err = repointSignalWire(ctx, cfg, os.Args[3], os.Args[4:])
		case len(os.Args) == 4 && os.Args[2] == "restore":
			err = restoreSignalWire(ctx, cfg, os.Args[3])
		case len(os.Args) >= 5 && os.Args[2] == "registry":
			err = attachRegistry(ctx, cfg, os.Args[3], os.Args[4], os.Args[5:])
		default:
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	case "migrate":
		err = migrate(ctx, cfg, logger)
	case "workspace":
		if len(os.Args) < 4 || len(os.Args) > 5 || os.Args[2] != "create" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		tz := "America/Los_Angeles"
		if len(os.Args) == 5 {
			tz = os.Args[4]
		}
		err = createWorkspace(ctx, cfg, os.Args[3], tz)
	case "invite":
		if len(os.Args) != 5 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		err = invite(ctx, cfg, os.Args[2], os.Args[3], os.Args[4])
	case "agent":
		err = agentCommand(ctx, cfg, os.Args[2:])
	case "seed":
		if len(os.Args) < 3 || os.Args[2] != "demo" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		name := "Acme Plumbing (demo)"
		if len(os.Args) > 3 {
			name = strings.Join(os.Args[3:], " ")
		}
		err = seedDemo(ctx, cfg, name)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	if cfg.Production() {
		return slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

func migrate(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	ran, err := db.Migrate(ctx, pool)
	for _, v := range ran {
		logger.Info("migrated", "version", v)
	}
	if err == nil && len(ran) == 0 {
		logger.Info("schema already current")
	}
	return err
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.RequireMigrated(ctx, pool); err != nil {
		return err
	}
	if cfg.InboundEmailSecret == "" {
		logger.Warn("INBOUND_EMAIL_SECRET is not set: inbound email will be rejected")
	}

	var provider sending.Provider
	var resend *sending.Resend
	if cfg.ResendKey != "" {
		resend, err = sending.NewResend(cfg.ResendKey)
		if err != nil {
			return err
		}
	}
	switch cfg.EmailProvider {
	case "resend":
		if resend == nil {
			return errors.New("RESEND_API_KEY is required when EMAIL_PROVIDER=resend")
		}
		if cfg.InboundDomain == "" {
			return errors.New("INBOUND_EMAIL_DOMAIN is required when EMAIL_PROVIDER=resend")
		}
		if cfg.ResendWebhookSecret == "" {
			return errors.New("RESEND_WEBHOOK_SECRET is required when EMAIL_PROVIDER=resend")
		}
		provider = resend
	case "ses":
		if cfg.SESKeyID != "" {
			provider, err = sending.NewSES(ctx, cfg.SESRegion, cfg.SESKeyID, cfg.SESSecret, cfg.SESConfigurationSet)
			if err != nil {
				return err
			}
		} else {
			logger.Warn("AWS_SES_ACCESS_KEY_ID is not set: sends through SES stay queued")
		}
	}
	if err := review.ValidateInboundDomain(cfg.InboundDomain); err != nil {
		return err
	}
	signalwire, err := signalWireProjects(cfg, pool)
	if err != nil {
		return err
	}
	if signalwire != nil && signalwire.Installation == nil && signalwire.Keys == nil {
		logger.Warn("SIGNALWIRE_PUBLIC_URL is set without SIGNALWIRE_* credentials or VISICOMMS_MASTER_KEY: no workspace can use SMS or fax")
	}
	notifier := &notify.Notifier{DB: pool, Logger: logger, From: cfg.NotifyFrom, SignalWire: signalwire, AppURL: cfg.AppURL}
	if provider != nil {
		notifier.Email = provider
	}
	var store recordings.Store
	switch {
	case cfg.RecordingsBucket != "" && cfg.RecordingsKeyID != "" && cfg.RecordingsSecret != "":
		if store, err = recordings.NewS3(cfg.RecordingsEndpoint, cfg.RecordingsRegion, cfg.RecordingsBucket, cfg.RecordingsKeyID, cfg.RecordingsSecret); err != nil {
			return err
		}
	case cfg.RecordingsBucket != "" || cfg.RecordingsKeyID != "" || cfg.RecordingsSecret != "":
		return errors.New("recordings storage needs RECORDINGS_BUCKET, RECORDINGS_S3_KEY_ID and RECORDINGS_S3_SECRET together")
	default:
		logger.Warn("RECORDINGS_BUCKET is not set: call recordings stay at SignalWire")
	}
	var archiver *recordings.Archiver
	if signalwire != nil {
		archiver = &recordings.Archiver{DB: pool, SignalWire: signalwire, Store: store, Logger: logger}
	}
	// Letters go through each workspace's own Lob account, sealed with the
	// master key; without it nobody can connect one.
	var mail *sending.LobAccounts
	if cfg.MasterKey != "" && cfg.PublicURL != "" {
		key, err := secrets.ParseMasterKey(cfg.MasterKey)
		if err != nil {
			return err
		}
		mail = &sending.LobAccounts{DB: pool, PublicURL: strings.TrimRight(cfg.PublicURL, "/")}
		if mail.Keys, err = secrets.New(key); err != nil {
			return err
		}
	}
	sends := false
	client, works, err := jobs.New(pool, logger, func(w *river.Workers) []*river.PeriodicJob {
		periodic := sending.Register(w, pool, provider, signalwire, mail)
		sends = len(periodic) > 0
		periodic = append(periodic, recordings.Register(w, archiver)...)
		return append(periodic, notify.Register(w, notifier)...)
	})
	if err != nil {
		return err
	}
	var enqueue func(context.Context, pgx.Tx, string) error
	if sends {
		enqueue = sending.Enqueue(client)
	}
	if works {
		if err := client.Start(ctx); err != nil {
			return fmt.Errorf("starting jobs: %w", err)
		}
	}

	var ai *voice.OpenAI
	switch {
	case cfg.OpenAIKey != "" && cfg.OpenAIProject != "" && cfg.OpenAIWebhookSecret != "":
		if cfg.OpenAIVoiceAPI != voice.APILive && cfg.OpenAIVoiceAPI != voice.APIRealtime {
			return fmt.Errorf("OPENAI_VOICE_API is %q: use live or realtime", cfg.OpenAIVoiceAPI)
		}
		ai = &voice.OpenAI{APIKey: cfg.OpenAIKey, ProjectID: cfg.OpenAIProject, WebhookSecret: cfg.OpenAIWebhookSecret, API: cfg.OpenAIVoiceAPI}
		voice.EnableAI(ai)
		logger.Info("AI answering is on", "api", cfg.OpenAIVoiceAPI)
	case cfg.OpenAIKey != "" || cfg.OpenAIProject != "" || cfg.OpenAIWebhookSecret != "":
		logger.Warn("AI answering needs OPENAI_API_KEY, OPENAI_PROJECT_ID and OPENAI_WEBHOOK_SECRET together: it is off")
	}

	authn, err := auth.New(pool, auth.Config{RPID: cfg.RPID, Origins: cfg.Origins})
	if err != nil {
		return err
	}
	chatHub := chat.NewHub(pool, logger)
	go chatHub.Run(ctx)

	// Slack needs the master key to seal each workspace's bot token.
	slackStore := &slack.Store{DB: pool}
	if cfg.MasterKey != "" {
		key, err := secrets.ParseMasterKey(cfg.MasterKey)
		if err != nil {
			return err
		}
		if slackStore.Keys, err = secrets.New(key); err != nil {
			return err
		}
		mirror := &slack.Mirror{Store: slackStore, Logger: logger, AppURL: cfg.AppURL}
		go mirror.Run(ctx, chatHub.Changes())
	}

	// OAuth for MCP clients needs the API's public origin as its issuer.
	var oauthServer *oauth.Server
	if cfg.PublicURL != "" {
		oauthServer = &oauth.Server{DB: pool, Issuer: strings.TrimRight(cfg.PublicURL, "/"), AppURL: cfg.AppURL}
	}

	// The parent project reads 10DLC registrations for sms_status. A
	// self-hosted installation has none.
	var registry *sending.SignalWireParent
	if cfg.SignalWireParentSpace != "" || cfg.SignalWireParentProject != "" || cfg.SignalWireParentToken != "" {
		if registry, err = signalWireParent(cfg); err != nil {
			logger.Warn("SMS registry unreadable", "err", err)
		}
	}

	handler := (&web.Server{
		DB: pool, Logger: logger, InboundEmailSecret: cfg.InboundEmailSecret,
		Auth: authn, AppURL: cfg.AppURL, SecureCookies: strings.HasPrefix(cfg.AppURL, "https://"),
		Sending: provider, Enqueue: enqueue, SignalWire: signalwire, Recordings: store,
		Resend: resend, ResendWebhookSecret: cfg.ResendWebhookSecret, InboundDomain: cfg.InboundDomain,
		SNS: &sending.SNSVerifier{}, SESEventsTopic: cfg.SESEventsTopic, Notifier: notifier,
		OpenAI: ai, Chat: chatHub, PublicURL: cfg.PublicURL,
		Slack: slackStore, SlackEvents: &slack.Events{Store: slackStore, Logger: logger},
		OAuth: oauthServer, SMSRegistry: registry, Mail: mail,
	}).Routes()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "version", Version, "env", cfg.Env)
		errs <- srv.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}
	if works {
		if err := client.Stop(shutdownCtx); err != nil {
			logger.Error("jobs shutdown", "err", err)
		}
	}
	return nil
}

func draftApprovedInfo(ctx context.Context, cfg config.Config, workspaceID, path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	version, err := review.DraftApprovedInfo(ctx, pool, workspaceID, content)
	if err != nil {
		return err
	}
	fmt.Printf("Approved info v%d drafted for workspace %s. Approve it in the console under Rules.\n", version, workspaceID)
	return nil
}

func invite(ctx context.Context, cfg config.Config, workspaceID, email, name string) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	authn, err := auth.New(pool, auth.Config{RPID: cfg.RPID, Origins: cfg.Origins})
	if err != nil {
		return err
	}
	token, err := authn.CreateInvite(ctx, workspaceID, email, name, nil)
	if err != nil {
		return err
	}
	fmt.Printf("Invite for %s <%s>, valid %s:\n%s/invite/%s\n", name, email, auth.InviteTTL, cfg.AppURL, token)
	return nil
}

// agentCommand manages agent tokens. A new token is printed once; only its
// hash is stored.
func agentCommand(ctx context.Context, cfg config.Config, args []string) error {
	valid := len(args) == 2 && args[0] == "list" ||
		len(args) >= 3 && args[0] == "create" ||
		len(args) == 3 && args[0] == "revoke"
	if !valid {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	switch args[0] {
	case "create":
		a, token, err := agent.Create(ctx, pool, args[1], strings.Join(args[2:], " "), nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Agent %q (%s) created in approve mode. Its token is shown once:\n", a.Name, a.ID)
		fmt.Println(token)
	case "list":
		list, err := agent.List(ctx, pool, args[1])
		if err != nil {
			return err
		}
		for _, a := range list {
			state := "active"
			if a.RevokedAt != nil {
				state = "revoked " + a.RevokedAt.Format(time.DateOnly)
			}
			fmt.Printf("%s  %-30s  %-9s  %s\n", a.ID, a.Name, a.Autonomy, state)
		}
	case "revoke":
		if err := agent.Revoke(ctx, pool, args[1], args[2]); err != nil {
			return err
		}
		fmt.Println("revoked")
	}
	return nil
}

func seedDemo(ctx context.Context, cfg config.Config, name string) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	seeded, err := demo.Seed(ctx, pool, name)
	if err != nil {
		return err
	}
	fmt.Printf("Seeded %q (workspace %s), inbound email at %s\n", name, seeded.WorkspaceID, seeded.Inbox)
	fmt.Printf("Invite yourself: visicomms invite %s you@example.com \"Your Name\"\n", seeded.WorkspaceID)
	return nil
}

func createWorkspace(ctx context.Context, cfg config.Config, name, timezone string) error {
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("unknown time zone %q: use an IANA name such as America/Los_Angeles", timezone)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ($1, $2) RETURNING id`,
		strings.TrimSpace(name), timezone).Scan(&id); err != nil {
		return err
	}
	fmt.Printf("Created %q (%s), workspace %s\n", name, timezone, id)
	fmt.Printf("Invite its first person: visicomms invite %s you@example.com \"Your Name\"\n", id)
	return nil
}

// signalWireProjects is nil when SignalWire is off: no SIGNALWIRE_* setting
// at all. SIGNALWIRE_PUBLIC_URL alone, with VISICOMMS_MASTER_KEY, is the
// hosted setup, where every workspace connects its own project.
func signalWireProjects(cfg config.Config, pool *pgxpool.Pool) (*sending.SignalWireProjects, error) {
	installation := cfg.SignalWireSpace != "" || cfg.SignalWireProject != "" || cfg.SignalWireToken != "" || cfg.SignalWireSigningKey != ""
	if !installation && cfg.SignalWirePublicURL == "" {
		return nil, nil
	}
	publicURL, err := sending.CheckPublicURL(cfg.SignalWirePublicURL)
	if err != nil {
		return nil, err
	}
	p := &sending.SignalWireProjects{DB: pool, PublicURL: publicURL}
	if installation {
		if p.Installation, err = sending.NewSignalWire(cfg.SignalWireSpace, cfg.SignalWireProject, cfg.SignalWireToken, cfg.SignalWireSigningKey, publicURL); err != nil {
			return nil, err
		}
	}
	if cfg.MasterKey != "" {
		key, err := secrets.ParseMasterKey(cfg.MasterKey)
		if err != nil {
			return nil, err
		}
		if p.Keys, err = secrets.New(key); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// openSignalWire connects to the database for a one-off SignalWire command.
func openSignalWire(ctx context.Context, cfg config.Config) (*sending.SignalWireProjects, func(), error) {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	if err := db.RequireMigrated(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	p, err := signalWireProjects(cfg, pool)
	if err == nil && p == nil {
		err = errors.New("SignalWire is not configured: set SIGNALWIRE_PUBLIC_URL")
	}
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return p, pool.Close, nil
}

func connectSignalWire(ctx context.Context, cfg config.Config, workspaceID, space, project string) error {
	p, done, err := openSignalWire(ctx, cfg)
	if err != nil {
		return err
	}
	defer done()
	sw, err := sending.NewSignalWire(space, project, os.Getenv("SIGNALWIRE_CONNECT_API_TOKEN"), os.Getenv("SIGNALWIRE_CONNECT_SIGNING_KEY"), p.PublicURL)
	if err != nil {
		return err
	}
	if err := p.ConnectProject(ctx, workspaceID, sw); err != nil {
		return err
	}
	fmt.Printf("SignalWire project %s connected to workspace %s.\nConnect its numbers with: visicomms sms connect / fax connect\n", project, workspaceID)
	return nil
}

// provisionSignalWire gives a workspace a new subproject of the parent. Its
// token and signing key go from SignalWire straight into the sealed connector.
func provisionSignalWire(ctx context.Context, cfg config.Config, workspaceID string) error {
	parent, err := signalWireParent(cfg)
	if err != nil {
		return err
	}
	p, done, err := openSignalWire(ctx, cfg)
	if err != nil {
		return err
	}
	defer done()
	sub, err := p.Provision(ctx, parent, workspaceID)
	if err != nil {
		return err
	}
	fmt.Printf("Created SignalWire subproject %q (%s) and connected it to workspace %s.\n", sub.Name, sub.ID, workspaceID)
	return nil
}

// adoptSignalWire connects an existing subproject of the parent with a newly
// minted token. Its signing key is the one in SIGNALWIRE_CONNECT_SIGNING_KEY,
// or with --rotate a new one, which stops the old key working.
func adoptSignalWire(ctx context.Context, cfg config.Config, workspaceID, subproject string, rotate bool) error {
	signingKey := os.Getenv("SIGNALWIRE_CONNECT_SIGNING_KEY")
	if rotate == (signingKey != "") {
		return errors.New("give the subproject's current signing key in SIGNALWIRE_CONNECT_SIGNING_KEY, or --rotate to issue a new one (the old key stops working)")
	}
	parent, err := signalWireParent(cfg)
	if err != nil {
		return err
	}
	p, done, err := openSignalWire(ctx, cfg)
	if err != nil {
		return err
	}
	defer done()
	if err := p.Adopt(ctx, parent, workspaceID, subproject, signingKey); err != nil {
		return err
	}
	fmt.Printf("SignalWire subproject %s connected to workspace %s with a new token.\n", subproject, workspaceID)
	if rotate {
		fmt.Println("Its signing key was rotated: anything else verifying its callbacks with the old key now fails.")
	}
	return nil
}

func signalWireParent(cfg config.Config) (*sending.SignalWireParent, error) {
	if cfg.SignalWireParentSpace == "" && cfg.SignalWireParentProject == "" && cfg.SignalWireParentToken == "" {
		return nil, errors.New("this goes through the hosted parent project: set SIGNALWIRE_PARENT_SPACE_URL, SIGNALWIRE_PARENT_PROJECT_ID and SIGNALWIRE_PARENT_API_TOKEN")
	}
	return sending.NewSignalWireParent(cfg.SignalWireParentSpace, cfg.SignalWireParentProject, cfg.SignalWireParentToken)
}

// listBrands prints every brand and campaign registered through the parent,
// read live. It needs no database.
// routingBackup is what repoint --apply saves before changing anything.
type routingBackup struct {
	Workspace string            `json:"workspace"`
	SavedAt   time.Time         `json:"saved_at"`
	Numbers   []sending.Routing `json:"numbers"`
}

// repointSignalWire moves a workspace's numbers to this server: calls to the
// voice webhook (unless SignalWire answers the number's calls as faxes), call
// status to the status callback, and texts to the SMS webhook.
func repointSignalWire(ctx context.Context, cfg config.Config, workspaceID string, args []string) error {
	apply := slices.Contains(args, "--apply")
	only := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--apply" })
	p, done, err := openSignalWire(ctx, cfg)
	if err != nil {
		return err
	}
	defer done()
	sw, err := p.ForWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	rows, err := p.DB.Query(ctx, `SELECT number FROM phone_numbers WHERE workspace_id = $1 ORDER BY number`, workspaceID)
	if err != nil {
		return err
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(only) > 0 {
		for _, n := range only {
			if !slices.Contains(numbers, n) {
				return fmt.Errorf("%s is not one of this workspace's numbers", n)
			}
		}
		numbers = slices.DeleteFunc(numbers, func(n string) bool { return !slices.Contains(only, n) })
	}
	if len(numbers) == 0 {
		return errors.New("this workspace has no numbers")
	}
	var before, after []sending.Routing
	for _, n := range numbers {
		cur, err := sw.Routing(ctx, n)
		if err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		next := sending.Routing{Number: n, SID: cur.SID, SMSURL: sw.InboundURL()}
		if !cur.TakesFaxes() {
			next.VoiceURL, next.StatusCallback = sw.PublicURL+voice.PathIncoming, sw.PublicURL+voice.PathStatus
		}
		before, after = append(before, cur), append(after, next)
		fmt.Printf("%s\n", n)
		show := func(what, from, to string) {
			if to == "" {
				fmt.Printf("  %-7s %s (unchanged: the number takes faxes)\n", what, cmp.Or(from, "none"))
			} else if from == to {
				fmt.Printf("  %-7s %s (already)\n", what, to)
			} else {
				fmt.Printf("  %-7s %s\n          -> %s\n", what, cmp.Or(from, "none"), to)
			}
		}
		show("calls", cur.VoiceURL, next.VoiceURL)
		show("status", cur.StatusCallback, next.StatusCallback)
		show("texts", cur.SMSURL, next.SMSURL)
	}
	if !apply {
		fmt.Println("\nNothing changed. Run again with --apply to make these changes.")
		return nil
	}
	backup := fmt.Sprintf("signalwire-routing-%s-%d.json", workspaceID, time.Now().Unix())
	raw, err := json.MarshalIndent(routingBackup{Workspace: workspaceID, SavedAt: time.Now().UTC(), Numbers: before}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(backup, raw, 0o600); err != nil {
		return fmt.Errorf("saving the old settings first: %w", err)
	}
	fmt.Printf("\nOld settings saved to %s (undo: visicomms signalwire restore %s)\n", backup, backup)
	for _, r := range after {
		if err := sw.SetRouting(ctx, r); err != nil {
			return fmt.Errorf("%s: %w (numbers before it were moved; restore puts all back)", r.Number, err)
		}
		fmt.Printf("%s now points here.\n", r.Number)
	}
	return nil
}

// restoreSignalWire puts back the settings repoint --apply saved.
func restoreSignalWire(ctx context.Context, cfg config.Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var b routingBackup
	if err := json.Unmarshal(raw, &b); err != nil || b.Workspace == "" {
		return fmt.Errorf("%s is not a repoint backup", path)
	}
	p, done, err := openSignalWire(ctx, cfg)
	if err != nil {
		return err
	}
	defer done()
	sw, err := p.ForWorkspace(ctx, b.Workspace)
	if err != nil {
		return err
	}
	for _, r := range b.Numbers {
		if err := sw.SetRouting(ctx, r); err != nil {
			return fmt.Errorf("%s: %w", r.Number, err)
		}
		fmt.Printf("%s restored: calls %s, texts %s\n", r.Number, cmp.Or(r.VoiceURL, "none"), cmp.Or(r.SMSURL, "none"))
	}
	return nil
}

func listBrands(ctx context.Context, cfg config.Config) error {
	parent, err := signalWireParent(cfg)
	if err != nil {
		return err
	}
	brands, err := parent.Brands(ctx)
	if err != nil {
		return err
	}
	for _, b := range brands {
		fmt.Printf("brand %s  %q  %s\n", b.ID, b.Name, b.State)
		campaigns, err := parent.Campaigns(ctx, b.ID)
		if err != nil {
			return err
		}
		for _, c := range campaigns {
			fmt.Printf("  campaign %s  %q  %s  %s\n", c.ID, c.Name, c.State, c.SMSUseCase)
		}
	}
	return nil
}

// attachRegistry records the 10DLC brand and campaigns a workspace's own
// project sends under, as registered through the parent project.
func attachRegistry(ctx context.Context, cfg config.Config, workspaceID, brandID string, campaignIDs []string) error {
	parent, err := signalWireParent(cfg)
	if err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.RequireMigrated(ctx, pool); err != nil {
		return err
	}
	brand, campaigns, err := sending.AttachRegistry(ctx, pool, parent, workspaceID, brandID, campaignIDs)
	if err != nil {
		return err
	}
	fmt.Printf("Workspace %s sends under brand %q (%s, %s):\n", workspaceID, brand.Name, brand.ID, brand.State)
	for _, c := range campaigns {
		fmt.Printf("  campaign %q (%s): %s, use case %s\n", c.Name, c.ID, c.State, c.SMSUseCase)
	}
	if len(campaigns) == 0 {
		fmt.Println("  no campaigns: SMS will not be delivered until one is approved and its numbers attached")
	}
	return nil
}

func connectSMS(ctx context.Context, cfg config.Config, workspaceID, number string) error {
	return connectNumber(ctx, cfg, workspaceID, number, "sms")
}

func connectFax(ctx context.Context, cfg config.Config, workspaceID, number string) error {
	return connectNumber(ctx, cfg, workspaceID, number, "fax")
}

// connectNumber checks the number against the workspace's own project, or the
// installation's when it has none.
func connectNumber(ctx context.Context, cfg config.Config, workspaceID, number, capability string) error {
	p, done, err := openSignalWire(ctx, cfg)
	if err != nil {
		return err
	}
	defer done()
	sw, err := p.ForWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	if err := sending.ConnectPhoneNumber(ctx, p.DB, sw, workspaceID, number, capability); err != nil {
		return err
	}
	if capability == "fax" {
		fmt.Printf("Fax number connected to workspace %s through project %s. Configure its Compatibility fax webhook:\n%s\n", workspaceID, sw.Project, sw.FaxInboundURL())
		return nil
	}
	fmt.Printf("SMS number connected to workspace %s through project %s. Configure its Compatibility SMS webhook:\n%s\n", workspaceID, sw.Project, sw.InboundURL())
	return nil
}
