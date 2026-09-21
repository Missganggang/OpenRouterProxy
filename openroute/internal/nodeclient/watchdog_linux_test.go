//go:build linux

package nodeclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestUpgradeWatchdogRestoresFailedStartup(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "node")
	path := filepath.Join(dir, "watchdog.json")
	os.WriteFile(current, []byte("new-unhealthy"), 0755)
	os.WriteFile(current+".previous", []byte("old-working"), 0755)
	marker := upgradeMarker{Executable: current, Service: "test-node.service", Version: "next", SHA256: hashText("new-unhealthy"), PreviousSHA256: hashText("old-working"), Deadline: time.Now().Add(time.Minute).Unix()}
	if err := saveUpgradeMarker(path, marker); err != nil {
		t.Fatal(err)
	}
	service, finished, err := recoverUpgrade(path, time.Now())
	if err != nil || finished || service != "" {
		t.Fatalf("early recovery: %s %t %v", service, finished, err)
	}
	service, finished, err = recoverUpgrade(path, time.Now().Add(2*time.Minute))
	if err != nil || finished || service != "test-node.service" {
		t.Fatalf("recovery: %s %t %v", service, finished, err)
	}
	data, err := os.ReadFile(current)
	if err != nil || string(data) != "old-working" {
		t.Fatal("previous executable not restored")
	}
	// A recovery process crash before systemctl succeeds must resume the restart.
	service, _, err = recoverUpgrade(path, time.Now().Add(3*time.Minute))
	if err != nil || service != "test-node.service" {
		t.Fatal("recovery restart was not durable")
	}
}

func TestUpgradeWatchdogRejectsChangedBackup(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "node")
	path := filepath.Join(dir, "watchdog.json")
	os.WriteFile(current, []byte("new"), 0755)
	os.WriteFile(current+".previous", []byte("tampered"), 0755)
	marker := upgradeMarker{Executable: current, Service: "test-node.service", Version: "next", SHA256: hashText("new"), PreviousSHA256: hashText("old"), Deadline: 1}
	saveUpgradeMarker(path, marker)
	if _, _, err := recoverUpgrade(path, time.Now()); err == nil {
		t.Fatal("corrupt backup restored")
	}
	data, _ := os.ReadFile(current)
	if string(data) != "new" {
		t.Fatal("current binary replaced with corrupt backup")
	}
}

func TestUpgradeRegistrationConfirmsOnlyMatchingExecutable(t *testing.T) {
	c := runtimeClient(t, Config{})
	path := filepath.Join(c.config.DataDir, "upgrade-watchdog.json")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := upgradeMarker{Executable: executable, Service: "test-node.service", Version: c.version, SHA256: hashText("another-executable"), PreviousSHA256: hashText("old"), Deadline: time.Now().Add(time.Minute).Unix()}
	saveUpgradeMarker(path, marker)
	if err = c.confirmUpgrade(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("old executable confirmed new installation")
	}
	marker.SHA256, err = fileSHA256("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	saveUpgradeMarker(path, marker)
	if err = c.confirmUpgrade(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("healthy new executable not confirmed")
	}
	if _, finished, err := recoverUpgrade(path, time.Now()); err != nil || !finished {
		t.Fatal("confirmed upgrade would be rolled back")
	}
}

func TestUpgradeDownloadRejectsUntrustedMetadataAndContent(t *testing.T) {
	for _, test := range []struct{ name, hash, version, payload, want string }{
		{"missing metadata", "", "", "garbage", "lacks SHA256"},
		{"wrong checksum", hashText("expected"), "next", "different", "SHA256 mismatch"},
		{"non executable", hashText("garbage"), "next", "garbage", "not a valid ELF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(nodeproto.NodeTokenHeader) != "" {
					t.Error("node credential sent to artifact endpoint")
				}
				w.Header().Set("X-Checksum-SHA256", test.hash)
				w.Header().Set("X-Node-Version", test.version)
				w.Write([]byte(test.payload))
			}))
			defer server.Close()
			c := runtimeClient(t, Config{BaseURL: server.URL})
			c.http = server.Client()
			_, err := c.installUpgrade(context.Background(), nodeproto.TaskItem{Type: nodeproto.TaskTypeUpgrade})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("wanted %s; got %v", test.want, err)
			}
		})
	}
}

func TestExecutableReplacementKeepsPreviousAndFailedReplacementPreservesCurrent(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "node")
	staged := filepath.Join(dir, "staged")
	os.WriteFile(current, []byte("old"), 0755)
	os.WriteFile(staged, []byte("new"), 0755)
	if err := replaceExecutable(current, staged); err != nil {
		t.Fatal(err)
	}
	currentData, _ := os.ReadFile(current)
	previousData, _ := os.ReadFile(current + ".previous")
	if string(currentData) != "new" || string(previousData) != "old" {
		t.Fatal("replacement or backup differs")
	}
	if err := replaceExecutable(current, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing replacement succeeded")
	}
	currentData, _ = os.ReadFile(current)
	if string(currentData) != "new" {
		t.Fatal("failed replacement broke current executable")
	}
}
