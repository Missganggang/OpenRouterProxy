package nodeclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

type forwardingEngine interface {
	Apply(nodeproto.ConfigResponse) []nodeproto.RuleSyncResult
	RunningRules() []nodeproto.RunningRule
	Stats() nodeproto.ReportStats
	Close()
}

// Client owns one node registration, its configuration and forwarding engine.
type Client struct {
	config             Config
	version            string
	http               *http.Client
	log                *log.Logger
	engine             forwardingEngine
	metrics            *metricsCollector
	nodeID             uint64
	configVersion      int64
	interval           time.Duration
	retryMin, retryMax time.Duration
	lastConfig         time.Time
	results            []nodeproto.RuleSyncResult
	configError        string
	pendingTraffic     map[string]*pendingTrafficBucket
	outbox             *nodeproto.ReportRequest
	lastMetrics        nodeproto.HeartbeatMetrics
	wake               chan struct{}
	restart            chan struct{}
	tasks              *taskManager
	forceConfig        bool
	now                func() time.Time
}

func NewClient(config Config, version string, logger *log.Logger) (*Client, error) {
	if err := config.ensureIdentity(); err != nil {
		return nil, fmt.Errorf("initialize node identity: %w", err)
	}
	if logger == nil {
		logger = log.Default()
	}
	c := &Client{
		config: config, version: version, log: logger,
		http:   &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		engine: NewEngine(config.BindInbound), metrics: newMetricsCollector(config.CountInterface),
		interval: time.Duration(nodeproto.HeartbeatIntervalDefault) * time.Second,
		retryMin: time.Second, retryMax: 30 * time.Second,
		pendingTraffic: make(map[string]*pendingTrafficBucket), now: time.Now,
		wake: make(chan struct{}, 1), restart: make(chan struct{}, 1),
	}
	if err := c.loadReports(); err != nil {
		return nil, err
	}
	tasks, err := newTaskManager(c)
	if err != nil {
		return nil, err
	}
	c.tasks = tasks
	return c, nil
}

// Run retries panel outages while keeping the last received forwarding rules active.
// Cancellation interrupts requests and timers, then releases all listeners.
func (c *Client) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); c.streamLoop(ctx) }()
	go func() { defer workers.Done(); c.tasks.run(ctx) }()
	defer func() {
		cancel()
		workers.Wait()
		c.engine.Close()
		c.captureTraffic()
		if err := c.saveReports(); err != nil {
			c.log.Printf("persist final traffic: %s", c.safe(err))
		}
	}()
	if err := c.restore(); err != nil && !os.IsNotExist(err) {
		c.log.Printf("local configuration not restored: %s", c.safe(err))
	}
	registered := false
	backoff := c.retryMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Persist samples even when registration/heartbeats cannot reach the panel.
		c.checkpointTraffic()
		var err error
		if !registered {
			err = c.register(ctx)
			if err == nil {
				registered = true
				c.log.Printf("node %d registered; client version %s", c.nodeID, c.version)
			}
		}
		if err == nil {
			err = c.heartbeat(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var status *httpStatusError
			if errors.As(err, &status) && (status.code == http.StatusUnauthorized || status.code == http.StatusForbidden) {
				registered = false
			}
			c.log.Printf("panel communication failed: %s; retry in %s", c.safe(err), backoff)
			if err := c.wait(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			if backoff > c.retryMax {
				backoff = c.retryMax
			}
			continue
		}
		backoff = c.retryMin
		if err := c.wait(ctx, c.interval); err != nil {
			return err
		}
	}
}

func (c *Client) register(ctx context.Context) error {
	request := nodeproto.RegisterRequest{Token: c.config.Token, Version: c.version, System: c.metrics.system(), ConfigVersion: c.configVersion, UUID: c.config.MachineID, DisableExecute: c.config.DisableExecute}
	var response nodeproto.RegisterResponse
	if err := c.request(ctx, http.MethodPost, "/api/node/register", request, &response); err != nil {
		return err
	}
	if response.NodeID == 0 || !response.Config.Full {
		return errors.New("registration response lacks node ID or full configuration")
	}
	c.nodeID = response.NodeID
	c.setInterval(response.HeartbeatInterval)
	c.apply(response.Config)
	if err := c.confirmUpgrade(); err != nil {
		return err
	}
	if err := c.tasks.registered(); err != nil {
		return err
	}
	return nil
}

