package db

import (
	"fmt"
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

// dbAliasIn returns the name file uses for this package, "" if it does not
// import it by a usable name.
func dbAliasIn(file *ast.File) string {
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
		return ""
	}
	return alias
}

// eachGoFile parses every non-test Go file of the repository and hands it to
// visit. It fails the test if it finds none.
func eachGoFile(t *testing.T, visit func(fset *token.FileSet, file *ast.File)) {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
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
		visit(fset, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files; the check would pass vacuously")
	}
}

// discardedDatabaseErrors lists every place in the file where a database call
// has its error thrown away: assigned to the blank identifier, or made as a
// bare statement (including defer and go).
func discardedDatabaseErrors(fset *token.FileSet, file *ast.File, returnsError map[string]bool) []string {
	inDB := file.Name.Name == "db"
	alias := dbAliasIn(file)
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
// an error dropped at the call (see discardedDatabaseErrors). An error that
// is assigned and tested, but not looked at where it is handled, is
// TestNoDatabaseErrorIsHandledUnexamined's.
func TestNoDatabaseErrorIsDiscarded(t *testing.T) {
	returnsError := errorReturningFuncs(t)
	if len(returnsError) == 0 {
		t.Fatal("found no error-returning functions in this package; the check would pass vacuously")
	}
	var found []string
	eachGoFile(t, func(fset *token.FileSet, file *ast.File) {
		found = append(found, discardedDatabaseErrors(fset, file, returnsError)...)
	})
	sort.Strings(found)
	if len(found) > 0 {
		t.Fatalf("%d database call(s) have their error discarded:\n%s", len(found), strings.Join(found, "\n"))
	}
}

// unexaminedByDesign names the functions that may handle a failed database
// call without looking at the error, each with why that loses nothing.
var unexaminedByDesign = map[string]string{
	"nodeLabel": "names a node inside an error message its caller is already returning; a name that cannot be read becomes the node's id",
}

// unexaminedDatabaseErrors lists every place in the file where a database
// call's error is tested against nil and the branch for "not nil" never
// mentions it:
//
//	u, err := GetUserByID(d, id)
//	if err != nil {
//		return notFound
//	}
//
// Each entry is "function\tfile:line: call". The error variable is taken
// from the call's own statement — the if's init, or the statement just
// before the if.
func unexaminedDatabaseErrors(fset *token.FileSet, file *ast.File, returnsError map[string]bool) []string {
	inDB := file.Name.Name == "db"
	alias := dbAliasIn(file)
	// errorOf: when stmt is `..., e := <database call>`, e and the call.
	errorOf := func(stmt ast.Stmt) (name, call string, ok bool) {
		assign, isAssign := stmt.(*ast.AssignStmt)
		if !isAssign || len(assign.Rhs) != 1 {
			return "", "", false
		}
		call, ok = databaseCall(assign.Rhs[0], alias, inDB, returnsError)
		if !ok {
			return "", "", false
		}
		id, isIdent := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
		if !isIdent || id.Name == "_" {
			return "", "", false
		}
		return id.Name, call, true
	}
	mentions := func(n ast.Node, name string) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			if id, ok := x.(*ast.Ident); ok && id.Name == name {
				found = true
			}
			return !found
		})
		return found
	}
	testsNotNil := func(cond ast.Expr, name string) bool {
		found := false
		ast.Inspect(cond, func(x ast.Node) bool {
			if b, ok := x.(*ast.BinaryExpr); ok && b.Op == token.NEQ {
				l, lok := b.X.(*ast.Ident)
				r, rok := b.Y.(*ast.Ident)
				if lok && rok && l.Name == name && r.Name == "nil" {
					found = true
				}
			}
			return !found
		})
		return found
	}
	var found []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			var list []ast.Stmt
			switch block := n.(type) {
			case *ast.BlockStmt:
				list = block.List
			case *ast.CaseClause:
				list = block.Body
			case *ast.CommClause:
				list = block.Body
			default:
				return true
			}
			for i, stmt := range list {
				ifStmt, ok := stmt.(*ast.IfStmt)
				if !ok {
					continue
				}
				var name, call string
				have := false
				if ifStmt.Init != nil {
					name, call, have = errorOf(ifStmt.Init)
				}
				if !have && i > 0 {
					name, call, have = errorOf(list[i-1])
				}
				if have && testsNotNil(ifStmt.Cond, name) && !mentions(ifStmt.Body, name) {
					p := fset.Position(ifStmt.Pos())
					found = append(found, fn.Name.Name+"\t"+p.Filename+":"+strconv.Itoa(p.Line)+": "+call)
				}
			}
			return true
		})
	}
	return found
}

