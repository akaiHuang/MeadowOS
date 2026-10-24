package productauth

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hey-meadow/meadow-code/go/core/agentclient"
	coreauth "github.com/hey-meadow/meadow-code/go/core/productauth"
)

type cliClient interface {
	Refresh(context.Context) (coreauth.Status, error)
	Redeem(context.Context, string) (coreauth.RedeemResult, error)
	ListDevices(context.Context) (coreauth.DeviceListResult, error)
	RevokeDevice(context.Context, string, string) (coreauth.DeviceRevokeResult, error)
}

var newCLIClient = func() cliClient { return New() }

type cliAuthCommand interface {
	Run([]string, io.Writer, io.Writer) int
	Context() context.Context
	Close() error
}

var newCLIAuthCommand = func() cliAuthCommand { return agentclient.NewAuthCLICommand() }

func RunCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "login", "status", "logout":
		return agentclient.RunAuthCLI(args, stdout, stderr)
	case "refresh", "devices":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: meadow auth refresh")
			return 2
		}
	case "redeem", "activate-key":
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			fmt.Fprintln(stderr, "usage: meadow auth redeem <serial>")
			return 2
		}
	case "revoke-device":
		if len(args) != 5 || args[2] != "--reason" || strings.TrimSpace(args[3]) == "" || args[4] != "--confirm" {
			fmt.Fprintln(stderr, "usage: meadow auth revoke-device <device-id> --reason <reason> --confirm")
			return 2
		}
	default:
		fmt.Fprintf(stderr, "unknown auth command %q (use login|status|redeem|refresh|logout|devices|revoke-device)\n", args[0])
		return 2
	}
	authCommand := newCLIAuthCommand()
	defer authCommand.Close()
	if authCommand.Run([]string{"status"}, io.Discard, io.Discard) != 0 {
		fmt.Fprintln(stderr, "Meadow authentication service is unavailable")
		return 1
	}
	ctx, cancel := context.WithTimeout(authCommand.Context(), 2*time.Minute)
	defer cancel()
	client := newCLIClient()
	if args[0] == "refresh" {
		status, err := client.Refresh(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		PrintStatus(stdout, status)
		return 0
	}
	if args[0] == "devices" {
		result, err := client.ListDevices(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "devices: %d/%d\n", result.ActiveDevices, result.MaximumDevices)
		for _, device := range result.Devices {
			current := ""
			if device.Current {
				current = " current"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s%s\n", device.ID, device.Name, device.Platform, device.Status, current)
		}
		return 0
	}
	if args[0] == "revoke-device" {
		result, err := client.RevokeDevice(ctx, args[1], args[3])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "device: %s\nstatus: %s\nrequest: %s\n", result.Receipt.DeviceID, result.Receipt.Status, result.Receipt.RequestID)
		return 0
	}
	result, err := client.Redeem(ctx, args[1])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	PrintStatus(stdout, result.Availability)
	return 0
}
