package resource

import (
	"context"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	"github.com/metacubex/mihomo/common/utils"
	"github.com/metacubex/mihomo/component/slowdown"
	P "github.com/metacubex/mihomo/constant/provider"
	"github.com/metacubex/mihomo/log"

	"github.com/metacubex/fswatch"
	"github.com/samber/lo"
	"golang.org/x/sync/semaphore"
)

type Parser[V any] func([]byte) (V, error)
type ContextParser[V any] func(context.Context, func(func() error) error, []byte) (V, error)
type BundleFile func() (fs.File, error)

type fetcherUpdateContext struct {
	ctx    context.Context
	commit func(func() error) error
}

type fetcherSchedule struct {
	next  time.Time
	state string
}

type Fetcher[V any] struct {
	ctx           context.Context
	ctxCancel     context.CancelFunc
	resourceType  string
	name          string
	vehicle       P.Vehicle
	bundleFile    BundleFile
	updatedAt     atomic.TypedValue[time.Time]
	schedule      atomic.TypedValue[fetcherSchedule]
	hash          utils.HashType
	parser        Parser[V]
	parserContext ContextParser[V]
	interval      time.Duration
	onUpdate      func(V)
	watcher       *fswatch.Watcher
	loadBufMutex  sync.Mutex
	operation     *semaphore.Weighted
	updateContext atomic.TypedValue[*fetcherUpdateContext]
	backoff       slowdown.Backoff
}

func (f *Fetcher[V]) Name() string {
	return f.name
}

func (f *Fetcher[V]) Vehicle() P.Vehicle {
	return f.vehicle
}

func (f *Fetcher[V]) VehicleType() P.VehicleType {
	return f.vehicle.Type()
}

func (f *Fetcher[V]) UpdatedAt() time.Time {
	return f.updatedAt.Load()
}

// UpdateInterval is the interval configured for this provider. A manual update
// does not enable or otherwise change its automatic update policy.
func (f *Fetcher[V]) UpdateInterval() time.Duration {
	return f.interval
}

// AutomaticSchedule reads the pull loop's actual deadline, including retry
// backoff. UpdatedAt cannot reconstruct it: manual updates do not reset the
// timer, and a failed automatic read can schedule a much earlier retry.
func (f *Fetcher[V]) AutomaticSchedule() (time.Time, string) {
	if f.interval <= 0 || f.vehicle.Type() == P.File || f.ctx.Err() != nil {
		return time.Time{}, ""
	}
	if binding := f.updateContext.Load(); binding != nil && binding.ctx.Err() != nil {
		return time.Time{}, ""
	}
	schedule := f.schedule.Load()
	return schedule.next, schedule.state
}

func (f *Fetcher[V]) SetContextParser(parser ContextParser[V]) {
	f.loadBufMutex.Lock()
	defer f.loadBufMutex.Unlock()
	f.parserContext = parser
}

// BindUpdateContext applies the configuration lifetime to automatic refreshes
// as well as explicit controller requests. A replaced provider cannot publish
// into a cache path later reused by a new configuration.
func (f *Fetcher[V]) BindUpdateContext(ctx context.Context, commit func(func() error) error) {
	f.updateContext.Store(&fetcherUpdateContext{ctx: ctx, commit: commit})
}

func (f *Fetcher[V]) Initial() (V, error) {
	if stat, fErr := os.Stat(f.vehicle.Path()); fErr == nil {
		// local file exists, use it first
		buf, err := os.ReadFile(f.vehicle.Path())
		modTime := stat.ModTime()
		contents, _, err := f.loadBuf(buf, utils.MakeHash(buf), false)
		f.updatedAt.Store(modTime) // reset updatedAt to file's modTime

		if err == nil {
			err = f.startPullLoop(time.Since(modTime) > f.interval)
			if err != nil {
				return lo.Empty[V](), err
			}
			return contents, nil
		}
	}

	// parse local file error, fallback to bundle file
	if f.bundleFile != nil {
		// bundle file exists, use it first
		if file, fErr := f.bundleFile(); fErr == nil {
			defer file.Close()
			buf, err := io.ReadAll(file)
			var modTime time.Time
			if stat, sErr := file.Stat(); sErr == nil {
				modTime = stat.ModTime()
			}
			contents, _, err := f.loadBuf(buf, utils.MakeHash(buf), true)
			f.updatedAt.Store(modTime) // reset updatedAt to file's modTime

			if err == nil {
				log.Infoln("[Provider] %s extract successful from bundle file", f.Name())
				err = f.startPullLoop(time.Since(modTime) > f.interval)
				if err != nil {
					return lo.Empty[V](), err
				}
				return contents, nil
			}
			log.Warnln("[Provider] %s read bundle file error: %s", f.Name(), err.Error())
		} else {
			log.Warnln("[Provider] %s read bundle file error: %s", f.Name(), fErr.Error())
		}
	}

	// parse local file error, fallback to remote
	contents, _, updateErr := f.Update()

	// start the pull loop even if f.Update() failed
	err := f.startPullLoop(false)
	if err != nil {
		return lo.Empty[V](), err
	}

	if updateErr != nil {
		return lo.Empty[V](), updateErr
	}

	return contents, nil
}

