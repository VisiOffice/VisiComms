package recordings

import (
	"context"
	"time"

	"github.com/riverqueue/river"
)

// SweepArgs is the periodic archive pass.
type SweepArgs struct{}

func (SweepArgs) Kind() string { return "recordings_sweep" }

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	a *Archiver
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	return w.a.Sweep(ctx, time.Now())
}

// Register runs the archive pass every minute. Without SignalWire there are
// no recordings, and nothing is registered.
func Register(workers *river.Workers, a *Archiver) []*river.PeriodicJob {
	if a == nil || a.SignalWire == nil {
		return nil
	}
	river.AddWorker(workers, &sweepWorker{a: a})
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
			return SweepArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
		}, &river.PeriodicJobOpts{RunOnStart: true}),
	}
}
