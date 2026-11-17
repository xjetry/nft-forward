package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// The operations in this file each change several rows that only make sense
// together, and each is one transaction: it is applied in full or it leaves
// the database as it was. A caller that issued the same statements one by one
// would, when a later one failed, be left with the earlier ones in effect and
// only "it failed" to tell its own caller — a grant gone but its rules still
// forwarding, a composite node without members.

// inTx runs fn in a transaction, committed if fn returns nil and rolled back
// otherwise.
func inTx(d *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSettings stores every key in values, or none of them.
func SetSettings(d *sql.DB, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return inTx(d, func(tx *sql.Tx) error {
		for _, k := range keys {
			if err := SetSetting(tx, k, values[k]); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteUserAndRules deletes a user together with the rules they own, and
// returns the nodes those rules ran on, which have to be re-pushed.
func DeleteUserAndRules(d *sql.DB, userID int64) ([]int64, error) {
	var nodes []int64
	err := inTx(d, func(tx *sql.Tx) error {
		var err error
		if nodes, err = DeleteRulesForUser(tx, userID); err != nil {
			return err
		}
		return DeleteUser(tx, userID)
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// DeleteCompositeAndRules deletes a composite node together with every rule
// that runs through it, and returns the physical nodes those rules ran on,
// which have to be re-pushed.
func DeleteCompositeAndRules(d *sql.DB, nodeID int64) ([]int64, error) {
	var nodes []int64
	err := inTx(d, func(tx *sql.Tx) error {
		var err error
		if nodes, err = DeleteRulesUsingNode(tx, nodeID); err != nil {
			return err
		}
		return DeleteNode(tx, nodeID)
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// GrantNodes gives a user a grant on each of the nodes, as GrantNode would
// one by one, or on none of them.
func GrantNodes(d *sql.DB, userID int64, nodeIDs []int64, maxForwards int, trafficQuotaBytes int64) error {
	return inTx(d, func(tx *sql.Tx) error {
		for _, nid := range nodeIDs {
			if err := GrantNode(tx, userID, nid, maxForwards, trafficQuotaBytes); err != nil {
				return err
			}
		}
		return nil
	})
}

// A GrantKey names one user's grant on one node.
type GrantKey struct{ UserID, NodeID int64 }

// RevokeGrants removes each grant together with the rules that ran behind it,
// and returns the nodes those rules ran on, which have to be re-pushed. A
// revoked grant whose rules stayed would keep forwarding, and billing, for a
// user who no longer has the node.
func RevokeGrants(d *sql.DB, grants []GrantKey) ([]int64, error) {
	var nodes []int64
	err := inTx(d, func(tx *sql.Tx) error {
		var err error
		nodes, err = revokeGrants(tx, grants)
		return err
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

func revokeGrants(tx *sql.Tx, grants []GrantKey) ([]int64, error) {
	var nodes []int64
	for _, g := range grants {
		if err := RevokeNode(tx, g.UserID, g.NodeID); err != nil {
			return nil, err
		}
		ns, err := DeleteRulesForUserNode(tx, g.UserID, g.NodeID)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, ns...)
	}
	return nodes, nil
}

// UpdateNodeGrantRoster changes who holds a grant on one node: every user in
// add gets one capped at maxForwards rules, every user in remove loses theirs
// as RevokeGrants would take it. It returns the nodes to re-push.
func UpdateNodeGrantRoster(d *sql.DB, nodeID int64, add []int64, maxForwards int, remove []int64) ([]int64, error) {
	var nodes []int64
	err := inTx(d, func(tx *sql.Tx) error {
		for _, uid := range add {
			if err := GrantNode(tx, uid, nodeID, maxForwards, 0); err != nil {
				return err
			}
		}
		revoked := make([]GrantKey, len(remove))
		for i, uid := range remove {
			revoked[i] = GrantKey{UserID: uid, NodeID: nodeID}
		}
		var err error
		nodes, err = revokeGrants(tx, revoked)
		return err
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// RewireError is the error of an operation that was not applied because one
// of the rules it has to re-generate could not be. Err says why; when it is a
// ChainError the rule cannot run over the nodes as the operation would leave
// them, and otherwise a statement failed.
type RewireError struct {
	RuleID int64
	Err    error
}

func (e *RewireError) Error() string {
	return fmt.Sprintf("规则 #%d 无法重新接线: %v", e.RuleID, e.Err)
}

func (e *RewireError) Unwrap() error { return e.Err }

// rewireRules re-generates each rule over the hops it has now and returns the
// nodes to re-push. A hop's target is the next hop's address and port, so the
// stored hops go stale whenever a hop is removed or a node on the chain
// changes address.
//
// A rule that no longer exists, or has no hop left, is passed over: the ids
// are collected before the change that made the re-generation necessary, and
// that change can itself have removed the rule (rules.node_id cascades when
// the entry node is deleted).
func rewireRules(tx DBTX, ruleIDs []int64) ([]int64, error) {
	seen := map[int64]bool{}
	var nodes []int64
	for _, id := range ruleIDs {
		r, err := GetRule(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, &RewireError{RuleID: id, Err: err}
		}
		hops, err := ListRuleHops(tx, id)
		if err != nil {
			return nil, &RewireError{RuleID: id, Err: err}
		}
		if len(hops) == 0 {
			continue
		}
		_, _, affected, err := RegenerateRule(tx, r, StoredHopInputs(hops), nil)
		if err != nil {
			return nil, &RewireError{RuleID: id, Err: err}
		}
		for _, n := range affected {
			if !seen[n] {
				seen[n] = true
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, nil
}

// StoredHopInputs 把一条规则落库的跳原样变成 RegenerateRule 的输入：每一跳的节点、
// 模式与所属的逻辑节点照抄，端口不给。没给端口的跳，RegenerateRule 沿用该节点上这条
// 规则现有的监听端口——只要它仍在节点的端口范围内、没被别的跳占用，否则另挑一个。
func StoredHopInputs(hops []*RuleHop) []HopInput {
	inputs := make([]HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = HopInput{NodeID: h.NodeID, Mode: h.Mode, LogicalNodeID: h.LogicalNodeID}
	}
	return inputs
}

// DeleteNodeAndRewire deletes a physical node and splices it out of every
// rule it was a hop of: its hop rows go with it (rule_hops.node_id cascades),
// and each such rule is re-generated over the hops that remain. It returns
// the nodes to re-push. If a rule cannot be re-generated the error is a
// *RewireError and the node is not deleted.
func DeleteNodeAndRewire(d *sql.DB, nodeID int64) ([]int64, error) {
	var nodes []int64
	err := inTx(d, func(tx *sql.Tx) error {
		// Before the delete: afterwards the hop rows that name the rules are
		// gone.
		ruleIDs, err := RulesReferencingNode(tx, nodeID)
		if err != nil {
			return err
		}
		if err := DeleteNode(tx, nodeID); err != nil {
			return err
		}
		nodes, err = rewireRules(tx, ruleIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// SetNodeRelayHostAndRewire stores a node's relay address (empty clears it)
// and re-generates every rule with a hop on the node, since the hop before it
// targets that address. It returns the nodes to re-push. If a rule cannot be
// re-generated — every hop node needs a relay address, so clearing one that
// rules run over is such a case — the error is a *RewireError and the address
// is not changed.
func SetNodeRelayHostAndRewire(d *sql.DB, nodeID int64, relayHost string) ([]int64, error) {
	var nodes []int64
	err := inTx(d, func(tx *sql.Tx) error {
		if err := UpdateNodeRelayHost(tx, nodeID, relayHost); err != nil {
			return err
		}
		ruleIDs, err := RulesReferencingNode(tx, nodeID)
		if err != nil {
			return err
		}
		nodes, err = rewireRules(tx, ruleIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

// StaleRulesIntoNode returns the rules in which the hop before one on the
// node does not target the node's stored relay address.
//
// RegenerateRule writes every hop but the last with the next hop's node's
// relay_host as its target, so on rules generated against the address the
// node has now this finds nothing. It finds a rule when the address was
// written without the rule being re-generated.
func StaleRulesIntoNode(d DBTX, nodeID int64) ([]int64, error) {
	return queryInt64s(d, `SELECT DISTINCT cur.rule_id
		FROM rule_hops cur
		JOIN rule_hops prev ON prev.rule_id = cur.rule_id AND prev.position = cur.position - 1
		JOIN nodes n ON n.id = cur.node_id
		WHERE cur.node_id = ? AND prev.target_host != n.relay_host
		ORDER BY cur.rule_id`, nodeID)
}

// RewireRulesIntoNode re-generates the rules that are stale against the
// node's stored relay address (StaleRulesIntoNode), and returns the nodes to
// re-push.
//
// It is for an address the panel does not get to refuse: one a node reports
// about itself. The address is stored first, by the caller, and the rules
// follow here. Each rule is re-generated in a transaction of its own, so a
// rule is left either as it was or re-generated in full, and one that cannot
// be re-generated does not hold back the others.
//
// The error joins a *RewireError for every rule that was not re-generated;
// the returned nodes are those of the rules that were, whether or not the
// error is nil. Nothing has to remember a rule that failed: it still does
// not target the stored address, so the next call finds it again.
func RewireRulesIntoNode(d *sql.DB, nodeID int64) ([]int64, error) {
	stale, err := StaleRulesIntoNode(d, nodeID)
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var nodes []int64
	var failed []error
	for _, id := range stale {
		var affected []int64
		err := inTx(d, func(tx *sql.Tx) error {
			var err error
			affected, err = rewireRules(tx, []int64{id})
			return err
		})
		if err != nil {
			var rewire *RewireError
			if !errors.As(err, &rewire) {
				err = &RewireError{RuleID: id, Err: err}
			}
			failed = append(failed, err)
			continue
		}
		for _, n := range affected {
			if !seen[n] {
				seen[n] = true
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, errors.Join(failed...)
}