func (f *Fetcher[V]) Update() (V, bool, error) {
	return f.UpdateContext(f.ctx)
}

// UpdateContext shares the same serialized read/parse/apply operation with
// scheduled updates. Cancelling a caller or closing the provider prevents a
// response from being applied after that operation is no longer wanted.
func (f *Fetcher[V]) UpdateContext(ctx context.Context) (V, bool, error) {
	return f.UpdateContextWithCommit(ctx, nil)
}

// UpdateContextWithCommit lets the configuration owner reject a stale provider
// at the final application boundary without holding its lock during network IO.
func (f *Fetcher[V]) UpdateContextWithCommit(ctx context.Context, commit func(func() error) error) (V, bool, error) {
	if err := f.ctx.Err(); err != nil {
		return lo.Empty[V](), false, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer stop()
	defer cancel()
	if binding := f.updateContext.Load(); binding != nil {
		if err := binding.ctx.Err(); err != nil {
			return lo.Empty[V](), false, err
		}
		stop := context.AfterFunc(binding.ctx, cancel)
		defer stop()
		if commit == nil {
			commit = binding.commit
		}
	}
	if err := f.operation.Acquire(ctx, 1); err != nil {
		return lo.Empty[V](), false, err
	}
	defer f.operation.Release(1)
	if err := ctx.Err(); err != nil {
		return lo.Empty[V](), false, err
	}
	f.loadBufMutex.Lock()
	oldHash := f.hash
	f.loadBufMutex.Unlock()
	buf, hash, err := f.vehicle.Read(ctx, oldHash)
	if err != nil {
		f.backoff.AddAttempt() // add a failed attempt to backoff
		return lo.Empty[V](), false, err
	}
	if err := ctx.Err(); err != nil {
		return lo.Empty[V](), false, err
	}
	return f.loadBufWithCommit(ctx, buf, hash, f.vehicle.Type() != P.File, commit)
}

func (f *Fetcher[V]) SideUpdate(buf []byte) (V, bool, error) {
	if err := f.operation.Acquire(f.ctx, 1); err != nil {
		return lo.Empty[V](), false, err
	}
	defer f.operation.Release(1)
	return f.loadBuf(buf, utils.MakeHash(buf), true)
}

func (f *Fetcher[V]) loadBuf(buf []byte, hash utils.HashType, updateFile bool) (V, bool, error) {
	return f.loadBufWithCommit(f.ctx, buf, hash, updateFile, nil)
}

func (f *Fetcher[V]) loadBufWithCommit(ctx context.Context, buf []byte, hash utils.HashType, updateFile bool, commit func(func() error) error) (V, bool, error) {
	f.loadBufMutex.Lock()
	defer f.loadBufMutex.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if binding := f.updateContext.Load(); binding != nil {
		if err := binding.ctx.Err(); err != nil {
			return lo.Empty[V](), false, err
		}
		stop := context.AfterFunc(binding.ctx, cancel)
		defer stop()
		if commit == nil {
			commit = binding.commit
		}
	}

	apply := func(fn func() error) error {
		checkAndApply := func() error {
			if err := f.ctx.Err(); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn()
		}
		if commit != nil {
			return commit(checkAndApply)
		}
		return checkAndApply()
	}
	now := time.Now()
	if f.hash.Equal(hash) {
		err := apply(func() error {
			if updateFile {
				_ = os.Chtimes(f.vehicle.Path(), now, now)
			}
			f.updatedAt.Store(now)
			f.backoff.Reset() // no error, reset backoff
			return nil
		})
		return lo.Empty[V](), true, err
	}

	if buf == nil { // f.hash has been changed between f.vehicle.Read but should not happen (cause by concurrent)
		return lo.Empty[V](), true, nil
	}

	var contents V
	var err error
	if f.parserContext != nil {
		contents, err = f.parserContext(ctx, commit, buf)
	} else {
		contents, err = f.parser(buf)
	}
	if err != nil {
		f.backoff.AddAttempt() // add a failed attempt to backoff
		return lo.Empty[V](), false, err
	}
	err = apply(func() error {
		if updateFile {
			if err := f.vehicle.Write(buf); err != nil {
				return err
			}
		}
		f.updatedAt.Store(now)
		f.hash = hash
		f.backoff.Reset() // applied successfully, reset backoff

		if f.onUpdate != nil {
			f.onUpdate(contents)
		}
		return nil
	})
	if err != nil {
		return lo.Empty[V](), false, err
	}

	return contents, false, nil
}

func (f *Fetcher[V]) Close() error {
	f.ctxCancel()
	if f.watcher != nil {
		_ = f.watcher.Close()
	}
	return nil
}

func (f *Fetcher[V]) pullLoop(forceUpdate bool) {
	defer f.schedule.Store(fetcherSchedule{})
	initialInterval := f.interval - time.Since(f.UpdatedAt())
	if initialInterval > f.interval {
		initialInterval = f.interval
	}

	if forceUpdate {
		log.Warnln("[Provider] %s not updated for a long time, force refresh", f.Name())
		f.schedule.Store(fetcherSchedule{state: "checking"})
		f.updateWithLog()
	}
	if attempt := f.backoff.Attempt(); attempt > 0 { // f.Update() was failed, decrease the interval from backoff to achieve fast retry
		if duration := f.backoff.ForAttempt(attempt); duration < initialInterval {
			initialInterval = duration
		}
	}

	f.recordNextCheck(initialInterval)
	timer := time.NewTimer(initialInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			f.schedule.Store(fetcherSchedule{state: "checking"})
			f.updateWithLog()
			interval := f.interval
			if attempt := f.backoff.Attempt(); attempt > 0 { // f.Update() was failed, decrease the interval from backoff to achieve fast retry
				if duration := f.backoff.ForAttempt(attempt); duration < interval {
					interval = duration
				}
			}
			f.recordNextCheck(interval)
			timer.Reset(interval)
		case <-f.ctx.Done():
			return
		}
	}
}

