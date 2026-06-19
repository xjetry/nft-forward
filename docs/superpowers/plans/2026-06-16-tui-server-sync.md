# TUI-Server 规则同步与统一管理 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Unify TUI and server rule management with server-authoritative CRUD, upgrade migration, downgrade preservation, and WebUI enhancements.

**Architecture:** Extend WebSocket protocol with `rule_create`/`rule_update`/`migrate_rules` commands. Daemon abstracts connection state behind unified CRUD API. TUI becomes mode-agnostic. WebUI adds node editing and rule display simplification.

**Tech Stack:** Go (Bubbletea TUI, chi HTTP, gorilla/websocket), SQLite, React SPA

---

## File Map

### Modified
- `internal/db/migrations/0001_init.sql` — add owner_id to nodes
- `internal/nft/nft.go:25-45` — add HopCount to Rule struct
- `internal/nft/nft_test.go` — update tests for HopCount
- `internal/wsproto/messages.go` — add RuleCreate/RuleUpdate/MigrateRules, remove PanelSegmentEdit
- `internal/wsproto/messages_test.go` — update tests
- `internal/db/rules.go` — add RuleHopCounts helper
- `internal/db/queries.go` — add owner_id to Node struct/scans, add node toggle/update queries
- `internal/server/hub.go` — add handleRuleCreate/Update/Migrate, remove applyPanelEdits
- `internal/server/server.go:151` — buildRules fill HopCount; EnsureSelfNode set owner_id
- `internal/server/selfnode.go:16` — EnsureSelfNode owner_id
- `internal/server/shared.go` — grant bypass for node owner
- `internal/server/api.go` — node toggle/owner endpoints, remove entry_port from create
- `internal/daemon/handlers.go` — rewrite to CRUD API
- `internal/daemon/dialer.go` — add CreateRule/UpdateRule/MigrateRules commands, remove panelCh/NotifyPanelEdited
- `internal/daemonclient/client.go` — rewrite to CRUD API
- `internal/tui/model.go` — new daemonClient interface, model refactor
- `internal/tui/form.go` — HopCount-based locking, optional port
- `internal/tui/list.go` — unified delete
- `internal/tui/view.go` — status bar, HopCount display
- `internal/tui/tui_test.go` — update tests
- `web/src/pages/nodes/Detail.jsx` — add toggle, owner display
- `web/src/pages/nodes/List.jsx` — owner_id in create modal
- `web/src/pages/rules/List.jsx` — remove entry_port, simplify node display
- `web/src/pages/rules/Detail.jsx` — remove entry_port from edit, simplify node display
- `web/src/pages/my/Rules.jsx` — simplify node display

---

### Task 1: Schema — add owner_id to nodes

**Files:**
- Modify: `internal/db/migrations/0001_init.sql:23-39`
- Modify: `internal/db/queries.go` (Node struct, scanNode, node queries)

- [ ] **Step 1: Add owner_id column to nodes table in schema**

In `0001_init.sql`, add `owner_id` column to nodes table:

```sql
-- In the nodes CREATE TABLE, after 'name TEXT NOT NULL UNIQUE':
owner_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
```

- [ ] **Step 2: Add OwnerID to Node struct and scanNode**

In `queries.go`, add `OwnerID *int64` to the Node struct and update `nodeCols` and `scanNode` to include it.

- [ ] **Step 3: Add node toggle and owner update queries**

```go
func ToggleNode(d *sql.DB, id int64) error {
    _, err := d.Exec(`UPDATE nodes SET disabled = NOT disabled WHERE id = ?`, id)
    return err
}

func UpdateNodeOwner(d *sql.DB, id int64, ownerID *int64) error {
    _, err := d.Exec(`UPDATE nodes SET owner_id = ? WHERE id = ?`, ownerID, id)
    return err
}
```

- [ ] **Step 4: Run existing tests to verify no regressions**

Run: `go test ./internal/db/...`

- [ ] **Step 5: Commit**

```bash
git add internal/db/migrations/0001_init.sql internal/db/queries.go
git commit -m "feat: add owner_id to nodes table for TUI rule ownership"
```

---

### Task 2: nft.Rule — add HopCount field

**Files:**
- Modify: `internal/nft/nft.go:25-45`

- [ ] **Step 1: Add HopCount to Rule struct**

After line 44 (`OwnerName`), add:

