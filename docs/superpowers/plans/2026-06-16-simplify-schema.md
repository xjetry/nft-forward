# Simplify Schema Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Merge tunnels/combos into nodes, merge forwards/chains into rules — reducing 5 user-facing concepts to 2.

**Architecture:** Node gains `node_type` (remote/self/composite) with `node_hops` for composite paths. Rule replaces both Forward and Chain, backed by `rule_hops` (internal execution records per physical node). User authorization collapses from user_tunnels + user_tunnel_combos into `user_nodes`.

**Tech Stack:** Go (SQLite via modernc.org/sqlite), React (Vite), chi router, bubbletea TUI

**Migration script:** Already complete at `scripts/migrate_v2.sh` — handles data transformation for existing deployments.

---

## File Structure

### DB Layer (foundation — all other layers depend on this)

| Action  | File | Responsibility |
|---------|------|----------------|
| Modify  | `internal/db/queries.go` | Node struct (NodeKind→NodeType, drop LastSeenAt), remove Forward struct + all forward queries, add Rule/RuleHop structs + queries, update UpsertSelfNode/scanNode/nodeCols |
| Create  | `internal/db/rules.go` | Rule CRUD, RuleHop management, RegenerateRule (from RegenerateChain), OccupiedPortsOnNode, PickFreePort, HopInput |
| Create  | `internal/db/grants.go` | NodeHop struct/CRUD, UserNode struct, GrantNode/RevokeNode/ListNodesForUser |
| Delete  | `internal/db/combos.go` | Removed — combo logic merged into composite nodes |
| Delete  | `internal/db/tenants.go` | Removed — tunnel/grant logic replaced by grants.go |
| Delete  | `internal/db/chains.go` | Removed — chain logic replaced by rules.go |
| Replace | `internal/db/migrations/0001_init.sql` | New schema for fresh installs |
| Delete  | `internal/db/migrations/0002_tunnel_combos.sql` | No longer needed |
| Delete  | `internal/db/migrations/0003_merge_tenants.sql` | No longer needed |

### Server Layer

| Action  | File | Responsibility |
|---------|------|----------------|
| Modify  | `internal/server/server.go` | Routes (remove tunnel/combo/chain/forward, add composite-node + rule), buildRules uses rule_hops, dispatch unchanged |
| Modify  | `internal/server/api.go` | Remove handlers: apiListTunnels/apiCreateTunnel/apiDeleteTunnel, all combo handlers, all chain handlers, apiListForwards/apiCreateForward/apiGetForward/apiUpdateForward/apiDeleteForward, apiMyListForwards/apiMyCreateForward/apiMyDeleteForward, apiMyListChains/apiMyCreateChain/apiMyDeleteChain. Add: apiCreateCompositeNode, apiListRules/apiCreateRule/apiGetRule/apiUpdateRule/apiDeleteRule, apiMyListRules/apiMyCreateRule/apiMyDeleteRule. Update: apiGetUser (node grants), apiGrantNode/apiRevokeNode, apiDashboard, apiGetNode |
| Modify  | `internal/server/shared.go` | Remove validateAgainstTunnel/exitAllowedByTunnel/checkUserChainQuota/chainView/buildChainView/regenerateChainByID. Add: ruleView/buildRuleView, checkUserRuleQuota. Keep: parseExit, validateCIDRList helpers |
| Modify  | `internal/server/hub.go` | applyChainHopEdit→applyRuleHopEdit, applyChainDelete→applyRuleDelete, applyCounters updates rule_hops |
| Modify  | `internal/server/probe.go` | probeChainEndpoint→probeRuleEndpoint |

### Protocol Layer

