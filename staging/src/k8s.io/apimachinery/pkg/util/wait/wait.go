/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wait

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/runtime"
)
/*
 目前来看该文件提供了各种定时循环检查condiftion的工具，直到检查成功/超时/检查次数过多才会返回
*/
// For any test of the style:
//   ...
//   <- time.After(timeout):
//      t.Errorf("Timed out")
// The value for timeout should effectively be "forever." Obviously we don't want our tests to truly lock up forever, but 30s
// is long enough that it is effectively forever for the things that can slow down a run on a heavily contended machine
// (GC, seeks, etc), but not so long as to make a developer ctrl-c a test run if they do happen to break that test.
var ForeverTestTimeout = time.Second * 30
// 永远不close的chan
// NeverStop may be passed to Until to make it never stop.
var NeverStop <-chan struct{} = make(chan struct{})
// 使用Startxx来开启线程去工作，wait函数的所有的线程都执行完才会返回
// Group allows to start a group of goroutines and wait for their completion.
type Group struct {
	wg sync.WaitGroup
}

func (g *Group) Wait() {
	g.wg.Wait()
}

// StartWithChannel starts f in a new goroutine in the group.
// stopCh is passed to f as an argument. f should stop when stopCh is available.
func (g *Group) StartWithChannel(stopCh <-chan struct{}, f func(stopCh <-chan struct{})) {
	g.Start(func() {
		f(stopCh)
	})
}

// StartWithContext starts f in a new goroutine in the group.
// ctx is passed to f as an argument. f should stop when ctx.Done() is available.
func (g *Group) StartWithContext(ctx context.Context, f func(context.Context)) {
	g.Start(func() {
		f(ctx)
	})
}

// Start starts f in a new goroutine in the group.
func (g *Group) Start(f func()) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		f()
	}()
}

// Forever calls f every period for ever.
//
// Forever is syntactic sugar on top of Until.
// 用固定的时间间隔干活儿，就是xxx分钟干一次
func Forever(f func(), period time.Duration) {
	Until(f, period, NeverStop)
}

// Until loops until stop channel is closed, running f every period.
//
// Until is syntactic sugar on top of JitterUntil with zero jitter factor and
// with sliding = true (which means the timer for period starts after the f
// completes).
// 用固定的时间间隔干活儿，干活儿时间不计入间隔时间
func Until(f func(), period time.Duration, stopCh <-chan struct{}) {
	JitterUntil(f, period, 0.0, true, stopCh)
}

// UntilWithContext loops until context is done, running f every period.
//
// UntilWithContext is syntactic sugar on top of JitterUntilWithContext
// with zero jitter factor and with sliding = true (which means the timer
// for period starts after the f completes).
func UntilWithContext(ctx context.Context, f func(context.Context), period time.Duration) {
	JitterUntilWithContext(ctx, f, period, 0.0, true)
}

// NonSlidingUntil loops until stop channel is closed, running f every
// period.
//
// NonSlidingUntil is syntactic sugar on top of JitterUntil with zero jitter
// factor, with sliding = false (meaning the timer for period starts at the same
// time as the function starts).
// 用固定的时间间隔干活儿，干活儿时间计入间隔时间
func NonSlidingUntil(f func(), period time.Duration, stopCh <-chan struct{}) {
	JitterUntil(f, period, 0.0, false, stopCh)
}

// NonSlidingUntilWithContext loops until context is done, running f every
// period.
//
// NonSlidingUntilWithContext is syntactic sugar on top of JitterUntilWithContext
// with zero jitter factor, with sliding = false (meaning the timer for period
// starts at the same time as the function starts).
func NonSlidingUntilWithContext(ctx context.Context, f func(context.Context), period time.Duration) {
	JitterUntilWithContext(ctx, f, period, 0.0, false)
}

// JitterUntil loops until stop channel is closed, running f every period.
//
// If jitterFactor is positive, the period is jittered before every run of f.
// If jitterFactor is not positive, the period is unchanged and not jittered.
//
// If sliding is true, the period is computed after f runs. If it is false then
// period includes the runtime for f.
//
// Close stopCh to stop. f may not be invoked if stop channel is already
// closed. Pass NeverStop to if you don't want it stop.
// 用JitteredBackoffManager作为backoff算法干活儿
func JitterUntil(f func(), period time.Duration, jitterFactor float64, sliding bool, stopCh <-chan struct{}) {
	BackoffUntil(f, NewJitteredBackoffManager(period, jitterFactor, &clock.RealClock{}), sliding, stopCh)
}