func (c *Client) heartbeat(ctx context.Context) error {
	c.lastMetrics = c.metrics.sample()
	request := nodeproto.HeartbeatRequest{NodeID: c.nodeID, Version: c.version, ConfigVersion: c.configVersion, ConfigHash: nodeproto.ConfigHash(c.effectiveConfig()), DisableExecute: c.config.DisableExecute, Metrics: c.lastMetrics, RunningRules: c.engine.RunningRules(), Timestamp: time.Now().Unix()}
	var response nodeproto.HeartbeatResponse
	if err := c.request(ctx, http.MethodPost, "/api/node/heartbeat", request, &response); err != nil {
		return err
	}
	c.setInterval(response.HeartbeatInterval)
	// Periodic full refresh also repairs restored databases whose revision went backwards.
	if c.forceConfig || response.NeedConfig || response.ConfigVersion != c.configVersion || time.Since(c.lastConfig) >= 2*time.Minute {
		var cfg nodeproto.ConfigResponse
		if err := c.request(ctx, http.MethodGet, "/api/node/config?version=0", nil, &cfg); err != nil {
			return err
		}
		if !cfg.Full {
			return errors.New("panel did not return the requested full configuration")
		}
		c.apply(cfg)
		c.forceConfig = false
	}
	// Reports and task polling share a short budget so they cannot starve heartbeats.
	auxCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.flushReports(auxCtx); err != nil {
		c.log.Printf("node report failed: %s", c.safe(err))
	}
	if err := c.tasks.report(auxCtx); err != nil {
		c.log.Printf("task result report failed: %s", c.safe(err))
	}
	tasks := response.Tasks
	if len(tasks) == 0 {
		var pending nodeproto.TasksResponse
		if err := c.request(auxCtx, http.MethodGet, "/api/node/tasks", nil, &pending); err != nil {
			c.log.Printf("task polling failed: %s", c.safe(err))
			return nil
		}
		tasks = pending.Tasks
	}
	c.tasks.submit(tasks)
	return nil
}

func (c *Client) apply(cfg nodeproto.ConfigResponse) {
	// Legacy engines must drain before changing their version; the real engine
	// records the version and hour with each successful byte write.
	c.checkpointTraffic()
	c.results = c.engine.Apply(cfg)
	c.configVersion = cfg.ConfigVersion
	c.lastConfig = time.Now()
	c.setInterval(cfg.HeartbeatInterval)
	c.configError = ""
	for _, result := range c.results {
		if result.Status == "failed" {
			c.log.Printf("rule %d failed: %s", result.RuleID, c.safe(errors.New(result.Error)))
			c.configError = "One or more rules failed; see rule synchronization errors."
		}
	}
	// Cache only the engine's effective state; a rejected change must not replace
	// a working configuration on disk. Older test engines fall back to all-or-none.
	effective := cfg
	if provider, ok := c.engine.(interface {
		EffectiveConfig() nodeproto.ConfigResponse
	}); ok {
		effective = provider.EffectiveConfig()
	} else if c.configError != "" {
		return
	}
	data, err := json.Marshal(savedConfig{Identity: c.identity(), Config: effective})
	if err == nil {
		err = atomicWrite(filepath.Join(c.config.DataDir, "config.json"), data)
	}
	if err != nil {
		c.configError = "Could not persist the node configuration."
		c.log.Printf("persist configuration failed: %s", c.safe(err))
	}
}

func (c *Client) effectiveConfig() nodeproto.ConfigResponse {
	if provider, ok := c.engine.(interface {
		EffectiveConfig() nodeproto.ConfigResponse
	}); ok {
		return provider.EffectiveConfig()
	}
	return nodeproto.ConfigResponse{Full: true, ConfigVersion: c.configVersion}
}

func (c *Client) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) wait(ctx context.Context, duration time.Duration) error {
	select {
	case <-c.restart:
		return ErrRestart
	default:
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.restart:
		return ErrRestart
	case <-c.wake:
		c.forceConfig = true
		return nil
	case <-timer.C:
		return nil
	}
}

type savedConfig struct {
	Identity string                   `json:"identity"`
	Config   nodeproto.ConfigResponse `json:"config"`
}

func (c *Client) identity() string {
	sum := sha256.Sum256([]byte(c.config.BaseURL + "\x00" + c.config.Token))
	return hex.EncodeToString(sum[:])
}

func (c *Client) restore() error {
	data, err := os.ReadFile(filepath.Join(c.config.DataDir, "config.json"))
	if err != nil {
		return err
	}
	var saved savedConfig
	if json.Unmarshal(data, &saved) != nil || saved.Identity != c.identity() || !saved.Config.Full {
		return errors.New("saved configuration is invalid or belongs to another node")
	}
	c.results = c.engine.Apply(saved.Config)
	c.configVersion = saved.Config.ConfigVersion
	c.log.Printf("restored configuration version %d", c.configVersion)
	return nil
}

func (c *Client) setInterval(seconds int) {
	if seconds > 0 && seconds <= 300 {
		c.interval = time.Duration(seconds) * time.Second
	}
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func (c *Client) request(ctx context.Context, method, path string, request, response any) error {
	var body io.Reader
	if request != nil {
		data, err := json.Marshal(request)
		if err != nil {
			return errors.New("cannot encode node request")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.config.BaseURL+path, body)
	if err != nil {
		return errors.New("cannot construct panel request")
	}
	req.Header.Set("Content-Type", "application/json")
	if path != "/api/node/register" {
		req.Header.Set(nodeproto.NodeTokenHeader, c.config.Token)
		if c.nodeID != 0 {
			req.Header.Set(nodeproto.NodeIDHeader, strconv.FormatUint(c.nodeID, 10))
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("panel request: %s", c.safe(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{code: resp.StatusCode}
	}
	if response == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
		return err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(response); err != nil {
		return errors.New("invalid JSON in panel response")
	}
	return nil
}

func (c *Client) safe(err error) string {
	message := err.Error()
	if c.config.Token != "" {
		message = strings.ReplaceAll(message, c.config.Token, "[redacted]")
	}
	return message
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