| Action  | File | Responsibility |
|---------|------|----------------|
| Modify  | `internal/wsproto/messages.go` | Rename ChainHopEdit→RuleHopEdit, ChainDelete→RuleDelete, ChainCmdAck→RuleCmdAck; keep Forward struct (agent-facing, separate from db.Forward) |
| Modify  | `internal/nft/nft.go` | Rename Rule fields: ChainID→RuleID, ChainName→RuleName, TenantName→OwnerName |
| Modify  | `internal/daemon/handlers.go` | handleChainEdit→handleRuleEdit, handleChainDelete→handleRuleDelete |
| Modify  | `internal/daemonclient/client.go` | ChainEdit→RuleEdit, ChainDelete→RuleDelete |
| Modify  | `internal/tui/model.go` | daemonClient interface: ChainEdit→RuleEdit, ChainDelete→RuleDelete; editingChainID→editingRuleID |

### Web Frontend

| Action  | File | Responsibility |
|---------|------|----------------|
| Modify  | `web/src/App.jsx` | Remove /tunnels, /combos, /forwards, /chains routes. Add /rules. Update /my routes |
| Modify  | `web/src/components/Layout.jsx` | Simplify nav: Nodes (with composite), Rules (unified) |
| Delete  | `web/src/pages/tunnels/List.jsx` | Removed |
| Delete  | `web/src/pages/combos/List.jsx` | Removed |
| Delete  | `web/src/pages/forwards/List.jsx` | Removed |
| Delete  | `web/src/pages/forwards/Edit.jsx` | Removed |
| Delete  | `web/src/pages/chains/List.jsx` | Removed |
| Delete  | `web/src/pages/chains/Detail.jsx` | Removed |
| Delete  | `web/src/pages/my/Forwards.jsx` | Removed |
| Delete  | `web/src/pages/my/Chains.jsx` | Removed |
| Create  | `web/src/pages/rules/List.jsx` | Unified rule CRUD (admin): create on single/composite node, show entry endpoint, hops |
| Create  | `web/src/pages/rules/Detail.jsx` | Rule detail: hop status, port reallocation |
| Create  | `web/src/pages/my/Rules.jsx` | User self-service: create rule on granted nodes |
| Modify  | `web/src/pages/Dashboard.jsx` | Remove tunnel/forward stats, show node + rule stats |
| Modify  | `web/src/pages/nodes/Detail.jsx` | Show rule_hops on this node; add composite node create |
| Modify  | `web/src/pages/users/Detail.jsx` | node grants instead of tunnel/combo grants |
| Modify  | `web/src/pages/users/List.jsx` | Minor label changes |
| Modify  | `web/src/pages/my/Dashboard.jsx` | Show granted nodes instead of tunnels |

### Test Files

| Action  | File | Notes |
|---------|------|-------|
| Delete  | `internal/db/chains_test.go` | Replaced by rules tests |
| Delete  | `internal/db/queries_test.go` | Rewrite for new types |
| Delete  | Various server test files | Will be rewritten |

---

## Tasks

### Task 1: DB Layer — Types and Node Updates (queries.go)

**Files:**
- Modify: `internal/db/queries.go`

- [ ] Update Node struct: `NodeKind string` → `NodeType string`, remove `LastSeenAt sql.NullInt64`
- [ ] Update `nodeCols` to match new schema columns (drop last_seen_at, dirty; rename node_kind→node_type)
- [ ] Update `scanNode` to match new columns
- [ ] Update `UpsertSelfNode` to use node_type='self' instead of node_kind='self'
- [ ] Add `Rule` struct with fields: ID, NodeID, OwnerID, Name, Proto, ExitHost, ExitPort, EntryListenPort, Comment, Disabled, CreatedAt
- [ ] Add `RuleHop` struct with fields: ID, RuleID, Position, NodeID, Proto, ListenPort, TargetHost, TargetPort, Mode, Comment, LastBytes, TotalBytes
- [ ] Remove Forward struct and all forward-related constants/functions (forwardCols, scanForward, CreateForward, GetForward, ListForwards, listForwardsWhere, ListForwardsByNode, ActiveForwardsForPush, ListForwardsForUser, CountForwardsForUser, CountForwardsForUserTunnel, CountForwardsByTunnel, DeleteForward, UpdateForward, UpdateForwardByID, ForwardMapByNode, GetForwardByNodeProtoPort, NormalizeForwardMode, DeleteForwardsForUser)
- [ ] Replace `DistinctUserNodes` to query rule_hops instead of forwards
- [ ] Add Rule scan/list/CRUD functions inline or in rules.go
- [ ] Add `ActiveRuleHopsForPush(nodeID)` — replaces ActiveForwardsForPush, queries rule_hops joined with rules (exclude disabled rules/users)
- [ ] Add `RuleHopMapByNode(nodeID)` — replaces ForwardMapByNode for counter matching
- [ ] Build and verify: `go build ./internal/db/...`