// BackoffUntil loops until stop channel is closed, run f every duration given by BackoffManager.
//
// If sliding is true, the period is computed after f runs. If it is false then
// period includes the runtime for f.
/*
f就是要干的活儿
backoff用来决定多长时间干一次活儿
stopCh用来控制干活儿结束
sliding用来控制f执行的时间要不要计入干活儿的间隔时间
*/
func BackoffUntil(f func(), backoff BackoffManager, sliding bool, stopCh <-chan struct{}) {
	var t clock.Timer
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		if !sliding {
			t = backoff.Backoff()
		}

		func() {
			defer runtime.HandleCrash()
			f()
		}()

		if sliding {
			t = backoff.Backoff()
		}

		// NOTE: b/c there is no priority selection in golang
		// it is possible for this to race, meaning we could
		// trigger t.C and stopCh, and t.C select falls through.
		// In order to mitigate we re-check stopCh at the beginning
		// of every loop to prevent extra executions of f().
		select {
		case <-stopCh:
			if !t.Stop() {
				<-t.C()
			}
			return
		case <-t.C():
		}
	}
}

// JitterUntilWithContext loops until context is done, running f every period.
//
// If jitterFactor is positive, the period is jittered before every run of f.
// If jitterFactor is not positive, the period is unchanged and not jittered.
//
// If sliding is true, the period is computed after f runs. If it is false then
// period includes the runtime for f.
//
// Cancel context to stop. f may not be invoked if context is already expired.
// 和JitterUntil一样，只是函数可以拿到ctx，并且由ctx来控制干活儿是否结束
func JitterUntilWithContext(ctx context.Context, f func(context.Context), period time.Duration, jitterFactor float64, sliding bool) {
	JitterUntil(func() { f(ctx) }, period, jitterFactor, sliding, ctx.Done())
}

// Jitter returns a time.Duration between duration and duration + maxFactor *
// duration.
//
// This allows clients to avoid converging on periodic behavior. If maxFactor
// is 0.0, a suggested default value will be chosen
// 在duration随机增加一个数字
func Jitter(duration time.Duration, maxFactor float64) time.Duration {
	if maxFactor <= 0.0 {
		maxFactor = 1.0
	}
	wait := duration + time.Duration(rand.Float64()*maxFactor*float64(duration))
	return wait
}

// ErrWaitTimeout is returned when the condition exited without success.
var ErrWaitTimeout = errors.New("timed out waiting for the condition")
// 返回true代表条件满足，返回error代表检查要停止
// ConditionFunc returns true if the condition is satisfied, or an error
// if the loop should be aborted.
type ConditionFunc func() (done bool, err error)

// ConditionWithContextFunc returns true if the condition is satisfied, or an error
// if the loop should be aborted.
//
// The caller passes along a context that can be used by the condition function.
type ConditionWithContextFunc func(context.Context) (done bool, err error)

// WithContext converts a ConditionFunc into a ConditionWithContextFunc
func (cf ConditionFunc) WithContext() ConditionWithContextFunc {
	return func(context.Context) (done bool, err error) {
		return cf()
	}
}
// 就是套了个捕获panic的函数
// runConditionWithCrashProtection runs a ConditionFunc with crash protection
func runConditionWithCrashProtection(condition ConditionFunc) (bool, error) {
	return runConditionWithCrashProtectionWithContext(context.TODO(), condition.WithContext())
}
// 就是套了个捕获panic的函数
// runConditionWithCrashProtectionWithContext runs a
// ConditionWithContextFunc with crash protection.
func runConditionWithCrashProtectionWithContext(ctx context.Context, condition ConditionWithContextFunc) (bool, error) {
	defer runtime.HandleCrash()
	return condition(ctx)
}

