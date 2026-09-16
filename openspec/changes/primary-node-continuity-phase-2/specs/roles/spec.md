# Delta for Roles and Leadership

## MODIFIED Requirements

### Requirement: Node write boundary

After enrollment reaches its safe commit point and the installation durably enters `NODE` mode, the NODE MUST reject ordinary DNS, proxy, certificate, functional-setting, replicated-administrator, and other desired-configuration mutations at the API authorization or policy boundary. The restriction MUST apply regardless of whether a route is hidden by the UI. A NODE MAY accept explicitly node-local settings needed for its local data plane or enrollment relationship, and Workers MUST remain able to apply fully validated snapshots through the synchronization pipeline.

(Previously: NODE mutations were contractually rejected for Phase 0/1, without tying enforcement to the Phase 2 enrollment commit point and all API surfaces.)

#### Scenario: Operator attempts a replicated mutation on a node

- GIVEN an Operator is authenticated to a committed NODE
- WHEN the Operator submits an ordinary configuration mutation through any API route
- THEN the NODE MUST reject it
- AND its active and desired configuration MUST remain unchanged

#### Scenario: Hidden endpoint is called directly

- GIVEN the NODE UI does not display an ordinary configuration control
- WHEN an authenticated caller directly invokes the corresponding mutation endpoint
- THEN server-side authorization or policy MUST reject the mutation

#### Scenario: Worker applies synchronized state

- GIVEN a committed NODE has a fully validated pulled snapshot
- WHEN its Worker invokes the canonical apply pipeline
- THEN the node write boundary MUST permit that controlled snapshot apply
- AND it MUST NOT grant ordinary callers configuration write authority

#### Scenario: Enrollment has not committed

- GIVEN an enrollment attempt is pending, failed, cancelled, or recoverable before commit
- WHEN an authorized Owner edits the still-standalone installation
- THEN the system MUST retain standalone write behavior
- AND it MUST NOT enforce committed-NODE restrictions prematurely