```go
HopCount  int    `json:"hop_count,omitempty"`
```

- [ ] **Step 2: Update Validate() — allow SrcPort=0 for auto-assign**

In `Validate()` (line 86-88), change port validation:

```go
// SrcPort: 0 means auto-assign, otherwise must be 1-65535
if r.SrcPort < 0 || r.SrcPort > 65535 {
    return fmt.Errorf("src_port %d out of range", r.SrcPort)
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/nft/...`

- [ ] **Step 4: Commit**

```bash
git add internal/nft/nft.go
git commit -m "feat: add HopCount metadata to nft.Rule, allow SrcPort=0 for auto-assign"
```

---

### Task 3: WebSocket protocol — new message types

**Files:**
- Modify: `internal/wsproto/messages.go`
- Modify: `internal/wsproto/messages_test.go`

- [ ] **Step 1: Add new message type constants**

After line 25 (`TypeRuleCmdAck`), add:

```go
TypeRuleCreate   = "rule_create"
TypeRuleUpdate   = "rule_update"
TypeMigrateRules = "migrate_rules"
```

Remove line 22: `TypePanelSegmentEdit = "panel_segment_edit"`

- [ ] **Step 2: Add new message structs**

Replace `PanelSegmentEdit` struct (lines 113-115) with:

```go
type RuleCreate struct {
    Proto      string `json:"proto"`
    ExitHost   string `json:"exit_host"`
    ExitPort   int    `json:"exit_port"`
    ListenPort int    `json:"listen_port"`
    Mode       string `json:"mode"`
    Comment    string `json:"comment"`
    Name       string `json:"name"`
}

type RuleUpdate struct {
    RuleID     int64  `json:"rule_id"`
    Proto      string `json:"proto"`
    ExitHost   string `json:"exit_host"`
    ExitPort   int    `json:"exit_port"`
    ListenPort int    `json:"listen_port"`
    Mode       string `json:"mode"`
    Comment    string `json:"comment"`
    Name       string `json:"name"`
}

type MigrateRules struct {
    Rules []nft.Rule `json:"rules"`
}
```

Remove the `Forward` struct (lines 50-58) and `PanelSegmentEdit` struct — no longer needed.

- [ ] **Step 3: Update tests**

