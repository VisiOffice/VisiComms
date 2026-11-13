package sending

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/files"
	"github.com/VisiOffice/VisiComms/internal/postal"
)

// sendTimeout bounds one call to the provider. Past it the outcome is
// unknown, not failed: the provider may have accepted the message.
const sendTimeout = 30 * time.Second

// Outcome is what one dispatch did.
type Outcome string

const (
	Sent      Outcome = "accepted"
	Failed    Outcome = "failed"
	Unknown   Outcome = "unknown"
	Cancelled Outcome = "cancelled"
	Skipped   Outcome = "skipped"
)

type action struct {
	id, workspaceID, threadID, requestedBy          string
	contactID                                       string
	payload, recipients                             json.RawMessage
	payloadSHA                                      []byte
	proposalLinked                                  json.RawMessage
	proposalInfoVersion                             *string
	fromName, fromAddress                           string
	channel, phoneNumber, actionName, remoteAddress string
	settings                                        map[string]string
}

type emailPayload struct {
	Subject   string  `json:"subject"`
	Body      string  `json:"body"`
	InReplyTo *string `json:"in_reply_to"`
}

// Dispatch sends one queued action through p, exactly once. Everything that
// could have changed since the action was approved is checked again first.
// The action is marked dispatching before the provider is called, so a crash
// mid-send leaves it dispatching (and later unknown), never queued for a
// second try.
func Dispatch(ctx context.Context, pool *pgxpool.Pool, p Sender, actionID string) (Outcome, error) {
	return dispatchAction(ctx, pool, actionID, transport{kind: ProviderKind(p), channel: "email", send: func(ctx context.Context, a action) (string, map[string]any, error) {
		var body emailPayload
		if err := json.Unmarshal(a.payload, &body); err != nil {
			return "", nil, err
		}
		var recipients []struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(a.recipients, &recipients); err != nil {
			return "", nil, err
		}
		msg := Message{FromName: a.fromName, FromAddress: a.fromAddress, Subject: body.Subject, Body: body.Body, Date: time.Now()}
		for _, r := range recipients {
			msg.To = append(msg.To, r.Email)
		}
		if body.InReplyTo != nil {
			msg.InReplyTo = *body.InReplyTo
		}
		id, err := p.Send(ctx, msg.Build(), map[string]string{"action_id": a.id, "workspace_id": a.workspaceID})
		return id, map[string]any{"subject": body.Subject, "text": body.Body}, err
	}})
}

func DispatchSMS(ctx context.Context, pool *pgxpool.Pool, p SMSSender, actionID string) (Outcome, error) {
	return dispatchAction(ctx, pool, actionID, transport{kind: "signalwire", channel: "sms", project: p.ProjectID(), send: func(ctx context.Context, a action) (string, map[string]any, error) {
		var msg SMSMessage
		if err := json.Unmarshal(a.payload, &msg); err != nil {
			return "", nil, err
		}
		var recipients []struct {
			Phone string `json:"phone"`
		}
		if err := json.Unmarshal(a.recipients, &recipients); err != nil {
			return "", nil, err
		}
		id, err := p.SendSMS(ctx, msg, recipients[0].Phone, a.id)
		return id, map[string]any{"text": msg.Body, "from": msg.From}, err
	}})
}

func DispatchFax(ctx context.Context, pool *pgxpool.Pool, p FaxSender, actionID string) (Outcome, error) {
	return dispatchAction(ctx, pool, actionID, transport{kind: "signalwire", channel: "fax", project: p.ProjectID(), send: func(ctx context.Context, a action) (string, map[string]any, error) {
		var msg FaxMessage
		var recipients []struct {
			Phone string `json:"phone"`
		}
		if err := json.Unmarshal(a.payload, &msg); err != nil {
			return "", nil, err
		}
		if err := json.Unmarshal(a.recipients, &recipients); err != nil {
			return "", nil, err
		}
		token, err := files.Grant(ctx, pool, a.workspaceID, msg.DocumentID, a.id)
		if err != nil {
			return "", nil, err
		}
		id, err := p.SendFax(ctx, msg, recipients[0].Phone, p.FaxMediaURL(token))
		return id, map[string]any{"document_id": msg.DocumentID, "sha256": msg.SHA256, "from": msg.From}, err
	}})
}