// Backoff holds parameters applied to a Backoff function.
// 这个结构体就是用来计算下一次尝试xxx操作之前应该sleep多长时间
type Backoff struct {
	// The initial duration.
	Duration time.Duration // 存储sleep时间
	// Duration is multiplied by factor each iteration, if factor is not zero
	// and the limits imposed by Steps and Cap have not been reached.
	// Should not be negative.
	// The jitter does not contribute to the updates to the duration parameter.
	Factor float64   // 用来计算下次sleep的时间，duration+duration*Factor
	// The sleep at each iteration is the duration plus an additional
	// amount chosen uniformly at random from the interval between
	// zero and `jitter*duration`.
	Jitter float64  // 这个值用来生成一个随机数，duration加上这个才是最终的duration
	// The remaining number of iterations in which the duration
	// parameter may change (but progress can be stopped earlier by
	// hitting the cap). If not positive, the duration is not
	// changed. Used for exponential backoff in combination with
	// Factor and Cap.
	Steps int  //可以让迭代的次数 如果迭代的次数超过这个数那么下次再计算duration就不会再乘以Factor
	// A limit on revised values of the duration parameter. If a
	// multiplication by the factor parameter would make the duration
	// exceed the cap then the duration is set to the cap and the
	// steps parameter is set to zero.
	Cap time.Duration  // 算出来的sleep的最大值，如果查过这个值，那么就用cap作为duration并把steps设置为0
}

// Step (1) returns an amount of time to sleep determined by the
// original Duration and Jitter and (2) mutates the provided Backoff
// to update its Steps and Duration.
func (b *Backoff) Step() time.Duration {
	// 如果迭代次数耗光了，那么直接返回duration+random*duration*Jitter 或者duration不变
	if b.Steps < 1 {
		if b.Jitter > 0 {
			return Jitter(b.Duration, b.Jitter)
		}
		return b.Duration
	}
	b.Steps--

	duration := b.Duration

	// calculate the next step
	// 次数没有耗光，那么就是b.Duration 和b.Factor还有Jitter共同计算出来一个数
	if b.Factor != 0 {
		b.Duration = time.Duration(float64(b.Duration) * b.Factor)
		if b.Cap > 0 && b.Duration > b.Cap {
			b.Duration = b.Cap
			b.Steps = 0
		}
	}

	if b.Jitter > 0 {
		duration = Jitter(duration, b.Jitter)
	}
	return duration
}

