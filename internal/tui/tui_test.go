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

// fixedPortion returns the byte-prefix of s whose lipgloss display width equals
// exactly targetCells. It stops at the rune boundary where accumulated width
// first reaches targetCells. If s is narrower than targetCells, s is returned as-is.
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

// TestRenderTableRow_ColumnAlignment verifies that header and data rows rendered
// through renderTableRow have identical fixed-column widths regardless of
// destination address length.
func TestRenderTableRow_ColumnAlignment(t *testing.T) {
	header := renderTableRow("协议", "本机端口", "目标", "远程端口", "备注")
	headerPlain := stripANSI(header)

	rows := []struct {
		proto   string
		port    string
		dest    string
		dstPort string
		comment string
	}{
		{"tcp", "8088", "100.100.100.11", "8088", "T"},
		{"tcp", "10010", "127.0.0.1", "10011", "local"},
		{"udp", "53", "8.8.8.8", "53", "dns"},
		// Long destination that must be truncated.
		{"tcp", "9999", "very-long-hostname.example.com", "12345", "overflow"},
	}

	expectedFixedCells := colProto + colSrcPort + colDest + colDstPort

	// The header's fixed portion must be exactly expectedFixedCells wide.
	hFixed := fixedPortion(headerPlain, expectedFixedCells)
	hFixedW := lipgloss.Width(hFixed)
	if hFixedW != expectedFixedCells {
		t.Fatalf("header fixed-column display width = %d, want %d (portion: %q)",
			hFixedW, expectedFixedCells, hFixed)
	}

	for _, row := range rows {
		line := renderTableRow(row.proto, row.port, row.dest, row.dstPort, row.comment)
		linePlain := stripANSI(line)

		// 1. Every row must be at least as wide as the fixed columns.
		lineW := lipgloss.Width(linePlain)
		if lineW < expectedFixedCells {
			t.Errorf("row dest=%q: total width %d < fixed column total %d",
				row.dest, lineW, expectedFixedCells)
		}

		// 2. The fixed-column portion of every data row must be the same display
		//    width as the header, so the comment column starts at the same cell.
		rFixed := fixedPortion(linePlain, expectedFixedCells)
		rFixedW := lipgloss.Width(rFixed)
		if rFixedW != expectedFixedCells {
			t.Errorf("row dest=%q: fixed-column width = %d, want %d (portion: %q)",
				row.dest, rFixedW, expectedFixedCells, rFixed)
		}
	}
}

// TestRenderTableRow_TruncationEllipsis verifies that a destination string longer
// than colDest cells is truncated and the dest cell ends with "…".
func TestRenderTableRow_TruncationEllipsis(t *testing.T) {
	longDest := strings.Repeat("x", 100)
	line := renderTableRow("tcp", "80", longDest, "8080", "")
	plain := stripANSI(line)

	// Skip the proto+srcPort prefix and examine the dest cell.
	afterProtoPort := fixedPortion(plain, colProto+colSrcPort)
	destAndRest := plain[len(afterProtoPort):]
	destCellStr := fixedPortion(destAndRest, colDest)

	if !strings.Contains(destCellStr, "…") {
		t.Errorf("expected ellipsis in truncated dest cell, got: %q", destCellStr)
	}

	// Also verify the dest cell is exactly colDest wide (lipgloss padding fills it).
	destCellW := lipgloss.Width(destCellStr)
	if destCellW != colDest {
		t.Errorf("dest cell width = %d, want %d", destCellW, colDest)
	}
}

// TestTruncateCell verifies CJK-aware truncation.
func TestTruncateCell(t *testing.T) {
	cases := []struct {
		input    string
		maxCells int
		truncate bool // whether we expect truncation
	}{
		{"hello", 10, false},
		{"hello world!", 8, true},
		// CJK: each char = 2 cells; "你好世界测试" = 12 cells; limit 6 → only 2 chars + "…"
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
			// Truncation expected: last rune must be "…" and total width <= maxCells.
			if len(runes) == 0 || string(runes[len(runes)-1]) != "…" {
				t.Errorf("truncateCell(%q, %d) = %q, want ellipsis suffix", c.input, c.maxCells, got)
			}
			w := lipgloss.Width(got)
			if w > c.maxCells {
				t.Errorf("truncateCell(%q, %d) width %d > maxCells %d", c.input, c.maxCells, w, c.maxCells)
			}
		}
	}
}

