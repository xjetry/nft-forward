package db

import (
	"strings"
	"testing"
)

// NodeTypeRemote、NodeTypeSelf、NodeTypeComposite 就是 nodes.node_type 上 CHECK 约束
// 允许的全部取值：三个常量各能写进一行，约束之外的值写不进去。常量与约束是分开写的
// 两处（Go 与迁移 SQL），这里把它们钉在一起。
func TestNodeTypeConstantsAreWhatTheSchemaAllows(t *testing.T) {
	d := openTestDB(t)
	insert := func(nodeType string) error {
		_, err := d.Exec(`INSERT INTO nodes(name, node_type, secret, port_range, created_at) VALUES (?, ?, '', '', 0)`,
			"type-"+nodeType, nodeType)
		return err
	}
	for _, nodeType := range []string{NodeTypeRemote, NodeTypeSelf, NodeTypeComposite} {
		if err := insert(nodeType); err != nil {
			t.Errorf("node_type %q 应能写入，实际报错：%v", nodeType, err)
		}
	}
	// “单点”是界面上对机器的叫法，不是一种取值。
	for _, nodeType := range []string{"single", "machine", ""} {
		err := insert(nodeType)
		if err == nil {
			t.Errorf("node_type %q 不在约束之列，应写不进去", nodeType)
		} else if !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("node_type %q 应被 CHECK 约束拒绝，实际报错：%v", nodeType, err)
		}
	}
}

// IsComposite 只对组合为真；两种机器（remote、self）都不是组合。
func TestIsCompositeSeparatesCompositesFromMachines(t *testing.T) {
	for nodeType, want := range map[string]bool{NodeTypeRemote: false, NodeTypeSelf: false, NodeTypeComposite: true} {
		if got := (&Node{NodeType: nodeType}).IsComposite(); got != want {
			t.Errorf("node_type %q：IsComposite 应为 %v，实际 %v", nodeType, want, got)
		}
	}
}
