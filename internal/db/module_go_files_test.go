package db

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writeTree 在 root 下建出 rels 里的每个文件（连同父目录），内容无关紧要。
func writeTree(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// moduleGoFiles 的每条排除规则各由一个只违反这一条的文件钉住：去掉哪一条，哪个文件
// 就出现在结果里。最后两个是真实出现过的情形——仓库根下 .claude/worktrees 里的工作
// 树——它同时被“点开头的目录”和“另一个模块”两条排除。
func TestModuleGoFilesAreTheOnesTheGoToolCounts(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root,
		"go.mod",
		"internal/db/rows.go",
		"cmd/tool/main.go",

		"internal/db/rows_test.go",            // 测试文件
		"internal/db/testdata/fixture.go",     // testdata 目录
		"internal/_scratch/draft.go",          // 下划线开头的目录
		"internal/db/_disabled.go",            // 下划线开头的文件
		"internal/db/.hidden.go",              // 点开头的文件
		".cache/tool/gen.go",                  // 点开头的目录，不是模块
		"web/node_modules/dep/dep.go",         // 前端依赖
		"checkouts/other/go.mod",              // 另一个模块，不在点开头的目录里
		"checkouts/other/internal/db/rows.go", //
		".claude/worktrees/agent/go.mod",
		".claude/worktrees/agent/internal/db/rows.go",
	)

	paths, err := moduleGoFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"cmd/tool/main.go": true, "internal/db/rows.go": true}
	var unexpected []string
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if !want[rel] {
			unexpected = append(unexpected, rel)
		}
		delete(want, rel)
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("不属于本模块的文件被算了进去: %s", strings.Join(unexpected, ", "))
	}
	for rel := range want {
		t.Errorf("本模块的文件没被算进去: %s", rel)
	}
}

// 给的目录不是模块根（没有 go.mod）时不扫描：扫出来的不会是本模块的文件。
func TestModuleGoFilesRefusesADirectoryThatIsNotAModuleRoot(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "internal/db/rows.go")
	paths, err := moduleGoFiles(root)
	if err == nil {
		t.Fatalf("没有 go.mod 的目录应被拒绝，实际扫出 %v", paths)
	}
	if !strings.Contains(err.Error(), "not a module root") {
		t.Errorf("报错应说明它不是模块根，实际：%v", err)
	}
}

// 检查扫描的就是本仓库这个模块：从这个包所在的位置往上两级是模块根，扫到的文件里有
// 本包自己的源码，也有 server 包的——“数据库错误不许丢”的两项检查看的正是它们。
func TestModuleGoFilesCoversThisRepository(t *testing.T) {
	paths, err := moduleGoFiles(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, path := range paths {
		seen[filepath.ToSlash(path)] = true
	}
	for _, want := range []string{"../../internal/db/queries.go", "../../internal/server/api.go", "../../cmd/nft-server/main.go"} {
		if !seen[want] {
			t.Errorf("应扫到 %s，实际没有（共扫到 %d 个文件）", want, len(paths))
		}
	}
}
