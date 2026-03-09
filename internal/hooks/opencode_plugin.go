package hooks

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// openCodePluginSource is the TypeScript plugin source for OpenCode.
// The TRANSITIVE_PORT env var tells the plugin which port to POST to.
const openCodePluginSource = `import type { Plugin } from "@opencode-ai/plugin";

const PORT = parseInt(process.env.TRANSITIVE_PORT || "7865", 10);
const HOOK_URL = ` + "`" + `http://localhost:${PORT}/hook` + "`" + `;
const TIMEOUT_MS = 5 * 60 * 1000; // 5 minutes

// For managed instances, pass the instance ID so the hook handler
// routes to the correct managed instance instead of creating a ghost.
const managedInstanceId = process.env.TRANSITIVE_INSTANCE_ID || "";

interface HookResponse {
  decision?: "allow" | "deny";
  reason?: string;
  [key: string]: any;
}

async function postHook(event: Record<string, any>): Promise<HookResponse> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);
  try {
    const resp = await fetch(HOOK_URL, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Claude-PID": String(process.pid),
        "X-Backend": "opencode",
        ...(managedInstanceId ? { "X-Instance-ID": managedInstanceId } : {}),
      },
      body: JSON.stringify(event),
      signal: controller.signal,
    });
    if (!resp.ok) {
      return { decision: "allow" };
    }
    const text = await resp.text();
    if (!text) return { decision: "allow" };
    return JSON.parse(text) as HookResponse;
  } catch {
    // Server unreachable — fail open so OpenCode isn't blocked.
    return { decision: "allow" };
  } finally {
    clearTimeout(timer);
  }
}

function fireAndForget(event: Record<string, any>): void {
  postHook(event).catch(() => {});
}

const plugin: Plugin = (ctx) => {
  // ctx.project is an object, not a string — derive a stable session ID.
  const sessionId = (ctx as any).project?.id || ctx.directory || "opencode";

  // Register this session on load.
  fireAndForget({
    session_id: sessionId,
    hook_event_name: "SessionStart",
    cwd: ctx.directory,
  });

  // For managed instances (server API), skip hooks that interfere with
  // the SSE/REST permission flow. Only attached (terminal) instances use hooks.
  const isManaged = !!managedInstanceId;

  return {
    "tool.execute.before": async (input, output) => {
      if (isManaged) return; // managed instances handle permissions via SSE
      if (input.tool === "background_process") return; // handled by custom tool
      const event = {
        session_id: input.sessionID || sessionId,
        hook_event_name: "PreToolUse",
        tool_name: input.tool,
        tool_input: output.args,
        tool_use_id: input.callID || "",
        cwd: ctx.directory,
      };
      const resp = await postHook(event);
      if (resp.decision === "deny") {
        throw new Error(resp.reason || "Denied by transitive");
      }
    },

    "tool.execute.after": async (input, output) => {
      if (isManaged) return; // managed instances get tool results via SSE
      if (input.tool === "background_process") return; // handled by custom tool
      fireAndForget({
        session_id: input.sessionID || sessionId,
        hook_event_name: "PostToolUse",
        tool_name: input.tool,
        tool_input: input.args,
        tool_response: output.output,
        tool_use_id: input.callID || "",
        cwd: ctx.directory,
      });
    },

    event: async (input) => {
      if (isManaged) return; // managed instances get session events via SSE
      const evt = input.event as any;
      const type = evt?.type || "";

      if (type === "session.created") {
        fireAndForget({
          session_id: evt.sessionID || sessionId,
          hook_event_name: "SessionStart",
          cwd: ctx.directory,
        });
      } else if (type === "session.idle") {
        fireAndForget({
          session_id: evt.sessionID || sessionId,
          hook_event_name: "Stop",
          cwd: ctx.directory,
        });
      }
    },

    "permission.ask": async (input, output) => {
      const perm = input as any;
      const toolName = perm.title || perm.type || "unknown";
      if (toolName === "background_process") {
        output.status = "allow"; // auto-allow for all instances; the custom tool handles execution
        return;
      }
      if (isManaged) return; // managed instances handle permissions via SSE
      const event = {
        session_id: perm.sessionID || sessionId,
        hook_event_name: "PreToolUse",
        tool_name: perm.title || perm.type || "unknown",
        tool_input: perm.metadata || {},
        tool_use_id: perm.callID || perm.id || "",
        cwd: ctx.directory,
      };
      const resp = await postHook(event);
      if (resp.decision === "deny") {
        output.status = "deny";
      } else if (resp.decision === "allow") {
        output.status = "allow";
      }
      // If no response or error, leave output.status as "ask" (default).
    },
  };
};

export default plugin;
`

