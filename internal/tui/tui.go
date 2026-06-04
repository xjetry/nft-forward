package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

// daemonClient is the subset of daemonclient.Client the TUI relies on.
// Declared locally so the TUI test suite can substitute a fake; the
// return type uses daemonclient.OwnerRuleset because Go's interface
// matching is strict on named-vs-unnamed map types — *daemonclient.Client
// declares OwnerRuleset, so the TUI's interface must use the same name
// for the structural match to hold.
type daemonClient interface {
	GetRuleset() (daemonclient.OwnerRuleset, error)
	PostRuleset(owner string, rules []nft.Rule) error
	ChainEdit(chainID int64, listenPort int, mode, comment string) error
	ChainDelete(chainID int64) error
}

type viewMode int

const (
	viewList viewMode = iota
	viewAdd
	viewEdit
	viewConfirmDelete
	viewConfirmClear
)

const (
	fProto    = 0
	fSrcPort  = 1
	fDestIP   = 2
	fDestPort = 3
	fComment  = 4
	fMode     = 5
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("57")).Foreground(lipgloss.Color("231"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)

	// selectorFocusedStyle highlights the active option in the proto selector.
	selectorFocusedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("231")).
				Background(lipgloss.Color("57"))
	// selectorBlurredStyle draws the active option when the field is not focused.
	selectorBlurredStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("245"))
)

// protoOptions is the ordered list of protocol choices for the selector.
var protoOptions = []string{"tcp", "udp", "tcp+udp"}

// modeOptions is the ordered list of data-plane mode choices for the selector.
// The zero index (kernel) is the default so that existing rules without an
// explicit Mode field behave identically to before.
var modeOptions = []string{nft.ModeKernel, nft.ModeUserspace}

type model struct {
	mode       viewMode
	rules      []nft.Rule
	panelRules []nft.Rule // server-pushed segment, shown read-only
	cursor     int
	// editingOwner records which segment the in-progress edit targets so
	// submitEdit posts back to the right owner ("tui" or "panel").
	editingOwner string
	// editingChainID is the chain a panel chain-hop edit targets (0 = the
	// row is not a chain hop). It routes submitEdit to the chain command path
	// and selects the field-lock set.
	editingChainID int64

	inputs       []textinput.Model
	focusedInput int
	protoIdx     int // index into protoOptions; owned separately from inputs[fProto]
	modeIdx      int // index into modeOptions; owned separately from inputs[fMode]

	status string
	err    string

	width  int
	height int

	client daemonClient
}

