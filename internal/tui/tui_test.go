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
	rules []nft.Rule // returned by ListRules, each with the address rowsOf gives it
	// rows 非 nil 时 ListRules 原样返回它，用来给出 rowsOf 给不出的地址组合。
	rows []daemonclient.Row
	// mode 为空表示单机形态。
	mode      daemonclient.Mode
	connected bool
	nodeName  string
	statusErr error

	// createResp is returned by CreateRule; createErr overrides with an error.
	createResp daemonclient.CreateRuleResp
	createErr  error
	creates    []daemonclient.CreateRuleReq

	ruleEdits []struct {
		Ref daemonclient.RuleRef
		Req daemonclient.UpdateRuleReq
	}
	ruleDeletes []daemonclient.RuleRef
	ruleErr     error
}

// rowsOf 给一组规则配上地址，配法与 daemon 的归属一致：带面板规则 id 的算面板
// 的，其余算本地的。只是测试夹具的简写——被测的 TUI 不这样推断，它用行自带的
// 地址（TestRowAddressComesFromTheDaemonNotFromRuleFields 钉住这一点）。
func rowsOf(rules []nft.Rule) []daemonclient.Row {
	rows := make([]daemonclient.Row, len(rules))
	for i, r := range rules {
		ref := daemonclient.LocalRule(r.ID)
		if r.RuleID != 0 {
			ref = daemonclient.PanelRule(r.RuleID)
		}
		rows[i] = daemonclient.Row{Rule: r, Ref: ref}
	}
	return rows
}

func (f *fakeDaemonClient) ListRules() ([]daemonclient.Row, error) {
	if f.rows != nil {
		return append([]daemonclient.Row(nil), f.rows...), nil
	}
	return rowsOf(f.rules), nil
}

