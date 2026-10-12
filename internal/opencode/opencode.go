// Package opencode runs missions with OpenCode's terminal CLI.
package opencode

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/justinrush/q/internal/mission"
)

// Options configure an [Agent].
type Options struct {
	// Args are extra arguments appended to every invocation, before the prompt.
	Args []string
}

// Agent runs missions with opencode.
type Agent struct {
	bin  string
	args []string
}

// New returns an agent invoking the opencode binary at bin.
func New(bin string, opts Options) *Agent {
	return &Agent{bin: bin, args: opts.Args}
}

// Tool reports that this is the opencode agent.
func (*Agent) Tool() mission.Tool { return mission.ToolOpencode }

// Bin is the absolute path to the opencode executable.
func (a *Agent) Bin() string { return a.bin }

// Prologue needs no environment exports for opencode.
func (*Agent) Prologue(mission.Invocation) string { return "" }

// HookEvents returns the events q subscribes to via the opencode plugin.
func (*Agent) HookEvents() []string {
	return []string{
		mission.EventSessionStart,
		mission.EventSessionEnd,
		mission.EventUserPromptSubmit,
		mission.EventPreToolUse,
		mission.EventPostToolUse,
		mission.EventPermissionRequest,
		mission.EventStop,
		mission.EventStopFailure,
	}
}

// Args builds opencode's arguments.
func (a *Agent) Args(inv mission.Invocation) []string {
	var args []string

	if inv.Resume && inv.SessionID != "" {
		args = append(args, "--session", mission.ShellQuote(inv.SessionID))
	}

	if inv.PlanMode {
		args = append(args, "--agent", "plan")
	}

	args = append(args, "--auto")

	if inv.Model != "" {
		args = append(args, "--model", mission.ShellQuote(inv.Model))
	}

	args = append(args, mission.ShellQuoteAll(a.args)...)

	return append(args, "--prompt", mission.PromptArg)
}

// Artifacts generates opencode.json and the q-status plugin in the mission's .opencode directory.
func (a *Agent) Artifacts(inv mission.Invocation) ([]mission.Artifact, error) {
	configData, err := renderConfig(inv)
	if err != nil {
		return nil, fmt.Errorf("rendering opencode config: %w", err)
	}

	configArtifact := mission.Artifact{
		Path: filepath.Join(inv.MissionDir, ".opencode", "opencode.json"),
		Data: configData,
		Merge: func(generated, existing []byte) ([]byte, bool) {
			return mergeConfig(inv, generated, existing)
		},
	}

	pluginData := renderPlugin(inv)
	pluginArtifact := mission.Artifact{
		Path: filepath.Join(inv.MissionDir, ".opencode", "plugins", "q-status.js"),
		Data: pluginData,
	}

	return []mission.Artifact{configArtifact, pluginArtifact}, nil
}

func renderConfig(inv mission.Invocation) ([]byte, error) {
	doc := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"permission": map[string]any{
			"external_directory": "allow",
		},
	}

	applyModels(doc, inv)

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(data, '\n'), nil
}

func mergeConfig(inv mission.Invocation, generated, existing []byte) ([]byte, bool) {
	if len(existing) == 0 {
		return generated, true
	}

	var root map[string]any
	if err := json.Unmarshal(existing, &root); err != nil || root == nil {
		return existing, false
	}

	child(root, "permission")["external_directory"] = "allow"

	applyModels(root, inv)

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return existing, false
	}

	return append(data, '\n'), true
}

// agentSettings is what one of opencode's agents runs on.
type agentSettings struct {
	model   string
	variant string
}