// Run starts the TUI bound to the given daemon client. Caller (cmd) is
// responsible for verifying the daemon is reachable before invoking Run.
func Run(client daemonClient) error {
	rules, panelRules, err := loadInitialRules(client)
	if err != nil {
		return err
	}
	p := tea.NewProgram(initialModel(client, rules, panelRules), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func initialModel(client daemonClient, rules, panelRules []nft.Rule) model {
	return model{
		mode:       viewList,
		rules:      rules,
		panelRules: panelRules,
		inputs:     buildInputs(),
		client:     client,
	}
}

// loadInitialRules fetches the local (tui) and server-pushed (panel) segments
// from the daemon. nil segments become empty slices so the rest of the TUI
// does not have to nil-check.
func loadInitialRules(client daemonClient) (tui []nft.Rule, panel []nft.Rule, err error) {
	owners, err := client.GetRuleset()
	if err != nil {
		return nil, nil, fmt.Errorf("加载规则失败: %w", err)
	}
	tui = owners["tui"]
	if tui == nil {
		tui = []nft.Rule{}
	}
	panel = owners["panel"]
	if panel == nil {
		panel = []nft.Rule{}
	}
	return tui, panel, nil
}

// totalRows is the count of selectable rows across both segments: the
// editable tui segment followed by the server-managed panel segment.
func (m model) totalRows() int {
	return len(m.rules) + len(m.panelRules)
}

// rowAt resolves a unified cursor index to its rule and owner. Indices
// [0,len(rules)) map to the tui segment; the remainder map to the panel
// segment. editable is always true: chain hops are editable for their safe
// fields (listen_port/mode/comment) — which fields are locked is decided per
// row by lockedFields, not here.
func (m model) rowAt(i int) (r nft.Rule, owner string, editable bool) {
	if i < len(m.rules) {
		return m.rules[i], "tui", true
	}
	return m.panelRules[i-len(m.rules)], "panel", true
}

// lockedFields returns the form field indices that stay read-only for the
// row being edited. tui rows lock nothing. panel non-chain rows lock
// proto+listen_port (their server-side reconcile key). panel chain rows lock
// proto+target (the relay skeleton owned by the server) but free
// listen_port/mode/comment.
//
// Invariant: tui-segment rules always have ChainID==0 (they are user-managed
// local rules, never relay hops; chain hops only ever appear in the panel
// segment). So "tui rows lock nothing" and submitEdit's chain branch keying on
// owner=="panel"&&editingChainID!=0 are both safe — a tui row can never carry a
// ChainID that would misroute it onto the chain-command path.
func (m model) lockedFields() map[int]bool {
	if m.editingOwner != "panel" {
		return nil
	}
	if m.editingChainID != 0 {
		return map[int]bool{fProto: true, fDestIP: true, fDestPort: true}
	}
	return map[int]bool{fProto: true, fSrcPort: true}
}

func buildInputs() []textinput.Model {
	mk := func(placeholder string, width int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.CharLimit = 64
		ti.Width = width
		return ti
	}
	// Slot 0 (fProto) and slot 5 (fMode) are placeholders so that the
	// focusedInput index constants (fProto=0 … fMode=5) remain valid and
	// cycleFocus arithmetic is unchanged. The slots are never rendered or
	// updated — their respective pill selectors are rendered from protoIdx /
	// modeIdx instead.
	return []textinput.Model{
		mk("", 0), // fProto placeholder
		mk("监听端口 1-65535", 12),
		mk("目标 IPv4 或域名", 32),
		mk("目标端口", 12),
		mk("可选备注", 40),
		mk("", 0), // fMode placeholder
	}
}

func (m model) Init() tea.Cmd { return textinput.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch m.mode {
		case viewList:
			return m.updateList(msg)
		case viewAdd:
			return m.updateAdd(msg)
		case viewEdit:
			return m.updateEdit(msg)
		case viewConfirmDelete:
			return m.updateConfirmDelete(msg)
		case viewConfirmClear:
			return m.updateConfirmClear(msg)
		}
	}
	return m, nil
}

func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < m.totalRows()-1 {
			m.cursor++
		}
	case "a", "n", "+":
		m.enterAddMode()
		return m, textinput.Blink
	case "e":
		if m.totalRows() == 0 {
			m.status = "no rule to edit"
			return m, nil
		}
		m.enterEditMode()
		return m, textinput.Blink
	case "d", "delete":
		if m.totalRows() == 0 {
			return m, nil
		}
		r, owner, _ := m.rowAt(m.cursor)
		if owner == "tui" || r.ChainID != 0 {
			// tui rows delete locally; chain rows delete the whole chain via
			// the server. Non-chain server rows aren't deletable from here.
			m.mode = viewConfirmDelete
			m.err = ""
		} else {
			m.status = "server 托管规则不能在此删除"
		}
	case "c":
		if len(m.rules) > 0 {
			m.mode = viewConfirmClear
			m.err = ""
		}
	case "r":
		m.refresh()
	}
	return m, nil
}

func (m *model) enterAddMode() {
	m.mode = viewAdd
	m.editingOwner = "tui" // new rules always belong to the tui segment
	m.editingChainID = 0
	m.err = ""
	m.status = ""
	m.inputs = buildInputs()
	m.focusedInput = fProto
	m.protoIdx = 0 // default: tcp
	m.modeIdx = 0  // default: kernel
}