// DispatchLetter sends one approved letter through the workspace's Lob
// account.
func DispatchLetter(ctx context.Context, pool *pgxpool.Pool, l LetterSender, actionID string) (Outcome, error) {
	return dispatchAction(ctx, pool, actionID, transport{kind: "lob", channel: "postal", send: func(ctx context.Context, a action) (string, map[string]any, error) {
		var msg postal.LetterPayload
		var to []postal.LetterRecipient
		if err := json.Unmarshal(a.payload, &msg); err != nil {
			return "", nil, &Rejected{Reason: "the letter could not be read: " + err.Error()}
		}
		if err := json.Unmarshal(a.recipients, &to); err != nil || len(to) != 1 {
			return "", nil, &Rejected{Reason: "a letter has exactly one recipient"}
		}
		// Nothing has reached Lob yet, so a failure here is a refusal, not an
		// unknown outcome.
		doc, err := files.Load(ctx, pool, a.workspaceID, msg.DocumentID)
		if err != nil {
			return "", nil, &Rejected{Reason: "the approved PDF could not be loaded: " + err.Error()}
		}
		address, err := postal.Get(ctx, pool, a.workspaceID, a.contactID, to[0].IdentityID)
		if err != nil {
			return "", nil, &Rejected{Reason: "the mailing address could not be loaded: " + err.Error()}
		}
		letter, err := l.CreateLetter(ctx, LetterRequest{To: address.Address, From: returnAddress(a.settings), PDF: doc.Content,
			Options: msg.Options, ActionID: a.id, WorkspaceID: a.workspaceID})
		detail := map[string]any{"document_id": msg.DocumentID, "sha256": msg.SHA256, "filename": doc.Filename, "from": msg.From,
			"address": to[0].Address, "name": to[0].Name, "cost_cents": msg.CostCents, "pages": msg.Pages,
			"extra_service": msg.ExtraService, "color": msg.Color, "double_sided": msg.DoubleSided, "test": l.Test()}
		if letter.ID != "" {
			detail["expected_delivery_date"], detail["send_date"], detail["url"] = letter.ExpectedDeliveryDate, letter.SendDate, letter.URL
			if letter.TrackingNumber != "" {
				detail["tracking_number"] = letter.TrackingNumber
			}
		}
		return letter.ID, detail, err
	}})
}

