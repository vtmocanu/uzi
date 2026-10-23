Synthetic plan (reconstructed for the replay corpus; not the historically approved plan)

1. Add an optional send_to_uzi field to the single-finding and grouped-finding file requests in the API types.
2. Apply the configured trigger labels in the file handlers only when the field is true, and record the consent on the filed issue.
3. Add the CLI flag and wire it through the uzi client and its fake.
4. Add the web opt-in to the finding card and the issue draft card and send it with the request.
5. Declare shared request types in the web API types and use them in the real and mock clients.
6. Add contract fixtures for both requests and extend the API contract test.
7. Document the opt-in in the findings and CLI docs.
8. Add handler, poller, client, CLI and web tests for consent and retry behavior.