Remove any tests referencing PanelSegmentEdit/Forward. Add round-trip tests for new types.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/wsproto/...`

- [ ] **Step 5: Commit**

```bash
git add internal/wsproto/
git commit -m "feat: add rule_create/rule_update/migrate_rules WS messages, remove panel_segment_edit"
```

---

### Task 4: DB helpers — RuleHopCounts + owner bypass

**Files:**
- Modify: `internal/db/rules.go`
- Modify: `internal/db/grants.go`

- [ ] **Step 1: Add RuleHopCounts query**

```go
func RuleHopCounts(d DBTX, ruleIDs []int64) (map[int64]int, error) {
    if len(ruleIDs) == 0 {
        return nil, nil
    }
    placeholders := make([]string, len(ruleIDs))
    args := make([]any, len(ruleIDs))
    for i, id := range ruleIDs {
        placeholders[i] = "?"
        args[i] = id
    }
    q := `SELECT rule_id, COUNT(*) FROM rule_hops WHERE rule_id IN (` +
        strings.Join(placeholders, ",") + `) GROUP BY rule_id`
    rows, err := d.Query(q, args...)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    m := make(map[int64]int, len(ruleIDs))
    for rows.Next() {
        var rid int64
        var cnt int
        if err := rows.Scan(&rid, &cnt); err != nil {
            return nil, err
        }
        m[rid] = cnt
    }
    return m, rows.Err()
}
```

- [ ] **Step 2: Add CheckNodeAccess with owner bypass**

In `grants.go`:

```go
func CheckNodeAccess(d *sql.DB, userID, nodeID int64) (*UserNode, error) {
    var ownerID *int64
    _ = d.QueryRow(`SELECT owner_id FROM nodes WHERE id = ?`, nodeID).Scan(&ownerID)
    if ownerID != nil && *ownerID == userID {
        return &UserNode{UserID: userID, NodeID: nodeID, MaxForwards: 9999}, nil
    }
    return GetNodeGrant(d, userID, nodeID)
}
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/db/...`

- [ ] **Step 4: Commit**

```bash
git add internal/db/rules.go internal/db/grants.go
git commit -m "feat: add RuleHopCounts query and CheckNodeAccess with owner bypass"
```

---

### Task 5: Server — buildRules fill HopCount + EnsureSelfNode owner_id

**Files:**
- Modify: `internal/server/server.go:151` (buildRules)
- Modify: `internal/server/selfnode.go:16` (EnsureSelfNode)

- [ ] **Step 1: Fill HopCount in buildRules**

In `buildRules()` (server.go ~line 151), after building the []nft.Rule list, bulk-query hop counts and fill:

```go
// Collect rule IDs from the built rules
ruleIDs := make([]int64, 0)
for _, r := range rules {
    if r.RuleID != 0 {
        ruleIDs = append(ruleIDs, r.RuleID)
    }
}
hopCounts, err := db.RuleHopCounts(s.db, ruleIDs)
if err != nil {
    return nil, "", err
}
for i := range rules {
    if c, ok := hopCounts[rules[i].RuleID]; ok {
        rules[i].HopCount = c
    }
}
```

- [ ] **Step 2: Set owner_id in EnsureSelfNode**

In `selfnode.go` EnsureSelfNode, after upserting the self-node, set owner_id to admin:

```go
// Set owner_id to first admin if not yet set
_, _ = d.Exec(`UPDATE nodes SET owner_id = (
    SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1
) WHERE node_type = 'self' AND owner_id IS NULL`)
```

- [ ] **Step 3: Run server tests**

Run: `go test ./internal/server/...`

- [ ] **Step 4: Commit**

```bash
git add internal/server/server.go internal/server/selfnode.go
git commit -m "feat: fill HopCount in buildRules, set owner_id on self-node"
```

---

### Task 6: Server Hub — new WS handlers + remove applyPanelEdits

**Files:**
- Modify: `internal/server/hub.go`

- [ ] **Step 1: Add handleRuleCreate**

New method on Hub, called from readerLoop when TypeRuleCreate received:

```go
func (h *Hub) handleRuleCreate(ac *agentConn, env wsproto.Envelope) {
    var req wsproto.RuleCreate
    if err := json.Unmarshal(env.Payload, &req); err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    node, err := db.GetNode(h.db, ac.nodeID)
    if err != nil || node.OwnerID == nil {
        sendRuleAckErr(ac, env.ID, "节点未设置操作者")
        return
    }
    ownerID := *node.OwnerID

    user, err := db.GetUserByID(h.db, ownerID)
    if err != nil || user.Disabled || (user.ExpiresAt != nil && user.ExpiresAt.Before(time.Now())) {
        sendRuleAckErr(ac, env.ID, "用户已禁用或过期")
        return
    }

    count, _ := db.CountRulesForUser(h.db, ownerID)
    if count >= user.MaxForwards {
        sendRuleAckErr(ac, env.ID, "已达规则上限")
        return
    }

    tx, _ := h.db.Begin()
    defer tx.Rollback()

    rule := &db.Rule{
        NodeID:   ac.nodeID,
        OwnerID:  ownerID,
        Name:     req.Name,
        Proto:    req.Proto,
        ExitHost: req.ExitHost,
        ExitPort: req.ExitPort,
        Comment:  req.Comment,
    }
    ruleID, err := db.CreateRule(tx, rule)
    if err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    hops := []db.HopInput{{NodeID: ac.nodeID, Mode: req.Mode, DesiredPort: req.ListenPort}}
    entry, affected, err := db.RegenerateRule(tx, rule, hops, nil)
    if err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }
    if err := tx.Commit(); err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    go h.onDispatch(affected)

    ac.enqueueWrite(wsproto.Envelope{
        Type: wsproto.TypeRuleCmdAck,
        ID:   env.ID,
        Payload: mustMarshal(wsproto.RuleCmdAck{OK: true, Entry: entry}),
    })
}
```

- [ ] **Step 2: Add handleRuleUpdate**

```go
func (h *Hub) handleRuleUpdate(ac *agentConn, env wsproto.Envelope) {
    var req wsproto.RuleUpdate
    if err := json.Unmarshal(env.Payload, &req); err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    rule, err := db.GetRule(h.db, req.RuleID)
    if err != nil {
        sendRuleAckErr(ac, env.ID, "规则不存在")
        return
    }

    node, _ := db.GetNode(h.db, ac.nodeID)
    if node.OwnerID == nil || rule.OwnerID != *node.OwnerID {
        sendRuleAckErr(ac, env.ID, "无权修改此规则")
        return
    }

    hops, _ := db.ListRuleHops(h.db, req.RuleID)
    if len(hops) != 1 {
        sendRuleAckErr(ac, env.ID, "仅支持修改单跳规则")
        return
    }

    tx, _ := h.db.Begin()
    defer tx.Rollback()

    rule.Name = req.Name
    rule.Proto = req.Proto
    rule.ExitHost = req.ExitHost
    rule.ExitPort = req.ExitPort
    rule.Comment = req.Comment
    if err := db.UpdateRuleHeader(tx, rule); err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    desiredPort := req.ListenPort
    if desiredPort == 0 {
        desiredPort = hops[0].ListenPort
    }
    hopInputs := []db.HopInput{{NodeID: ac.nodeID, Mode: req.Mode, DesiredPort: desiredPort}}
    entry, affected, err := db.RegenerateRule(tx, rule, hopInputs, nil)
    if err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }
    if err := tx.Commit(); err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    go h.onDispatch(affected)

    ac.enqueueWrite(wsproto.Envelope{
        Type: wsproto.TypeRuleCmdAck,
        ID:   env.ID,
        Payload: mustMarshal(wsproto.RuleCmdAck{OK: true, Entry: entry}),
    })
}
```

- [ ] **Step 3: Add handleMigrateRules**

```go
func (h *Hub) handleMigrateRules(ac *agentConn, env wsproto.Envelope) {
    var req wsproto.MigrateRules
    if err := json.Unmarshal(env.Payload, &req); err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    node, _ := db.GetNode(h.db, ac.nodeID)
    if node.OwnerID == nil {
        sendRuleAckErr(ac, env.ID, "节点未设置操作者")
        return
    }
    ownerID := *node.OwnerID

    user, _ := db.GetUserByID(h.db, ownerID)
    existing, _ := db.CountRulesForUser(h.db, ownerID)
    if existing+len(req.Rules) > user.MaxForwards {
        sendRuleAckErr(ac, env.ID, fmt.Sprintf("规则总数将超过上限 %d", user.MaxForwards))
        return
    }

    tx, _ := h.db.Begin()
    defer tx.Rollback()

    for _, r := range req.Rules {
        rule := &db.Rule{
            NodeID:   ac.nodeID,
            OwnerID:  ownerID,
            Name:     r.Comment,
            Proto:    r.Proto,
            ExitHost: r.DestHost,
            ExitPort: r.DestPort,
            Comment:  r.Comment,
        }
        if rule.ExitHost == "" {
            rule.ExitHost = r.DestIP
        }
        ruleID, err := db.CreateRule(tx, rule)
        if err != nil {
            sendRuleAckErr(ac, env.ID, fmt.Sprintf("迁移失败: %v", err))
            return
        }
        _ = ruleID
        hops := []db.HopInput{{NodeID: ac.nodeID, Mode: r.EffectiveMode(), DesiredPort: r.SrcPort}}
        if _, _, err := db.RegenerateRule(tx, rule, hops, nil); err != nil {
            sendRuleAckErr(ac, env.ID, fmt.Sprintf("迁移失败: %v", err))
            return
        }
    }

    if err := tx.Commit(); err != nil {
        sendRuleAckErr(ac, env.ID, err.Error())
        return
    }

    go h.onDispatch([]int64{ac.nodeID})

    ac.enqueueWrite(wsproto.Envelope{
        Type: wsproto.TypeRuleCmdAck,
        ID:   env.ID,
        Payload: mustMarshal(wsproto.RuleCmdAck{OK: true}),
    })
}
```

- [ ] **Step 4: Update readerLoop dispatch**

In `readerLoop()` switch (line ~218), add new cases and remove PanelSegmentEdit:

```go
case wsproto.TypeRuleCreate:
    h.handleRuleCreate(ac, env)
