# Terminal client

Build with `go build -o /tmp/orbitctl ./cmd/orbitctl`. Set `ORBIT_URL` to the public HTTPS Orbit URL, `ORBIT_OIDC_ISSUER` to one authentik provider's exact issuer URL, and `ORBIT_OIDC_CLIENT_ID` to that provider's public client ID. The equivalent flags are `-url`, `-issuer` and `-client-id`, before the command.

The authentik provider must enable the device-code grant for a public client, publish `device_authorization_endpoint` and `token_endpoint` in its discovery document, and include the person and groups claims Orbit requires. Orbit's accepted issuers and client IDs must include this provider. No client secret or operator token is used. Endpoints require HTTPS; device and token endpoints must share the issuer's host. Redirects are refused.

Every invocation displays the verification URL and user code, waits for sign-in, and executes its command with the access token held in memory. Tokens are never written to disk or printed. This initial client signs in for each invocation; there is no persistent login session or refresh storage.

```sh
orbitctl agents
orbitctl conversations assistant
orbitctl thread CONVERSATION_ID
orbitctl send-agent assistant 'What needs attention?'
orbitctl send CONVERSATION_ID 'Please continue.'
orbitctl stream RUN_ID
orbitctl approve ACTION_ID
orbitctl reject ACTION_ID
```

`send-agent` uses the agent's default conversation. `send` targets an existing conversation. Both wake a run and follow its stream when the API returns a run ID. Listing and decision commands print API JSON; streams print the API's Server-Sent Events, including status and text events. Interrupt with Control-C. The client does not retry failed writes automatically. A stream disconnect is reported; use `stream` with the printed run ID to replay it.