func (m *model) enterEditMode() {
	r, owner, _ := m.rowAt(m.cursor)
	m.editingOwner = owner
	m.editingChainID = r.ChainID
	m.mode = viewEdit
	m.err = ""
	m.status = ""
	m.inputs = buildInputs()
	m.focusedInput = fProto

	// Unknown stored proto (legacy data) silently falls back to tcp (idx 0).
	m.protoIdx = 0
	for i, p := range protoOptions {
		if p == r.Proto {
			m.protoIdx = i
			break
		}
	}
	// EffectiveMode maps an empty Mode (legacy kernel rules) to the kernel option at idx 0.
	m.modeIdx = 0
	for i, md := range modeOptions {
		if md == r.EffectiveMode() {
			m.modeIdx = i
			break
		}
	}
	m.inputs[fSrcPort].SetValue(strconv.Itoa(r.SrcPort))
	destValue := r.DestIP
	if r.DestHost != "" {
		destValue = r.DestHost
	}
	m.inputs[fDestIP].SetValue(destValue)
	m.inputs[fDestPort].SetValue(strconv.Itoa(r.DestPort))
	m.inputs[fComment].SetValue(r.Comment)
}

func (m model) updateAdd(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = viewList
		m.err = ""
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "tab", "down":
		m.cycleFocus(1)
		return m, textinput.Blink
	case "shift+tab", "up":
		m.cycleFocus(-1)
		return m, textinput.Blink
	case "enter":
		return m.submitAdd()
	}
	// When a pill-selector field is focused, route left/right to that selector;
	// all other keys are ignored (not forwarded to the placeholder textinput).
	if m.focusedInput == fProto {
		switch msg.String() {
		case "left", "h":
			m.protoIdx = (m.protoIdx - 1 + len(protoOptions)) % len(protoOptions)
		case "right", "l":
			m.protoIdx = (m.protoIdx + 1) % len(protoOptions)
		}
		return m, nil
	}
	if m.focusedInput == fMode {
		switch msg.String() {
		case "left", "h":
			m.modeIdx = (m.modeIdx - 1 + len(modeOptions)) % len(modeOptions)
		case "right", "l":
			m.modeIdx = (m.modeIdx + 1) % len(modeOptions)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.inputs[m.focusedInput], cmd = m.inputs[m.focusedInput].Update(msg)
	return m, cmd
}

func (m model) updateEdit(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = viewList
		m.err = ""
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "tab", "down":
		m.cycleFocus(1)
		return m, textinput.Blink
	case "shift+tab", "up":
		m.cycleFocus(-1)
		return m, textinput.Blink
	case "enter":
		return m.submitEdit()
	}
	// Locked fields swallow input. The lock set differs by row type: panel
	// non-chain pins proto+listen_port (its reconcile key); panel chain pins
	// proto+target (the relay skeleton).
	if m.lockedFields()[m.focusedInput] {
		return m, nil
	}
	// When a pill-selector field is focused, route left/right to that selector;
	// all other keys are ignored (not forwarded to the placeholder textinput).
	if m.focusedInput == fProto {
		switch msg.String() {
		case "left", "h":
			m.protoIdx = (m.protoIdx - 1 + len(protoOptions)) % len(protoOptions)
		case "right", "l":
			m.protoIdx = (m.protoIdx + 1) % len(protoOptions)
		}
		return m, nil
	}
	if m.focusedInput == fMode {
		switch msg.String() {
		case "left", "h":
			m.modeIdx = (m.modeIdx - 1 + len(modeOptions)) % len(modeOptions)
		case "right", "l":
			m.modeIdx = (m.modeIdx + 1) % len(modeOptions)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.inputs[m.focusedInput], cmd = m.inputs[m.focusedInput].Update(msg)
	return m, cmd
}

func (m model) submitEdit() (tea.Model, tea.Cmd) {
	proto := protoOptions[m.protoIdx]
	srcPortStr := strings.TrimSpace(m.inputs[fSrcPort].Value())
	destInput := strings.TrimSpace(m.inputs[fDestIP].Value())
	destPortStr := strings.TrimSpace(m.inputs[fDestPort].Value())
	comment := strings.TrimSpace(m.inputs[fComment].Value())

	srcPort, err := strconv.Atoi(srcPortStr)
	if err != nil {
		m.err = "端口必须为数字"
		return m, nil
	}

	owner := m.editingOwner

	// Chain hops are server-authoritative: only listen_port/mode/comment are
	// editable (proto/target are the locked relay skeleton). Send a command
	// and let the server re-dispatch — don't optimistically mutate the local
	// row, since the real result (including upstream changes on other nodes)
	// arrives via the next push. The locked target port is never sent, so it is
	// deliberately not parsed/validated before this branch — a chain edit must
	// not be blockable by a field the operator cannot reach.
	if owner == "panel" && m.editingChainID != 0 {
		if err := m.client.ChainEdit(m.editingChainID, srcPort, modeOptions[m.modeIdx], comment); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.mode = viewList
		m.status = fmt.Sprintf("已提交链路端口/模式变更（监听 %d），按 r 刷新查看", srcPort)
		m.err = ""
		return m, nil
	}

	destPort, err := strconv.Atoi(destPortStr)
	if err != nil {
		m.err = "端口必须为数字"
		return m, nil
	}

	var seg []nft.Rule
	var idx int
	if owner == "panel" {
		seg = m.panelRules
		idx = m.cursor - len(m.rules)
	} else {
		seg = m.rules
		idx = m.cursor
	}

	// Panel rows key on (proto, listen_port) server-side; pin both to the
	// original values so an edit can never re-key the row and silently lose
	// the reconcile. The form layer also locks these inputs — this is a
	// second guard in case that lock is bypassed.
	if owner == "panel" {
		proto = seg[idx].Proto
		srcPort = seg[idx].SrcPort
	}

	r := nft.Rule{
		ID:        seg[idx].ID,
		Proto:     proto,
		Mode:      modeOptions[m.modeIdx],
		SrcPort:   srcPort,
		DestPort:  destPort,
		Comment:   comment,
		ChainID:   seg[idx].ChainID, // preserved; for editable rows this is 0
		ChainName: seg[idx].ChainName,
	}
	if resolver.IsHostname(destInput) {
		r.DestHost = destInput
	} else {
		r.DestIP = destInput
	}
	if err := nft.Validate(r); err != nil {
		m.err = err.Error()
		return m, nil
	}
	for i, existing := range seg {
		if i != idx && existing.Proto == r.Proto && existing.SrcPort == r.SrcPort {
			m.err = fmt.Sprintf("%s/%d 已被转发占用", r.Proto, r.SrcPort)
			return m, nil
		}
	}

	next := append([]nft.Rule{}, seg...)
	next[idx] = r
	applied, err := commitOwner(m.client, owner, next)
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	if owner == "panel" {
		m.panelRules = applied
	} else {
		m.rules = applied
	}
	m.mode = viewList
	statusTarget := r.DestIP
	if r.DestHost != "" {
		statusTarget = r.DestHost
	}
	m.status = fmt.Sprintf("已更新 %s/%d → %s:%d", r.Proto, r.SrcPort, statusTarget, r.DestPort)
	m.err = ""
	return m, nil
}

func (m *model) cycleFocus(dir int) {
	m.inputs[m.focusedInput].Blur()
	m.focusedInput = (m.focusedInput + dir + len(m.inputs)) % len(m.inputs)
	m.inputs[m.focusedInput].Focus()
}

func (m model) submitAdd() (tea.Model, tea.Cmd) {
	proto := protoOptions[m.protoIdx]
	srcPortStr := strings.TrimSpace(m.inputs[fSrcPort].Value())
	destInput := strings.TrimSpace(m.inputs[fDestIP].Value())
	destPortStr := strings.TrimSpace(m.inputs[fDestPort].Value())
	comment := strings.TrimSpace(m.inputs[fComment].Value())

	srcPort, err1 := strconv.Atoi(srcPortStr)
	destPort, err2 := strconv.Atoi(destPortStr)
	if err1 != nil || err2 != nil {
		m.err = "端口必须为数字"
		return m, nil
	}

	r := nft.Rule{
		ID:       nft.NewRuleID(),
		Proto:    proto,
		Mode:     modeOptions[m.modeIdx],
		SrcPort:  srcPort,
		DestPort: destPort,
		Comment:  comment,
	}
	if resolver.IsHostname(destInput) {
		r.DestHost = destInput
	} else {
		r.DestIP = destInput
	}
	if err := nft.Validate(r); err != nil {
		m.err = err.Error()
		return m, nil
	}
	for _, existing := range m.rules {
		if existing.Proto == r.Proto && existing.SrcPort == r.SrcPort {
			m.err = fmt.Sprintf("%s/%d 已被转发占用", r.Proto, r.SrcPort)
			return m, nil
		}
	}

	next := append([]nft.Rule{}, m.rules...)
	next = append(next, r)
	applied, err := commit(m.client, next)
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	m.rules = applied
	m.mode = viewList
	statusTarget := r.DestIP
	if r.DestHost != "" {
		statusTarget = r.DestHost
	}
	m.status = fmt.Sprintf("已添加 %s/%d → %s:%d", r.Proto, r.SrcPort, statusTarget, r.DestPort)
	m.err = ""
	return m, nil
}

func (m model) updateConfirmDelete(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		r, owner, _ := m.rowAt(m.cursor)
		if owner == "panel" && r.ChainID != 0 {
			if err := m.client.ChainDelete(r.ChainID); err != nil {
				m.err = err.Error()
				m.mode = viewList
				return m, nil
			}
			m.status = fmt.Sprintf("已提交删除链路「%s」，按 r 刷新查看", r.ChainName)
			m.mode = viewList
			return m, nil
		}
		if owner != "tui" {
			m.mode = viewList
			return m, nil
		}
		removed := m.rules[m.cursor]
		next := append([]nft.Rule{}, m.rules[:m.cursor]...)
		next = append(next, m.rules[m.cursor+1:]...)
		applied, err := commit(m.client, next)
		if err != nil {
			m.err = err.Error()
			m.mode = viewList
			return m, nil
		}
		m.rules = applied
		if m.cursor >= m.totalRows() && m.cursor > 0 {
			m.cursor--
		}
		m.status = fmt.Sprintf("已删除 %s/%d", removed.Proto, removed.SrcPort)
		m.mode = viewList
	case "n", "N", "esc":
		m.mode = viewList
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m model) updateConfirmClear(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		applied, err := commit(m.client, nil)
		if err != nil {
			m.err = err.Error()
			m.mode = viewList
			return m, nil
		}
		m.rules = applied
		m.cursor = 0
		m.status = "已清空全部转发规则"
		m.mode = viewList
	case "n", "N", "esc":
		m.mode = viewList
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) refresh() {
	owners, err := m.client.GetRuleset()
	if err != nil {
		m.err = err.Error()
		return
	}
	tui := owners["tui"]
	if tui == nil {
		tui = []nft.Rule{}
	}
	m.rules = tui
	panel := owners["panel"]
	if panel == nil {
		panel = []nft.Rule{}
	}
	m.panelRules = panel
	if m.cursor >= m.totalRows() {
		m.cursor = m.totalRows() - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.status = "已从 daemon 重新加载"
}

// commitOwner posts a full segment snapshot for owner to the daemon. Raw
// rules go on the wire — the daemon resolves hostnames at apply time — so
// DestHost/DestIP are sent as the user typed them.
func commitOwner(client daemonClient, owner string, rules []nft.Rule) ([]nft.Rule, error) {
	if rules == nil {
		rules = []nft.Rule{}
	}
	if err := client.PostRuleset(owner, rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func commit(client daemonClient, rules []nft.Rule) ([]nft.Rule, error) {
	return commitOwner(client, "tui", rules)
}

func (m model) View() string {
	var inner string
	switch m.mode {
	case viewAdd, viewEdit:
		inner = m.viewForm()
	case viewConfirmDelete:
		if r, owner, _ := m.rowAt(m.cursor); owner == "panel" && r.ChainID != 0 {
			inner = m.viewConfirm(fmt.Sprintf(
				"确认删除整条链路「%s」？\n\n  这会删除该链路在所有节点上的全部转发，不可恢复。\n", r.ChainName))
		} else {
			inner = m.viewConfirm(
				fmt.Sprintf("确认删除该规则？\n\n  %s\n", r.Display()))
		}
	case viewConfirmClear:
		inner = m.viewConfirm(
			fmt.Sprintf("确认清空全部 %d 条转发规则？", len(m.rules)))
	default:
		inner = m.viewList()
	}
	// Wrap the entire viewport with a 2-cell horizontal margin on each side.
	// PaddingLeft/PaddingRight (not Margin) is used so the background of the
	// padding area inherits the terminal default, keeping the margins plain
	// while the selected-row highlight is contained within the inner content.
	return lipgloss.NewStyle().PaddingLeft(colMargin).PaddingRight(colMargin).Render(inner)
}

// Column widths in terminal cells (CJK double-width characters count as 2 cells).
// These constants must match between the header and every data row. Each fixed
// column reserves colGap trailing cells (content is truncated to width-colGap)
// so adjacent columns never visually merge, even when content fills the width.
const (
	colGap     = 2
	colOwner   = 16 // 本地 / server / 链路 X（链路名过长则截断）
	colTenant  = 12 // 租户名 / —
	colProto   = 10 // tcp+udp / tcp (U)
	colSrcPort = 12 // 65535
	colDest    = 24 // IPv4(15) 或常见域名 + gap
	colDstPort = 12 // 65535
	// colComment is flexible — it consumes the remainder of the line.

	// colMargin is the horizontal margin (in cells) on each side of the viewport.
	colMargin = 2
)

// cellStyle returns a lipgloss style that pads/truncates to exactly w terminal cells.
func cellStyle(w int) lipgloss.Style {
	return lipgloss.NewStyle().Width(w).MaxWidth(w)
}

// truncateCell truncates s so that its display width does not exceed maxCells,
// appending an ellipsis if truncation occurs. Width is measured via lipgloss
// (which uses go-runewidth internally but applies its own Unicode normalization,
// e.g. treating → as 1 cell). Using lipgloss.Width here keeps measurements
// consistent with what lipgloss actually renders.
func truncateCell(s string, maxCells int) string {
	if lipgloss.Width(s) <= maxCells {
		return s
	}
	// Reserve 1 cell for the ellipsis "…".
	limit := maxCells - 1
	width := 0
	var out []rune
	for _, r := range []rune(s) {
		rw := lipgloss.Width(string(r))
		if width+rw > limit {
			break
		}
		out = append(out, r)
		width += rw
	}
	return string(out) + "…"
}

// padCol renders s into a fixed colW-cell column, truncating content to
// colW-colGap so at least colGap trailing cells separate it from the next
// column.
func padCol(s string, colW int) string {
	return cellStyle(colW).Render(truncateCell(s, colW-colGap))
}

// renderTableRow assembles a fixed-width table row from five cell strings:
// proto, srcPort, dest, dstPort (each a gap-padded fixed column) and comment
// (flexible, already styled by the caller). The assembled line carries no
// styling of its own — callers apply row styles after.
func renderTableRow(proto, srcPort, dest, dstPort, comment string) string {
	return padCol(proto, colProto) +
		padCol(srcPort, colSrcPort) +
		padCol(dest, colDest) +
		padCol(dstPort, colDstPort) +
		comment
}

func (m model) viewList() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("nft-forward — IPv4 端口转发") + "\n\n")

	if m.totalRows() == 0 {
		b.WriteString(helpStyle.Render("  （暂无规则 — 按 a 新增）") + "\n")
	} else {
		header := padCol("来源", colOwner) +
			padCol("用户", colTenant) +
			renderTableRow("协议", "本机端口", "目标", "远程端口", "备注")
		b.WriteString(headerStyle.Render(header) + "\n")

		fixedWidth := colOwner + colTenant + colProto + colSrcPort + colDest + colDstPort
		const minComment = 10
		innerWidth := m.width - 2*colMargin
		if innerWidth < fixedWidth+minComment {
			// Narrow terminal: keep a minimum comment column so commentWidth
			// never goes negative; the row may exceed the viewport and the
			// terminal soft-wraps, which is preferable to a broken render.
			innerWidth = fixedWidth + minComment
		}
		commentWidth := innerWidth - fixedWidth

		for i := 0; i < m.totalRows(); i++ {
			r, owner, _ := m.rowAt(i)
			destHost := r.DestIP
			if r.DestHost != "" {
				destHost = r.DestHost
			}
			protoCell := strings.ToLower(r.Proto)
			if r.EffectiveMode() == nft.ModeUserspace {
				protoCell += " (U)"
			}
			ownerTag := "本地"
			if owner == "panel" {
				if r.ChainID != 0 {
					ownerTag = "链路 " + r.ChainName
				} else {
					ownerTag = "server"
				}
			}
			tenantTag := "—"
			if r.TenantName != "" {
				tenantTag = r.TenantName
			}
			line := padCol(ownerTag, colOwner) +
				padCol(tenantTag, colTenant) +
				renderTableRow(
					protoCell,
					strconv.Itoa(r.SrcPort),
					destHost,
					strconv.Itoa(r.DestPort),
					cellStyle(commentWidth).Render(r.Comment),
				)
			if i == m.cursor {
				b.WriteString(selectedStyle.Render(line) + "\n")
			} else {
				b.WriteString(line + "\n")
			}
		}
	}

	b.WriteString("\n")
	if m.err != "" {
		b.WriteString(errStyle.Render("错误: "+m.err) + "\n")
	} else if m.status != "" {
		b.WriteString(okStyle.Render(m.status) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ 选择 • a 新增 • e 编辑 • d 删除 • c 清空 • r 重载 • q 退出"))
	return b.String()
}

func (m model) formTitle() string {
	if m.mode == viewEdit {
		return "编辑转发规则"
	}
	return "新增转发规则"
}

// renderProtoSelector renders the horizontal pill selector for the proto field.
// When focused, the active option is highlighted with selectorFocusedStyle;
// when blurred, it uses selectorBlurredStyle.
func (m model) renderProtoSelector() string {
	focused := m.focusedInput == fProto
	var parts []string
	for i, opt := range protoOptions {
		if i == m.protoIdx {
			label := "[ " + opt + " ]"
			if focused {
				parts = append(parts, selectorFocusedStyle.Render(label))
			} else {
				parts = append(parts, selectorBlurredStyle.Render(label))
			}
		} else {
			parts = append(parts, "  "+opt+"  ")
		}
	}
	return strings.Join(parts, " ")
}

// renderModeSelector renders the horizontal pill selector for the mode field.
// The UX mirrors renderProtoSelector so both selectors behave consistently.
func (m model) renderModeSelector() string {
	focused := m.focusedInput == fMode
	var parts []string
	for i, opt := range modeOptions {
		if i == m.modeIdx {
			label := "[ " + opt + " ]"
			if focused {
				parts = append(parts, selectorFocusedStyle.Render(label))
			} else {
				parts = append(parts, selectorBlurredStyle.Render(label))
			}
		} else {
			parts = append(parts, "  "+opt+"  ")
		}
	}
	return strings.Join(parts, " ")
}

func (m model) viewForm() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.formTitle()) + "\n\n")

	labels := []string{
		"协议       ",
		"监听端口   ",
		"目标地址   ",
		"目标端口   ",
		"备注       ",
		"模式       ",
	}
	for i, ti := range m.inputs {
		marker := "  "
		if i == m.focusedInput {
			marker = "▌ "
		}
		var fieldView string
		if i == fProto {
			fieldView = m.renderProtoSelector()
		} else if i == fMode {
			fieldView = m.renderModeSelector()
		} else {
			fieldView = ti.View()
		}
		if m.lockedFields()[i] {
			fieldView += helpStyle.Render("  (固定)")
		}
		b.WriteString(fmt.Sprintf("%s%s  %s\n", marker, labels[i], fieldView))
	}

	b.WriteString("\n")
	if m.err != "" {
		b.WriteString(errStyle.Render("错误: "+m.err) + "\n\n")
	}
	helpText := "Tab 下一项 • Shift+Tab 上一项 • Enter 保存 • Esc 取消"
	if m.focusedInput == fProto || m.focusedInput == fMode {
		helpText = "← → 切换选项 • " + helpText
	}
	b.WriteString(helpStyle.Render(helpText))
	return b.String()
}

func (m model) viewConfirm(question string) string {
	var b strings.Builder
	b.WriteString(warnStyle.Render("确认") + "\n\n")
	b.WriteString(question + "\n")
	b.WriteString(helpStyle.Render("y 确认 • n / Esc 取消"))
	return b.String()
}