case wsproto.TypeRuleUpdate:
    h.handleRuleUpdate(ac, env)
case wsproto.TypeMigrateRules:
    h.handleMigrateRules(ac, env)
```

Remove the `case wsproto.TypePanelSegmentEdit:` block (lines 229-235).

- [ ] **Step 5: Remove applyPanelEdits method**

Delete the `applyPanelEdits()` function (lines 487-566) and the `node_tui_snapshot` references.

- [ ] **Step 6: Add onDispatch callback to Hub**

Hub needs a dispatch callback (set by Server). Add field:

```go
type Hub struct {
    // ... existing fields ...
    onDispatch func(nodeIDs []int64)  // server wires this to dispatchAfterFanout
}
```

Wire in `server.go` New():
```go
h.Hub.onDispatch = func(ids []int64) { s.dispatchAfterFanout(ids) }
```

- [ ] **Step 7: Run tests**

Run: `go test ./internal/server/...`

- [ ] **Step 8: Commit**

```bash
git add internal/server/hub.go internal/server/server.go
git commit -m "feat: add rule_create/update/migrate WS handlers, remove panel_segment_edit"
```

---

### Task 7: Server API — node toggle + owner endpoints

**Files:**
- Modify: `internal/server/api.go`
- Modify: `internal/server/server.go` (add route)

- [ ] **Step 1: Add apiToggleNode handler**

```go
func (s *Server) apiToggleNode(w http.ResponseWriter, r *http.Request) {
    id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
    if err := db.ToggleNode(s.db, id); err != nil {
        http.Error(w, err.Error(), 500)
        return
    }
    // Re-dispatch to apply/remove rules based on new disabled state
    go s.dispatchAfterMutation(id)
    writeJSON(w, map[string]bool{"ok": true})
}
```

- [ ] **Step 2: Add apiUpdateNodeOwner handler**

```go
func (s *Server) apiUpdateNodeOwner(w http.ResponseWriter, r *http.Request) {
    id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
    var body struct {
        OwnerID *int64 `json:"owner_id"`
    }
    json.NewDecoder(r.Body).Decode(&body)
    if err := db.UpdateNodeOwner(s.db, id, body.OwnerID); err != nil {
        http.Error(w, err.Error(), 500)
        return
    }
    writeJSON(w, map[string]bool{"ok": true})
}
```

- [ ] **Step 3: Register routes**

In `Router()` admin group:
```go
r.Post("/api/nodes/{id}/toggle", s.apiToggleNode)
r.Post("/api/nodes/{id}/owner", s.apiUpdateNodeOwner)
```

- [ ] **Step 4: Remove entry_port from apiCreateRule**

In `apiCreateRule`, make entry_port always 0 (auto-assign). Remove parsing of entry_port from request body.

- [ ] **Step 5: Update apiGetNode to include owner info**

Return `owner_id` and `owner_name` in node detail API response.

- [ ] **Step 6: Commit**

```bash
git add internal/server/api.go internal/server/server.go
git commit -m "feat: add node toggle/owner API, auto-assign entry port on create"
```

---

### Task 8: Daemon — rewrite to CRUD API

**Files:**
- Modify: `internal/daemon/handlers.go`
- Modify: `internal/daemon/dialer.go`

- [ ] **Step 1: Rewrite daemon HTTP handlers**

Replace segment-based endpoints with CRUD:

```go
func (d *Daemon) Handler() http.Handler {
    r := chi.NewRouter()
    r.Get("/v1/health", d.handleHealth)
    r.Get("/v1/status", d.handleStatus)
    r.Get("/v1/rules", d.handleListRules)
    r.Post("/v1/rules", d.handleCreateRule)
    r.Put("/v1/rules/{id}", d.handleUpdateRule)
    r.Delete("/v1/rules/{id}", d.handleDeleteRule)
    return r
}
```

- [ ] **Step 2: Implement handleStatus**

```go
func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
    dialer := d.Dialer()
    resp := struct {
        Connected bool   `json:"connected"`
        NodeName  string `json:"node_name,omitempty"`
        NodeID    int64  `json:"node_id,omitempty"`
    }{
        Connected: dialer != nil && dialer.IsConnected(),
    }
    if dialer != nil {
        resp.NodeName = dialer.NodeName()
        resp.NodeID = dialer.NodeID()
    }
    writeJSON(w, resp)
}
```

- [ ] **Step 3: Implement handleListRules**

```go
func (d *Daemon) handleListRules(w http.ResponseWriter, r *http.Request) {
    d.mu.RLock()
    defer d.mu.RUnlock()
    var rules []nft.Rule
    if d.Dialer() != nil {
        rules = d.owners["panel"]
    } else {
        rules = d.owners["tui"]
    }
    if rules == nil {
        rules = []nft.Rule{}
    }
    writeJSON(w, map[string]any{"rules": rules})
}
```

- [ ] **Step 4: Implement handleCreateRule**

Route to server (WS) or local based on connection state:

```go
func (d *Daemon) handleCreateRule(w http.ResponseWriter, r *http.Request) {
    var req struct {
        Proto      string `json:"proto"`
        ExitHost   string `json:"exit_host"`
        ExitPort   int    `json:"exit_port"`
        ListenPort int    `json:"listen_port"`
        Mode       string `json:"mode"`
        Comment    string `json:"comment"`
        Name       string `json:"name"`
    }
    json.NewDecoder(r.Body).Decode(&req)

    dialer := d.Dialer()
    if dialer != nil {
        ack, err := dialer.CreateRule(req)
        // return ack to TUI
    } else {
        // create locally in "tui" segment, auto-assign port, apply
    }
}
```

- [ ] **Step 5: Implement handleUpdateRule and handleDeleteRule**

Similar routing pattern. For update: connected → `rule_update` or `rule_hop_edit` (based on HopCount). Disconnected → local update.

For delete: connected → `rule_delete`. Disconnected → local remove.

- [ ] **Step 6: Implement local port auto-assign**

```go
func (d *Daemon) pickLocalFreePort(proto string) int {
    occupied := make(map[int]bool)
    for _, rules := range d.owners {
        for _, r := range rules {
            if r.Proto == proto || r.Proto == "tcp+udp" || proto == "tcp+udp" {
                occupied[r.SrcPort] = true
            }
        }
    }
    return db.PickFreePort(db.ChainPortMin, db.ChainPortMax, occupied)
}
```

- [ ] **Step 7: Implement migration flow in dialer**

In `dialer.go`, after hello_ack in `runOnce()`, replace panel_segment_edit sync with migration:

```go
// After hello_ack success, check for local rules to migrate
owners := cfg.GetState()
if tuiRules := owners["tui"]; len(tuiRules) > 0 {
    ack, err := d.sendCommand(ctx, wsproto.TypeMigrateRules,
        wsproto.MigrateRules{Rules: tuiRules})
    if err == nil && ack.OK {
        cfg.ClearTUISegment()
    }
}
```

- [ ] **Step 8: Implement downgrade flow**

In daemon startup (daemon.go or handlers.go init), check for downgrade:

```go
func (d *Daemon) checkDowngrade() {
    if d.dialerConfig != nil {
        return // still configured for server
    }
    panel := d.owners["panel"]
    if len(panel) == 0 {
        return
    }
    // Convert panel → tui
    for i := range panel {
        panel[i].RuleID = 0
        panel[i].RuleName = ""
        panel[i].OwnerName = ""
        panel[i].HopCount = 0
    }
    // Merge into tui, skip port conflicts
    d.owners["tui"] = mergeWithoutConflict(d.owners["tui"], panel)
    d.owners["panel"] = nil
    d.saveState()
}
```

- [ ] **Step 9: Add dialer command methods**

```go
func (d *Dialer) CreateRule(req wsproto.RuleCreate) (wsproto.RuleCmdAck, error) {
    return d.sendRuleCommand(wsproto.TypeRuleCreate, req)
}

