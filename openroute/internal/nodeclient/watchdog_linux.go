//go:build linux

package nodeclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type upgradeMarker struct {
	Executable     string `json:"executable"`
	Service        string `json:"service"`
	Version        string `json:"version"`
	SHA256         string `json:"sha256"`
	PreviousSHA256 string `json:"previous_sha256"`
	Deadline       int64  `json:"deadline"`
	RolledBack     bool   `json:"rolled_back"`
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validService(unit string) bool {
	if !strings.HasSuffix(unit, ".service") || len(unit) > 200 {
		return false
	}
	for _, r := range unit {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.@", r)) {
			return false
		}
	}
	return true
}

func currentService() (string, error) {
	if value := os.Getenv("OPENROUTE_SYSTEMD_UNIT"); validService(value) {
		return value, nil
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, part := range strings.Split(parts[2], "/") {
			if validService(part) {
				return part, nil
			}
		}
	}
	return "", errors.New("online upgrade requires a systemd-managed node service")
}

func withUpgradeLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func readUpgradeMarker(path string) (upgradeMarker, error) {
	var marker upgradeMarker
	data, err := os.ReadFile(path)
	if err != nil {
		return marker, err
	}
	err = json.Unmarshal(data, &marker)
	if err == nil && (!filepath.IsAbs(marker.Executable) || !validService(marker.Service) || len(marker.SHA256) != 64 || len(marker.PreviousSHA256) != 64 || marker.Deadline <= 0) {
		err = errors.New("invalid upgrade recovery marker")
	}
	return marker, err
}

func saveUpgradeMarker(path string, marker upgradeMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func (c *Client) armUpgradeWatchdog(ctx context.Context, executable, version, checksum string) (func(), error) {
	runner, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, errors.New("online upgrade requires systemd-run for automatic rollback")
	}
	service, err := currentService()
	if err != nil {
		return nil, err
	}
	markerPath := filepath.Join(c.config.DataDir, "upgrade-watchdog.json")
	marker := upgradeMarker{Executable: executable, Service: service, Version: version, SHA256: checksum, Deadline: time.Now().Add(120 * time.Second).Unix()}
	err = withUpgradeLock(markerPath, func() error {
		if _, err := os.Stat(markerPath); err == nil {
			return errors.New("another upgrade recovery is still pending")
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := backupExecutable(executable); err != nil {
			return err
		}
		previousHash, err := fileSHA256(executable + ".previous")
		if err != nil {
			return err
		}
		marker.PreviousSHA256 = previousHash
		return saveUpgradeMarker(markerPath, marker)
	})
	if err != nil {
		return nil, err
	}
	stop := func() { _ = withUpgradeLock(markerPath, func() error { return os.Remove(markerPath) }) }
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	unit := "openroute-upgrade-" + newID() + ".service"
	cmd := exec.CommandContext(startCtx, runner, "--quiet", "--collect", "--unit="+unit, "--property=Type=exec", "--property=Restart=on-failure", "--property=RestartSec=5s", executable+".previous", "-upgrade-watchdog", markerPath)
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		stop()
		return nil, fmt.Errorf("could not start independent upgrade recovery: %w: %s", err, output.String())
	}
	return stop, nil
}

func (c *Client) confirmUpgrade() error {
	path := filepath.Join(c.config.DataDir, "upgrade-watchdog.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return withUpgradeLock(path, func() error {
		marker, err := readUpgradeMarker(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if marker.RolledBack {
			return errors.New("upgrade rollback is in progress; registration will be retried")
		}
		if marker.Version != c.version {
			return nil
		}
		// /proc/self/exe hashes this process's executable inode, including when the
		// old process is still exiting after its on-disk pathname was replaced.
		hash, err := fileSHA256("/proc/self/exe")
		if err != nil {
			return err
		}
		if hash != marker.SHA256 {
			return nil
		}
		if err = os.Remove(path); err != nil {
			return err
		}
		return syncDirectory(filepath.Dir(path))
	})
}

// recoverUpgrade checks and replaces files while holding the same lock used by
// registration acknowledgement, so confirmation and rollback cannot race.
func recoverUpgrade(path string, now time.Time) (service string, finished bool, err error) {
	err = withUpgradeLock(path, func() error {
		marker, readErr := readUpgradeMarker(path)
		if os.IsNotExist(readErr) {
			finished = true
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if now.Unix() < marker.Deadline {
			return nil
		}
		current, hashErr := fileSHA256(marker.Executable)
		if hashErr != nil {
			return hashErr
		}
		if current == marker.SHA256 && !marker.RolledBack {
			previous, hashErr := fileSHA256(marker.Executable + ".previous")
			if hashErr != nil {
				return hashErr
			}
			if previous != marker.PreviousSHA256 {
				return errors.New("upgrade recovery executable checksum mismatch")
			}
			if err := os.Rename(marker.Executable+".previous", marker.Executable); err != nil {
				return err
			}
			if err := syncDirectory(filepath.Dir(marker.Executable)); err != nil {
				return err
			}
		} else if current != marker.PreviousSHA256 {
			return errors.New("upgrade executable changed unexpectedly; refusing recovery")
		}
		marker.RolledBack = true
		if err := saveUpgradeMarker(path, marker); err != nil {
			return err
		}
		service = marker.Service
		return nil
	})
	return
}

// RunUpgradeWatchdog is invoked by an independent transient systemd service
// using the old executable. It never reads or accepts the node credential.
func RunUpgradeWatchdog(path string) error {
	for {
		service, finished, err := recoverUpgrade(path, time.Now())
		if err != nil {
			return err
		}
		if finished {
			return nil
		}
		if service != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err = exec.CommandContext(ctx, "systemctl", "restart", service).Run()
			cancel()
			if err != nil {
				return fmt.Errorf("restart recovered node service: %w", err)
			}
			return withUpgradeLock(path, func() error {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
				return syncDirectory(filepath.Dir(path))
			})
		}
		time.Sleep(time.Second)
	}
}
