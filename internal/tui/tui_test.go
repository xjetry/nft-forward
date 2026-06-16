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
	rules     []nft.Rule // returned by ListRules
	connected bool
	nodeName  string

	// createResp is returned by CreateRule; createErr overrides with an error.
	createResp daemonclient.CreateRuleResp
	createErr  error
	creates    []daemonclient.CreateRuleReq

	ruleEdits []struct {
		ID  string
		Req daemonclient.UpdateRuleReq
	}
	ruleDeletes []string
	ruleErr     error
}

func (f *fakeDaemonClient) ListRules() ([]nft.Rule, error) {
	if f.rules == nil {
		return []nft.Rule{}, nil
	}
	return append([]nft.Rule(nil), f.rules...), nil
}

func (f *fakeDaemonClient) Status() (daemonclient.StatusResp, error) {
	return daemonclient.StatusResp{
		Connected: f.connected,
		NodeName:  f.nodeName,
	}, nil
}

func (f *fakeDaemonClient) CreateRule(req daemonclient.CreateRuleReq) (daemonclient.CreateRuleResp, error) {
	f.creates = append(f.creates, req)
	if f.createErr != nil {
		return daemonclient.CreateRuleResp{}, f.createErr
	}
	// Simulate: daemon assigns a listen port and adds the rule to its store.
	lp := req.ListenPort
	if lp == 0 {
		lp = 40000 // fake auto-assign
	}
	f.rules = append(f.rules, nft.Rule{
		ID:       nft.NewRuleID(),
		Proto:    req.Proto,
		SrcPort:  lp,
		DestHost: req.ExitHost,
		DestPort: req.ExitPort,
		Comment:  req.Comment,
		Mode:     req.Mode,
	})
	resp := f.createResp
	if resp.ListenPort == 0 {
		resp.ListenPort = lp
	}
	return resp, nil
}

func (f *fakeDaemonClient) UpdateRule(id string, req daemonclient.UpdateRuleReq) error {
	f.ruleEdits = append(f.ruleEdits, struct {
		ID  string
		Req daemonclient.UpdateRuleReq
	}{id, req})
	return f.ruleErr
}

func (f *fakeDaemonClient) DeleteRule(id string) error {
	f.ruleDeletes = append(f.ruleDeletes, id)
	if f.ruleErr != nil {
		return f.ruleErr
	}
	// Remove the rule from the local store so refresh sees the change.
	for i := range f.rules {
		rid := f.rules[i].ID
		if f.rules[i].RuleID != 0 {
			rid = strconv.FormatInt(f.rules[i].RuleID, 10)
		}
		if rid == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			break
		}
	}
	return nil
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

func newTestModel(fc *fakeDaemonClient, rules []nft.Rule) model {
	if fc.rules == nil && rules != nil {
		fc.rules = append([]nft.Rule(nil), rules...)
	}
	return initialModel(fc, rules, fc.connected, fc.nodeName)
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
	m := newTestModel(&fakeDaemonClient{}, nil)
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
		m := newTestModel(&fakeDaemonClient{}, rules)
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
	m := newTestModel(&fakeDaemonClient{}, nil)
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

func TestSubmitAdd_CarriesUserspaceMode(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, nil)
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
	if len(fc.creates) != 1 {
		t.Fatalf("expected 1 CreateRule call, got %d", len(fc.creates))
	}
	if fc.creates[0].Mode != nft.ModeUserspace {
		t.Fatalf("CreateRule should carry userspace mode, got %q", fc.creates[0].Mode)
	}
}

func TestSubmitAdd_AutoAssignPort(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, nil)
	m.enterAddMode()
	m.protoIdx = 0
	// Leave SrcPort empty for auto-assign.
	m.inputs[fDestIP].SetValue("10.0.0.1")
	m.inputs[fDestPort].SetValue("443")
	nm, _ := m.submitAdd()
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if len(fc.creates) != 1 {
		t.Fatalf("expected 1 CreateRule call, got %d", len(fc.creates))
	}
	if fc.creates[0].ListenPort != 0 {
		t.Fatalf("empty SrcPort should send ListenPort=0, got %d", fc.creates[0].ListenPort)
	}
}