func (d *Dialer) UpdateRule(req wsproto.RuleUpdate) (wsproto.RuleCmdAck, error) {
    return d.sendRuleCommand(wsproto.TypeRuleUpdate, req)
}

func (d *Dialer) MigrateRules(rules []nft.Rule) (wsproto.RuleCmdAck, error) {
    return d.sendRuleCommand(wsproto.TypeMigrateRules, wsproto.MigrateRules{Rules: rules})
}
```

Remove `NotifyPanelEdited()`, `panelCh`, and the `rulesToForwards()` helper.

- [ ] **Step 10: Run tests**

Run: `go test ./internal/daemon/...`

- [ ] **Step 11: Commit**

```bash
git add internal/daemon/
git commit -m "feat: rewrite daemon to unified CRUD API with server/local routing"
```

---

### Task 9: DaemonClient — rewrite to CRUD API

**Files:**
- Modify: `internal/daemonclient/client.go`
- Modify: `internal/daemonclient/client_test.go`

- [ ] **Step 1: Replace methods**

Remove: `GetRuleset`, `PostRuleset`, `RuleEdit`, `RuleDelete`

Add:

```go
type StatusResp struct {
    Connected bool   `json:"connected"`
    NodeName  string `json:"node_name,omitempty"`
    NodeID    int64  `json:"node_id,omitempty"`
}

