---
title: MCP server
description: Run the stageset-controller Model Context Protocol server so an agent reads StageSet status and drives reconciliation in-cluster.
tags: [mcp, claude, operator]
---

The controller exposes StageSet introspection — and, when opted in, control — as
[Model Context Protocol](https://modelcontextprotocol.io/) tools, so an LLM
agent (Claude Code, Claude Desktop, or any MCP client) calls them directly. The
server runs inside the controller pod and serves over streamable HTTP; the tools
read and patch StageSet resources as the controller's ServiceAccount, so an agent
can never exceed the controller's own RBAC.

## Enable the server

Set `--mcp-bind-address` on the controller to serve the read tools (empty, the
default, disables it):

```shell
stageset-controller --mcp-bind-address :8084
```

Reach it from your machine with a port-forward:

```shell
kubectl --namespace stageset-system port-forward deploy/stageset-controller 8084:8084
```

The read tools:

| Tool | Purpose |
|---|---|
| `list_stagesets` | List StageSet resources with their Ready status, reason, suspend state, rolled-out version, and observed generation. Omit the namespace to list across every namespace the controller can read. |
| `get_stageset` | One StageSet's full status: the Ready condition (status, reason, message), the per-reason [runbook](../../runbooks/) URL, suspend state, version, per-stage phases and applied revisions, and any pending migrations. |

## Gated mutations

The server is read-only by default. Add `--mcp-allow-mutations` to also expose
write tools:

```shell
stageset-controller --mcp-bind-address :8084 --mcp-allow-mutations
```

| Tool | Effect |
|---|---|
| `reconcile_stageset` | Stamp the `reconcile.fluxcd.io/requestedAt` annotation to request an immediate reconcile — the same trigger as `flux reconcile`. |
| `suspend_stageset` | Set `spec.suspend=true` so the controller stops reconciling the StageSet. |
| `resume_stageset` | Clear `spec.suspend` to resume reconciliation. |

These act on the StageSet as the controller's ServiceAccount, so they can never
exceed the controller's own RBAC. Keep them off unless you intend the agent to
drive reconciliation, and have your MCP client confirm each call. Like the
[stage gate](../../), the server is best-effort: if its port can't bind the
controller logs the failure and keeps reconciling.
