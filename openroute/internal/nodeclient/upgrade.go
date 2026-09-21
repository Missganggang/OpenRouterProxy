package nodeclient

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

func (c *Client) installUpgrade(parent context.Context, task nodeproto.TaskItem) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("self-upgrade requires Linux")
	}
	if !strings.HasPrefix(c.config.BaseURL, "https://") {
		return "", errors.New("self-upgrade requires an HTTPS panel URL")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	version, _ := task.Payload["version"].(string)
	if version == "latest" {
		version = ""
	}
	path := "/api/node/binary/" + runtime.GOARCH
	if version != "" {
		path += "?version=" + url.QueryEscape(version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.BaseURL+path, nil)
	if err != nil {
		return "", err
	}
	// The download route is public. Do not send the node credential to an artifact endpoint.
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: c.http.Transport}
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upgrade download HTTP %d", response.StatusCode)
	}
	wantHash := strings.ToLower(strings.TrimSpace(response.Header.Get("X-Checksum-SHA256")))
	wantVersion := strings.TrimSpace(response.Header.Get("X-Node-Version"))
	decoded, err := hex.DecodeString(wantHash)
	if err != nil || len(decoded) != sha256.Size || wantVersion == "" {
		return "", errors.New("upgrade response lacks SHA256 or version metadata")
	}
	if version != "" && version != wantVersion {
		return "", errors.New("upgrade version differs from requested version")
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	staged, err := os.CreateTemp(filepath.Dir(executable), ".node-upgrade-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(staged.Name())
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(staged, hash), io.LimitReader(response.Body, 128<<20+1))
	if err == nil && n > 128<<20 {
		err = errors.New("upgrade binary exceeds 128 MiB")
	}
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != wantHash {
		err = errors.New("upgrade SHA256 mismatch")
	}
	if err == nil {
		err = staged.Chmod(0755)
	}
	if err == nil {
		err = staged.Sync()
	}
	if closeErr := staged.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := verifyUpgrade(ctx, staged.Name(), wantVersion, c.config.ConfigPath); err != nil {
		return "", err
	}
	stopWatchdog, err := c.armUpgradeWatchdog(ctx, executable, wantVersion, wantHash)
	if err != nil {
		return "", err
	}
	if err := os.Rename(staged.Name(), executable); err != nil {
		stopWatchdog()
		return "", err
	}
	if err := syncDirectory(filepath.Dir(executable)); err != nil {
		return "", err
	}
	return wantVersion, nil
}

func verifyUpgrade(ctx context.Context, path, version, configPath string) error {
	binary, err := elf.Open(path)
	if err != nil {
		return errors.New("upgrade is not a valid ELF executable")
	}
	defer binary.Close()
	wantMachine := elf.EM_NONE
	switch runtime.GOARCH {
	case "amd64":
		wantMachine = elf.EM_X86_64
	case "arm64":
		wantMachine = elf.EM_AARCH64
	}
	if binary.Machine != wantMachine || binary.Class != elf.ELFCLASS64 || binary.Data != elf.ELFDATA2LSB {
		return errors.New("upgrade executable architecture mismatch")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var output boundedOutput
	cmd := exec.CommandContext(checkCtx, path, "-version")
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("upgrade executable self-check failed: %w", err)
	}
	if strings.TrimSpace(output.String()) != version {
		return errors.New("upgrade executable version does not match metadata")
	}
	if configPath != "" {
		cmd = exec.CommandContext(checkCtx, path, "-c", configPath, "-check")
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("upgrade configuration self-check failed: %w", err)
		}
	}
	return nil
}

func replaceExecutable(current, staged string) error {
	if err := backupExecutable(current); err != nil {
		return err
	}
	if err := os.Rename(staged, current); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(current))
}

func backupExecutable(current string) error {
	old, err := os.Open(current)
	if err != nil {
		return err
	}
	defer old.Close()
	backup, err := os.CreateTemp(filepath.Dir(current), ".node-previous-*")
	if err != nil {
		return err
	}
	defer os.Remove(backup.Name())
	_, err = io.Copy(backup, old)
	if err == nil {
		err = backup.Chmod(0755)
	}
	if err == nil {
		err = backup.Sync()
	}
	if closeErr := backup.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(backup.Name(), current+".previous"); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(current))
}