### Task 2: DB Layer — Rule Operations (rules.go)

**Files:**
- Create: `internal/db/rules.go` (from chains.go logic)

- [ ] Move OccupiedPortsOnNode (query rule_hops instead of forwards), PickFreePort, hostPort, DBTX, ChainPortMin/Max, NormalizeForwardMode here
- [ ] Define HopInput struct (same fields minus TunnelID: NodeID, Mode, DesiredPort, Comment)
- [ ] CreateRule(tx, rule) — insert into rules table
- [ ] GetRule(d, id) — single rule by ID
- [ ] UpdateRuleHeader(tx, rule) — update name/proto/exit fields
- [ ] ListAllRules(d), ListRulesByUser(d, userID), ListRulesByNode(d, nodeID) — various listing queries
- [ ] ListRuleHops(d, ruleID) — ordered hops for a rule
- [ ] DeleteRule(d, id) — collect affected node IDs, cascade delete
- [ ] RegenerateRule(tx, rule, hops, avoid) — adapted from RegenerateChain: validates hops, allocates ports, writes rule_hops. Key differences: no tunnel_id checks, port range is always ChainPortMin-ChainPortMax, nodes come from the composite node's node_hops or directly from HopInput
- [ ] RulesReferencingNode(d, nodeID) — replaces ChainsReferencingNode
- [ ] EntryRuleHopIDs(d, nodeID) — replaces EntryChainForwardIDs for traffic billing
- [ ] DeleteRulesForUser(d, userID) — replaces DeleteForwardsForUser
- [ ] CountRulesForUser(d, userID) — replaces CountForwardsForUser
- [ ] Build: `go build ./internal/db/...`

### Task 3: DB Layer — Grants and NodeHops (grants.go)

**Files:**
- Create: `internal/db/grants.go` (from tenants.go logic)

- [ ] NodeHop struct: NodeID, Position, HopNodeID, Mode
- [ ] UserNode struct: UserID, NodeID, MaxForwards, GrantedAt
- [ ] CreateNodeHops(d, nodeID, hops) — insert node_hops for a composite node
- [ ] ListNodeHops(d, nodeID) — ordered hops of a composite node
- [ ] DeleteNodeHops(d, nodeID) — clear hops (for updating composite node)
- [ ] GrantNode(d, userID, nodeID, maxForwards)
- [ ] RevokeNode(d, userID, nodeID)
- [ ] GetNodeGrant(d, userID, nodeID) — single grant check
- [ ] ListNodesForUser(d, userID) — nodes granted to a user with grant details
- [ ] CountRulesForUserNode(d, userID, nodeID) — replaces CountForwardsForUserTunnel
- [ ] Build and verify

### Task 4: DB Layer — Migrations and Cleanup

**Files:**
- Replace: `internal/db/migrations/0001_init.sql`
- Delete: `internal/db/migrations/0002_tunnel_combos.sql`, `0003_merge_tenants.sql`
- Delete: `internal/db/combos.go`, `internal/db/tenants.go`, `internal/db/chains.go`
- Delete: `internal/db/chains_test.go`, `internal/db/queries_test.go`

