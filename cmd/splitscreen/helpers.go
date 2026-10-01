package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/analystaio/splitscreen/internal/mcpstdio"
	"github.com/analystaio/splitscreen/internal/runner"
)

func socketFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "socket", os.Getenv("SPLITSCREEN_SOCKET"),
		"path to the local runner socket")
}

// ---------------------------------------------------------------------------
// git credential helper
// ---------------------------------------------------------------------------

func credentialHelperCmd() *cobra.Command {
	var socket string

	cmd := &cobra.Command{
		Use:   "credential-helper [get|store|erase]",
		Short: "Git credential helper backed by the gateway",
		Long: `Answers git's credential protocol by asking the gateway to mint a token
scoped to the repository being accessed.

Git invokes credential helpers with the protocol, host, and path of the target
repository. That is what makes per-repository scoping enforceable: the gateway
mints a credential valid for one repository, so a runner cannot reach anything
outside its policy regardless of what the agent attempts.

Wire it up with:

    git config --global credential.helper '!splitscreen credential-helper'
    git config --global credential.useHttpPath true`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			op := "get"
			if len(args) > 0 {
				op = args[0]
			}
			// Tokens are short-lived and minted per operation, so there is
			// nothing to store and nothing to erase.
			if op != "get" {
				return nil
			}

			attrs, err := readCredentialAttrs(os.Stdin)
			if err != nil {
				return err
			}
			// useHttpPath must be on for this to be populated; without it the
			// gateway cannot tell which repository is being reached.
			repo := repoFromPath(attrs["path"])
			if repo == "" {
				return fmt.Errorf(
					"splitscreen: git did not supply a repository path; enable it with `git config credential.useHttpPath true`")
			}

			resp, err := runner.CallIPC(socket, runner.IPCRequest{
				Op:       runner.OpCredential,
				Resource: repo,
			}, 60*time.Second)
			if err != nil {
				return err
			}
			if !resp.OK {
				return fmt.Errorf("splitscreen: %s", resp.Error)
			}

			var cred runner.CredentialResult
			if err := json.Unmarshal(resp.Data, &cred); err != nil {
				return err
			}
			out := bufio.NewWriter(os.Stdout)
			fmt.Fprintf(out, "username=%s\n", cred.Username)
			fmt.Fprintf(out, "password=%s\n", cred.Password)
			return out.Flush()
		},
	}
	socketFlag(cmd, &socket)
	return cmd
}

// repoFromPath normalizes git's path attribute into an "owner/name" reference.
// Git supplies it with or without a leading slash and with or without a .git
// suffix depending on the remote URL, and the gateway matches policy against
// the normalized form.
func repoFromPath(path string) string {
	return strings.Trim(strings.TrimSuffix(strings.TrimSpace(path), ".git"), "/")
}

func readCredentialAttrs(r io.Reader) (map[string]string, error) {
	attrs := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			attrs[k] = v
		}
	}
	return attrs, sc.Err()
}

// ---------------------------------------------------------------------------
// permission prompt server
// ---------------------------------------------------------------------------

type permissionHandler struct {
	socket string
	thread string
}

func (h *permissionHandler) Tools(context.Context) ([]mcpstdio.Tool, error) {
	schema := json.RawMessage(`{
      "type": "object",
      "properties": {
        "tool_name":   {"type": "string"},
        "input":       {"type": "object"},
        "tool_use_id": {"type": "string"}
      },
      "required": ["tool_name", "input"]
    }`)
	return []mcpstdio.Tool{{
		Name:        "permission_prompt",
		Description: "Ask the operators whether this tool call may proceed.",
		InputSchema: schema,
	}}, nil
}