type CreateRuleReq struct {
    Proto      string `json:"proto"`
    ExitHost   string `json:"exit_host"`
    ExitPort   int    `json:"exit_port"`
    ListenPort int    `json:"listen_port"`
    Mode       string `json:"mode"`
    Comment    string `json:"comment"`
    Name       string `json:"name"`
}

type CreateRuleResp struct {
    Entry      string `json:"entry"`
    ListenPort int    `json:"listen_port"`
}

type UpdateRuleReq struct {
    Proto      string `json:"proto"`
    ExitHost   string `json:"exit_host"`
    ExitPort   int    `json:"exit_port"`
    ListenPort int    `json:"listen_port"`
    Mode       string `json:"mode"`
    Comment    string `json:"comment"`
    Name       string `json:"name"`
}

func (c *Client) Status() (StatusResp, error)
func (c *Client) ListRules() ([]nft.Rule, error)
func (c *Client) CreateRule(req CreateRuleReq) (CreateRuleResp, error)
func (c *Client) UpdateRule(id string, req UpdateRuleReq) error
func (c *Client) DeleteRule(id string) error
```

- [ ] **Step 2: Update tests**

- [ ] **Step 3: Run tests**

Run: `go test ./internal/daemonclient/...`

- [ ] **Step 4: Commit**

```bash
git add internal/daemonclient/
git commit -m "feat: rewrite daemonclient to CRUD API"
```

---

### Task 10: TUI — model refactor + unified CRUD

**Files:**
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/form.go`
- Modify: `internal/tui/list.go`
- Modify: `internal/tui/view.go`
- Modify: `internal/tui/tui_test.go`