// A branch that runs when a database call failed, and does not look at the
// error, gives one answer to every failure. That answer is right for at most
// one of them. "Not found" is the usual one: right when the row is not there,
// and wrong when the query could not be made, where it sends whoever reads it
// looking for a row that exists. The same goes for "no access", "already
// exists", a default value, a skipped item.
//
// So the branch has to mention the error: to tell the cases apart
// (errors.Is(err, sql.ErrNoRows)), to pass it on, or to log it. This walks
// the syntax of every non-test Go file in the repository; it sees the shape
// in unexaminedDatabaseErrors and cannot tell whether the mention is a
// sufficient one.
func TestNoDatabaseErrorIsHandledUnexamined(t *testing.T) {
	returnsError := errorReturningFuncs(t)
	if len(returnsError) == 0 {
		t.Fatal("found no error-returning functions in this package; the check would pass vacuously")
	}
	var found []string
	exempted := map[string]bool{}
	eachGoFile(t, func(fset *token.FileSet, file *ast.File) {
		for _, entry := range unexaminedDatabaseErrors(fset, file, returnsError) {
			fn, where, _ := strings.Cut(entry, "\t")
			if _, ok := unexaminedByDesign[fn]; ok {
				exempted[fn] = true
				continue
			}
			found = append(found, where+" (in "+fn+")")
		}
	})
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("%d database call(s) have their failure handled without the error being looked at:\n%s", len(found), strings.Join(found, "\n"))
	}
	for fn := range unexaminedByDesign {
		if !exempted[fn] {
			t.Errorf("unexaminedByDesign names %s, where nothing of the kind is found any more", fn)
		}
	}
}

// uncheckedResults lists the results of a Query in file whose Err is never
// called in the function that holds them, as "function\tposition".
func uncheckedResults(fset *token.FileSet, file *ast.File) (unchecked []string, results int) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		held := map[string]token.Pos{}
		checked := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Lhs) == 0 || len(n.Rhs) != 1 {
					return true
				}
				call, ok := n.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Query" && sel.Sel.Name != "QueryContext") {
					return true
				}
				if id, ok := n.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
					held[id.Name] = call.Pos()
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Err" {
					if id, ok := sel.X.(*ast.Ident); ok {
						checked[id.Name] = true
					}
				}
			}
			return true
		})
		results += len(held)
		for name, pos := range held {
			if !checked[name] {
				p := fset.Position(pos)
				unchecked = append(unchecked, fmt.Sprintf("%s\t%s:%d: %s is read without %s.Err() being checked", fn.Name.Name, p.Filename, p.Line, name, name))
			}
		}
	}
	return unchecked, results
}

// TestEveryQueryResultIsCheckedForAnErrorAfterReading: reading a result can
// fail partway, and rows.Next then just returns false — the same as at the
// end of the result. Only rows.Err tells the two apart; a function that does
// not ask goes on with the rows it got as if they were all there are.
//
// The statement sweep fails fetches too (faultAt.fetch), but cannot tell an
// unchecked result from a checked one where the result is empty anyway: no
// rows is what the caller ends up with either way. This check does not
// depend on what the result holds.
func TestEveryQueryResultIsCheckedForAnErrorAfterReading(t *testing.T) {
	var found []string
	results := 0
	eachGoFile(t, func(fset *token.FileSet, file *ast.File) {
		unchecked, n := uncheckedResults(fset, file)
		results += n
		for _, entry := range unchecked {
			fn, where, _ := strings.Cut(entry, "\t")
			found = append(found, where+" (in "+fn+")")
		}
	})
	if results == 0 {
		t.Fatal("found no Query result in the tree; the check would pass vacuously")
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("%d query result(s) are read without their error being checked:\n%s", len(found), strings.Join(found, "\n"))
	}
}
