package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
)

// stripANSI removes ANSI color/style escape sequences from s.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// fakeDaemonClient stubs the daemonClient interface for TUI tests so
// they don't have to spin up a real daemon over unix sockets.
type fakeDaemonClient struct {
	owners      daemonclient.OwnerRuleset
	postedOwner string
	postedRules []nft.Rule
	postErr     error

	chainEdits []struct {
		ChainID    int64
		ListenPort int
		Mode       string
		Comment    string
	}
	chainDeletes []int64
	chainErr     error
}

func (f *fakeDaemonClient) GetRuleset() (daemonclient.OwnerRuleset, error) {
	if f.owners == nil {
		return daemonclient.OwnerRuleset{}, nil
	}
	return f.owners, nil
}

func (f *fakeDaemonClient) PostRuleset(owner string, rules []nft.Rule) error {
	if f.postErr != nil {
		return f.postErr
	}
	f.postedOwner = owner
	f.postedRules = append([]nft.Rule(nil), rules...)
	return nil
}

func (f *fakeDaemonClient) ChainEdit(chainID int64, listenPort int, mode, comment string) error {
	f.chainEdits = append(f.chainEdits, struct {
		ChainID    int64
		ListenPort int
		Mode       string
		Comment    string
	}{chainID, listenPort, mode, comment})
	return f.chainErr
}

func (f *fakeDaemonClient) ChainDelete(chainID int64) error {
	f.chainDeletes = append(f.chainDeletes, chainID)
	return f.chainErr
}

// fixedPortion returns the byte-prefix of s whose lipgloss display width equals
// exactly targetCells.
func fixedPortion(s string, targetCells int) string {
	acc := 0
	for i, r := range s {
		rw := lipgloss.Width(string(r))
		if acc+rw > targetCells {
			return s[:i]
		}
		acc += rw
		if acc == targetCells {
			return s[:i+len(string(r))]
		}
	}
	return s
}

func TestRenderTableRow_ColumnAlignment(t *testing.T) {
	header := renderTableRow("协议", "本机端口", "目标", "远程端口", "备注")
	headerPlain := stripANSI(header)

	rows := []struct {
		proto, port, dest, dstPort, comment string
	}{
		{"tcp", "8088", "100.100.100.11", "8088", "T"},
		{"tcp", "10010", "127.0.0.1", "10011", "local"},
		{"udp", "53", "8.8.8.8", "53", "dns"},
		{"tcp", "9999", "very-long-hostname.example.com", "12345", "overflow"},
	}

	expectedFixedCells := colProto + colSrcPort + colDest + colDstPort
	hFixed := fixedPortion(headerPlain, expectedFixedCells)
	if lipgloss.Width(hFixed) != expectedFixedCells {
		t.Fatalf("header fixed-column display width = %d, want %d",
			lipgloss.Width(hFixed), expectedFixedCells)
	}

	for _, row := range rows {
		line := renderTableRow(row.proto, row.port, row.dest, row.dstPort, row.comment)
		linePlain := stripANSI(line)
		rFixed := fixedPortion(linePlain, expectedFixedCells)
		if lipgloss.Width(rFixed) != expectedFixedCells {
			t.Errorf("row dest=%q: fixed-column width = %d, want %d",
				row.dest, lipgloss.Width(rFixed), expectedFixedCells)
		}
	}
}

func TestRenderTableRow_TruncationEllipsis(t *testing.T) {
	longDest := strings.Repeat("x", 100)
	line := renderTableRow("tcp", "80", longDest, "8080", "")
	plain := stripANSI(line)

	afterProtoPort := fixedPortion(plain, colProto+colSrcPort)
	destAndRest := plain[len(afterProtoPort):]
	destCellStr := fixedPortion(destAndRest, colDest)

	if !strings.Contains(destCellStr, "…") {
		t.Errorf("expected ellipsis in truncated dest cell, got: %q", destCellStr)
	}
	if lipgloss.Width(destCellStr) != colDest {
		t.Errorf("dest cell width = %d, want %d", lipgloss.Width(destCellStr), colDest)
	}
}

func TestTruncateCell(t *testing.T) {
	cases := []struct {
		input    string
		maxCells int
		truncate bool
	}{
		{"hello", 10, false},
		{"hello world!", 8, true},
		{"你好世界测试", 6, true},
	}
	for _, c := range cases {
		got := truncateCell(c.input, c.maxCells)
		runes := []rune(got)
		if !c.truncate {
			if got != c.input {
				t.Errorf("truncateCell(%q, %d) = %q, want no change", c.input, c.maxCells, got)
			}
		} else {
			if len(runes) == 0 || string(runes[len(runes)-1]) != "…" {
				t.Errorf("truncateCell(%q, %d) = %q, want ellipsis suffix", c.input, c.maxCells, got)
			}
			if lipgloss.Width(got) > c.maxCells {
				t.Errorf("truncateCell(%q, %d) width %d > maxCells", c.input, c.maxCells, lipgloss.Width(got))
			}
		}
	}
}

