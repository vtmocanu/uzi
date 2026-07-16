// The hosted-worker controller (PRD #58): its own module, deliberately.
//
// It is the only uzi component that will ever hold kube-apiserver credentials
// (Decision 1), and a separate module is what keeps k8s.io/client-go — which M3
// adds here — structurally out of api/go.mod. "The api gets zero kube access"
// then holds as a property of the dependency graph rather than a convention a
// reviewer has to keep enforcing.
//
// It shares no Go types with the api: what crosses is the wire contract in
// internal/protocol, re-declared on this side and pinned to the api's producer
// test by a shared golden file — exactly how the TypeScript agent consumes the
// worker protocol today (api/internal/workersvc/claim_wire_contract_test.go vs.
// agent/test/claim-skills-contract.test.ts). A `replace ../api` would instead drag
// pgx, chi, the GitLab client, Slack and OIDC into this module's graph to reuse a
// few structs.
//
// Stdlib only, and worth keeping that way: this module's whole job is one HTTP
// poll and (from M3) one kube client.
module gitlab.example.com/vtmocanu/uzi/controller

go 1.26.4
