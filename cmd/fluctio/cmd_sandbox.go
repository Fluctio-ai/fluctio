package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fluctio-ai/fluctio/internal/sandbox"
)

// sandboxCmd handles sandbox management subcommands.
func sandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Manage sandboxed execution environments",
	}
	cmd.AddCommand(sandboxCreateCmd())
	cmd.AddCommand(sandboxListCmd())
	cmd.AddCommand(sandboxConnectCmd())
	cmd.AddCommand(sandboxDestroyCmd())
	return cmd
}

// dockerOutput runs a docker command with a bounded lifetime (CLI docker
// calls have no other deadline — a hung daemon used to park the command
// forever) and returns stdout. stderr is passed through on failure.
func dockerOutput(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("docker %s: timed out after %s", strings.Join(args, " "), timeout)
	}
	return strings.TrimSpace(string(out)), err
}

// requireSandboxLabel verifies the target container carries the
// fluctio=sandbox label. connect/destroy take arbitrary ids from the
// command line — without this check `fluctio sandbox destroy <id>` could
// force-remove ANY container on the host, sandbox or not.
func requireSandboxLabel(containerID string) error {
	inspect, err := dockerOutput(10*time.Second, "inspect",
		"--format", "{{ index .Config.Labels \"fluctio\" }}", containerID)
	if err != nil {
		return fmt.Errorf("container %q not found or docker unavailable: %w", containerID, err)
	}
	if inspect != "sandbox" {
		return fmt.Errorf("container %q is not a fluctio sandbox (label fluctio=%q) — refusing to operate on it", containerID, inspect)
	}
	return nil
}

func sandboxCreateCmd() *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a standalone sandbox container",
		RunE: func(cmd *cobra.Command, args []string) error {
			sb := sandbox.NewDockerSandbox(image, "", nil)
			if err := sb.Create(); err != nil {
				return fmt.Errorf("create sandbox: %w", err)
			}
			fmt.Printf("Sandbox created: %s (image: %s)\n", sb.ContainerID(), image)
			return nil
		},
	}
	cmd.Flags().StringVar(&image, "image", "ghcr.io/fluctio-ai/fluctio-sandbox:latest", "Docker image to use")
	return cmd
}

func sandboxListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List running sandbox containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			listCmd := exec.Command("docker", "ps", "--filter", "label=fluctio=sandbox", "--format",
				"table {{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Names}}")
			listCmd.Stdout = os.Stdout
			listCmd.Stderr = os.Stderr
			return listCmd.Run()
		},
	}
}

func sandboxConnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect <container-id>",
		Short: "Exec into a sandbox container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireSandboxLabel(args[0]); err != nil {
				return err
			}
			execCmd := exec.Command("docker", "exec", "-it", args[0], "/bin/sh")
			execCmd.Stdin = os.Stdin
			execCmd.Stdout = os.Stdout
			execCmd.Stderr = os.Stderr
			return execCmd.Run()
		},
	}
}

func sandboxDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy <container-id>",
		Short: "Remove a sandbox container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireSandboxLabel(args[0]); err != nil {
				return err
			}
			rmCmd := exec.Command("docker", "rm", "-f", args[0])
			rmCmd.Stdout = os.Stdout
			rmCmd.Stderr = os.Stderr
			if err := rmCmd.Run(); err != nil {
				return err
			}
			fmt.Printf("Sandbox %s destroyed.\n", args[0])
			return nil
		},
	}
}
