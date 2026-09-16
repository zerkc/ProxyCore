# Enrollment UI Specification

## Purpose

Define only the minimal Phase 2 settings experience required to enroll an independent installation safely. Node dashboards, primary inventory, rich drift or health diagnostics, promotion, rejoin, disconnect/reset, and other Phase 3–5 operations remain out of scope.

## Requirements

### Requirement: Minimal enrollment form

The standalone settings UI MUST provide the local node IP, primary address, enrollment token, and save and cancel actions needed to begin enrollment. It MUST validate required input and MUST treat the enrollment token as secret input. It MUST NOT imply that selecting `NODE` or saving the form has already replaced configuration.

#### Scenario: Owner submits enrollment settings

- GIVEN an authenticated Owner is viewing standalone role settings
- WHEN the Owner provides valid local node IP, primary address, and enrollment token and selects save
- THEN the UI MUST begin identity verification without committing replacement
- AND it MUST NOT redisplay or persist the plaintext token in ordinary client-visible state after submission

#### Scenario: Owner cancels before verification

- GIVEN the Owner has entered enrollment settings
- WHEN the Owner selects cancel before enrollment commits
- THEN the UI MUST leave the installation standalone
- AND no destructive replacement MUST occur

### Requirement: Identity preview and destructive confirmation

After contacting the proposed primary, the UI MUST display sufficient verified primary identity for the Owner to distinguish the intended installation. It MUST state that the current standalone configuration will be archived and replaced, and MUST require a separate explicit confirmation before destructive enrollment proceeds. Save, connectivity success, token validity, or viewing the preview MUST NOT count as confirmation.

#### Scenario: Owner explicitly confirms replacement

- GIVEN the UI displays the verified primary identity and archive-and-replace warning
- WHEN the Owner explicitly confirms replacement
- THEN the UI MAY authorize the staged enrollment operation
- AND it MUST present the operation as pending until the server reports committed enrollment

#### Scenario: Identity does not match expectation

- GIVEN the UI displays a verified primary identity that the Owner does not recognize
- WHEN the Owner cancels
- THEN the UI MUST stop the enrollment flow
- AND the server MUST preserve standalone editable state

### Requirement: Recoverable enrollment presentation

The UI MUST distinguish pending, committed, failed, and recoverable enrollment outcomes and MUST offer only server-authorized cancel or recovery actions appropriate to the current stage. It MUST NOT represent a failed or interrupted attempt as a committed NODE.

#### Scenario: Browser or service interruption occurs

- GIVEN enrollment was pending when the browser, API, or host restarted
- WHEN the Owner returns to role settings
- THEN the UI MUST render the durable server-reported enrollment state
- AND a recoverable attempt MUST provide its permitted recovery or cancellation action

### Requirement: Minimal committed-node settings

After enrollment commits, the UI MUST expose only Phase 2-essential node role settings and synchronization state needed to confirm the relationship and invoke server-supported recovery or retry. Ordinary replicated configuration editing controls MUST be hidden, while the API MUST remain the authoritative write boundary. The UI MUST NOT expose secret enrollment material.

#### Scenario: Owner opens settings on a committed node

- GIVEN enrollment has committed the installation as a NODE
- WHEN an Owner opens node role settings
- THEN the UI MUST identify the enrolled primary and current essential synchronization state
- AND ordinary DNS, proxy, certificate, functional-setting, and replicated-user editing controls MUST NOT be available
- AND enrollment tokens, node credentials, and cluster KEK material MUST NOT be displayed
