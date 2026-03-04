package cmd

import (
	"fmt"
	"os"

	"github.com/nextlevelbuilder/goclaw/pkg/config"
)

// isManagedMode returns true if the config specifies managed (Postgres) mode.
func isManagedMode() bool {
	cfg, err := config.Load(resolveConfigPath())
	if err != nil {
		return false
	}
	return cfg.Database.Mode == "managed" && cfg.Database.PostgresDSN != ""
}

// requireGatewayForManaged exits with a helpful error if managed mode is active
// and the gateway is not reachable.
func requireGatewayForManaged() {
	if !isManagedMode() {
		return
	}
	if !isGatewayReachable() {
		fmt.Fprintln(os.Stderr, "Error: managed mode requires the gateway to be running.")
		fmt.Fprintln(os.Stderr, "Start it first:  goclaw")
		os.Exit(1)
	}
}

// isGatewayReachable tries a quick RPC ping to check if the gateway is up.
func isGatewayReachable() bool {
	_, err := gatewayRPC("ping", nil)
	// Any response (even error) means the gateway is up.
	// Only connection failure means it's down.
	return err == nil
}
