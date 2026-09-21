//go:build linux

package nodeclient

import "testing"

func TestLinuxMetricsReadActualSystem(t *testing.T) {
	collector := newMetricsCollector("")
	m := collector.sample()
	if m.MemTotal <= 0 || m.MemUsed < 0 || m.MemUsed > m.MemTotal {
		t.Fatalf("invalid memory metrics: %d/%d", m.MemUsed, m.MemTotal)
	}
	if m.DiskTotal <= 0 || m.DiskUsed < 0 || m.DiskUsed > m.DiskTotal {
		t.Fatalf("invalid disk metrics: %d/%d", m.DiskUsed, m.DiskTotal)
	}
	if m.Uptime <= 0 || m.CPU < 0 || m.CPU > 100 {
		t.Fatal("invalid uptime or CPU")
	}
	s := collector.system()
	if s.KernelVer == "" || s.CPUCores <= 0 || s.BootTime <= 0 || s.OS == "" {
		t.Fatal("missing Linux registration system information")
	}
}