// contextForChannel derives a child context from a parent channel.
//
// The derived context's Done channel is closed when the returned cancel function
// is called or when the parent channel is closed, whichever happens first.
//
// Note the caller must *always* call the CancelFunc, otherwise resources may be leaked.
func contextForChannel(parentCh <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		select {
		case <-parentCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// BackoffManager manages backoff with a particular scheme based on its underlying implementation. It provides
// an interface to return a timer for backoff, and caller shall backoff until Timer.C() drains. If the second Backoff()
// is called before the timer from the first Backoff() call finishes, the first timer will NOT be drained and result in
// undetermined behavior.
// The BackoffManager is supposed to be called in a single-threaded environment.
type BackoffManager interface {
	Backoff() clock.Timer
}

type exponentialBackoffManagerImpl struct {
	backoff              *Backoff
	backoffTimer         clock.Timer  // 当前backoff使用的timer，tiner.C可读的时候就可以进行操作了
	lastBackoffStart     time.Time    // 上次调用backoff的时间
	initialBackoff       time.Duration
	backoffResetDuration time.Duration  // 如果now - lastBackoffStart >= backoffResetDuration 那么久重置backoff
	clock                clock.Clock
}

// NewExponentialBackoffManager returns a manager for managing exponential backoff. Each backoff is jittered and
// backoff will not exceed the given max. If the backoff is not called within resetDuration, the backoff is reset.
// This backoff manager is used to reduce load during upstream unhealthiness.
func NewExponentialBackoffManager(initBackoff, maxBackoff, resetDuration time.Duration, backoffFactor, jitter float64, c clock.Clock) BackoffManager {
	return &exponentialBackoffManagerImpl{
		backoff: &Backoff{
			Duration: initBackoff,
			Factor:   backoffFactor,
			Jitter:   jitter,

			// the current impl of wait.Backoff returns Backoff.Duration once steps are used up, which is not
			// what we ideally need here, we set it to max int and assume we will never use up the steps
			Steps: math.MaxInt32,
			Cap:   maxBackoff,
		},
		backoffTimer:         nil,
		initialBackoff:       initBackoff,
		lastBackoffStart:     c.Now(),
		backoffResetDuration: resetDuration,
		clock:                c,
	}
}
/*
 exponentialBackoffManagerImpl 获取下次上市操作之前要sleep的时间
*/
func (b *exponentialBackoffManagerImpl) getNextBackoff() time.Duration {
	// 两次backoff超过backoffResetDuration时长，重置backoff结构体
	if b.clock.Now().Sub(b.lastBackoffStart) > b.backoffResetDuration {
		b.backoff.Steps = math.MaxInt32
		b.backoff.Duration = b.initialBackoff
	}
	b.lastBackoffStart = b.clock.Now()
	return b.backoff.Step()  // 计算sleep时间
}

// Backoff 返回一个timer,当timer.C读取成功的时候就是可以进行操作了
// Backoff implements BackoffManager.Backoff, it returns a timer so caller can block on the timer for exponential backoff.
// The returned timer must be drained before calling Backoff() the second time
func (b *exponentialBackoffManagerImpl) Backoff() clock.Timer {
	if b.backoffTimer == nil {
		b.backoffTimer = b.clock.NewTimer(b.getNextBackoff())
	} else {
		b.backoffTimer.Reset(b.getNextBackoff())
	}
	return b.backoffTimer
}

type jitteredBackoffManagerImpl struct {
	clock        clock.Clock
	duration     time.Duration
	jitter       float64
	backoffTimer clock.Timer
}
// 该算法是这样的每次sleep时间是： duration+duration*jitter*ramdonFloat
// NewJitteredBackoffManager returns a BackoffManager that backoffs with given duration plus given jitter. If the jitter
// is negative, backoff will not be jittered.
func NewJitteredBackoffManager(duration time.Duration, jitter float64, c clock.Clock) BackoffManager {
	return &jitteredBackoffManagerImpl{
		clock:        c,
		duration:     duration,
		jitter:       jitter,
		backoffTimer: nil,
	}
}

func (j *jitteredBackoffManagerImpl) getNextBackoff() time.Duration {
	jitteredPeriod := j.duration
	if j.jitter > 0.0 {
		jitteredPeriod = Jitter(j.duration, j.jitter)
	}
	return jitteredPeriod
}

// Backoff implements BackoffManager.Backoff, it returns a timer so caller can block on the timer for jittered backoff.
// The returned timer must be drained before calling Backoff() the second time
func (j *jitteredBackoffManagerImpl) Backoff() clock.Timer {
	backoff := j.getNextBackoff()
	if j.backoffTimer == nil {
		j.backoffTimer = j.clock.NewTimer(backoff)
	} else {
		j.backoffTimer.Reset(backoff)
	}
	return j.backoffTimer
}

// ExponentialBackoff repeats a condition check with exponential backoff.
//
// It repeatedly checks the condition and then sleeps, using `backoff.Step()`
// to determine the length of the sleep and adjust Duration and Steps.
// Stops and returns as soon as:
// 1. the condition check returns true or an error,
// 2. `backoff.Steps` checks of the condition have been done, or
// 3. a sleep truncated by the cap on duration has been completed.
// In case (1) the returned error is what the condition function returned.
// In all other cases, ErrWaitTimeout is returned.
// 根据backoff定时检查天剑是否满足，如果满足/err/检查次数耗尽。那么返回
func ExponentialBackoff(backoff Backoff, condition ConditionFunc) error {
	for backoff.Steps > 0 {
		if ok, err := runConditionWithCrashProtection(condition); err != nil || ok {
			return err
		}
		if backoff.Steps == 1 {
			break
		}
		time.Sleep(backoff.Step())
	}
	return ErrWaitTimeout
}

// Poll tries a condition func until it returns true, an error, or the timeout
// is reached.
//
// Poll always waits the interval before the run of 'condition'.
// 'condition' will always be invoked at least once.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.
//
// If you want to Poll something forever, see PollInfinite.
func Poll(interval, timeout time.Duration, condition ConditionFunc) error {
	return PollWithContext(context.Background(), interval, timeout, condition.WithContext())
}

// PollWithContext tries a condition func until it returns true, an error,
// or when the context expires or the timeout is reached, whichever
// happens first.
//
// PollWithContext always waits the interval before the run of 'condition'.
// 'condition' will always be invoked at least once.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.
//
// If you want to Poll something forever, see PollInfinite.
/*
  每隔interval时间段检查一次条件。直到检查成功/timeout/ctx.done
*/
func PollWithContext(ctx context.Context, interval, timeout time.Duration, condition ConditionWithContextFunc) error {
	return poll(ctx, false, poller(interval, timeout), condition)
}

// PollUntil tries a condition func until it returns true, an error or stopCh is
// closed.
//
// PollUntil always waits interval before the first run of 'condition'.
// 'condition' will always be invoked at least once.
/*
  每隔interval间隔检查一次条件，如果条件合格或者stopCh close了，那么停止检查
*/
func PollUntil(interval time.Duration, condition ConditionFunc, stopCh <-chan struct{}) error {
	ctx, cancel := contextForChannel(stopCh)
	defer cancel()
	return PollUntilWithContext(ctx, interval, condition.WithContext())
}

// PollUntilWithContext tries a condition func until it returns true,
// an error or the specified context is cancelled or expired.
//
// PollUntilWithContext always waits interval before the first run of 'condition'.
// 'condition' will always be invoked at least once.
// 每隔interval时间检查一次，直到检查成功或者ctxdone
func PollUntilWithContext(ctx context.Context, interval time.Duration, condition ConditionWithContextFunc) error {
	return poll(ctx, false, poller(interval, 0), condition)
}

// PollInfinite tries a condition func until it returns true or an error
//
// PollInfinite always waits the interval before the run of 'condition'.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.
// 和PollInfiniteWithContext一样，只是传了一个废物ctx
func PollInfinite(interval time.Duration, condition ConditionFunc) error {
	return PollInfiniteWithContext(context.Background(), interval, condition.WithContext())
}

// PollInfiniteWithContext tries a condition func until it returns true or an error
//
// PollInfiniteWithContext always waits the interval before the run of 'condition'.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.
// 目前看起来和PollUntilWithContext一样，就是隔xxx时间检查一次直到成功或者ctx done
func PollInfiniteWithContext(ctx context.Context, interval time.Duration, condition ConditionWithContextFunc) error {
	return poll(ctx, false, poller(interval, 0), condition)
}

// PollImmediate tries a condition func until it returns true, an error, or the timeout
// is reached.
//
// PollImmediate always checks 'condition' before waiting for the interval. 'condition'
// will always be invoked at least once.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.
//
// If you want to immediately Poll something forever, see PollImmediateInfinite.
// 和PollImmediateWithContext一样。就是进去马上先执行一次，如果失败就每interval间隔检查一次，直到成功或者超时
func PollImmediate(interval, timeout time.Duration, condition ConditionFunc) error {
	return PollImmediateWithContext(context.Background(), interval, timeout, condition.WithContext())
}

// PollImmediateWithContext tries a condition func until it returns true, an error,
// or the timeout is reached or the specified context expires, whichever happens first.
//
// PollImmediateWithContext always checks 'condition' before waiting for the interval.
// 'condition' will always be invoked at least once.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.
//
// If you want to immediately Poll something forever, see PollImmediateInfinite.
// 和PollImmediateWithContext一样。就是进去马上先执行一次，如果失败就每interval间隔检查一次，直到成功或者超时或者ctx done了
func PollImmediateWithContext(ctx context.Context, interval, timeout time.Duration, condition ConditionWithContextFunc) error {
	return poll(ctx, true, poller(interval, timeout), condition)
}

// PollImmediateUntil tries a condition func until it returns true, an error or stopCh is closed.
//
// PollImmediateUntil runs the 'condition' before waiting for the interval.
// 'condition' will always be invoked at least once.
// 和PollImmediateWithContext一样。就是进去马上先执行一次，如果失败就每interval间隔检查一次，直到成功或者超时或者stopCh close了
func PollImmediateUntil(interval time.Duration, condition ConditionFunc, stopCh <-chan struct{}) error {
	ctx, cancel := contextForChannel(stopCh)
	defer cancel()
	return PollImmediateUntilWithContext(ctx, interval, condition.WithContext())
}

// PollImmediateUntilWithContext tries a condition func until it returns true,
// an error or the specified context is cancelled or expired.
//
// PollImmediateUntilWithContext runs the 'condition' before waiting for the interval.
// 'condition' will always be invoked at least once.
func PollImmediateUntilWithContext(ctx context.Context, interval time.Duration, condition ConditionWithContextFunc) error {
	return poll(ctx, true, poller(interval, 0), condition)
}

// PollImmediateInfinite tries a condition func until it returns true or an error
//
// PollImmediateInfinite runs the 'condition' before waiting for the interval.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.

// 跟上面函数差不多，直接看上面
func PollImmediateInfinite(interval time.Duration, condition ConditionFunc) error {
	return PollImmediateInfiniteWithContext(context.Background(), interval, condition.WithContext())
}

// PollImmediateInfiniteWithContext tries a condition func until it returns true
// or an error or the specified context gets cancelled or expired.
//
// PollImmediateInfiniteWithContext runs the 'condition' before waiting for the interval.
//
// Some intervals may be missed if the condition takes too long or the time
// window is too short.
/*
  上来先执行condition做一次条件检查，如果成功那么直接返回，如果失败那么每隔interval时间进行一次条件检查，直到检查成功或者ctxDone了
*/
func PollImmediateInfiniteWithContext(ctx context.Context, interval time.Duration, condition ConditionWithContextFunc) error {
	return poll(ctx, true, poller(interval, 0), condition)
}

// Internally used, each of the the public 'Poll*' function defined in this
// package should invoke this internal function with appropriate parameters.
// ctx: the context specified by the caller, for infinite polling pass
// a context that never gets cancelled or expired.
// immediate: if true, the 'condition' will be invoked before waiting for the interval,
// in this case 'condition' will always be invoked at least once.
// wait: user specified WaitFunc function that controls at what interval the condition
// function should be invoked periodically and whether it is bound by a timeout.
// condition: user specified ConditionWithContextFunc function.
/*
immediate用来控制是否上来就检查，如果检查成功那么直接返回
如果不是immediate那么执行WaitForWithContext，根绝wait每隔一段时间检查一次，直到超时或者检查成功


该函数是定时执行condition函数来检查条件是否满足，如果满足直接返回，如果不满足那么根据wait来定时检查，直到wait超时或者wait中的ctxdown了。如果immediate 为true，那么在进入该函数的时候就直接先检查一次，否知的话会根据wait来定时检查。
poll函数有很多包装函数:
   名字中带Immediate的代表参数Immediate为true
   名字中带了WithContext的代表外部可以传递ctx来控制wait的结束
名字中带代表可以传进来一个stopchan来控制wait的结束
  名字中带Infinite的代表不会使用timeout参数来控制wait的执行
*/
func poll(ctx context.Context, immediate bool, wait WaitWithContextFunc, condition ConditionWithContextFunc) error {
	if immediate {
		done, err := runConditionWithCrashProtectionWithContext(ctx, condition)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}

	select {
	case <-ctx.Done():
		// returning ctx.Err() will break backward compatibility
		return ErrWaitTimeout
	default:
		return WaitForWithContext(ctx, wait, condition)
	}
}

// WaitFunc creates a channel that receives an item every time a test
// should be executed and is closed when the last test should be invoked.
type WaitFunc func(done <-chan struct{}) <-chan struct{}

// WithContext converts the WaitFunc to an equivalent WaitWithContextFunc
func (w WaitFunc) WithContext() WaitWithContextFunc {
	return func(ctx context.Context) <-chan struct{} {
		return w(ctx.Done())
	}
}

// WaitWithContextFunc creates a channel that receives an item every time a test
// should be executed and is closed when the last test should be invoked.
//
// When the specified context gets cancelled or expires the function
// stops sending item and returns immediately.
type WaitWithContextFunc func(ctx context.Context) <-chan struct{}

// WaitFor continually checks 'fn' as driven by 'wait'.
//
// WaitFor gets a channel from 'wait()'', and then invokes 'fn' once for every value
// placed on the channel and once more when the channel is closed. If the channel is closed
// and 'fn' returns false without error, WaitFor returns ErrWaitTimeout.
//
// If 'fn' returns an error the loop ends and that error is returned. If
// 'fn' returns true the loop ends and nil is returned.
//
// ErrWaitTimeout will be returned if the 'done' channel is closed without fn ever
// returning true.
//
// When the done channel is closed, because the golang `select` statement is
// "uniform pseudo-random", the `fn` might still run one or multiple time,
// though eventually `WaitFor` will return.
// 根据wait来定时检查ConditionFunc，当检查成功或者wait close了或者done close，就退出
func WaitFor(wait WaitFunc, fn ConditionFunc, done <-chan struct{}) error {
	ctx, cancel := contextForChannel(done)
	defer cancel()
	return WaitForWithContext(ctx, wait.WithContext(), fn.WithContext())
}

// WaitForWithContext continually checks 'fn' as driven by 'wait'.
//
// WaitForWithContext gets a channel from 'wait()'', and then invokes 'fn'
// once for every value placed on the channel and once more when the
// channel is closed. If the channel is closed and 'fn'
// returns false without error, WaitForWithContext returns ErrWaitTimeout.
//
// If 'fn' returns an error the loop ends and that error is returned. If
// 'fn' returns true the loop ends and nil is returned.
//
// context.Canceled will be returned if the ctx.Done() channel is closed
// without fn ever returning true.
//
// When the ctx.Done() channel is closed, because the golang `select` statement is
// "uniform pseudo-random", the `fn` might still run one or multiple times,
// though eventually `WaitForWithContext` will return.
// 根据wait函数每隔一段时间执行一次fn进行条件检查，直到成果或者超时
func WaitForWithContext(ctx context.Context, wait WaitWithContextFunc, fn ConditionWithContextFunc) error {
	waitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := wait(waitCtx)
	for {
		select {
		case _, open := <-c:
			ok, err := runConditionWithCrashProtectionWithContext(ctx, fn)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			if !open {  // channel关闭，说明超时了
				return ErrWaitTimeout
			}
		case <-ctx.Done():
			// returning ctx.Err() will break backward compatibility
			return ErrWaitTimeout
		}
	}
}

// poller returns a WaitFunc that will send to the channel every interval until
// timeout has elapsed and then closes the channel.
//
// Over very short intervals you may receive no ticks before the channel is
// closed. A timeout of 0 is interpreted as an infinity, and in such a case
// it would be the caller's responsibility to close the done channel.
// Failure to do so would result in a leaked goroutine.
//
// Output ticks are not buffered. If the channel is not ready to receive an
// item, the tick is skipped.
// 返回一个函数，该函数每interval间隔发一次消息到chan中，直到timeout或者ctx。done。这个时候会close chan
func poller(interval, timeout time.Duration) WaitWithContextFunc {
	return WaitWithContextFunc(func(ctx context.Context) <-chan struct{} {
		ch := make(chan struct{})

		go func() {
			defer close(ch)

			tick := time.NewTicker(interval)
			defer tick.Stop()

			var after <-chan time.Time
			if timeout != 0 {
				// time.After is more convenient, but it
				// potentially leaves timers around much longer
				// than necessary if we exit early.
				timer := time.NewTimer(timeout)
				after = timer.C
				defer timer.Stop()
			}

			for {
				select {
				case <-tick.C:  //每隔interval定时往ch里面写东西
					// If the consumer isn't ready for this signal drop it and
					// check the other channels.
					select {
					case ch <- struct{}{}:
					default:
					}
				case <-after:  // 根据timeout判断是否超时
					return
				case <-ctx.Done():
					return
				}
			}
		}()

		return ch
	})
}

// ExponentialBackoffWithContext works with a request context and a Backoff. It ensures that the retry wait never
// exceeds the deadline specified by the request context.
// 定时根据backoff检查condition 直到检查成功/ctx down了/backoff次数耗尽，返回
func ExponentialBackoffWithContext(ctx context.Context, backoff Backoff, condition ConditionFunc) error {
	for backoff.Steps > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if ok, err := runConditionWithCrashProtection(condition); err != nil || ok {
			return err
		}

		if backoff.Steps == 1 {
			break
		}

		waitBeforeRetry := backoff.Step()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitBeforeRetry):
		}
	}

	return ErrWaitTimeout
}
