package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

func TestAnonymousInstallerDoesNotRequireDatabaseOrPublishToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// A nil app/DB deliberately detects any attempt to select a default node.
	router.GET("/install.sh", (&Registry{}).InstallScriptHandler)
	request := httptest.NewRequest(http.MethodGet, "https://panel.example/install.sh", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "NODE_TOKEN=''\n") || strings.Contains(body, "nsk_") {
		t.Fatal("anonymous installer must have an empty token default")
	}
	if !strings.Contains(body, "PANEL_URL='https://panel.example'") {
		t.Fatal("installer did not infer the panel URL")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("installer should not be cached, including token-bearing variants")
	}
}

type installerHarness struct {
	t       *testing.T
	bash    string
	root    string
	script  string
	env     []string
	optPath string
	unitDir string
}

func newInstallerHarness(t *testing.T, options InstallOptions) *installerHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("installer integration tests require a Unix bash environment")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required for installer integration tests")
	}
	root := t.TempDir()
	h := &installerHarness{t: t, bash: bash, root: root, optPath: filepath.Join(root, "opt"), unitDir: filepath.Join(root, "systemd")}
	bin := filepath.Join(root, "bin")
	for _, path := range []string{bin, h.optPath, h.unitDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	h.write(filepath.Join(root, "os-release"), "ID=debian\nVERSION_ID=11\nPRETTY_NAME=Debian\n", 0600)
	// Replace only host filesystem paths. Keep parameter parsing, shell quoting,
	// staged downloads, validation, and generated unit/uninstaller untouched.
	script := strings.NewReplacer(
		"/opt/", h.optPath+"/",
		"/etc/systemd/system", h.unitDir,
		"/etc/os-release", filepath.Join(root, "os-release"),
	).Replace(InstallScript(options))
	h.script = filepath.Join(root, "install.sh")
	h.write(h.script, script, 0700)
	h.write(filepath.Join(bin, "id"), "#!/bin/sh\necho 0\n", 0700)
	h.write(filepath.Join(bin, "uname"), "#!/bin/sh\necho x86_64\n", 0700)
	h.write(filepath.Join(bin, "sleep"), "#!/bin/sh\nexit 0\n", 0700)
	h.write(filepath.Join(bin, "curl"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output="$2"; shift 2 ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\n' "$url" >> "$TEST_ROOT/curl.log"
[ "${TEST_DOWNLOAD_FAIL:-0}" = 0 ] || exit 22
cp "$TEST_ROOT/client" "$output"
`, 0700)
	h.write(filepath.Join(root, "client"), `#!/bin/sh
case "$1" in
  -h) exit "${TEST_HELP_EXIT:-0}" ;;
  -c)
    if [ "$3" = -check ]; then
      [ -s "$2" ] || exit 3
      exit "${TEST_CHECK_EXIT:-0}"
    fi
    INSTALLER_CLIENT_HELPER=1 "$INSTALLER_TEST_BINARY" -test.run='^TestInstallerClientHelper$' -- "$@"
    ;;
  *) exit 2 ;;
esac
`, 0700)
	h.write(filepath.Join(bin, "systemctl"), `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_ROOT/systemctl.log"
case "$1" in
  show-environment) exit "${TEST_SYSTEMD_EXIT:-0}" ;;
  is-active) exit "${TEST_ACTIVE_EXIT:-0}" ;;