- [ ] Replace 0001_init.sql with the new v2 schema (users, sessions, nodes, node_hops, user_nodes, rules, rule_hops, settings, audit_logs, node_tui_snapshot) — single file for fresh installs
- [ ] Delete 0002 and 0003 migration files
- [ ] Delete combos.go, tenants.go, chains.go (old Go files)
- [ ] Delete old test files
- [ ] Add schema_migrations seed entries (0001_init.sql + 0004_simplify_schema.sql) in the init SQL
- [ ] Build: `go build ./internal/db/...` — must compile cleanly

### Task 5: Server Layer — Dispatch and Routes (server.go)

**Files:**
- Modify: `internal/server/server.go`

- [ ] Update `buildRules` to use `db.ActiveRuleHopsForPush` instead of `db.ActiveForwardsForPush`. Map RuleHop→nft.Rule (no tunnel bandwidth lookup needed; RuleHop has all fields). Look up Rule by rule_id for metadata (RuleID, RuleName, OwnerName)
- [ ] Update `dispatchToNode` accordingly
- [ ] Update `Router()`: remove tunnel/combo/chain/forward routes, add composite node routes (POST /nodes with type=composite), add rule routes (GET/POST/PUT/DELETE /api/rules, /api/rules/{id}, /api/rules/{id}/hops/{pos}/reallocate), update user grant routes (POST /users/{id}/grants → node_id based), add /api/my/rules routes
- [ ] Build: `go build ./internal/server/...`

### Task 6: Server Layer — API Handlers (api.go)

**Files:**
- Modify: `internal/server/api.go`

- [ ] Remove all tunnel handlers (apiListTunnels, apiCreateTunnel, apiDeleteTunnel)
- [ ] Remove all combo handlers (apiListCombos, apiCreateCombo, apiUpdateCombo, apiDeleteCombo)
- [ ] Remove all chain handlers (apiListChains, apiCreateChain, apiGetChain, apiUpdateChain, apiDeleteChain, apiReallocateHop)
- [ ] Remove all forward handlers (apiListForwards, apiCreateForward, apiGetForward, apiUpdateForward, apiDeleteForward, apiCreateForwardFromCombo)
- [ ] Remove all my/ forward/chain handlers (apiMyListForwards, apiMyCreateForward, apiMyDeleteForward, apiMyListChains, apiMyCreateChain, apiMyDeleteChain, apiUserCreateForwardFromCombo)
- [ ] Add `apiCreateCompositeNode` — creates a composite node with node_hops
- [ ] Add `apiListRules` — unified listing with node_type info
- [ ] Add `apiCreateRule` — create rule on single or composite node, calls RegenerateRule for composite
- [ ] Add `apiGetRule` — rule detail with hops
- [ ] Add `apiUpdateRule` — edit rule header + regenerate
- [ ] Add `apiDeleteRule` — delete + re-dispatch
- [ ] Add `apiReallocateRuleHop` — same concept as apiReallocateHop
- [ ] Add `apiMyListRules`, `apiMyCreateRule`, `apiMyDeleteRule` — user self-service
- [ ] Update `apiGrantTunnel` → `apiGrantNode` (accept node_id instead of tunnel_id)
- [ ] Update `apiRevokeTunnel` → `apiRevokeNode`
- [ ] Remove `apiGrantCombo`, `apiRevokeCombo`
- [ ] Update `apiGetUser` — return node grants instead of tunnel/combo grants
- [ ] Update `apiDashboard` — remove tunnel/forward counts, add rule counts
- [ ] Update `apiGetNode` — return rule_hops on this node instead of forwards
- [ ] Update `apiDeleteNode` — rewire rules instead of chains
- [ ] Update `apiSetNodeRelayHost` — rewire rules instead of chains
- [ ] Build: `go build ./internal/server/...`

### Task 7: Server Layer — Shared and Hub (shared.go, hub.go, probe.go)

**Files:**
- Modify: `internal/server/shared.go`
- Modify: `internal/server/hub.go`
- Modify: `internal/server/probe.go`