func TestProtoSelectorCycles(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil)
	m.enterAddMode()
	if m.protoIdx != 0 {
		t.Fatalf("expected protoIdx=0, got %d", m.protoIdx)
	}
	next, _ := m.updateAdd(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.protoIdx != 1 {
		t.Fatalf("after right: expected 1, got %d", m.protoIdx)
	}
	next, _ = m.updateAdd(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.protoIdx != 2 {
		t.Fatalf("after 2nd right: expected 2, got %d", m.protoIdx)
	}
	next, _ = m.updateAdd(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.protoIdx != 0 {
		t.Fatalf("after wrap: expected 0, got %d", m.protoIdx)
	}
	next, _ = m.updateAdd(tea.KeyMsg{Type: tea.KeyLeft})
	m = next.(model)
	if m.protoIdx != 2 {
		t.Fatalf("after left wrap: expected 2, got %d", m.protoIdx)
	}
}

func TestProtoSelectorEditPreFill(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		{Proto: "tcp+udp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80},
		{Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.1", DestPort: 443},
	}
	for _, r := range rules {
		m := initialModel(&fakeDaemonClient{}, rules)
		for i, rule := range rules {
			if rule.Proto == r.Proto && rule.SrcPort == r.SrcPort {
				m.cursor = i
			}
		}
		m.enterEditMode()
		expected := -1
		for i, p := range protoOptions {
			if p == r.Proto {
				expected = i
				break
			}
		}
		if m.protoIdx != expected {
			t.Errorf("proto=%q: expected protoIdx=%d, got %d", r.Proto, expected, m.protoIdx)
		}
	}
}

func TestProtoSelectorRenderContainsOptions(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil)
	m.enterAddMode()
	view := m.renderProtoSelector()
	plain := stripANSI(view)
	for _, opt := range protoOptions {
		if !strings.Contains(plain, opt) {
			t.Errorf("selector render missing option %q", opt)
		}
	}
	if !strings.Contains(plain, "[ tcp ]") {
		t.Errorf("active option should be '[ tcp ]', got: %q", plain)
	}
}

func TestColProtoFitsLongestOption(t *testing.T) {
	if colProto < lipgloss.Width("TCP+UDP") {
		t.Errorf("colProto too narrow for TCP+UDP")
	}
}

func TestRenderTableRow_FiveColumns(t *testing.T) {
	const expectedFixed = colProto + colSrcPort + colDest + colDstPort

	cases := []struct {
		rule        nft.Rule
		wantDstPort string
		desc        string
	}{
		{nft.Rule{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.1", DestPort: 9000, Comment: "short-host"}, "9000", "IPv4"},
		{nft.Rule{Proto: "udp", SrcPort: 1234, DestIP: "192.168.1.1", DestPort: 65535, Comment: "max-port"}, "65535", "max port"},
		{nft.Rule{Proto: "tcp+udp", SrcPort: 443, DestHost: "my.ddns.example.com", DestIP: "203.0.113.5", DestPort: 8443, Comment: "ddns"}, "8443", "DDNS"},
	}

	for _, c := range cases {
		r := c.rule
		destHost := r.DestIP
		if r.DestHost != "" {
			destHost = r.DestHost
		}
		line := renderTableRow(strings.ToLower(r.Proto), strconv.Itoa(r.SrcPort), destHost, strconv.Itoa(r.DestPort), r.Comment)
		plain := stripANSI(line)
		rFixed := fixedPortion(plain, expectedFixed)
		if lipgloss.Width(rFixed) != expectedFixed {
			t.Errorf("[%s] fixed-column width = %d, want %d", c.desc, lipgloss.Width(rFixed), expectedFixed)
		}
		afterThree := fixedPortion(plain, colProto+colSrcPort+colDest)
		dstPortCell := strings.TrimSpace(fixedPortion(plain[len(afterThree):], colDstPort))
		if dstPortCell != c.wantDstPort {
			t.Errorf("[%s] dstPort cell = %q, want %q", c.desc, dstPortCell, c.wantDstPort)
		}
	}
}

func TestViewList_ColumnConsistency(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 8088, DestIP: "100.100.100.11", DestPort: 8088, Comment: "T"},
		{Proto: "tcp", SrcPort: 10010, DestIP: "127.0.0.1", DestPort: 10011, Comment: "local"},
		{Proto: "udp", SrcPort: 53, DestHost: "dns.example.com", DestIP: "8.8.8.8", DestPort: 53, Comment: "dns-forward"},
	}

	header := renderTableRow("协议", "本机端口", "目标", "远程端口", "备注")
	headerPlain := stripANSI(header)
	expectedFixed := colProto + colSrcPort + colDest + colDstPort

	if lipgloss.Width(fixedPortion(headerPlain, expectedFixed)) != expectedFixed {
		t.Fatalf("header fixed width wrong")
	}

	for _, r := range rules {
		destHost := r.DestIP
		if r.DestHost != "" {
			destHost = r.DestHost
		}
		line := renderTableRow(strings.ToLower(r.Proto), fmt.Sprintf("%d", r.SrcPort), destHost, fmt.Sprintf("%d", r.DestPort), r.Comment)
		plain := stripANSI(line)
		rFixed := fixedPortion(plain, expectedFixed)
		if lipgloss.Width(rFixed) != expectedFixed {
			t.Errorf("rule %s/%d: fixed-column width = %d, want %d",
				r.Proto, r.SrcPort, lipgloss.Width(rFixed), expectedFixed)
		}
	}
}

func TestCommitPostsToPanel(t *testing.T) {
	fake := &fakeDaemonClient{}
	rules := []nft.Rule{{Proto: "tcp", SrcPort: 80, DestHost: "home.example.com", DestPort: 80}}
	applied, err := commit(fake, rules)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if fake.postedOwner != "panel" {
		t.Errorf("owner = %q, want panel", fake.postedOwner)
	}
	if len(applied) != 1 || applied[0].DestHost != "home.example.com" {
		t.Errorf("expected raw rule with DestHost set, got %+v", applied)
	}
}

func TestSubmitAdd_CarriesUserspaceMode(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, nil)
	m.enterAddMode()
	m.protoIdx = 0
	m.modeIdx = 1
	m.inputs[fSrcPort].SetValue("8443")
	m.inputs[fDestIP].SetValue("10.0.0.1")
	m.inputs[fDestPort].SetValue("443")
	nm, _ := m.submitAdd()
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if len(mm.rules) != 1 || mm.rules[0].Mode != nft.ModeUserspace {
		t.Fatalf("rule should carry userspace mode: %+v", mm.rules)
	}
}

func TestSubmitAdd_RejectsUDPUserspace(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, nil)
	m.enterAddMode()
	m.protoIdx = 1
	m.modeIdx = 1
	m.inputs[fSrcPort].SetValue("8443")
	m.inputs[fDestIP].SetValue("10.0.0.1")
	m.inputs[fDestPort].SetValue("443")
	nm, _ := m.submitAdd()
	if nm.(model).err == "" {
		t.Fatal("expected validation error for udp+userspace")
	}
}