func (f *fakeDaemonClient) Status() (daemonclient.StatusResp, error) {
	if f.statusErr != nil {
		return daemonclient.StatusResp{}, f.statusErr
	}
	mode := f.mode
	if mode == "" {
		mode = daemonclient.ModeStandalone
	}
	return daemonclient.StatusResp{
		Mode:      mode,
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

func (f *fakeDaemonClient) UpdateRule(ref daemonclient.RuleRef, req daemonclient.UpdateRuleReq) error {
	f.ruleEdits = append(f.ruleEdits, struct {
		Ref daemonclient.RuleRef
		Req daemonclient.UpdateRuleReq
	}{ref, req})
	return f.ruleErr
}

func (f *fakeDaemonClient) DeleteRule(ref daemonclient.RuleRef) error {
	f.ruleDeletes = append(f.ruleDeletes, ref)
	if f.ruleErr != nil {
		return f.ruleErr
	}
	// Remove the rule from the local store so refresh sees the change. The
	// fake resolves the address the way the daemon does: by namespace.
	for i := range f.rules {
		stored := daemonclient.LocalRule(f.rules[i].ID)
		if f.rules[i].RuleID != 0 {
			stored = daemonclient.PanelRule(f.rules[i].RuleID)
		}
		if stored == ref {
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
	st, _ := fc.Status()
	return initialModel(fc, st, rowsOf(rules))
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
	nm, _ := m.commitForm(false)
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
	nm, _ := m.commitForm(false)
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
	nm, _ := m.commitForm(false)
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
		rows: rowsOf([]nft.Rule{
			{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
			{Proto: "tcp", SrcPort: 17171, DestIP: "72.234.229.145", DestPort: 17171, Comment: "ss"},
		}),
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
		rows:  rowsOf([]nft.Rule{{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443}}),
	}
	_ = m.View() // must not panic
}

// 标题说的是“这台机器归谁管、此刻连没连上”，三种情形各有各的说法。托管但离线
// 不是本地模式：规则仍由面板管，列表里的面板规则照常显示，并说明哪些操作要等连接
// 恢复。
func TestViewListTitleTellsRunModeApartFromSessionState(t *testing.T) {
	panelRule := nft.Rule{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443, RuleID: 7, RuleName: "r"}
	view := func(st daemonclient.StatusResp) string {
		m := model{mode: viewList, width: 140, daemon: st, rows: rowsOf([]nft.Rule{panelRule})}
		return stripANSI(m.View())
	}

	out := view(daemonclient.StatusResp{Mode: daemonclient.ModeStandalone})
	if !strings.Contains(out, "本地模式") || strings.Contains(out, "面板") {
		t.Errorf("单机形态应显示本地模式、不提面板:\n%s", out)
	}

	for _, mode := range []daemonclient.Mode{daemonclient.ModeRemotePanel, daemonclient.ModeLocalPanel} {
		out = view(daemonclient.StatusResp{Mode: mode, Connected: true, NodeName: "test-node"})
		if !strings.Contains(out, "已连接 test-node") || strings.Contains(out, "离线") || strings.Contains(out, "面板不可达") {
			t.Errorf("%s 在线应显示已连接、不提离线:\n%s", mode, out)
		}

		out = view(daemonclient.StatusResp{Mode: mode, Connected: false, NodeName: "test-node"})
		if !strings.Contains(out, "面板托管 · 离线（test-node）") || strings.Contains(out, "本地模式") {
			t.Errorf("%s 离线应显示面板托管 · 离线并带上节点名，不能说成本地模式:\n%s", mode, out)
		}
		if !strings.Contains(out, "面板不可达") {
			t.Errorf("%s 离线应说明面板不可达:\n%s", mode, out)
		}
		if !strings.Contains(out, "30000") || !strings.Contains(out, "10.0.0.9") {
			t.Errorf("%s 离线时面板规则仍在转发，列表里必须还在:\n%s", mode, out)
		}

		// 从未握手成功过：没有节点名可带。
		out = view(daemonclient.StatusResp{Mode: mode})
		if !strings.Contains(out, "面板托管 · 离线") || strings.Contains(out, "（") {
			t.Errorf("%s 从未连上过应显示面板托管 · 离线、不带空的节点名:\n%s", mode, out)
		}
	}
}

// 托管形态下，面板不知道的本地规则在列表里标出来；单机形态下每一行都是本地规则，
// 不标。
func TestViewListMarksLocalRulesOnlyWhenManaged(t *testing.T) {
	rows := rowsOf([]nft.Rule{
		{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443, RuleID: 7, Comment: "from-panel"},
		{ID: "aabbccdd", Proto: "tcp", SrcPort: 30001, DestIP: "10.0.0.8", DestPort: 443, Comment: "leftover"},
		{ID: "aabbccde", Proto: "tcp", SrcPort: 30002, DestIP: "10.0.0.7", DestPort: 443},
	})
	lineWith := func(out, needle string) string {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, needle) {
				return line
			}
		}
		t.Fatalf("没有找到含 %q 的行:\n%s", needle, out)
		return ""
	}

	managed := stripANSI(model{mode: viewList, width: 140, rows: rows,
		daemon: daemonclient.StatusResp{Mode: daemonclient.ModeRemotePanel, Connected: true, NodeName: "n"}}.View())
	if l := lineWith(managed, "30000"); strings.Contains(l, "本地规则") {
		t.Errorf("面板规则不应标成本地规则: %q", l)
	}
	if l := lineWith(managed, "30001"); !strings.Contains(l, "本地规则 · leftover") {
		t.Errorf("托管形态下的本地规则应标出并保留备注: %q", l)
	}
	if l := lineWith(managed, "30002"); !strings.Contains(l, "本地规则") {
		t.Errorf("托管形态下没有备注的本地规则也应标出: %q", l)
	}

	standalone := stripANSI(model{mode: viewList, width: 140, rows: rows,
		daemon: daemonclient.StatusResp{Mode: daemonclient.ModeStandalone}}.View())
	if strings.Contains(standalone, "本地规则") {
		t.Errorf("单机形态下不应给行加本地规则的标记:\n%s", standalone)
	}
}

func TestLoadSnapshotReturnsStatusAndRows(t *testing.T) {
	fc := &fakeDaemonClient{
		rules:     []nft.Rule{{Proto: "tcp", SrcPort: 200, DestIP: "10.0.0.2", DestPort: 200, RuleID: 5, RuleName: "seednet-vless"}},
		mode:      daemonclient.ModeRemotePanel,
		connected: true,
		nodeName:  "my-node",
	}
	st, rows, err := loadSnapshot(fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SrcPort != 200 || rows[0].Ref != daemonclient.PanelRule(5) {
		t.Fatalf("expected 1 panel rule on port 200, got %+v", rows)
	}
	if st.Mode != daemonclient.ModeRemotePanel || !st.Connected || st.NodeName != "my-node" {
		t.Fatalf("expected remote-panel, connected, my-node; got %+v", st)
	}
}

// 状态读不到时不拿零值顶上：零值会被显示成“本地模式”，那是替 daemon 编了一个它
// 没说过的形态。
func TestLoadSnapshotFailsWhenStatusIsUnavailable(t *testing.T) {
	fc := &fakeDaemonClient{
		rules:     []nft.Rule{{Proto: "tcp", SrcPort: 200, DestIP: "10.0.0.2", DestPort: 200}},
		statusErr: fmt.Errorf("daemon status: 无法识别的运行形态"),
	}
	if _, _, err := loadSnapshot(fc); err == nil || !strings.Contains(err.Error(), "读取 daemon 状态失败") {
		t.Fatalf("状态读不到时应报错，实际 %v", err)
	}
}

// 改与删用的是这一行自带的地址，不看规则的字段。这里故意给出与字段“长相”相反的
// 地址：一条带面板规则 id 的行挂着本地地址，一条不带的行挂着面板地址。
func TestRowAddressComesFromTheDaemonNotFromRuleFields(t *testing.T) {
	rows := []daemonclient.Row{
		{Rule: nft.Rule{ID: "aabbccdd", RuleID: 7, Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}, Ref: daemonclient.LocalRule("aabbccdd")},
		{Rule: nft.Rule{ID: "11223344", Proto: "tcp", SrcPort: 101, DestIP: "10.0.0.2", DestPort: 101}, Ref: daemonclient.PanelRule(9)},
	}
	for i, want := range []daemonclient.RuleRef{daemonclient.LocalRule("aabbccdd"), daemonclient.PanelRule(9)} {
		fc := &fakeDaemonClient{rows: rows}
		st, _ := fc.Status()
		m := initialModel(fc, st, rows)
		m.cursor = i

		m.enterEditMode()
		if _, _ = m.commitForm(true); len(fc.ruleEdits) != 1 || fc.ruleEdits[0].Ref != want {
			t.Errorf("第 %d 行：编辑应发往 %v，实际 %+v", i, want, fc.ruleEdits)
		}

		m.mode = viewConfirmDelete
		m.updateConfirmDelete(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
		if len(fc.ruleDeletes) != 1 || fc.ruleDeletes[0] != want {
			t.Errorf("第 %d 行：删除应发往 %v，实际 %v", i, want, fc.ruleDeletes)
		}
	}

	// 清空逐行删除，用的同样是每一行自带的地址。
	fc := &fakeDaemonClient{rows: rows}
	st, _ := fc.Status()
	m := initialModel(fc, st, rows)
	m.mode = viewConfirmClear
	m.updateConfirmClear(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if len(fc.ruleDeletes) != 2 || fc.ruleDeletes[0] != daemonclient.LocalRule("aabbccdd") || fc.ruleDeletes[1] != daemonclient.PanelRule(9) {
		t.Errorf("清空应按每行的地址逐条删除，实际 %v", fc.ruleDeletes)
	}
}

func TestRowAtReturnsRule(t *testing.T) {
	m := model{
		rows: rowsOf([]nft.Rule{
			{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100},
			{Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
		}),
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
	nm, _ := m.commitForm(true)
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if len(fc.ruleEdits) != 1 {
		t.Fatalf("expected 1 UpdateRule call, got %d", len(fc.ruleEdits))
	}
	e := fc.ruleEdits[0]
	if e.Ref != daemonclient.LocalRule("aabb") {
		t.Fatalf("expected local rule aabb, got %s", e.Ref)
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
	nm, _ := m.commitForm(true)
	mm := nm.(model)
	if mm.err != "" {
		t.Fatalf("unexpected err: %s", mm.err)
	}
	if len(fc.ruleEdits) != 1 {
		t.Fatalf("expected one UpdateRule call, got %d", len(fc.ruleEdits))
	}
	e := fc.ruleEdits[0]
	// A panel rule is addressed by its RuleID, not by the row's local ID.
	if e.Ref != daemonclient.PanelRule(7) {
		t.Fatalf("expected panel rule 7, got %s", e.Ref)
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
	nm, _ := m.commitForm(true)
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
	if len(fc.ruleDeletes) != 1 || fc.ruleDeletes[0] != daemonclient.LocalRule("cc11") {
		t.Fatalf("expected DeleteRule(local/cc11), got %v", fc.ruleDeletes)
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
	if len(fc.ruleDeletes) != 1 || fc.ruleDeletes[0] != daemonclient.PanelRule(7) {
		t.Fatalf("expected DeleteRule(panel/7), got %v", fc.ruleDeletes)
	}
	_ = nm
}

// 本地规则的 ID 是 8 位十六进制串，可能恰好全是数字。这样的行仍是本地规则：
// 改和删都必须发往本地命名空间，不能因为 ID 长得像整数就当成面板规则。
func TestAllDigitLocalIDStaysLocal(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := newTestModel(fc, []nft.Rule{
		{ID: "12345678", Proto: "tcp", SrcPort: 30000, DestIP: "10.0.0.9", DestPort: 443},
	})
	m.cursor = 0
	m.enterEditMode()
	m.inputs[fComment].SetValue("new")
	nm, _ := m.commitForm(true)
	if e := nm.(model).err; e != "" {
		t.Fatalf("unexpected err: %s", e)
	}
	if len(fc.ruleEdits) != 1 || fc.ruleEdits[0].Ref != daemonclient.LocalRule("12345678") {
		t.Fatalf("edit must target local/12345678, got %+v", fc.ruleEdits)
	}

	m = nm.(model)
	m.cursor = 0
	nm, _ = m.updateList(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = nm.(model)
	_, _ = m.updateConfirmDelete(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if len(fc.ruleDeletes) != 1 || fc.ruleDeletes[0] != daemonclient.LocalRule("12345678") {
		t.Fatalf("delete must target local/12345678, got %v", fc.ruleDeletes)
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

func TestLockedFieldsByHopCount(t *testing.T) {
	// Single-hop or local: all fields editable.
	m := model{
		rows:   rowsOf([]nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100, HopCount: 1}}),
		cursor: 0,
	}
	if len(m.lockedFields()) != 0 {
		t.Fatalf("single-hop should lock nothing, got %v", m.lockedFields())
	}

	// Multi-hop: lock proto/dest.
	m = model{
		rows:   rowsOf([]nft.Rule{{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100, HopCount: 3, RuleID: 7}}),
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
	if m.mode != viewEdit {
		t.Fatalf("mode=%v, want viewEdit", m.mode)
	}
	if got := m.inputs[fSrcPort].Value(); got != "21000" {
		t.Fatalf("edit form should be prefilled from the row: listen port %q, want 21000", got)
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
	// Should have 3 delete calls: two local rules and one panel rule.
	if len(fc.ruleDeletes) != 3 {
		t.Fatalf("expected 3 DeleteRule calls, got %d: %v", len(fc.ruleDeletes), fc.ruleDeletes)
	}
	if fc.ruleDeletes[0] != daemonclient.LocalRule("aa") || fc.ruleDeletes[1] != daemonclient.LocalRule("bb") || fc.ruleDeletes[2] != daemonclient.PanelRule(5) {
		t.Fatalf("wrong delete targets: %v", fc.ruleDeletes)
	}
}