// checkLetter re-checks a letter just before it is sent: the return address
// it was approved with, the PDF, the recipient's address, and the monthly
// limit. It returns why the letter cannot go, or "".
func checkLetter(ctx context.Context, tx pgx.Tx, a action) (string, error) {
	var msg postal.LetterPayload
	var to []postal.LetterRecipient
	if json.Unmarshal(a.payload, &msg) != nil || json.Unmarshal(a.recipients, &to) != nil || len(to) != 1 {
		return "The letter no longer matches what was approved.", nil
	}
	if msg.From != a.fromAddress {
		return "The return address changed after the letter was approved.", nil
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM documents WHERE id::text = $1 AND workspace_id = $2 AND encode(sha256, 'hex') = $3)
		AND EXISTS (SELECT 1 FROM identities i JOIN postal_addresses p ON p.identity_id = i.id
			WHERE i.id::text = $4 AND i.workspace_id = $2 AND i.contact_id = $5 AND i.channel = 'postal' AND i.address = $6
			AND p.returned_at IS NULL AND p.deliverability <> 'undeliverable')`,
		msg.DocumentID, a.workspaceID, msg.SHA256, to[0].IdentityID, a.contactID, to[0].Address).Scan(&valid); err != nil {
		return "", err
	}
	if !valid {
		return "The approved PDF or mailing address is no longer available, or a letter to the address came back.", nil
	}
	limit, _ := strconv.Atoi(a.settings["cap_cents"])
	spent, err := postal.MonthSpend(ctx, tx, a.workspaceID, a.id)
	if err != nil {
		return "", err
	}
	if spent+msg.CostCents > limit {
		return "This letter would pass the workspace's monthly mail limit.", nil
	}
	return "", nil
}

// returnAddress is the return address stored on a Lob connector.
func returnAddress(settings map[string]string) postal.Address {
	return postal.Address{Name: settings["return_name"], Company: settings["return_company"], Line1: settings["return_line1"],
		Line2: settings["return_line2"], City: settings["return_city"], State: settings["return_state"], Zip: settings["return_zip"]}
}

type transport struct {
	kind, channel, project string
	send                   func(context.Context, action) (string, map[string]any, error)
}

func dispatchAction(ctx context.Context, pool *pgxpool.Pool, actionID string, tr transport) (Outcome, error) {
	var a action
	var cancelReason string
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var state, kind, health string
		var paused *time.Time
		var takenOverBy *string
		var settings map[string]string
		err := tx.QueryRow(ctx, `SELECT a.id, a.workspace_id, a.thread_id, a.requested_by, t.contact_id, a.state,
				coalesce(c.kind, ''), coalesce(c.settings, '{}'), w.sending_paused_at, t.taken_over_by,
				a.payload, a.recipients, a.payload_sha256,
				coalesce(pr.linked_states, '[]'), pr.approved_info_version_id, a.action, t.channel, coalesce(t.phone_number, ''), coalesce(c.health, 'unknown'), coalesce(t.remote_address, '')
			FROM actions a
			JOIN threads t ON t.id = a.thread_id
			JOIN workspaces w ON w.id = a.workspace_id
			LEFT JOIN connectors c ON c.id = a.connector_id
			LEFT JOIN proposals pr ON pr.id = a.proposal_id
			WHERE a.id = $1 FOR UPDATE OF a, t`, actionID).
			Scan(&a.id, &a.workspaceID, &a.threadID, &a.requestedBy, &a.contactID, &state, &kind, &settings, &paused, &takenOverBy,
				&a.payload, &a.recipients, &a.payloadSHA, &a.proposalLinked, &a.proposalInfoVersion, &a.actionName, &a.channel, &a.phoneNumber, &health, &a.remoteAddress)
		if db.NotFound(err) {
			return errSkip
		}
		if err != nil {
			return err
		}
		if state != "queued" || kind != tr.kind || a.channel != tr.channel || a.actionName != tr.channel+".send" {
			return errSkip
		}
		a.fromName, a.fromAddress, a.settings = settings["from_name"], settings["from_address"], settings

		switch {
		case paused != nil:
			cancelReason = "Sending is paused for this workspace."
		case health != "ok":
			cancelReason = "The sending connection is not healthy."
		case a.fromAddress == "":
			cancelReason = "No From address is set for this workspace."
		case takenOverBy != nil && *takenOverBy != a.requestedBy:
			cancelReason = "Someone took over the thread before it was sent."
		case !matchesApproval(a):
			cancelReason = "The message no longer matches what was approved."
		}
		if cancelReason == "" {
			var optedOut bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM opt_outs WHERE contact_id = $1 AND channel = $2)`, a.contactID, tr.channel).Scan(&optedOut); err != nil {
				return err
			}
			if optedOut {
				cancelReason = "The contact opted out of " + tr.channel + "."
			}
		}
		if cancelReason == "" && tr.channel == "sms" {
			var msg SMSMessage
			var recipients []struct {
				Phone string `json:"phone"`
			}
			if json.Unmarshal(a.payload, &msg) != nil || json.Unmarshal(a.recipients, &recipients) != nil || len(recipients) != 1 || strings.TrimSpace(msg.Body) == "" || utf8.RuneCountInString(msg.Body) > 1600 || msg.From != a.fromAddress || msg.From != a.phoneNumber || recipients[0].Phone != a.remoteAddress {
				cancelReason = "The SMS no longer matches this conversation's number or message limits."
			} else {
				var valid bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM phone_numbers n JOIN identities i ON i.workspace_id=n.workspace_id
   WHERE n.number=$1 AND n.workspace_id=$2 AND n.provider_project_id=$3 AND n.capabilities->>'sms'='true' AND i.contact_id=$4 AND i.channel='sms' AND i.address=$5)`, msg.From, a.workspaceID, tr.project, a.contactID, recipients[0].Phone).Scan(&valid); err != nil {
					return err
				}
				if !valid {
					cancelReason = "The SMS sender or recipient is no longer connected to this workspace."
				}
			}
		}
		if cancelReason == "" && tr.channel == "fax" {
			var msg FaxMessage
			var recipients []struct {
				Phone string `json:"phone"`
			}
			if json.Unmarshal(a.payload, &msg) != nil || json.Unmarshal(a.recipients, &recipients) != nil || len(recipients) != 1 || msg.From != a.fromAddress || msg.From != a.phoneNumber || recipients[0].Phone != a.remoteAddress {
				cancelReason = "The fax no longer matches this conversation's numbers."
			} else {
				var valid bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM documents d JOIN phone_numbers n ON n.workspace_id=d.workspace_id
   WHERE d.id=$1 AND d.workspace_id=$2 AND encode(d.sha256,'hex')=$3 AND n.number=$4 AND n.provider_project_id=$5 AND n.capabilities->>'fax'='true')`, msg.DocumentID, a.workspaceID, msg.SHA256, msg.From, tr.project).Scan(&valid); err != nil {
					return err
				}
				if !valid {
					cancelReason = "The approved PDF or fax number is no longer available."
				}
			}
		}
		if cancelReason == "" && tr.channel == "postal" {
			reason, err := checkLetter(ctx, tx, a)
			if err != nil {
				return err
			}
			cancelReason = reason
		}
		if cancelReason == "" && a.proposalInfoVersion != nil {
			var current bool
			if err := tx.QueryRow(ctx, `SELECT $1 = (SELECT id FROM approved_info_versions WHERE workspace_id = $2
				AND approved_at IS NOT NULL ORDER BY version DESC LIMIT 1)`, *a.proposalInfoVersion, a.workspaceID).Scan(&current); err != nil {
				return err
			}
			if !current {
				cancelReason = "Approved info changed after this was approved."
			}
		}
		if cancelReason != "" {
			return finish(ctx, tx, a, "cancelled", tr.channel+".cancelled", map[string]any{"action_id": a.id, "reason": cancelReason})
		}
		_, err = tx.Exec(ctx, `UPDATE actions SET state = 'dispatching', updated_at = now() WHERE id = $1`, a.id)
		return err
	})
	if errors.Is(err, errSkip) {
		return Skipped, nil
	}
	if err != nil {
		return "", err
	}
	if cancelReason != "" {
		return Cancelled, nil
	}

	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	messageID, detail, sendErr := tr.send(sendCtx, a)
	cancel()
	if detail == nil {
		detail = map[string]any{}
	}
	detail["action_id"], detail["to"], detail["provider"] = a.id, a.recipients, tr.kind

	outcome, state, evType := Sent, "accepted", tr.channel+".sent"

	var rejected *Rejected
	switch {
	case sendErr == nil:
		detail["provider_message_id"] = messageID
	case errors.As(sendErr, &rejected):
		outcome, state, evType = Failed, "failed", tr.channel+".failed"
		detail["reason"] = rejected.Reason
	default:
		outcome, state, evType = Unknown, "unknown", tr.channel+".send_unknown"
		detail["note"] = "The provider did not answer clearly. Check delivery before sending again: " + sendErr.Error()
	}

	// Recorded with a fresh context: the send has happened (or may have), so
	// its record must be written even if the caller has given up.
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer recordCancel()
	err = db.Tx(recordCtx, pool, func(tx pgx.Tx) error {
		ctx := recordCtx
		attempt := map[string]any{"error": errString(sendErr)}
		var mid *string
		if messageID != "" {
			mid = &messageID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_attempts (action_id, connector_id, provider_message_id, outcome, provider_response)
			SELECT id, connector_id, $2, $3, $4 FROM actions WHERE id = $1`,
			a.id, mid, attemptOutcome(outcome), attempt); err != nil {
			return err
		}
		if err := finish(ctx, tx, a, state, evType, detail); err != nil {
			return err
		}
		if outcome == Sent {
			_, err := tx.Exec(ctx, `UPDATE threads SET state = 'waiting', version = version + 1, updated_at = now() WHERE id = $1`, a.threadID)
			return err
		}
		return nil
	})
	if err == nil {
		err = ReconcileEvents(recordCtx, pool)
	}
	return outcome, err
}

var errSkip = errors.New("nothing to send")

// matchesApproval recomputes the hash the approval was bound to. It mirrors
// review.Bound: recipients, payload, linked states, approved info version.
func matchesApproval(a action) bool {
	bound := struct {
		Recipients          json.RawMessage `json:"recipients"`
		Payload             json.RawMessage `json:"payload"`
		LinkedStates        json.RawMessage `json:"linked_states"`
		ApprovedInfoVersion *string         `json:"approved_info_version_id"`
	}{compact(a.recipients), compact(a.payload), compact(a.proposalLinked), a.proposalInfoVersion}
	data, _ := json.Marshal(bound)
	sum := sha256.Sum256(data)
	return bytes.Equal(sum[:], a.payloadSHA)
}

// compact normalises JSON read back from Postgres jsonb, which reorders keys
// and spacing, to what encoding/json would have produced from the same value.
func compact(raw json.RawMessage) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, _ := json.Marshal(v)
	return out
}

func finish(ctx context.Context, tx pgx.Tx, a action, state, evType string, payload map[string]any) error {
	if _, err := tx.Exec(ctx, `UPDATE actions SET state = $2, updated_at = now() WHERE id = $1`, a.id, state); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload)
		VALUES ($1, $2, $3, $4, $5, $7, $6)`, a.workspaceID, a.threadID, a.contactID, evType, a.requestedBy, payload, a.channel)
	if err != nil {
		return fmt.Errorf("recording %s: %w", evType, err)
	}
	return nil
}

func attemptOutcome(o Outcome) string {
	switch o {
	case Sent:
		return "accepted"
	case Failed:
		return "rejected"
	default:
		return "unknown"
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