func TestViewListRendersUnifiedRules(t *testing.T) {
	m := model{
		mode:  viewList,
		width: 140,
		rules: []nft.Rule{
			{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
			{Proto: "tcp", SrcPort: 17171, DestIP: "72.234.229.145", DestPort: 17171, Comment: "ss"},
		},
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "10.0.0.1") || !strings.Contains(out, "72.234.229.145") {
		t.Fatalf("all rules should be rendered:\n%s", out)
	}
}

func TestViewListClampsCommentWidthOnNarrowTerminal(t *testing.T) {
	m := model{
		mode:  viewList,
		width: 40,
		rules: []nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}},
	}
	_ = m.View() // must not panic
}

func TestLoadInitialRulesMergesSegments(t *testing.T) {
	fc := &fakeDaemonClient{owners: daemonclient.OwnerRuleset{
		"tui":   {{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
		"panel": {{Proto: "tcp", SrcPort: 200, DestIP: "10.0.0.2", DestPort: 200, ChainID: 5, ChainName: "seednet-vless"}},
	}}
	rules, err := loadInitialRules(fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 merged rules, got %d", len(rules))
	}
}

func TestRowAtReturnsRule(t *testing.T) {
	m := model{
		rules: []nft.Rule{
			{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
			{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
		},
	}
	if m.totalRows() != 2 {
		t.Fatalf("totalRows = %d, want 2", m.totalRows())
	}
	if r := m.rowAt(0); r.SrcPort != 100 {
		t.Fatalf("row 0 SrcPort = %d, want 100", r.SrcPort)
	}
	if r := m.rowAt(1); r.SrcPort != 30000 {
		t.Fatalf("row 1 SrcPort = %d, want 30000", r.SrcPort)
	}
}

func TestDeleteAnyRuleAllowed(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
	})
	m.cursor = 0
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	mm := nm.(model)
	if mm.mode != viewConfirmDelete {
		t.Fatal("delete should be allowed on any rule now")
	}
}

func TestRefreshClampsCursor(t *testing.T) {
	fc := &fakeDaemonClient{owners: daemonclient.OwnerRuleset{
		"panel": {{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
	}}
	m := initialModel(fc, nil)
	m.cursor = 5
	m.refresh()
	if m.cursor != 0 {
		t.Fatalf("refresh must clamp cursor, got %d", m.cursor)
	}
}

func TestSubmitEditPostsPanel(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443, Comment: "old"},
	})
	m.cursor = 0
	m.enterEditMode()
	m.inputs[fDestIP].SetValue("10.9.9.9")
	m.inputs[fDestPort].SetValue("8443")
	m.inputs[fComment].SetValue("new")
	nm, _ := m.submitEdit()
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if fc.postedOwner != "panel" {
		t.Fatalf("expected post to panel, got %q", fc.postedOwner)
	}
	if len(mm.rules) != 1 || mm.rules[0].DestIP != "10.9.9.9" {
		t.Fatalf("rule not updated: %+v", mm.rules)
	}
}

func TestLockedFieldsByRowType(t *testing.T) {
	m := model{editingChainID: 0}
	if len(m.lockedFields()) != 0 {
		t.Fatalf("non-chain should lock nothing, got %v", m.lockedFields())
	}
	m = model{editingChainID: 7}
	lf := m.lockedFields()
	if !lf[fProto] || !lf[fDestIP] || !lf[fDestPort] || lf[fSrcPort] {
		t.Fatalf("chain locks wrong fields: %v", lf)
	}
}

func TestEnterEditModeChainRow(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()
	if m.mode != viewEdit || m.editingChainID != 7 {
		t.Fatalf("editingChainID=%d, want 7", m.editingChainID)
	}
}

func TestSubmitEditChainRowSendsChainEdit(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless", Comment: "old"},
	})
	m.cursor = 0
	m.enterEditMode()
	m.inputs[fSrcPort].SetValue("21555")
	m.inputs[fComment].SetValue("new note")
	m.modeIdx = 1
	nm, _ := m.submitEdit()
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if len(fc.chainEdits) != 1 {
		t.Fatalf("expected one ChainEdit call, got %d", len(fc.chainEdits))
	}
	e := fc.chainEdits[0]
	if e.ChainID != 7 || e.ListenPort != 21555 || e.Mode != nft.ModeUserspace || e.Comment != "new note" {
		t.Fatalf("ChainEdit args wrong: %+v", e)
	}
}

