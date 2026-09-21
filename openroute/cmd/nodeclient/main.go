// nodeclient runs the OpenRoute node service installed by /install.sh.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/openroute/openroute/internal/nodeclient"
)

var version = "dev"

func main() {
	var path, baseURL, token, watchdog string
	var check, showVersion bool
	flag.StringVar(&path, "config", "config.yml", "node YAML configuration")
	flag.StringVar(&path, "c", "config.yml", "node YAML configuration (alias)")
	flag.StringVar(&baseURL, "u", "", "panel URL (overrides configuration)")
	flag.StringVar(&token, "t", "", "node token (prefer storing in config.yml)")
	flag.BoolVar(&check, "check", false, "validate configuration and exit")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&watchdog, "upgrade-watchdog", "", "run independent upgrade recovery")
	flag.Parse()
	if watchdog != "" {
		if err := nodeclient.RunUpgradeWatchdog(watchdog); err != nil {
			log.Fatal(err)
		}
		return
	}
	if showVersion {
		fmt.Println(version)
		return
	}
	cfg, err := nodeclient.LoadConfig(path, baseURL, token)
	if err != nil {
		log.Fatal(err)
	}
	if check {
		fmt.Println("configuration valid")
		return
	}
	client, err := nodeclient.NewClient(cfg, version, log.Default())
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = client.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, nodeclient.ErrRestart) {
		log.Fatal(err)
	}
}
