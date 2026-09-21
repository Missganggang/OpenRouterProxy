package nodeclient

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
	"golang.org/x/time/rate"
)

// Usage remains shared by every rule of a user, including live connections
// established under an older rule configuration.
type usageState struct {
	mu                     sync.Mutex
	connections            int
	ips, devices           map[string]time.Time
	ipActive, deviceActive map[string]int
	bytes                  int64
	reportedUsed           int64
	initialized            bool
	limiter                *rate.Limiter
	speed                  int64
}

func newUsage() *usageState {
	return &usageState{ips: map[string]time.Time{}, devices: map[string]time.Time{}, ipActive: map[string]int{}, deviceActive: map[string]int{}}
}
func (u *usageState) acquire(ip, device string, maxConn, maxIP, maxDevice int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	for key, seen := range u.ips {
		if now.Sub(seen) > 5*time.Minute && u.ipActive[key] == 0 {
			delete(u.ips, key)
			delete(u.ipActive, key)
		}
	}
	for key, seen := range u.devices {
		if now.Sub(seen) > 5*time.Minute && u.deviceActive[key] == 0 {
			delete(u.devices, key)
			delete(u.deviceActive, key)
		}
	}
	if maxConn > 0 && u.connections >= maxConn {
		return false
	}
	if _, ok := u.ips[ip]; !ok && maxIP > 0 && len(u.ips) >= maxIP {
		return false
	}
	if _, ok := u.devices[device]; !ok && maxDevice > 0 && len(u.devices) >= maxDevice {
		return false
	}
	u.connections++
	u.ips[ip] = now
	u.devices[device] = now
	u.ipActive[ip]++
	u.deviceActive[device]++
	return true
}
func (u *usageState) release(ip, device string) {
	u.mu.Lock()
	u.connections--
	u.ipActive[ip]--
	u.deviceActive[device]--
	u.ips[ip] = time.Now()
	u.devices[device] = time.Now()
	u.mu.Unlock()
}
func (u *usageState) wait(ctx context.Context, speed int64, n int) error {
	if speed <= 0 {
		return nil
	}
	u.mu.Lock()
	if u.limiter == nil || u.speed != speed {
		u.limiter = rate.NewLimiter(rate.Limit(speed), int(max(speed, 32768)))
		u.speed = speed
	}
	limiter := u.limiter
	u.mu.Unlock()
	for n > 0 {
		chunk := min(n, limiter.Burst())
		if err := limiter.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}
func (u *usageState) consume(n int, limits nodeproto.UserLimits, mult float64) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.initialized {
		u.syncUsed(limits.TrafficUsed)
	}
	if limits.Disabled {
		return errors.New("user is disabled")
	}
	if limits.TrafficLimit > 0 && u.reportedUsed+u.bytes >= limits.TrafficLimit {
		return errors.New("user traffic quota exhausted")
	}
	if mult < 0 {
		mult = 0
	}
	delta := int64(math.Ceil(float64(n) * mult))
	if limits.TrafficLimit > 0 && delta > limits.TrafficLimit-u.reportedUsed-u.bytes {
		return errors.New("user traffic quota exhausted")
	}
	u.bytes += delta
	return nil
}

// Call with mu held. The panel total acknowledges already counted local bytes;
// keep any local usage that is still ahead of that acknowledged total.
func (u *usageState) syncUsed(total int64) {
	if u.initialized && total < u.reportedUsed {
		u.bytes = 0
		u.reportedUsed = total
	}
	if total > u.reportedUsed {
		u.bytes = max(0, u.bytes-(total-u.reportedUsed))
		u.reportedUsed = total
	}
	u.initialized = true
}
func sourceIP(source string) string {
	host, _, err := net.SplitHostPort(source)
	if err == nil {
		return host
	}
	return source
}

type admission struct {
	rule, user *usageState
	done       sync.Once
	ip, device string
}

func (a *admission) close() {
	if a != nil {
		a.done.Do(func() {
			a.rule.release(a.ip, a.device)
			if a.user != nil {
				a.user.release(a.ip, a.device)
			}
		})
	}
}
func (f *forwarder) admit(source, device string) (*admission, error) {
	ip := sourceIP(source)
	if f.rule.UserLimits.Disabled {
		return nil, errors.New("user is disabled")
	}
	if device == "" {
		device = ip
	}
	if !f.usage.acquire(ip, device, f.rule.ConnLimit, f.rule.IPLimit, 0) {
		return nil, errors.New("rule connection or IP limit reached")
	}
	a := &admission{rule: f.usage, ip: ip, device: device}
	if f.user != nil {
		limits := f.rule.UserLimits
		if err := f.user.consume(0, limits, 0); err != nil {
			a.close()
			return nil, err
		}
		if !f.user.acquire(ip, device, limits.ConnLimit, limits.IPLimit, limits.DeviceLimit) {
			a.close()
			return nil, errors.New("user connection, IP or device limit reached")
		}
		a.user = f.user
	}
	return a, nil
}
func (f *forwarder) trafficMultiplier() float64 {
	return f.rule.InboundMultiplier + float64(len(f.routePath()))*f.rule.OutboundMultiplier
}