func (h *permissionHandler) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if name != "permission_prompt" {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	var params struct {
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, err
	}

	resp, err := runner.CallIPC(h.socket, runner.IPCRequest{
		Op:        runner.OpPermission,
		Thread:    h.thread,
		Tool:      params.ToolName,
		Input:     params.Input,
		ToolUseID: params.ToolUseID,
	}, 20*time.Minute)
	if err != nil {
		// Failing closed is the only safe direction: a runner that cannot reach
		// the gateway must not approve on its behalf.
		return mcpstdio.JSONResult(map[string]any{
			"behavior": "deny",
			"message":  "could not reach the control plane: " + err.Error(),
		})
	}
	if !resp.OK {
		return mcpstdio.JSONResult(map[string]any{"behavior": "deny", "message": resp.Error})
	}

	var decision runner.PermissionResult
	if err := json.Unmarshal(resp.Data, &decision); err != nil {
		return mcpstdio.JSONResult(map[string]any{"behavior": "deny", "message": "malformed decision"})
	}
	if decision.Behavior == "allow" {
		return mcpstdio.JSONResult(map[string]any{
			"behavior":     "allow",
			"updatedInput": json.RawMessage(params.Input),
		})
	}
	return mcpstdio.JSONResult(map[string]any{
		"behavior": "deny",
		"message":  decision.Message,
	})
}

func permissionShimCmd() *cobra.Command {
	var socket, thread string
	cmd := &cobra.Command{
		Use:    "permission-shim",
		Short:  "MCP server that routes permission decisions to the gateway",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return mcpstdio.ServeStdio(cmd.Context(), "splitscreen",
				&permissionHandler{socket: socket, thread: thread})
		},
	}
	socketFlag(cmd, &socket)
	cmd.Flags().StringVar(&thread, "thread", "", "thread this session belongs to")
	return cmd
}

// ---------------------------------------------------------------------------
// proxied MCP shim
// ---------------------------------------------------------------------------

type proxyHandler struct {
	socket string
	thread string
	server string
}

func (h *proxyHandler) Tools(ctx context.Context) ([]mcpstdio.Tool, error) {
	resp, err := runner.CallIPC(h.socket, runner.IPCRequest{
		Op:     runner.OpMCPList,
		Thread: h.thread,
		Server: h.server,
	}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	// The upstream reply is an MCP tools/list result; unwrap it if present so
	// the harness sees the real tool set rather than an envelope.
	var listed struct {
		Tools []mcpstdio.Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp.Data, &listed); err == nil && len(listed.Tools) > 0 {
		return listed.Tools, nil
	}
	return nil, nil
}

func (h *proxyHandler) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	resp, err := runner.CallIPC(h.socket, runner.IPCRequest{
		Op:     runner.OpMCPCall,
		Thread: h.thread,
		Server: h.server,
		Tool:   name,
		Args:   args,
	}, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Data, nil
}

func mcpShimCmd() *cobra.Command {
	var socket, thread, server string
	cmd := &cobra.Command{
		Use:    "mcp-shim",
		Short:  "MCP server that forwards to a gateway-held credentialed server",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server == "" {
				return fmt.Errorf("splitscreen: --server is required")
			}
			return mcpstdio.ServeStdio(cmd.Context(), "splitscreen-"+server,
				&proxyHandler{socket: socket, thread: thread, server: server})
		},
	}
	socketFlag(cmd, &socket)
	cmd.Flags().StringVar(&thread, "thread", "", "thread this session belongs to")
	cmd.Flags().StringVar(&server, "server", "", "proxied server name")
	return cmd
}

// ---------------------------------------------------------------------------
// send-file
// ---------------------------------------------------------------------------

func sendFileCmd() *cobra.Command {
	var socket, thread, comment string
	cmd := &cobra.Command{
		Use:   "send-file <path>",
		Short: "Send a local file to the chat surface",
		Long: `Streams a file to the surface through the gateway.

The runner holds no chat credentials, so this cannot post directly — which is
also what makes the transfer auditable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if thread == "" {
				thread = os.Getenv("SPLITSCREEN_THREAD")
			}
			resp, err := runner.CallIPC(socket, runner.IPCRequest{
				Op:      runner.OpSendFile,
				Thread:  thread,
				Path:    args[0],
				Comment: comment,
			}, 5*time.Minute)
			if err != nil {
				return err
			}
			if !resp.OK {
				return fmt.Errorf("splitscreen: %s", resp.Error)
			}
			fmt.Println("sent")
			return nil
		},
	}
	socketFlag(cmd, &socket)
	cmd.Flags().StringVar(&thread, "thread", "", "thread to deliver into")
	cmd.Flags().StringVar(&comment, "comment", "", "message to accompany the file")
	return cmd
}
