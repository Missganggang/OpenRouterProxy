package nodeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

var ErrRestart = errors.New("node service restart requested")

type taskRecord struct {
	Task         nodeproto.TaskItem `json:"task"`
	State        string             `json:"state"`
	Result       string             `json:"result"`
	Version      string             `json:"version,omitempty"`
	Acknowledged bool               `json:"acknowledged"`
	CompletedAt  int64              `json:"completed_at"`
}

type taskJournal struct {
	Identity string                 `json:"identity"`
	Records  map[uint64]*taskRecord `json:"records"`
}

type taskManager struct {
	c       *Client
	mu      sync.Mutex
	records map[uint64]*taskRecord
	resume  []uint64
	queue   chan nodeproto.TaskItem
	exec    func(context.Context, string, time.Duration) (string, error)
	upgrade func(context.Context, nodeproto.TaskItem) (string, error)
}

func newTaskManager(c *Client) (*taskManager, error) {
	m := &taskManager{c: c, records: make(map[uint64]*taskRecord), queue: make(chan nodeproto.TaskItem, 50), exec: executeCommand}
	m.upgrade = c.installUpgrade
	data, err := os.ReadFile(filepath.Join(c.config.DataDir, "tasks.json"))
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var saved taskJournal
	if json.Unmarshal(data, &saved) != nil {
		return nil, errors.New("task journal is invalid")
	}
	if saved.Identity != c.identity() {
		err := os.Rename(filepath.Join(c.config.DataDir, "tasks.json"), filepath.Join(c.config.DataDir, "tasks.previous-node-"+newID()+".json"))
		return m, err
	}
	if saved.Records != nil {
		m.records = saved.Records
	}
	for id, rec := range m.records {
		if rec == nil || id == 0 || rec.Task.TaskID != id {
			return nil, errors.New("task journal contains an invalid task record")
		}
		switch rec.State {
		case "queued", "running", "restart_pending", "done", "failed":
		default:
			return nil, errors.New("task journal contains an invalid execution state")
		}
		if rec.State == "queued" {
			delete(m.records, id)
			continue
		}
		if rec.State == "restart_pending" {
			m.resume = append(m.resume, id)
		}
		if rec.State == "running" {
			// It is impossible to safely infer whether a command committed external
			// side effects before a crash. Never execute it a second time.
			rec.State, rec.Result, rec.CompletedAt = "failed", "Node restarted during task execution; result is unknown and the command was not replayed.", time.Now().Unix()
		}
	}
	return m, m.saveLocked()
}

func (m *taskManager) saveLocked() error {
	data, err := json.Marshal(taskJournal{Identity: m.c.identity(), Records: m.records})
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(m.c.config.DataDir, "tasks.json"), data)
}

func (m *taskManager) registered() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.resume {
		rec := m.records[id]
		if rec == nil || rec.State != "restart_pending" {
			continue
		}
		rec.State, rec.Result = "done", "Node restarted and registered successfully; version "+m.c.version
		if rec.Version != "" && rec.Version != m.c.version {
			rec.State, rec.Result = "failed", "Node restarted with an unexpected version; previous executable was retained."
		}
		rec.CompletedAt = time.Now().Unix()
	}
	if len(m.resume) == 0 {
		return nil
	}
	m.resume = nil
	return m.saveLocked()
}

func (m *taskManager) submit(tasks []nodeproto.TaskItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, task := range tasks {
		if task.TaskID == 0 || m.records[task.TaskID] != nil {
			continue
		}
		select {
		case m.queue <- task:
			m.records[task.TaskID] = &taskRecord{Task: task, State: "queued"}
		default:
			return // The next HTTP poll will offer this task again.
		}
	}
}

func (m *taskManager) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-m.queue:
			if m.execute(ctx, task) {
				return
			}
		}
	}
}

func (m *taskManager) execute(ctx context.Context, task nodeproto.TaskItem) bool {
	if ctx.Err() != nil {
		return false
	}
	m.mu.Lock()
	rec := m.records[task.TaskID]
	if rec == nil {
		rec = &taskRecord{Task: task}
		m.records[task.TaskID] = rec
	}
	rec.State = "running"
	if err := m.saveLocked(); err != nil {
		delete(m.records, task.TaskID)
		m.mu.Unlock()
		m.c.log.Printf("task not executed: cannot persist execution marker: %s", m.c.safe(err))
		return false
	}
	m.mu.Unlock()
	var output, version string
	var err error
	restart := false
	if m.c.config.DisableExecute {
		err = errors.New("DISABLE_EXECUTE=1: remote execution, terminals, restart and upgrade are disabled")
	} else {
		switch task.Type {
		case nodeproto.TaskTypeExec:
			command, _ := task.Payload["command"].(string)
			if strings.TrimSpace(command) == "" || len(command) > 65536 {
				err = errors.New("invalid remote command")
				break
			}
			timeout := 30
			if value, ok := task.Payload["timeout"]; ok {
				if n, parseErr := strconv.Atoi(fmt.Sprint(value)); parseErr == nil {
					timeout = n
				}
			}
			if timeout <= 0 || timeout > 300 {
				timeout = 300
			}
			output, err = m.exec(ctx, command, time.Duration(timeout)*time.Second)
		case nodeproto.TaskTypeRestart:
			restart = true
		case nodeproto.TaskTypeUpgrade:
			version, err = m.upgrade(ctx, task)
			restart = err == nil
		default:
			err = errors.New("unsupported node task type")
		}
	}
	if err != nil {
		output = strings.TrimSpace(output + "\n" + m.c.safe(err))
	}
	if len(output) > 8192 {
		output = output[:8192]
	}
	m.mu.Lock()
	rec.State, rec.Result, rec.CompletedAt = "done", output, time.Now().Unix()
	if err != nil {
		rec.State = "failed"
	}
	if restart {
		rec.State, rec.Version = "restart_pending", version
	}
	saveErr := m.saveLocked()
	m.mu.Unlock()
	if saveErr != nil {
		m.c.log.Printf("persist task result failed: %s", m.c.safe(saveErr))
		return false
	}
	if restart {
		select {
		case m.c.restart <- struct{}{}:
		default:
		}
	}
	m.c.notify()
	return restart
}

func (m *taskManager) report(ctx context.Context) error {
	m.mu.Lock()
	// Persist completed work before acknowledging it; disk failures must not make
	// a side-effecting task eligible for execution again after a process restart.
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	var pending []nodeproto.TaskResultRequest
	for id, rec := range m.records {
		if !rec.Acknowledged && (rec.State == "done" || rec.State == "failed") {
			pending = append(pending, nodeproto.TaskResultRequest{TaskID: id, Status: rec.State, Result: rec.Result, Timestamp: rec.CompletedAt})
		}
	}
	m.mu.Unlock()
	for _, result := range pending {
		var response nodeproto.TaskResultResponse
		if err := m.c.request(ctx, "POST", "/api/node/task-result", result, &response); err != nil {
			return err
		}
		if !response.OK {
			return errors.New("panel did not acknowledge task result")
		}
		m.mu.Lock()
		m.records[result.TaskID].Acknowledged = true
		err := m.saveLocked()
		m.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