func TestSubmitAdd_DaemonError(t *testing.T) {
	fc := &fakeDaemonClient{createErr: fmt.Errorf("UDP 不支持用户态转发")}
	m := newTestModel(fc, nil)
	m.enterAddMode()
	m.protoIdx = 1
	m.modeIdx = 1
	m.inputs[fSrcPort].SetValue("8443")
	m.inputs[fDestIP].SetValue("10.0.0.1")
	m.inputs[fDestPort].SetValue("443")
	nm, _ := m.submitAdd()
	if nm.(model).err == "" {
		t.Fatal("expected daemon error to surface")
	}
	if !strings.Contains(nm.(model).err, "UDP") {
		t.Fatalf("expected UDP error, got: %q", nm.(model).err)
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

func TestViewListShowsConnectionStatus(t *testing.T) {
	m := model{
		mode:      viewList,
		width:     140,
		connected: true,
		nodeName:  "test-node",
		rules:     []nft.Rule{},
	}
	out := stripANSI(m.View())
	if !strings.Contains(out, "已连接 test-node") {
		t.Fatalf("expected connection status in title, got:\n%s", out)
	}

	m.connected = false
	out = stripANSI(m.View())
	if !strings.Contains(out, "本地模式") {
		t.Fatalf("expected local mode in title, got:\n%s", out)
	}
}

func TestLoadInitialRulesReturnsActiveRules(t *testing.T) {
	fc := &fakeDaemonClient{
		rules:     []nft.Rule{{Proto: "tcp", SrcPort: 200, DestIP: "10.0.0.2", DestPort: 200, RuleID: 5, RuleName: "seednet-vless"}},
		connected: true,
		nodeName:  "my-node",
	}
	rules, connected, nodeName, err := loadInitialRules(fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].SrcPort != 200 {
		t.Fatalf("expected 1 rule on port 200, got %+v", rules)
	}
	if !connected || nodeName != "my-node" {
		t.Fatalf("expected connected=true nodeName=my-node, got connected=%v nodeName=%q", connected, nodeName)
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
	m := newTestModel(fc, []nft.Rule{
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
	fc := &fakeDaemonClient{rules: []nft.Rule{
		{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
	}}
	m := newTestModel(fc, nil)
	m.cursor = 5
	m.refresh()
	if m.cursor != 0 {
		t.Fatalf("refresh must clamp cursor, got %d", m.cursor)
	}
}

func TestSubmitEditCallsUpdateRule(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, []nft.Rule{
		{ID: "aabb", Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443, Comment: "old"},
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
	if len(fc.ruleEdits) != 1 {
		t.Fatalf("expected 1 UpdateRule call, got %d", len(fc.ruleEdits))
	}
	e := fc.ruleEdits[0]
	if e.ID != "aabb" {
		t.Fatalf("expected hex ID 'aabb', got %q", e.ID)
	}
	if e.Req.ExitHost != "10.9.9.9" || e.Req.ExitPort != 8443 || e.Req.Comment != "new" {
		t.Fatalf("UpdateRule args wrong: %+v", e)
	}
}

func TestSubmitEditServerRule(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, []nft.Rule{
		{ID: "aabb", Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, RuleID: 7, RuleName: "vless", Comment: "old"},
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
	if len(fc.ruleEdits) != 1 {
		t.Fatalf("expected one UpdateRule call, got %d", len(fc.ruleEdits))
	}
	e := fc.ruleEdits[0]
	// Server rule uses numeric RuleID.
	if e.ID != "7" {
		t.Fatalf("expected numeric ID '7', got %q", e.ID)
	}
	if e.Req.ListenPort != 21555 || e.Req.Mode != nft.ModeUserspace || e.Req.Comment != "new note" {
		t.Fatalf("UpdateRule args wrong: %+v", e)
	}
}

func TestSubmitEditRuleRowSurfacesServerError(t *testing.T) {
	fc := &fakeDaemonClient{ruleErr: fmt.Errorf("端口被占用")}
	m := newTestModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, RuleID: 7, RuleName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()
	m.inputs[fSrcPort].SetValue("80")
	nm, _ := m.submitEdit()
	if !strings.Contains(nm.(model).err, "端口被占用") {
		t.Fatalf("server error not surfaced: %q", nm.(model).err)
	}
}

func TestDeleteRuleCallsDeleteRule(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, []nft.Rule{
		{ID: "cc11", Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
	})
	m.cursor = 0
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = nm.(model)
	if m.mode != viewConfirmDelete {
		t.Fatal("d should enter confirm-delete")
	}
	nm, _ = m.updateConfirmDelete(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if len(fc.ruleDeletes) != 1 || fc.ruleDeletes[0] != "cc11" {
		t.Fatalf("expected DeleteRule(\"cc11\"), got %v", fc.ruleDeletes)
	}
}

func TestDeleteServerRuleUsesRuleID(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, RuleID: 7, RuleName: "vless"},
	})
	m.cursor = 0
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = nm.(model)
	nm, _ = m.updateConfirmDelete(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if len(fc.ruleDeletes) != 1 || fc.ruleDeletes[0] != "7" {
		t.Fatalf("expected DeleteRule(\"7\"), got %v", fc.ruleDeletes)
	}
	_ = nm
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

func TestLockedFieldsByHopCount(t *testing.T) {
	// Single-hop or local: all fields editable.
	m := model{
		rules:  []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100, HopCount: 1}},
		cursor: 0,
	}
	if len(m.lockedFields()) != 0 {
		t.Fatalf("single-hop should lock nothing, got %v", m.lockedFields())
	}

	// Multi-hop: lock proto/dest.
	m = model{
		rules:  []nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100, HopCount: 3, RuleID: 7}},
		cursor: 0,
	}
	lf := m.lockedFields()
	if !lf[fProto] || !lf[fDestIP] || !lf[fDestPort] || lf[fSrcPort] {
		t.Fatalf("multi-hop locks wrong fields: %v", lf)
	}
}

func TestEnterEditModeRuleRow(t *testing.T) {
	m := newTestModel(&fakeDaemonClient{}, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, RuleID: 7, RuleName: "vless"},
	})
	m.cursor = 0
	m.enterEditMode()
	if m.mode != viewEdit || m.editingRuleID != 7 {
		t.Fatalf("editingRuleID=%d, want 7", m.editingRuleID)
	}
}

func TestUpdateEditMultiHopLocksTargetNotListenPort(t *testing.T) {
	m := newTestModel(&fakeDaemonClient{}, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, RuleID: 7, RuleName: "vless", HopCount: 3},
	})
	m.cursor = 0
	m.enterEditMode()

	m.focusedInput = fDestIP
	before := m.inputs[fDestIP].Value()
	nm, _ := m.updateEdit(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if nm.(model).inputs[fDestIP].Value() != before {
		t.Fatal("target IP must be read-only for multi-hop rows")
	}

	m.focusedInput = fSrcPort
	m.inputs[fSrcPort].Focus()
	nm, _ = m.updateEdit(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if nm.(model).inputs[fSrcPort].Value() == "21000" {
		t.Fatal("listen port must be editable for multi-hop rows")
	}
}

func TestUpdateEditSingleHopAllFieldsEditable(t *testing.T) {
	m := newTestModel(&fakeDaemonClient{}, []nft.Rule{
		{Proto: "tcp", SrcPort: 21000, DestIP: "1.2.3.4", DestPort: 443, RuleID: 7, RuleName: "vless", HopCount: 1},
	})
	m.cursor = 0
	m.enterEditMode()

	// Single-hop server rule: all fields editable.
	m.focusedInput = fDestIP
	m.inputs[fDestIP].Focus()
	nm, _ := m.updateEdit(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("9")})
	if nm.(model).inputs[fDestIP].Value() == "1.2.3.4" {
		t.Fatal("target IP must be editable for single-hop server rules")
	}
}

func TestClearAllDeletesEachRule(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, []nft.Rule{
		{ID: "aa", Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
		{ID: "bb", Proto: "udp", SrcPort: 200, DestIP: "10.0.0.2", DestPort: 200},
		{Proto: "tcp", SrcPort: 300, DestIP: "10.0.0.3", DestPort: 300, RuleID: 5},
	})
	nm, _ := m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = nm.(model)
	if m.mode != viewConfirmClear {
		t.Fatal("c should enter confirm-clear")
	}
	nm, _ = m.updateConfirmClear(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	// Should have 3 delete calls: two hex IDs and one numeric RuleID.
	if len(fc.ruleDeletes) != 3 {
		t.Fatalf("expected 3 DeleteRule calls, got %d: %v", len(fc.ruleDeletes), fc.ruleDeletes)
	}
	if fc.ruleDeletes[0] != "aa" || fc.ruleDeletes[1] != "bb" || fc.ruleDeletes[2] != "5" {
		t.Fatalf("wrong delete IDs: %v", fc.ruleDeletes)
	}
}
