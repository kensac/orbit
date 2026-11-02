# Using orbit

Orbit keeps agents with ongoing responsibilities: review fetched data on a schedule, answer messages, remember useful context, ask questions, and propose actions under rules you choose. It keeps runs, conversations, inputs and results so you can inspect what happened. Start with one small responsibility and add sources or permissions only when it needs them.

This guide describes the repository's current interfaces, not a deployment check. You need a running, configured service with a usable runner and access to its API. Your operator supplies the service URL and sign-in method; public API access requires a valid access token. See [authentication](../api.md#authentication-authentik) and the [example deployment configuration](../../examples/orbit.yaml). Email, notifications and building tickets each need additional configuration.

## An agent in a minute

On an existing service, create a simple agent with `POST /api/v1/agents`. The request below uses the deployment's default runner, starts no schedule, and lets a message wake the agent. Send it with the same authenticated connection used for other API requests. Use `dry_run: true` first to validate without saving, then `false` to create it.

```json
{
  "spec": {
    "name": "daily-review",
    "description": "Help me review today's priorities.",
    "charter": "Help me choose three priorities from what I tell you. Ask when information is missing. Do not claim access to sources I have not supplied.",
    "wake_on_message": true
  },
  "dry_run": false
}
```

An agent without sources sees what you tell it and the context orbit supplies; it does not acquire access to your accounts just because its charter mentions them. Deployment defaults can also supply channels and other settings. Inspect the returned `spec` before using the agent. The [agent definition API](../api.md#defining-agents) explains updates, disabling and archiving; file-defined agents are edited in configuration instead.

For a schedule or fetched data, follow the [configuration reference](../agents/configuration.md) and [finance example](../../examples/agents/finance.yaml). The example uses placeholder hosts and environment variables; it is a template, not a ready account connection. An operator can validate a configuration directory with `go run ./cmd/orbit validate <dir>` from the repository.

## Start a conversation

Send `POST /api/v1/agents/daily-review/conversations` with a first message:

```json
{
  "title": "Today's priorities",
  "body": "I have two hours today. Help me prioritize writing a proposal and clearing my inbox.",
  "wake": true
}
```

The response includes `conversation_id`, `message_id` and, when a run was queued, `run_id`. Read the reply with `GET /api/v1/conversations/{conversation_id}/thread`; continue with `POST /api/v1/conversations/{conversation_id}/messages` and `{"body":"My proposal is due tomorrow.","wake":true}`. If the agent asks a question, include its message ID as `reply_to`. A queued run is not a completed answer: inspect its status, report and any error in the thread.

Use separate conversations for separate topics. Each has its own messages and session; memory, schedules and action rules belong to the agent and are shared across its conversations. Scheduled and manual runs use its default conversation, named `General`. Archiving keeps the conversation readable; a new message brings it back. See [conversations](../api.md#conversations) for paging, attachments and retry identifiers.

## Review actions and approvals

An agent proposes an action with a reason and parameters. Its rule determines whether orbit approves it automatically (`auto`), waits for you (`approve`), or refuses it (`deny`); a missing permission does not grant access. Read the action and its parameters before approving. Approval permits execution, so check the eventual result as well.

Find waiting proposals with `GET /api/v1/actions?status=pending`, or in the conversation's run. Inspect one with `GET /api/v1/actions/{id}`, then use `POST /api/v1/actions/{id}/approve` or `POST /api/v1/actions/{id}/reject`. An approval returns `{"ok":true}`; it does not mean execution succeeded. Read the action again for its status, result or error. Configured notifications can also carry an approval link. See [actions](../api.md#actions).

Coding work has a separate [ticket workflow](../tickets.md): discuss a request, approve a particular plan version, and follow its build and pull request. It requires an allowed repository, configured repository access, a build agent and the sandbox service. Availability of the ordinary chat API does not establish that a real ticket can complete a build or deployment.

## Talk by email

Email is available only when the operator configures both outbound SMTP and orbit's inbound mailbox. The mailbox needs IMAP access, allowed senders and delivery of plus-addressed replies. Replies to orbit's signed reply address continue the same conversation, and orbit answers every mail by mail, as a reply to the sender. To start a conversation with a new email, the operator must also set `mail.agent`; without it, mail that is not a reply is recorded and ignored.

Use the configured orbit address, and reply to the address supplied by its message rather than constructing one. New mail starts a conversation with the configured agent, not an agent chosen from the subject. Incoming automatic mail and failed sender-authentication results can be ignored. The mailbox is dedicated to orbit because polling marks unseen mail as seen. See [mail configuration](../agents/configuration.md#mail) for prerequisites and sender restrictions.

## Connect an MCP client

A compatible client can connect to `<service-url>/mcp` using Streamable HTTP. This requires a reachable endpoint and a valid access token. Interactive sign-in is available only when the operator has configured the OAuth proxy and allowed the client's redirect host; exposing the endpoint alone does not enable sign-in. Ask the operator for the endpoint and supported sign-in setup. Keep the operator token on the internal listener.

Once connected, use `list_agents`, `create_conversation` or `send_message`, and `read_thread`. A reply still running can be checked with `wait_for_reply`; questions can be answered with `send_message` and `reply_to`. Use `list_actions`, `approve_action` and `reject_action` for proposals. These calls use the same conversations and approval rules as the API. This connection lets a client talk to orbit; it does not connect other account services to an agent. See the [MCP contract and sign-in prerequisites](../api.md#mcp).