func (f *Fetcher[V]) recordNextCheck(delay time.Duration) {
	if delay < 0 {
		delay = 0 // time.Timer also fires immediately for an expired deadline.
	}
	state := "scheduled"
	if f.backoff.Attempt() > 0 {
		state = "retrying"
	}
	f.schedule.Store(fetcherSchedule{next: time.Now().Add(delay), state: state})
}

func (f *Fetcher[V]) startPullLoop(forceUpdate bool) (err error) {
	// pull contents automatically
	if f.vehicle.Type() == P.File {
		f.watcher, err = fswatch.NewWatcher(fswatch.Options{
			Path:     []string{f.vehicle.Path()},
			Callback: f.updateCallback,
		})
		if err != nil {
			return err
		}
		err = f.watcher.Start()
		if err != nil {
			return err
		}
	} else if f.interval > 0 {
		f.schedule.Store(fetcherSchedule{state: "waiting"})
		go f.pullLoop(forceUpdate)
	}
	return
}

func (f *Fetcher[V]) updateCallback(path string) {
	f.updateWithLog()
}

func (f *Fetcher[V]) updateWithLog() {
	_, same, err := f.Update()
	if err != nil {
		log.Errorln("[Provider] %s pull error: %s", f.Name(), err.Error())
		return
	}

	if same {
		log.Debugln("[Provider] %s's content doesn't change", f.Name())
		return
	}

	log.Infoln("[Provider] %s's content update", f.Name())
	return
}

func NewFetcher[V any](name string, interval time.Duration, vehicle P.Vehicle, bundleFile BundleFile, parser Parser[V], onUpdate func(V)) *Fetcher[V] {
	ctx, cancel := context.WithCancel(context.Background())
	minBackoff := 10 * time.Second
	if interval < minBackoff {
		minBackoff = interval
	}
	return &Fetcher[V]{
		ctx:        ctx,
		ctxCancel:  cancel,
		name:       name,
		bundleFile: bundleFile,
		vehicle:    vehicle,
		parser:     parser,
		onUpdate:   onUpdate,
		interval:   interval,
		operation:  semaphore.NewWeighted(1),
		backoff: slowdown.Backoff{
			Factor: 2,
			Jitter: false,
			Min:    minBackoff,
			Max:    interval,
		},
	}
}