- [ ] **Step 1: Rewrite daemonClient interface**

```go
type daemonClient interface {
    Status() (daemonclient.StatusResp, error)
    ListRules() ([]nft.Rule, error)
    CreateRule(daemonclient.CreateRuleReq) (daemonclient.CreateRuleResp, error)
    UpdateRule(id string, daemonclient.UpdateRuleReq) error
    DeleteRule(id string) error
}
```

- [ ] **Step 2: Update model struct**

```go
type model struct {
    mode         viewMode
    rules        []nft.Rule
    cursor       int
    editingRule  nft.Rule  // replaces editingRuleID

    inputs       []textinput.Model
    focusedInput int
    protoIdx     int
    modeIdx      int

    status       string
    err          string
    width, height int

    client       daemonClient
    connected    bool
    nodeName     string
}
```

- [ ] **Step 3: Update lockedFields based on HopCount**

```go
func (m model) lockedFields() map[int]bool {
    if m.editingRule.HopCount > 1 {
        return map[int]bool{fProto: true, fDestIP: true, fDestPort: true}
    }
    return nil
}
```

- [ ] **Step 4: Update form — allow empty SrcPort**

In `commitForm()`, change SrcPort parsing to allow empty (0):

```go
srcPort := 0
if s := strings.TrimSpace(m.inputs[fSrcPort].Value()); s != "" {
    p, err := strconv.Atoi(s)
    if err != nil || p < 1 || p > 65535 {
        return m, "监听端口无效"
    }
    srcPort = p
}
```

- [ ] **Step 5: Rewrite commitAdd to use CreateRule**

```go
func (m model) commitAdd(...) (model, string) {
    resp, err := m.client.CreateRule(daemonclient.CreateRuleReq{
        Proto: proto, ExitHost: destInput, ExitPort: dp,
        ListenPort: srcPort, Mode: mode, Comment: comment,
    })
    if err != nil {
        return m, err.Error()
    }
    // Reload rules
    rules, _ := m.client.ListRules()
    m.rules = rules
    m.status = fmt.Sprintf("已创建 入口 %s", resp.Entry)
    return m, ""
}
```

- [ ] **Step 6: Rewrite commitEdit to use UpdateRule**

```go
func (m model) commitEdit(...) (model, string) {
    id := m.ruleID()  // helper: RuleID > 0 → strconv, else rule.ID
    err := m.client.UpdateRule(id, daemonclient.UpdateRuleReq{
        Proto: proto, ExitHost: destInput, ExitPort: dp,
        ListenPort: srcPort, Mode: mode, Comment: comment,
    })
    if err != nil {
        return m, err.Error()
    }
    rules, _ := m.client.ListRules()
    m.rules = rules
    return m, ""
}
```