func TestSubmitEditChainRowSurfacesServerError(t *testing.T) {
	fc := &fakeDaemonClient{chainErr: fmt.Errorf("端口被占用")}
	m := initialModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()
	m.inputs[fSrcPort].SetValue("80")
	nm, _ := m.submitEdit()
	if !strings.Contains(nm.(model).err, "端口被占用") {
		t.Fatalf("server error not surfaced: %q", nm.(model).err)
	}
}

func TestDeleteChainRowSendsChainDelete(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = nm.(model)
	if m.mode != viewConfirmDelete {
		t.Fatal("d on a chain row should enter confirm-delete")
	}
	nm, _ = m.updateConfirmDelete(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if len(fc.chainDeletes) != 1 || fc.chainDeletes[0] != 7 {
		t.Fatalf("expected ChainDelete(7), got %v", fc.chainDeletes)
	}
}

func TestRenderTableRow_ColumnsHaveTrailingGap(t *testing.T) {
	longDest := "seednet.xjetry.fun"
	line := stripANSI(renderTableRow("tcp", "42421", longDest, "8443", "x"))
	idx := strings.Index(line, longDest)
	if idx < 0 {
		t.Fatalf("dest not rendered intact: %q", line)
	}
	after := line[idx+len(longDest):]
	if !strings.HasPrefix(after, strings.Repeat(" ", colGap)) {
		t.Fatalf("dest must be followed by >=%d spaces", colGap)
	}
}

func TestUpdateEditChainRowLocksTargetNotListenPort(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, ChainID: 7, ChainName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()

	m.focusedInput = fDestIP
	before := m.inputs[fDestIP].Value()
	nm, _ := m.updateEdit(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if nm.(model).inputs[fDestIP].Value() != before {
		t.Fatal("target IP must be read-only for chain rows")
	}

	m.focusedInput = fSrcPort
	m.inputs[fSrcPort].Focus()
	nm, _ = m.updateEdit(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if nm.(model).inputs[fSrcPort].Value() == "21000" {
		t.Fatal("listen port must be editable for chain rows")
	}
}

func TestCommitOwnerPostsGivenOwner(t *testing.T) {
	fc := &fakeDaemonClient{}
	applied, err := commitOwner(fc, "panel", []nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}})
	if err != nil {
		t.Fatal(err)
	}
	if fc.postedOwner != "panel" || len(applied) != 1 {
		t.Fatalf("commitOwner posted owner=%q applied=%+v", fc.postedOwner, applied)
	}
}
