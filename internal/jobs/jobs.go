// Package jobs is the River client. Jobs are inserted in the same transaction
// as the state change that causes them, so a rolled-back change never leaves
// a job behind.
package jobs

import (
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Client is the River client for this binary.
type Client = river.Client[pgx.Tx]

// New returns the client and whether it works jobs. register adds workers and
// returns any periodic jobs; with no workers registered the client is
// insert-only, because River refuses to start without any.
func New(pool *pgxpool.Pool, logger *slog.Logger, register func(*river.Workers) []*river.PeriodicJob) (*Client, bool, error) {
	cfg := &river.Config{Logger: logger}
	workers := river.NewWorkers()
	var periodic []*river.PeriodicJob
	if register != nil {
		periodic = register(workers)
	}
	works := len(periodic) > 0
	if works {
		cfg.Workers = workers
		cfg.PeriodicJobs = periodic
		cfg.Queues = map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}}
	}
	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	return client, works, err
}