// openCodeToolSource is the TypeScript custom tool for background process management.
// Placed in ~/.config/opencode/tools/ so OpenCode discovers it as a tool the agent can call.
const openCodeToolSource = `import { tool } from "@opencode-ai/plugin";

const PORT = parseInt(process.env.TRANSITIVE_PORT || "7865", 10);
const HOOK_URL = ` + "`" + `http://localhost:${PORT}/hook` + "`" + `;
const instanceId = process.env.TRANSITIVE_INSTANCE_ID || "";

export default tool({
  description: "Manage background processes. Use action 'start' with a command (and optional name/port), 'stop' or 'restart' with a process_id, or 'list' to see all processes.",
  args: {
    action: tool.schema.string().describe("Action to perform: start, stop, restart, or list"),
    command: tool.schema.string().optional().describe("Shell command to run (required for 'start')"),
    name: tool.schema.string().optional().describe("Display name for the process (optional, defaults to command)"),
    port: tool.schema.number().optional().describe("Port the process will listen on (optional)"),
    process_id: tool.schema.string().optional().describe("Process ID (required for 'stop' and 'restart')"),
  },
  async execute(args, context) {
    try {
      const resp = await fetch(HOOK_URL, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Backend": "opencode",
          ...(instanceId ? { "X-Instance-ID": instanceId } : {}),
        },
        body: JSON.stringify({
          session_id: context.sessionID || "",
          hook_event_name: "PreToolUse",
          tool_name: "background_process",
          tool_input: args,
          cwd: context.directory || "",
        }),
      });
      const data = await resp.json() as any;
      return JSON.stringify(data.toolResult || data, null, 2);
    } catch (err: any) {
      return JSON.stringify({ error: "Failed to reach transitive server: " + err.message });
    }
  },
});
`

// openCodeToolPath returns the path where the custom tool file is installed.
func openCodeToolPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "opencode", "tools", "background_process.ts")
}

// openCodePluginPath returns the path where the plugin file is installed.
func openCodePluginPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "opencode", "plugins", "transitive.ts")
}

// InstallOpenCode writes the TypeScript plugin and custom tool files.
func InstallOpenCode(port int) error {
	pluginPath := openCodePluginPath()
	if pluginPath == "" {
		return fmt.Errorf("could not determine home directory")
	}

	dir := filepath.Dir(pluginPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create plugin dir: %w", err)
	}

	if err := os.WriteFile(pluginPath, []byte(openCodePluginSource), 0o644); err != nil {
		return fmt.Errorf("write plugin: %w", err)
	}
	log.Printf("OpenCode plugin installed at %s", pluginPath)

	// Install background_process custom tool.
	toolPath := openCodeToolPath()
	if toolPath != "" {
		toolDir := filepath.Dir(toolPath)
		if err := os.MkdirAll(toolDir, 0o755); err != nil {
			return fmt.Errorf("create tool dir: %w", err)
		}
		if err := os.WriteFile(toolPath, []byte(openCodeToolSource), 0o644); err != nil {
			return fmt.Errorf("write tool: %w", err)
		}
		log.Printf("OpenCode custom tool installed at %s", toolPath)
	}

	return nil
}

// UninstallOpenCode removes the plugin and custom tool files.
func UninstallOpenCode() error {
	pluginPath := openCodePluginPath()
	if pluginPath == "" {
		return nil
	}
	if err := os.Remove(pluginPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plugin: %w", err)
	}
	log.Printf("OpenCode plugin removed from %s", pluginPath)

	toolPath := openCodeToolPath()
	if toolPath != "" {
		if err := os.Remove(toolPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove tool: %w", err)
		}
		log.Printf("OpenCode custom tool removed from %s", toolPath)
	}

	return nil
}

// OpenCodePluginInstalled reports whether the plugin file exists.
func OpenCodePluginInstalled() bool {
	p := openCodePluginPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
