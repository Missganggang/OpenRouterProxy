// Package job 实现全部后台定时任务。
//
// 设计约束（规格书 1.2）：不引入消息队列、不引入分布式调度，
// 在单进程内用 ticker 驱动全部任务。每个任务都可以被手动触发
// （对应 API `POST /api/v1/tasks/:name/run`）。
package job

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Task 是一个可调度的后台任务。
type Task struct {
	// Name 是任务标识，同时用作手动触发的路径参数。
	Name string
	// Desc 是任务的中文说明，展示在任务列表页。
	Desc string
	// Interval 是执行间隔；为 0 表示只在启动时执行一次。
	Interval time.Duration
	// Run 是任务体。ctx 在进程关闭时被取消。
	Run func(ctx context.Context) error
	// RunOnStart 为 true 时启动后立即执行一次。
	RunOnStart bool
}

// Scheduler 管理全部后台任务的生命周期。
type Scheduler struct {
	log   *zap.Logger
	tasks []*Task

	mu      sync.Mutex
	running map[string]bool
	// lastRun / lastResult 记录每个任务的最近执行情况，供任务列表页展示。
	lastRun    map[string]time.Time
	lastResult map[string]string
	lastErr    map[string]string

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewScheduler 构造调度器。
func NewScheduler(log *zap.Logger) *Scheduler {
	return &Scheduler{
		log:        log,
		running:    make(map[string]bool),
		lastRun:    make(map[string]time.Time),
		lastResult: make(map[string]string),
		lastErr:    make(map[string]string),
		stopCh:     make(chan struct{}),
	}
}

// Register 注册一个任务。
//
// 同名任务会被忽略，避免重复注册导致重复执行。
func (s *Scheduler) Register(t *Task) {
	if t == nil || t.Name == "" || t.Run == nil {
		return
	}
	for _, existing := range s.tasks {
		if existing.Name == t.Name {
			s.log.Warn("任务已注册，忽略重复注册", zap.String("task", t.Name))
			return
		}
	}
	s.tasks = append(s.tasks, t)
}

// Tasks 返回全部已注册任务的只读快照。
func (s *Scheduler) Tasks() []*Task {
	out := make([]*Task, len(s.tasks))
	copy(out, s.tasks)
	return out
}

// TaskStatus 是任务列表页需要的运行时状态。
type TaskStatus struct {
	Name        string     `json:"name"`
	Desc        string     `json:"desc"`
	IntervalSec int        `json:"interval_sec"`
	Running     bool       `json:"running"`
	LastRunAt   *time.Time `json:"last_run_at"`
	LastResult  string     `json:"last_result"`
	LastError   string     `json:"last_error"`
	NextRunAt   *time.Time `json:"next_run_at"`
}

// Status 返回全部任务的运行时状态。
func (s *Scheduler) Status() []TaskStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]TaskStatus, 0, len(s.tasks))
	now := time.Now().UTC()
	for _, t := range s.tasks {
		st := TaskStatus{
			Name:        t.Name,
			Desc:        t.Desc,
			IntervalSec: int(t.Interval.Seconds()),
			Running:     s.running[t.Name],
			LastResult:  s.lastResult[t.Name],
			LastError:   s.lastErr[t.Name],
		}
		if lr, ok := s.lastRun[t.Name]; ok {
			v := lr
			st.LastRunAt = &v
			if t.Interval > 0 {
				next := lr.Add(t.Interval)
				// 已经错过的任务，下一次执行时间是「现在」。
				if next.Before(now) {
					next = now
				}
				st.NextRunAt = &next
			}
		}
		out = append(out, st)
	}
	return out
}

// Start 启动全部任务。
//
// 每个任务在独立的 goroutine 中按 ticker 运行；
// RunOnStart 的任务会立即执行一次。
func (s *Scheduler) Start(ctx context.Context) {
	for _, t := range s.tasks {
		t := t
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.loop(ctx, t)
		}()
	}
	s.log.Info("后台任务已启动", zap.Int("count", len(s.tasks)))
}

// loop 是单个任务的执行循环。
func (s *Scheduler) loop(ctx context.Context, t *Task) {
	if t.RunOnStart {
		s.execute(ctx, t)
	}
	// Interval 为 0 表示只跑一次启动那一轮。
	if t.Interval <= 0 {
		return
	}

	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.execute(ctx, t)
		}
	}
}

// execute 执行一个任务，并记录状态。
//
// 使用「运行中」标记防止上一轮未结束时重复进入——
// 对于采集类任务，重叠执行会造成数据重复统计。
func (s *Scheduler) execute(ctx context.Context, t *Task) {
	s.mu.Lock()
	if s.running[t.Name] {
		s.mu.Unlock()
		s.log.Debug("任务仍在运行，跳过本轮", zap.String("task", t.Name))
		return
	}
	s.running[t.Name] = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running[t.Name] = false
		s.lastRun[t.Name] = time.Now().UTC()
		s.mu.Unlock()
	}()

	start := time.Now()
	var err error
	// 用闭包捕获 panic：单个任务崩溃不应拖垮整个调度器。
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = &panicError{task: t.Name, value: r}
			}
		}()
		err = t.Run(ctx)
	}()

	s.mu.Lock()
	if err != nil {
		s.lastResult[t.Name] = "failed"
		s.lastErr[t.Name] = err.Error()
	} else {
		s.lastResult[t.Name] = "success"
		s.lastErr[t.Name] = ""
	}
	s.mu.Unlock()

	if err != nil {
		s.log.Error("任务执行失败", zap.String("task", t.Name), zap.Error(err))
	} else {
		s.log.Debug("任务执行完成",
			zap.String("task", t.Name),
			zap.Duration("cost", time.Since(start)))
	}
}

// RunNow 手动触发一个任务，同步等待执行结果。
//
// 供 API `POST /api/v1/tasks/:name/run` 使用。
// 返回任务名不存在时的错误。
func (s *Scheduler) RunNow(ctx context.Context, name string) error {
	for _, t := range s.tasks {
		if t.Name != name {
			continue
		}
		s.execute(ctx, t)
		s.mu.Lock()
		defer s.mu.Unlock()
		if e := s.lastErr[name]; e != "" {
			return &runError{task: name, msg: e}
		}
		return nil
	}
	return &notFoundError{name: name}
}

// Stop 停止调度并等待在途任务结束。
func (s *Scheduler) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
}

// -------------------- 任务错误类型 --------------------

type panicError struct {
	task  string
	value interface{}
}

func (e *panicError) Error() string {
	return "任务 " + e.task + " 发生 panic"
}

type runError struct {
	task string
	msg  string
}

func (e *runError) Error() string { return e.msg }

type notFoundError struct{ name string }

func (e *notFoundError) Error() string { return "任务不存在: " + e.name }

// IsNotFound 判断错误是否为「任务不存在」。
func IsNotFound(err error) bool {
	_, ok := err.(*notFoundError)
	return ok
}