// TestProtoSelectorCycles verifies that left/right key presses cycle through protoOptions.
func TestProtoSelectorCycles(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil, nil)
	m.enterAddMode()

	// Initial state: idx=0 (tcp)
	if m.protoIdx != 0 {
		t.Fatalf("expected protoIdx=0 after enterAddMode, got %d", m.protoIdx)
	}

	// Simulate right arrow: tcp → udp
	next, _ := m.updateAdd(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.protoIdx != 1 {
		t.Fatalf("after right: expected protoIdx=1 (udp), got %d", m.protoIdx)
	}

	// right again: udp → tcp+udp
	next, _ = m.updateAdd(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.protoIdx != 2 {
		t.Fatalf("after 2nd right: expected protoIdx=2 (tcp+udp), got %d", m.protoIdx)
	}

	// right again: wraps back to tcp
	next, _ = m.updateAdd(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if m.protoIdx != 0 {
		t.Fatalf("after wrap: expected protoIdx=0 (tcp), got %d", m.protoIdx)
	}

	// left from tcp wraps to tcp+udp
	next, _ = m.updateAdd(tea.KeyMsg{Type: tea.KeyLeft})
	m = next.(model)
	if m.protoIdx != 2 {
		t.Fatalf("after left wrap: expected protoIdx=2 (tcp+udp), got %d", m.protoIdx)
	}
}

// TestProtoSelectorEditPreFill verifies that enterEditMode sets protoIdx correctly.
func TestProtoSelectorEditPreFill(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		{Proto: "tcp+udp", SrcPort: 80, DestIP: "10.0.0.1", DestPort: 80},
		{Proto: "tcp", SrcPort: 443, DestIP: "10.0.0.1", DestPort: 443},
	}
	for _, r := range rules {
		m := initialModel(&fakeDaemonClient{}, rules, nil)
		// find cursor index
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

// TestProtoSelectorRenderContainsOptions verifies the selector renders all three options.
func TestProtoSelectorRenderContainsOptions(t *testing.T) {
	m := initialModel(&fakeDaemonClient{}, nil, nil)
	m.enterAddMode()

	view := m.renderProtoSelector()
	plain := stripANSI(view)
	for _, opt := range protoOptions {
		if !strings.Contains(plain, opt) {
			t.Errorf("selector render missing option %q; got: %q", opt, plain)
		}
	}
	// Active (idx=0, tcp) should be wrapped in brackets.
	if !strings.Contains(plain, "[ tcp ]") {
		t.Errorf("active option 'tcp' should be shown as '[ tcp ]', got: %q", plain)
	}
}

// TestColProtoFitsLongestOption ensures colProto is wide enough for "tcp+udp" (7 chars + 1 pad).
func TestColProtoFitsLongestOption(t *testing.T) {
	longestOption := "TCP+UDP"
	w := lipgloss.Width(longestOption)
	if colProto < w {
		t.Errorf("colProto=%d is too narrow for %q (%d cells)", colProto, longestOption, w)
	}
}

// TestRenderTableRow_FiveColumns verifies the 5-column layout (proto | srcPort | dest | dstPort | comment).
// It checks:
//  1. Each row's fixed portion is exactly 46 cells (colProto+colSrcPort+colDest+colDstPort).
//  2. The 远程端口 cell contains the expected port string.
func TestRenderTableRow_FiveColumns(t *testing.T) {
	const expectedFixed = colProto + colSrcPort + colDest + colDstPort // 46

	cases := []struct {
		rule        nft.Rule
		wantDstPort string
		desc        string
	}{
		{
			rule:        nft.Rule{Proto: "tcp", SrcPort: 8080, DestIP: "10.0.0.1", DestPort: 9000, Comment: "short-host"},
			wantDstPort: "9000",
			desc:        "IPv4 short host",
		},
		{
			rule:        nft.Rule{Proto: "udp", SrcPort: 1234, DestIP: "192.168.1.1", DestPort: 65535, Comment: "max-port"},
			wantDstPort: "65535",
			desc:        "max remote port",
		},
		{
			rule:        nft.Rule{Proto: "tcp+udp", SrcPort: 443, DestHost: "my.ddns.example.com", DestIP: "203.0.113.5", DestPort: 8443, Comment: "ddns"},
			wantDstPort: "8443",
			desc:        "DDNS DestHost preferred over DestIP",
		},
	}

	for _, c := range cases {
		r := c.rule
		destHost := r.DestIP
		if r.DestHost != "" {
			destHost = r.DestHost
		}
		dstPortStr := strconv.Itoa(r.DestPort)

		line := renderTableRow(
			strings.ToLower(r.Proto),
			strconv.Itoa(r.SrcPort),
			destHost,
			dstPortStr,
			r.Comment,
		)
		plain := stripANSI(line)

		// 1. Fixed portion must be exactly 58 cells.
		rFixed := fixedPortion(plain, expectedFixed)
		rFixedW := lipgloss.Width(rFixed)
		if rFixedW != expectedFixed {
			t.Errorf("[%s] fixed-column width = %d, want %d (portion: %q)",
				c.desc, rFixedW, expectedFixed, rFixed)
		}

		// 2. The 远程端口 cell (4th fixed column) must contain the expected port.
		// Extract the dstPort cell: bytes between (colProto+colSrcPort+colDest) and end of fixed.
		afterThree := fixedPortion(plain, colProto+colSrcPort+colDest)
		dstPortCell := fixedPortion(plain[len(afterThree):], colDstPort)
		dstPortCellTrimmed := strings.TrimSpace(dstPortCell)
		if dstPortCellTrimmed != c.wantDstPort {
			t.Errorf("[%s] 远程端口 cell = %q (trimmed: %q), want %q",
				c.desc, dstPortCell, dstPortCellTrimmed, c.wantDstPort)
		}
	}
}

// TestViewList_ColumnConsistency builds rows for mixed-length rules and
// checks that each data row has the same fixed-column display width as the header.
func TestViewList_ColumnConsistency(t *testing.T) {
	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 8088, DestIP: "100.100.100.11", DestPort: 8088, Comment: "T"},
		{Proto: "tcp", SrcPort: 10010, DestIP: "127.0.0.1", DestPort: 10011, Comment: "local"},
		{Proto: "udp", SrcPort: 53, DestHost: "dns.example.com", DestIP: "8.8.8.8", DestPort: 53, Comment: "dns-forward"},
	}

	header := renderTableRow("协议", "本机端口", "目标", "远程端口", "备注")
	headerPlain := stripANSI(header)
	expectedFixed := colProto + colSrcPort + colDest + colDstPort

	hFixed := fixedPortion(headerPlain, expectedFixed)
	if lipgloss.Width(hFixed) != expectedFixed {
		t.Fatalf("header fixed width = %d, want %d", lipgloss.Width(hFixed), expectedFixed)
	}

	for _, r := range rules {
		destHost := r.DestIP
		if r.DestHost != "" {
			destHost = r.DestHost
		}

		line := renderTableRow(
			strings.ToLower(r.Proto),
			fmt.Sprintf("%d", r.SrcPort),
			destHost,
			fmt.Sprintf("%d", r.DestPort),
			r.Comment,
		)
		plain := stripANSI(line)
		rFixed := fixedPortion(plain, expectedFixed)
		rFixedW := lipgloss.Width(rFixed)
		if rFixedW != expectedFixed {
			t.Errorf("rule %s/%d dest=%q: fixed-column width = %d, want %d",
				r.Proto, r.SrcPort, destHost, rFixedW, expectedFixed)
		}
	}
}

// TestCommitPostsRawRules verifies that commit posts rules with DestHost set
// and DestIP empty, without attempting to resolve hostnames.
func TestCommitPostsRawRules(t *testing.T) {
	fake := &fakeDaemonClient{}
	rules := []nft.Rule{
		{Proto: "tcp", SrcPort: 80, DestHost: "home.example.com", DestPort: 80},
	}
	applied, err := commit(fake, rules)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if fake.postedOwner != "tui" {
		t.Errorf("owner = %q, want tui", fake.postedOwner)
	}
	if len(fake.postedRules) != 1 {
		t.Errorf("expected 1 rule posted, got %d", len(fake.postedRules))
	} else if fake.postedRules[0].DestHost != "home.example.com" || fake.postedRules[0].DestIP != "" {
		t.Errorf("expected raw rule with DestHost=home.example.com and DestIP empty, got %+v", fake.postedRules[0])
	}
	if len(applied) != 1 || applied[0].DestHost != "home.example.com" || applied[0].DestIP != "" {
		t.Errorf("expected commit to return raw rule with DestHost set, got %+v", applied)
	}
}

// TestSubmitAdd_CarriesUserspaceMode verifies that selecting the userspace mode
// in the form produces a rule with Mode=ModeUserspace after submit.
func TestSubmitAdd_CarriesUserspaceMode(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, nil, nil)
	m.enterAddMode()
	m.protoIdx = 0 // tcp
	m.modeIdx = 1  // userspace
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

// TestSubmitAdd_RejectsUDPUserspace verifies that the existing nft.Validate
// rejection of udp+userspace surfaces as a form error rather than a crash.
func TestSubmitAdd_RejectsUDPUserspace(t *testing.T) {
	fc := &fakeDaemonClient{}
	m := initialModel(fc, nil, nil)
	m.enterAddMode()
	m.protoIdx = 1 // udp
	m.modeIdx = 1  // userspace
	m.inputs[fSrcPort].SetValue("8443")
	m.inputs[fDestIP].SetValue("10.0.0.1")
	m.inputs[fDestPort].SetValue("443")
	nm, _ := m.submitAdd()
	if nm.(model).err == "" {
		t.Fatal("expected validation error for udp+userspace")
	}
}

func TestLoadInitialRulesSplitsTuiAndPanel(t *testing.T) {
	fc := &fakeDaemonClient{owners: daemonclient.OwnerRuleset{
		"tui":   {{Proto: "tcp", SrcPort: 100, DestIP: "10.0.0.1", DestPort: 100}},
		"panel": {{Proto: "tcp", SrcPort: 200, DestIP: "10.0.0.2", DestPort: 200,
			ChainID: 5, ChainName: "seednet-vless"}},
	}}
	tuiRules, panelRules, err := loadInitialRules(fc)
	if err != nil {
		t.Fatal(err)
	}
	if len(tuiRules) != 1 || tuiRules[0].SrcPort != 100 {
		t.Fatalf("tui segment wrong: %+v", tuiRules)
	}
	if len(panelRules) != 1 || panelRules[0].ChainName != "seednet-vless" {
		t.Fatalf("panel segment wrong: %+v", panelRules)
	}
}
