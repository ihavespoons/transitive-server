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

  // Register the background_process tool.
  if ((ctx as any).registerTool) {
    (ctx as any).registerTool({
      name: "background_process",
      description: "Start, stop, restart, or list background processes (web servers, watchers, etc.)",
      parameters: {
        type: "object",
        properties: {
          action: { type: "string", enum: ["start", "stop", "restart", "list"], description: "Action to perform" },
          command: { type: "string", description: "Shell command to run (for start)" },
          name: { type: "string", description: "Human-readable name (for start)" },
          process_id: { type: "string", description: "Process ID (for stop/restart)" },
          port: { type: "number", description: "Port the process listens on (optional, for start)" },
        },
        required: ["action"],
      },
      execute: async (args: any) => {
        const event = {
          session_id: sessionId,
          hook_event_name: "PreToolUse",
          tool_name: "background_process",
          tool_input: args,
          cwd: ctx.directory,
        };
        const resp = await postHook(event);
        return resp;
      },
    });
  }

  return {
    "tool.execute.before": async (input, output) => {
      if (isManaged) return; // managed instances handle permissions via SSE
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
      if (isManaged) return; // managed instances handle permissions via SSE
      const perm = input as any;
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

// openCodePluginPath returns the path where the plugin file is installed.
func openCodePluginPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "opencode", "plugins", "transitive.ts")
}

// InstallOpenCode writes the TypeScript plugin to ~/.config/opencode/plugins/transitive.ts.
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
	return nil
}

// UninstallOpenCode removes the plugin file.
func UninstallOpenCode() error {
	pluginPath := openCodePluginPath()
	if pluginPath == "" {
		return nil
	}
	if err := os.Remove(pluginPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plugin: %w", err)
	}
	log.Printf("OpenCode plugin removed from %s", pluginPath)
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
