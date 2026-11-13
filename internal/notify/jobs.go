package notify

import (
	"context"
	"time"

	"github.com/riverqueue/river"
)

type InstantArgs struct{}

func (InstantArgs) Kind() string { return "notify_instant" }

type DigestArgs struct{}

func (DigestArgs) Kind() string { return "notify_digest" }

type instantWorker struct {
	river.WorkerDefaults[InstantArgs]
	n *Notifier
}

func (w *instantWorker) Work(ctx context.Context, _ *river.Job[InstantArgs]) error {
	return w.n.RunInstant(ctx)
}

type digestWorker struct {
	river.WorkerDefaults[DigestArgs]
	n *Notifier
}

func (w *digestWorker) Work(ctx context.Context, _ *river.Job[DigestArgs]) error {
	return w.n.RunDigests(ctx, time.Now())
}

// Register runs instant notifications every 30 seconds and checks for daily
// summaries every 15 minutes.
func Register(workers *river.Workers, n *Notifier) []*river.PeriodicJob {
	river.AddWorker(workers, &instantWorker{n: n})
	river.AddWorker(workers, &digestWorker{n: n})
	every := func(d time.Duration, args river.JobArgs) *river.PeriodicJob {
		return river.NewPeriodicJob(river.PeriodicInterval(d), func() (river.JobArgs, *river.InsertOpts) {
			return args, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: d}}
		}, &river.PeriodicJobOpts{RunOnStart: true})
	}
	return []*river.PeriodicJob{every(30*time.Second, InstantArgs{}), every(15*time.Minute, DigestArgs{})}
}
