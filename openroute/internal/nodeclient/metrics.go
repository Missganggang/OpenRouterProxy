package nodeclient

import (
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

type metricsCollector struct {
	interfaces        map[string]bool
	lastTime          time.Time
	lastCPU, lastIdle uint64
	lastIn, lastOut   int64
}

func newMetricsCollector(interfaces string) *metricsCollector {
	m := &metricsCollector{interfaces: make(map[string]bool)}
	for _, iface := range strings.Split(interfaces, ",") {
		if iface = strings.TrimSpace(iface); iface != "" {
			m.interfaces[iface] = true
		}
	}
	m.sample()
	return m
}

func readText(path string) string     { data, _ := os.ReadFile(path); return string(data) }
func intValue(value string) int64     { n, _ := strconv.ParseInt(value, 10, 64); return n }
func floatValue(value string) float64 { n, _ := strconv.ParseFloat(value, 64); return n }

func (m *metricsCollector) sample() nodeproto.HeartbeatMetrics {
	var result nodeproto.HeartbeatMetrics
	now := time.Now()
	mem := map[string]int64{}
	for _, line := range strings.Split(readText("/proc/meminfo"), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			mem[strings.TrimSuffix(f[0], ":")] = intValue(f[1]) * 1024
		}
	}
	result.MemTotal = mem["MemTotal"]
	available, ok := mem["MemAvailable"]
	if !ok {
		available = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	result.MemUsed = result.MemTotal - available
	result.SwapTotal = mem["SwapTotal"]
	result.SwapUsed = result.SwapTotal - mem["SwapFree"]
	result.DiskTotal, result.DiskUsed = diskUsage()
	for _, line := range strings.Split(readText("/proc/stat"), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var total, idle uint64
		for i := 1; i < len(f) && i <= 8; i++ {
			n, _ := strconv.ParseUint(f[i], 10, 64)
			total += n
			if i == 4 || i == 5 {
				idle += n
			}
		}
		if m.lastCPU != 0 && total > m.lastCPU && idle >= m.lastIdle {
			elapsed, idleElapsed := total-m.lastCPU, idle-m.lastIdle
			if idleElapsed <= elapsed {
				result.CPU = 100 * float64(elapsed-idleElapsed) / float64(elapsed)
			}
		}
		m.lastCPU, m.lastIdle = total, idle
		break
	}
	for _, line := range strings.Split(readText("/proc/net/dev"), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || (len(m.interfaces) > 0 && !m.interfaces[name]) {
			continue
		}
		f := strings.Fields(value)
		if len(f) >= 9 {
			result.NetIn += intValue(f[0])
			result.NetOut += intValue(f[8])
		}
	}
	if seconds := now.Sub(m.lastTime).Seconds(); !m.lastTime.IsZero() && seconds > 0 {
		if result.NetIn >= m.lastIn {
			result.NetInSpeed = int64(float64(result.NetIn-m.lastIn) / seconds)
		}
		if result.NetOut >= m.lastOut {
			result.NetOutSpeed = int64(float64(result.NetOut-m.lastOut) / seconds)
		}
	}
	m.lastTime, m.lastIn, m.lastOut = now, result.NetIn, result.NetOut
	if f := strings.Fields(readText("/proc/loadavg")); len(f) >= 3 {
		result.Load1, result.Load5, result.Load15 = floatValue(f[0]), floatValue(f[1]), floatValue(f[2])
	}
	if f := strings.Fields(readText("/proc/uptime")); len(f) > 0 {
		result.Uptime = int64(floatValue(f[0]))
	}
	result.TcpConn = socketCount("/proc/net/tcp", true) + socketCount("/proc/net/tcp6", true)
	result.UdpConn = socketCount("/proc/net/udp", false) + socketCount("/proc/net/udp6", false)
	return result
}

func socketCount(path string, tcp bool) int {
	count := 0
	for _, line := range strings.Split(readText(path), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] != "sl" && (!tcp || f[3] != "0A") {
			count++
		}
	}
	return count
}

func (m *metricsCollector) system() nodeproto.RegisterSystem {
	metrics := m.sample()
	s := nodeproto.RegisterSystem{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUCores: runtime.NumCPU(), MemTotal: metrics.MemTotal, DiskTotal: metrics.DiskTotal, KernelVer: strings.TrimSpace(readText("/proc/sys/kernel/osrelease"))}
	for _, line := range strings.Split(readText("/etc/os-release"), "\n") {
		if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			s.OS = strings.Trim(value, "\"'")
		}
	}
	for _, line := range strings.Split(readText("/proc/cpuinfo"), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && (strings.TrimSpace(key) == "model name" || strings.TrimSpace(key) == "Hardware") {
			s.CPUModel = strings.TrimSpace(value)
			break
		}
	}
	if metrics.Uptime > 0 {
		s.BootTime = time.Now().Unix() - metrics.Uptime
	}
	for _, line := range strings.Split(readText("/proc/stat"), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "btime" {
			s.BootTime = intValue(f[1])
		}
	}
	addresses, _ := net.InterfaceAddrs()
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || !ip.IsGlobalUnicast() {
			continue
		}
		if ip.IsPrivate() {
			if s.PrivateIP == "" {
				s.PrivateIP = ip.String()
			}
		} else if ip.To4() != nil {
			if s.PublicIPv4 == "" {
				s.PublicIPv4 = ip.String()
			}
		} else if s.PublicIPv6 == "" {
			s.PublicIPv6 = ip.String()
		}
	}
	return s
}