// applyModels writes a mission's model and effort into an opencode config.
//
// A planning mission names both ends of the change it will make. opencode's
// plan and build agents each take a model of their own, and accepting a plan
// hands the session from one to the other, so giving each its settings up front
// is the whole of planning on one model and implementing on another: nothing
// has to happen at the moment of approval.
//
// Only what the mission names is written. An unset value leaves whatever the
// file already says, which may be the user's own choice.
func applyModels(root map[string]any, inv mission.Invocation) {
	model := inv.Model
	plan := agentSettings{variant: inv.Effort}
	build := agentSettings{variant: inv.Effort}

	if inv.PlanMode {
		model = inv.ImplementModel
		plan = agentSettings{model: inv.Model, variant: inv.Effort}
		build = agentSettings{model: inv.ImplementModel, variant: inv.ImplementEffort}
	}

	if model != "" {
		root["model"] = model
	}

	for name, settings := range map[string]agentSettings{"plan": plan, "build": build} {
		if settings == (agentSettings{}) {
			continue
		}

		agent := child(child(root, "agent"), name)

		if settings.model != "" {
			agent["model"] = settings.model
		}

		if settings.variant != "" {
			agent["variant"] = settings.variant
		}
	}
}

// child returns the object at key, creating it when it is missing or is not an
// object.
func child(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok && existing != nil {
		return existing
	}

	created := map[string]any{}
	parent[key] = created

	return created
}

func renderPlugin(inv mission.Invocation) []byte {
	script := fmt.Sprintf(`// Generated by q. Reports OpenCode events to the q daemon.
import { spawn } from 'node:child_process';

export const QStatus = async () => {
  const qBin = process.env.Q_BIN || %q;
  const seenUserMsgs = new Set();

  const send = (event, payload) => {
    try {
      const child = spawn(qBin, ['hook', 'opencode', event], {
        stdio: ['pipe', 'ignore', 'ignore'],
        env: process.env,
      });
      child.on('error', () => {});
      child.stdin.write(JSON.stringify(payload || {}));
      child.stdin.end();
      if (child.unref) {
        child.unref();
      }
    } catch (e) {}
  };

  return {
    event: async (input) => {
      const ev = input && input.event;
      if (!ev || !ev.type) return;
      const p = ev.properties || {};
      const info = p.info || {};
      const sessionID = info.id || p.sessionID || p.id || '';

      switch (ev.type) {
        case 'session.created':
          send('session-start', {
            session_id: sessionID,
            cwd: info.directory || p.directory || process.cwd(),
            source: 'startup',
          });
          break;
        case 'session.deleted':
          send('session-end', { session_id: sessionID });
          break;
        case 'session.idle':
          send('stop', { session_id: sessionID });
          break;
        case 'session.error': {
          const errReason = (p.error && (p.error.message || p.error.name)) || 'session error';
          send('stop-failure', { session_id: sessionID, reason: errReason });
          break;
        }
        case 'permission.updated':
          send('permission-request', {
            session_id: sessionID,
            tool_name: p.type || 'permission',
          });
          break;
        case 'permission.replied':
          send('post-tool-use', {
            session_id: sessionID,
            tool_name: p.type || '',
          });
          break;
        case 'question.asked':
          send('permission-request', {
            session_id: (p.data && p.data.sessionID) || sessionID,
            tool_name: 'question',
          });
          break;
        case 'question.replied':
        case 'question.rejected':
          send('post-tool-use', {
            session_id: (p.data && p.data.sessionID) || sessionID,
            tool_name: 'question',
          });
          break;
        case 'message.updated':
          if ((info.role || p.role) !== 'user') return;
          if (info.id) {
            if (seenUserMsgs.has(info.id)) return;
            seenUserMsgs.add(info.id);
            if (seenUserMsgs.size > 500) {
              for (const first of seenUserMsgs) {
                seenUserMsgs.delete(first);
                break;
              }
            }
          }
          send('user-prompt-submit', {
            session_id: info.sessionID || sessionID,
            prompt: '',
          });
          break;
      }
    },
    'tool.execute.before': async (input) => {
      if (!input) return;
      send('pre-tool-use', {
        session_id: input.sessionID || '',
        tool_name: input.tool || '',
      });
    },
    'tool.execute.after': async (input) => {
      if (!input) return;
      send('post-tool-use', {
        session_id: input.sessionID || '',
        tool_name: input.tool || '',
      });
    },
  };
};
`, inv.QBin)

	return []byte(script)
}
