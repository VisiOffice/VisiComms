package sending

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/phone"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNumberAssigned = errors.New("this phone number is assigned to another workspace")

type PhoneNumber struct {
	Number       string          `json:"number"`
	Capabilities map[string]bool `json:"capabilities"`
}

// ConnectSMSNumber is operator-only (the CLI), not a tenant-accessible claim
// endpoint. Owning an instance's API key does not prove a tenant owns every
// unassigned number in the hosted project. It does not buy or reconfigure one.
func ConnectSMSNumber(ctx context.Context, pool *pgxpool.Pool, p *SignalWire, workspaceID, number string) error {
	return ConnectPhoneNumber(ctx, pool, p, workspaceID, number, "sms")
}

func ConnectPhoneNumber(ctx context.Context, pool *pgxpool.Pool, p *SignalWire, workspaceID, number, capability string) error {
	number, err := phone.Normalize(number)
	if err != nil {
		return err
	}
	remote, err := p.CheckNumber(ctx, number, capability)
	if err != nil {
		return err
	}
	capabilities, err := json.Marshal(remote.Capabilities)
	if err != nil {
		return err
	}
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "sms-number:"+number); err != nil {
			return err
		}
		var owner string
		err := tx.QueryRow(ctx, `SELECT workspace_id FROM phone_numbers WHERE number=$1`, number).Scan(&owner)
		if err == nil {
			if owner != workspaceID {
				return ErrNumberAssigned
			}
			if _, err = tx.Exec(ctx, `UPDATE phone_numbers SET capabilities=$2, provider_project_id=$3 WHERE number=$1`, number, json.RawMessage(capabilities), p.ProjectID()); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE connectors SET settings=jsonb_build_object('from_address',$1::text,'project_id',$2::text),health='ok',checked_at=now(),updated_at=now()
 WHERE id=(SELECT connector_id FROM phone_numbers WHERE number=$1)`, number, p.ProjectID())
			return err
		}
		if !db.NotFound(err) {
			return err
		}
		var connector string
		settings := map[string]string{"from_address": number, "project_id": p.ProjectID()}
		err = tx.QueryRow(ctx, `INSERT INTO connectors (workspace_id,kind,name,settings,health,checked_at)
   VALUES ($1,'signalwire',$2,$3,'ok',now()) RETURNING id`, workspaceID, "SignalWire "+number, settings).Scan(&connector)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO phone_numbers (number,workspace_id,connector_id,provider_project_id,capabilities) VALUES ($1,$2,$3,$4,$5)`, number, workspaceID, connector, p.ProjectID(), json.RawMessage(capabilities))
		return err
	})
}

func SMSNumbers(ctx context.Context, q db.Querier, workspaceID string) ([]PhoneNumber, error) {
	rows, err := q.Query(ctx, `SELECT number,capabilities FROM phone_numbers WHERE workspace_id=$1 ORDER BY number`, workspaceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PhoneNumber, error) {
		var n PhoneNumber
		var raw []byte
		if err := r.Scan(&n.Number, &raw); err != nil {
			return n, err
		}
		return n, json.Unmarshal(raw, &n.Capabilities)
	})
}