- [ ] **Step 7: Rewrite delete to use DeleteRule**

In `updateConfirmDelete`, unify:

```go
case "y", "Y":
    id := m.ruleID()
    if err := m.client.DeleteRule(id); err != nil {
        m.err = err.Error()
    } else {
        rules, _ := m.client.ListRules()
        m.rules = rules
        m.status = "已删除"
    }
```

- [ ] **Step 8: Update view — status bar + HopCount display**

In `viewList()`, add connection status to title:

```go
suffix := "本地模式"
if m.connected {
    suffix = "已连接 " + m.nodeName
}
title := titleStyle.Render(fmt.Sprintf(" nft-forward ─── %s ", suffix))
```

Chain rule display: change `r.RuleID != 0` to `r.HopCount > 1` for the "链路" prefix.

- [ ] **Step 9: Update loadInitialRules**

```go
func loadInitialRules(client daemonClient) ([]nft.Rule, bool, string) {
    st, err := client.Status()
    if err != nil {
        return nil, false, ""
    }
    rules, err := client.ListRules()
    if err != nil {
        return nil, st.Connected, st.NodeName
    }
    return rules, st.Connected, st.NodeName
}
```

- [ ] **Step 10: Update tests — fakeDaemonClient**

Rewrite `fakeDaemonClient` to implement new interface.

- [ ] **Step 11: Run tests**

Run: `go test ./internal/tui/...`

- [ ] **Step 12: Commit**

```bash
git add internal/tui/
git commit -m "feat: rewrite TUI to unified CRUD with HopCount-based locking"
```

---

### Task 11: WebUI — node editing + rule simplification

**Files:**
- Modify: `web/src/pages/nodes/Detail.jsx`
- Modify: `web/src/pages/nodes/List.jsx`
- Modify: `web/src/pages/rules/List.jsx`
- Modify: `web/src/pages/rules/Detail.jsx`
- Modify: `web/src/pages/my/Rules.jsx`

- [ ] **Step 1: Node Detail — add toggle + owner**

Add disabled toggle button:
```jsx
<button onClick={() => api.post(`/nodes/${node.id}/toggle`).then(reload)}>
  {node.disabled ? '启用' : '禁用'}
</button>
```

Add owner display/selector:
```jsx
<Select value={node.owner_id} onChange={ownerID =>
  api.post(`/nodes/${node.id}/owner`, { owner_id: ownerID }).then(reload)
}>
  {users.map(u => <option key={u.id} value={u.id}>{u.username}</option>)}
</Select>
```

- [ ] **Step 2: Node List — add owner_id to create modal**

Add optional owner selector in AddNodeModal form.

- [ ] **Step 3: Rules List — remove entry_port from create, simplify node display**

In CreateRuleModal: remove `entry_port` field.

In table: remove NodeTypeBadge, show just node name text.

- [ ] **Step 4: Rules Detail — remove entry_port from edit, simplify node**

In EditRuleCard: remove `entry_port` field.

In entry info: show just node name (no badge).

- [ ] **Step 5: My Rules — simplify node display**

Remove NodeTypeBadge from table and CreateMyRuleModal.

- [ ] **Step 6: Test in browser**

Start dev server, verify:
- Node detail: toggle works, owner selector works
- Rule create: no entry_port field, auto-assigned
- Rule list: only node names, no badges
- My rules: same simplification

- [ ] **Step 7: Commit**

```bash
git add web/src/
git commit -m "feat: add node toggle/owner editing, simplify rule node display, auto-assign ports"
```

---

### Task 12: Integration — wire everything + test

- [ ] **Step 1: Fix any remaining compile errors**

Run: `go build ./...`

- [ ] **Step 2: Run full test suite**

Run: `go test ./...`

- [ ] **Step 3: Fix failing tests**

- [ ] **Step 4: Manual integration test**

Build binary, run daemon in local mode:
- Create rule with empty port → verify auto-assigned
- Edit rule → verify update
- Delete rule → verify removed

- [ ] **Step 5: Final commit**

```bash
git add -A
git commit -m "fix: integration fixes for TUI-server sync"
```
