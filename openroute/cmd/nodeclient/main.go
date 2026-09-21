// nodeclient runs the OpenRoute node service installed by /install.sh.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/openroute/openroute/internal/nodeclient"
	"github.com/openroute/openroute/internal/nodeproto"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

type commandOptions struct {
	path, baseURL, token, watchdog, writeConfig string
	check, showVersion, isOutbound              bool
	network                                     nodeproto.NodeNetworkConfig
	overrides                                   nodeclient.ConfigOverrides
}

func parseOptions(args []string, output io.Writer) (commandOptions, error) {
	var opts commandOptions
	flags := flag.NewFlagSet("rel_nodeclient", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.path, "config", "config.yml", "node YAML configuration")
	flags.StringVar(&opts.path, "c", "config.yml", "node YAML configuration (alias)")
	flags.StringVar(&opts.baseURL, "u", "", "panel URL (overrides configuration)")
	flags.StringVar(&opts.token, "t", "", "node token (prefer storing in config.yml)")
	flags.BoolVar(&opts.check, "check", false, "validate configuration and exit")
	flags.BoolVar(&opts.showVersion, "version", false, "print version and exit")
	flags.StringVar(&opts.watchdog, "upgrade-watchdog", "", "run independent upgrade recovery")
	flags.StringVar(&opts.writeConfig, "write-config", "", "merge configuration into this file and exit without contacting the panel")
	flags.BoolVar(&opts.isOutbound, "is-outbound", false, "preserve installer role in --write-config mode (runtime role comes from panel)")
	flags.StringVar(&opts.network.ConnectHost, "connect-host", "", "IP or hostname advertised to peers (empty inherits the panel address)")
	ports := map[string]*int{
		"direct-port": &opts.network.DirectPort, "ws-port": &opts.network.WsPort, "tls-port": &opts.network.TlsPort,
		"udp-port": &opts.network.UdpPort, "rev-port": &opts.network.RevPort,
		"connect-direct-port": &opts.network.ConnectDirectPort, "connect-ws-port": &opts.network.ConnectWsPort,
		"connect-tls-port": &opts.network.ConnectTlsPort, "connect-udp-port": &opts.network.ConnectUdpPort,
		"connect-rev-port": &opts.network.ConnectRevPort,
	}
	for name, value := range ports {
		flags.Func(name, "decimal local listening or advertised connection port; 0 inherits the panel/default", func(raw string) error {
			port, err := strconv.Atoi(raw)
			if err != nil {
				return errors.New("port must be a decimal integer")
			}
			*value = port
			return nil
		})
	}
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, errors.New("unexpected positional node arguments")
	}
	opts.overrides = make(nodeclient.ConfigOverrides)
	invalidWritePath := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "write-config" && strings.TrimSpace(opts.writeConfig) == "" {
			invalidWritePath = true
		}
		if value, ok := ports[f.Name]; ok {
			opts.overrides[f.Name] = *value
		} else if f.Name == "connect-host" {
			opts.overrides[f.Name] = opts.network.ConnectHost
		} else if f.Name == "is-outbound" {
			opts.overrides[f.Name] = opts.isOutbound
		}
	})
	if invalidWritePath {
		return opts, errors.New("--write-config requires a nonempty output path")
	}
	if _, supplied := opts.overrides["is-outbound"]; supplied && opts.writeConfig == "" {
		return opts, errors.New("--is-outbound requires --write-config; the panel controls the runtime role")
	}
	return opts, nil
}

func run(args []string, output io.Writer) error {
	opts, err := parseOptions(args, output)
	if err != nil {
		return err
	}
	if opts.watchdog != "" {
		return nodeclient.RunUpgradeWatchdog(opts.watchdog)
	}
	if opts.showVersion {
		fmt.Fprintln(output, version)
		return nil
	}
	if opts.writeConfig != "" {
		return nodeclient.WriteConfig(opts.path, opts.writeConfig, opts.baseURL, opts.token, opts.overrides)
	}
	cfg, err := nodeclient.LoadConfigWithOverrides(opts.path, opts.baseURL, opts.token, opts.overrides)
	if err != nil {
		return err
	}
	if opts.check {
		fmt.Fprintln(output, "configuration valid")
		return nil
	}
	client, err := nodeclient.NewClient(cfg, version, log.Default())
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = client.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, nodeclient.ErrRestart) {
		return err
	}
	return nil
}