- [ ] shared.go: Remove chainView, buildChainView, regenerateChainByID, validateAgainstTunnel, exitAllowedByTunnel, checkUserChainQuota. Add ruleView/buildRuleView (path display from node_hops), regenerateRuleByID, checkUserRuleQuota (uses GetNodeGrant + CountRulesForUserNode)
- [ ] hub.go: Rename applyChainHopEdit→applyRuleHopEdit, applyChainDelete→applyRuleDelete, update SQL queries from chain_hops/forwards to rule_hops/rules, update applyCounters to use RuleHopMapByNode + rule-based traffic billing
- [ ] probe.go: Update probeChainEndpoint to work with rules (walk rule_hops instead of chain_hops)
- [ ] Build

### Task 8: Protocol Layer (wsproto, nft, daemon, daemonclient, TUI)

**Files:**
- Modify: `internal/wsproto/messages.go`
- Modify: `internal/nft/nft.go`
- Modify: `internal/daemon/handlers.go`
- Modify: `internal/daemonclient/client.go`
- Modify: `internal/tui/model.go`

- [ ] nft.go: Rename ChainID→RuleID, ChainName→RuleName, TenantName→OwnerName in nft.Rule struct + computeRev exclusions
- [ ] wsproto: Rename TypeChainHopEdit→TypeRuleHopEdit, TypeChainDelete→TypeRuleDelete, TypeChainCmdAck→TypeRuleCmdAck; rename ChainHopEdit→RuleHopEdit (ChainID→RuleID), ChainDelete→RuleDelete, ChainCmdAck→RuleCmdAck
- [ ] daemon/handlers.go: Rename handleChainEdit→handleRuleEdit, handleChainDelete→handleRuleDelete, update URL paths
- [ ] daemonclient: Rename ChainEdit→RuleEdit, ChainDelete→RuleDelete, update URL paths
- [ ] tui/model.go: Update daemonClient interface (ChainEdit→RuleEdit, ChainDelete→RuleDelete), editingChainID→editingRuleID
- [ ] Build: `go build ./...`

### Task 9: Web Frontend

**Files:**
- Modify: `web/src/App.jsx`, `web/src/components/Layout.jsx`
- Delete: tunnels/List.jsx, combos/List.jsx, forwards/List.jsx, forwards/Edit.jsx, chains/List.jsx, chains/Detail.jsx, my/Forwards.jsx, my/Chains.jsx
- Create: `web/src/pages/rules/List.jsx`, `web/src/pages/rules/Detail.jsx`, `web/src/pages/my/Rules.jsx`
- Modify: Dashboard.jsx, nodes/Detail.jsx, users/Detail.jsx, users/List.jsx, my/Dashboard.jsx

- [ ] Delete old page files (tunnels, combos, forwards, chains, my/Forwards, my/Chains)
- [ ] Update App.jsx routes: /rules, /rules/:id, /my/rules
- [ ] Update Layout.jsx nav: admin sees "节点" (nodes) + "规则" (rules); user sees "我的规则"
- [ ] Create rules/List.jsx: list rules, create rule (pick node single/composite, fill exit), show entry endpoint, hops path
- [ ] Create rules/Detail.jsx: rule detail with hop status table, port reallocation, edit form
- [ ] Create my/Rules.jsx: user's rules with node grant awareness
- [ ] Update Dashboard.jsx: node + rule counts
- [ ] Update nodes/Detail.jsx: show rule_hops on this node, add "创建组合节点" button
- [ ] Update users/Detail.jsx: node grants (single + composite) instead of tunnel/combo grants
- [ ] Update my/Dashboard.jsx: show granted nodes instead of tunnels
- [ ] Test in browser

### Task 10: Final Build, Test, and Cleanup

- [ ] Full build: `go build ./...`
- [ ] Run existing tests: `go test ./...`
- [ ] Fix any remaining compilation errors
- [ ] Start dev server and test golden path in browser
- [ ] Remove any remaining references to old types (grep for tunnel, combo, forward, chain in Go files)
- [ ] Commit
