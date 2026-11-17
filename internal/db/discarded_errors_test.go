package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const dbImportPath = "nft-forward/internal/db"

// errorReturningFuncs parses this package's non-test sources and returns the
// names of its package-level functions whose last result is an error.
func errorReturningFuncs(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Type.Results == nil {
					continue
				}
				results := fn.Type.Results.List
				if id, ok := results[len(results)-1].Type.(*ast.Ident); ok && id.Name == "error" {
					out[fn.Name.Name] = true
				}
			}
		}
	}
	return out
}

// sqlMethods are the database/sql methods whose error says whether a
// statement ran or what it returned. Rollback and Close are left out on
// purpose: they are called where the outcome is already decided — after a
// failure, or after the rows were read and rows.Err checked.
var sqlMethods = map[string]bool{
	"Exec": true, "ExecContext": true,
	"Query": true, "QueryContext": true,
	"Begin": true, "BeginTx": true, "Commit": true,
	"RowsAffected": true, "LastInsertId": true,
}

// databaseCall reports whether e is a call whose error comes from the
// database, and names it. The match is by shape, not by type:
//
//   - a function of this package whose last result is an error — db.F(...)
//     through the importing file's name for the package, F(...) inside it;
//   - a method named like one of sqlMethods;
//   - a Scan with arguments (a row's Scan; bufio's takes none).
//
// Identifiers that name a package with a same-named function (os/exec,
// syscall, fmt) are excluded so that exec.Command(...) style calls and
// fmt.Scan do not match.
func databaseCall(e ast.Expr, dbAlias string, inDB bool, returnsError map[string]bool) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if inDB && returnsError[fn.Name] {
			return fn.Name, true
		}
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			if dbAlias != "" && x.Name == dbAlias {
				if returnsError[fn.Sel.Name] {
					return dbAlias + "." + fn.Sel.Name, true
				}
				return "", false
			}
			switch x.Name {
			case "exec", "syscall", "fmt", "os":
				return "", false
			}
		}
		if sqlMethods[fn.Sel.Name] {
			return "(sql)." + fn.Sel.Name, true
		}
		if fn.Sel.Name == "Scan" && len(call.Args) > 0 {
			return "(sql).Scan", true
		}
	}
	return "", false
}

// discardedDatabaseErrors lists every place in the file where a database call
// has its error thrown away: assigned to the blank identifier, or made as a
// bare statement (including defer and go).
func discardedDatabaseErrors(fset *token.FileSet, file *ast.File, returnsError map[string]bool) []string {
	inDB := file.Name.Name == "db"
	alias := ""
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == dbImportPath {
			alias = "db"
			if imp.Name != nil {
				alias = imp.Name.Name
			}
		}
	}
	if alias == "_" {
		alias = ""
	}
	var found []string
	flag := func(pos token.Pos, name string) {
		p := fset.Position(pos)
		found = append(found, p.Filename+":"+strconv.Itoa(p.Line)+": "+name)
	}
	check := func(pos token.Pos, e ast.Expr) {
		if name, ok := databaseCall(e, alias, inDB, returnsError); ok {
			flag(pos, name)
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) != 1 {
				return true
			}
			if last, ok := s.Lhs[len(s.Lhs)-1].(*ast.Ident); ok && last.Name == "_" {
				check(s.Pos(), s.Rhs[0])
			}
		case *ast.ExprStmt:
			check(s.Pos(), s.X)
		case *ast.DeferStmt:
			check(s.Pos(), s.Call)
		case *ast.GoStmt:
			check(s.Pos(), s.Call)
		}
		return true
	})
	return found
}

// A database error that is thrown away turns a failed query into an empty
// result and a failed write into a success. Either way the code after it —
// and the client behind it — acts on something that is not true: an empty
// list is shown as "there are none", a write that did not happen is reported
// as done. So every such error must be looked at: returned, reported to the
// client, or logged where there is no one to report to.
//
// This walks the syntax of every non-test Go file in the repository. It sees
// an error dropped at the call (see discardedDatabaseErrors); it does not see
// one that is assigned to a variable and then not acted on.
func TestNoDatabaseErrorIsDiscarded(t *testing.T) {
	returnsError := errorReturningFuncs(t)
	if len(returnsError) == 0 {
		t.Fatal("found no error-returning functions in this package; the check would pass vacuously")
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var found []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", ".worktrees", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		found = append(found, discardedDatabaseErrors(fset, file, returnsError)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files; the check would pass vacuously")
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Fatalf("%d database call(s) have their error discarded:\n%s", len(found), strings.Join(found, "\n"))
	}
}