esac
exit 0
`, 0700)
	// Filter installer knobs so the developer's shell cannot alter a test run.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "PATH", "S", "UUID", "OPTIMIZE", "INSTALL_TOOLS", "BIND_INBOUND", "BIND_OUTBOUND_4", "BIND_OUTBOUND_6", "OUTBOUND_FWMARK", "TUNNEL_BIND_INBOUND", "TUNNEL_BIND_OUTBOUND_4", "TUNNEL_BIND_OUTBOUND_6", "TUNNEL_FWMARK", "TUNNEL_INTERFACE", "COUNT_INTERFACE", "DISABLE_EXECUTE", "HEALTH_CHECK", "INSTALLER_CLIENT_HELPER", "INSTALLER_TEST_BINARY":
			continue
		}
		if !strings.HasPrefix(name, "TEST_") {
			h.env = append(h.env, entry)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h.env = append(h.env, "PATH="+bin+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "INSTALLER_TEST_BINARY="+executable)
	return h
}

// The fake downloaded client checks the installer's merge contract. YAML merge
// and network validation themselves are covered by nodeclient's runtime tests.
func TestInstallerClientHelper(t *testing.T) {
	if os.Getenv("INSTALLER_CLIENT_HELPER") != "1" {
		return
	}
	if os.Getenv("TEST_WRITE_EXIT") != "" {
		os.Exit(9)
	}
	args := []string{}
	for i, value := range os.Args {
		if value == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	logged, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("TEST_ROOT"), "config-call.json"), logged, 0600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for len(args) > 0 {
		key := args[0]
		args = args[1:]
		if k, v, ok := strings.Cut(key, "="); ok {
			values[strings.TrimLeft(k, "-")] = v
		} else {
			if len(args) == 0 {
				t.Fatalf("missing value for %s", key)
			}
			values[strings.TrimLeft(key, "-")] = args[0]
			args = args[1:]
		}
	}
	config := map[string]interface{}{}
	if data, err := os.ReadFile(values["c"]); err == nil {
		if err := yaml.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	config["base-url"], config["token"] = values["u"], values["t"]
	for key, value := range values {
		switch {
		case key == "connect-host":
			config[key] = value
		case key == "is-outbound":
			config[key] = value == "true"
		case strings.HasSuffix(key, "-port"):
			port, err := strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}
			config[key] = port
		}
	}
	if id := os.Getenv("UUID"); id != "" {
		config["machine-id"] = id
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(values["write-config"], data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (h *installerHarness) write(path, contents string, mode os.FileMode) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		h.t.Fatal(err)
	}
}

func (h *installerHarness) read(path string) string {
	h.t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(contents)
}

func (h *installerHarness) run(args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command(h.bash, append([]string{h.script}, args...)...)
	cmd.Env = h.env
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestInstallerArgumentsIsolationAndUninstall(t *testing.T) {
	h := newInstallerHarness(t, InstallOptions{BaseURL: "https://old.example", Token: "old-token", ServiceName: "old-service", Arch: "arm64"})
	panelConfig := filepath.Join(h.optPath, "openroute", "config.yml")
	h.write(panelConfig, "panel: must-survive\n", 0600)
	installDir := filepath.Join(h.optPath, "integration-node")
	envPath := filepath.Join(installDir, NodeEnvFileName)
	h.write(envPath, "CUSTOM_SETTING=preserve\n", 0600)
	h.env = append(h.env, "S=env-service", "BIND_INBOUND=override-must-not-win")
	marker := filepath.Join(h.root, "must-not-exist")
	token := "nsk_'\"$(touch " + marker + ")\\test"
	output, err := h.run("-u", "https://panel.example/", "-t", token, "-s", "integration", "-o", "1", "-a", "amd64v3", "-v", "v1.2.3")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	var config NodeConfig
	if err := yaml.Unmarshal([]byte(h.read(filepath.Join(installDir, NodeConfigFileName))), &config); err != nil {
		t.Fatal(err)
	}
	if config.BaseURL != "https://panel.example" || config.Token != token || !config.IsOutbound {
		t.Fatalf("CLI values did not survive rendering/parsing: %#v", config)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("token was executed as shell syntax")
	}
	if got := h.read(filepath.Join(h.root, "curl.log")); got != "https://panel.example/api/node/binary/amd64v3?version=v1.2.3\n" {
		t.Fatalf("incorrect download URL: %q", got)
	}
	if got := h.read(envPath); got != "CUSTOM_SETTING=preserve\n" {
		t.Fatalf("existing environment overwritten: %q", got)
	}
	unit := h.read(filepath.Join(h.unitDir, "integration-node.service"))
	if !strings.Contains(unit, "ExecStart="+filepath.Join(installDir, NodeBinaryName)+" -c "+filepath.Join(installDir, NodeConfigFileName)) || strings.Contains(unit, token) {
		t.Fatalf("incorrect or secret-bearing unit: %s", unit)
	}
	if h.read(panelConfig) != "panel: must-survive\n" {
		t.Fatal("panel config was overwritten")
	}
	cmd := exec.Command(h.bash, filepath.Join(installDir, NodeUninstallFileName))
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader("y\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(installDir); !os.IsNotExist(err) {
		t.Fatal("uninstall did not remove the selected node directory")
	}
	if h.read(panelConfig) != "panel: must-survive\n" {
		t.Fatal("uninstaller changed the panel config")
	}
	if _, err := os.Stat(filepath.Join(h.unitDir, "integration-node.service")); !os.IsNotExist(err) {
		t.Fatal("uninstaller did not remove the selected unit")
	}
}

func TestInstallerSafeTemplateDefaultsAndAutomaticArchitecture(t *testing.T) {
	h := newInstallerHarness(t, InstallOptions{BaseURL: "https://panel.example", Token: "placeholder"})
	marker := filepath.Join(h.root, "injection")
	token := "nsk_'$(touch " + marker + ")"
	bindValue := "address with \"quotes\"\\$(touch " + marker + ")`touch " + marker + "`"
	// Re-render with shell metacharacters directly in the template defaults.
	h.write(h.script, strings.NewReplacer("/opt/", h.optPath+"/", "/etc/systemd/system", h.unitDir, "/etc/os-release", filepath.Join(h.root, "os-release")).Replace(InstallScript(InstallOptions{BaseURL: "https://panel.example", Token: token})), 0700)
	h.env = append(h.env, "BIND_INBOUND="+bindValue, "COUNT_INTERFACE=eth0", "DISABLE_EXECUTE=1",
		"TUNNEL_BIND_INBOUND=10.0.0.2", "TUNNEL_BIND_OUTBOUND_4=10.0.0.2", "TUNNEL_BIND_OUTBOUND_6=fd00::2", "TUNNEL_FWMARK=0x10", "TUNNEL_INTERFACE=eth1")
	if output, err := h.run(); err != nil {
		t.Fatalf("install failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("template default executed as shell syntax")
	}
	if got := h.read(filepath.Join(h.root, "curl.log")); got != "https://panel.example/api/node/binary/amd64\n" {
		t.Fatalf("auto should use baseline amd64: %q", got)
	}
	installDir := filepath.Join(h.optPath, "openroute-node")
	var config NodeConfig
	if err := yaml.Unmarshal([]byte(h.read(filepath.Join(installDir, NodeConfigFileName))), &config); err != nil || config.Token != token {
		t.Fatalf("invalid token escaping: %#v (%v)", config, err)
	}
	env := h.read(filepath.Join(installDir, NodeEnvFileName))
	for _, expected := range []string{"COUNT_INTERFACE=\"eth0\"", "DISABLE_EXECUTE=\"1\"", "TUNNEL_BIND_INBOUND=\"10.0.0.2\"", "TUNNEL_BIND_OUTBOUND_4=\"10.0.0.2\"", "TUNNEL_BIND_OUTBOUND_6=\"fd00::2\"", "TUNNEL_FWMARK=\"0x10\"", "TUNNEL_INTERFACE=\"eth1\""} {
		if !strings.Contains(env, expected) {
			t.Fatalf("missing environment setting %q: %s", expected, env)
		}
	}
	// env.sh can also be safely sourced by an operator; systemd's double-quote
	// rules support these same escapes without performing shell expansion.
	cmd := exec.Command(h.bash, "-c", `. "$1"; printf '%s' "$BIND_INBOUND"`, "--", filepath.Join(installDir, NodeEnvFileName))
	cmd.Env = h.env
	if output, err := cmd.CombinedOutput(); err != nil || string(output) != bindValue {
		t.Fatalf("environment value changed or executed while sourcing: %v, %q", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("environment value was executed as shell syntax")
	}
}

func TestInstallerNetworkOverridesAndReinstallPreservesYAML(t *testing.T) {
	h := newInstallerHarness(t, InstallOptions{BaseURL: "https://panel.example", Token: "new-token"})
	configPath := filepath.Join(h.optPath, "openroute-node", NodeConfigFileName)
	h.write(configPath, "base-url: https://old.example\ntoken: old-token\nconnect-host: 10.0.0.2\ndirect-port: 21000\nconnect-direct-port: 31000\nis-outbound: true\nmachine-id: existing-yaml-identity\ncustom-network:\n  route: private-route\n", 0600)
	if output, err := h.run(); err != nil {
		t.Fatalf("reinstall failed: %v\n%s", err, output)
	}
	readConfig := func() map[string]interface{} {
		t.Helper()
		var config map[string]interface{}
		if err := yaml.Unmarshal([]byte(h.read(configPath)), &config); err != nil {
			t.Fatal(err)
		}
		return config
	}
	first := readConfig()
	if first["connect-host"] != "10.0.0.2" || first["direct-port"] != 21000 || first["connect-direct-port"] != 31000 || first["is-outbound"] != true || first["machine-id"] != "existing-yaml-identity" || first["token"] != "new-token" {
		t.Fatalf("reinstall reset existing configuration: %#v", first)
	}
	if custom, ok := first["custom-network"].(map[string]interface{}); !ok || custom["route"] != "private-route" {
		t.Fatalf("unknown YAML keys were not preserved: %#v", first)
	}
	if output, err := h.run("--connect-host=", "--direct-port", "0", "--ws-port=00123", "--tls-port", "22002", "--udp-port=22003", "--rev-port", "22004", "--connect-direct-port=32000", "--connect-ws-port", "32001", "--connect-tls-port=32002", "--connect-udp-port", "32003", "--connect-rev-port=32004"); err != nil {
		t.Fatalf("network overrides failed: %v\n%s", err, output)
	}
	second := readConfig()
	want := map[string]interface{}{"connect-host": "", "direct-port": 0, "ws-port": 123, "tls-port": 22002, "udp-port": 22003, "rev-port": 22004,
		"connect-direct-port": 32000, "connect-ws-port": 32001, "connect-tls-port": 32002, "connect-udp-port": 32003, "connect-rev-port": 32004,
		"is-outbound": true, "machine-id": "existing-yaml-identity"}
	for key, value := range want {
		if second[key] != value {
			t.Errorf("%s=%#v want %#v", key, second[key], value)
		}
	}
	var args []string
	if err := json.Unmarshal([]byte(h.read(filepath.Join(h.root, "config-call.json"))), &args); err != nil {
		t.Fatal(err)
	}
	if len(args) < 2 || args[0] != "-c" || args[1] != configPath {
		t.Fatalf("merge did not receive existing source path: %q", args)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--is-outbound=") {
			t.Fatal("implicit -o overwrote existing YAML role")
		}
	}
	if !strings.Contains(strings.Join(args, "\n"), "--ws-port=123\n") {
		t.Fatalf("port with leading zeros was not normalized to decimal: %q", args)
	}
}

func TestInstallerFailuresDoNotReportSuccessOrReplaceOldClient(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"download", "TEST_DOWNLOAD_FAIL=1"},
		{"incompatible_binary", "TEST_HELP_EXIT=126"},
		{"invalid_config", "TEST_CHECK_EXIT=1"},
		{"merge_failed", "TEST_WRITE_EXIT=1"},
		{"systemd_unavailable", "TEST_SYSTEMD_EXIT=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newInstallerHarness(t, InstallOptions{BaseURL: "https://panel.example", Token: "test-token"})
			installDir := filepath.Join(h.optPath, "openroute-node")
			const originalConfig = "base-url: https://old.example\ntoken: existing-token\n"
			h.write(filepath.Join(installDir, NodeBinaryName), "old-client", 0700)
			h.write(filepath.Join(installDir, NodeConfigFileName), originalConfig, 0600)
			h.env = append(h.env, tc.env)
			output, err := h.run()
			if err == nil || strings.Contains(output, "节点安装完成") {
				t.Fatalf("installer claimed success: %v\n%s", err, output)
			}
			if h.read(filepath.Join(installDir, NodeBinaryName)) != "old-client" || h.read(filepath.Join(installDir, NodeConfigFileName)) != originalConfig {
				t.Fatal("failed install replaced previous working files")
			}
			entries, err := os.ReadDir(installDir)
			if err != nil || len(entries) != 2 {
				t.Fatalf("temporary download/config files leaked: %v, %v", entries, err)
			}
		})
	}
}

func TestInstallerInactiveServiceFails(t *testing.T) {
	h := newInstallerHarness(t, InstallOptions{BaseURL: "https://panel.example", Token: "test-token"})
	h.env = append(h.env, "TEST_ACTIVE_EXIT=3")
	output, err := h.run()
	if err == nil || strings.Contains(output, "节点安装完成") || !strings.Contains(output, "服务未能正常运行") {
		t.Fatalf("inactive service did not fail installation: %v\n%s", err, output)
	}
}

func TestInstallerRejectsInvalidOrMissingArguments(t *testing.T) {
	for _, args := range [][]string{
		{}, {"-t", "token", "-s", "../openroute"}, {"-t", "token", "-a", "unsupported"},
		{"-t", "token", "-o", "maybe"}, {"-t"}, {"-x"},
		{"-t", "token", "--connect-host"}, {"-t", "token", "--unknown=value"},
		{"-t", "token", "--direct-port=65536"}, {"-t", "token", "--ws-port=-1"},
		{"-t", "token", "--connect-tls-port", "oops"}, {"-t", "token", "--udp-port="},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			h := newInstallerHarness(t, InstallOptions{BaseURL: "https://panel.example"})
			if output, err := h.run(args...); err == nil {
				t.Fatalf("invalid input succeeded: %s", output)
			}
			if _, err := os.Stat(filepath.Join(h.root, "curl.log")); !os.IsNotExist(err) {
				t.Fatal("installer downloaded a binary before validating input")
			}
		})
	}
}
