package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
	"nft-forward/internal/store"
	"nft-forward/internal/systemd"
)

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
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("57")).Foreground(lipgloss.Color("231"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
)

type model struct {
	mode   viewMode
	rules  []nft.Rule
	cursor int

	inputs       []textinput.Model
	focusedInput int

	status string
	err    string

	width  int
	height int
}

func Run(rules []nft.Rule) error {
	p := tea.NewProgram(initialModel(rules), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func initialModel(rules []nft.Rule) model {
	return model{mode: viewList, rules: rules, inputs: buildInputs()}
}

func buildInputs() []textinput.Model {
	mk := func(placeholder string, width int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.CharLimit = 64
		ti.Width = width
		return ti
	}
	protoIn := mk("tcp 或 udp", 12)
	protoIn.SetValue("tcp")
	protoIn.Focus()
	return []textinput.Model{
		protoIn,
		mk("监听端口 1-65535", 12),
		mk("目标 IPv4 或域名", 32),
		mk("目标端口", 12),
		mk("可选备注", 40),
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
		if m.cursor < len(m.rules)-1 {
			m.cursor++
		}
	case "a", "n", "+":
		m.enterAddMode()
		return m, textinput.Blink
	case "e":
		if len(m.rules) == 0 {
			m.status = "no rule to edit"
			return m, nil
		}
		m.enterEditMode()
		return m, textinput.Blink
	case "d", "delete":
		if len(m.rules) > 0 {
			m.mode = viewConfirmDelete
			m.err = ""
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
	m.err = ""
	m.status = ""
	m.inputs = buildInputs()
	m.focusedInput = fProto
}

func (m *model) enterEditMode() {
	m.mode = viewEdit
	m.err = ""
	m.status = ""
	m.inputs = buildInputs()
	m.focusedInput = fProto

	r := m.rules[m.cursor]
	m.inputs[fProto].SetValue(r.Proto)
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
	var cmd tea.Cmd
	m.inputs[m.focusedInput], cmd = m.inputs[m.focusedInput].Update(msg)
	return m, cmd
}

func (m model) submitEdit() (tea.Model, tea.Cmd) {
	proto := strings.ToLower(strings.TrimSpace(m.inputs[fProto].Value()))
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
		ID:       m.rules[m.cursor].ID,
		Proto:    proto,
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
	for i, existing := range m.rules {
		if i != m.cursor && existing.Proto == r.Proto && existing.SrcPort == r.SrcPort {
			m.err = fmt.Sprintf("%s/%d 已被转发占用", r.Proto, r.SrcPort)
			return m, nil
		}
	}

	next := append([]nft.Rule{}, m.rules...)
	next[m.cursor] = r
	applied, err := commit(next)
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
	proto := strings.ToLower(strings.TrimSpace(m.inputs[fProto].Value()))
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
	applied, err := commit(next)
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
		if m.cursor >= len(m.rules) {
			m.mode = viewList
			return m, nil
		}
		removed := m.rules[m.cursor]
		next := append([]nft.Rule{}, m.rules[:m.cursor]...)
		next = append(next, m.rules[m.cursor+1:]...)
		applied, err := commit(next)
		if err != nil {
			m.err = err.Error()
			m.mode = viewList
			return m, nil
		}
		m.rules = applied
		if m.cursor >= len(m.rules) && m.cursor > 0 {
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
		applied, err := commit(nil)
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
	rules, err := store.Load()
	if err != nil {
		m.err = err.Error()
		return
	}
	m.rules = rules
	m.status = "已从磁盘重新加载"
}

func commit(rules []nft.Rule) ([]nft.Rule, error) {
	if rules == nil {
		rules = []nft.Rule{}
	}
	resolved, _, dnsErr := nft.ResolveHosts(context.Background(), rules, resolver.New())
	if dnsErr != nil {
		return nil, dnsErr
	}
	for _, rl := range resolved {
		if rl.DestIP == "" {
			return nil, fmt.Errorf("%s/%d: 无法解析目标域名 %s", rl.Proto, rl.SrcPort, rl.DestHost)
		}
	}
	if err := nft.Apply(resolved); err != nil {
		return nil, err
	}
	if err := store.Save(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func (m model) View() string {
	switch m.mode {
	case viewAdd, viewEdit:
		return m.viewForm()
	case viewConfirmDelete:
		return m.viewConfirm(
			fmt.Sprintf("确认删除该规则？\n\n  %s\n", m.rules[m.cursor].Display()))
	case viewConfirmClear:
		return m.viewConfirm(
			fmt.Sprintf("确认清空全部 %d 条转发规则？", len(m.rules)))
	default:
		return m.viewList()
	}
}

// Column widths in terminal cells (CJK double-width characters count as 2 cells).
// These constants must match between the header and every data row.
const (
	colProto    = 6  // "TCP   " or "UDP   "
	colSrcPort  = 8  // "  8088  "
	colDest     = 28 // "→ 100.100.100.11:8088       "
	// colComment is flexible — it consumes the remainder of the line
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

// renderTableRow assembles a fixed-width table row from four cell strings.
// Columns: proto (colProto cells), srcPort (colSrcPort cells),
// dest (colDest cells), comment (flexible).
// The assembled line contains no styling — callers apply styles after.
func renderTableRow(proto, srcPort, dest, comment string) string {
	return cellStyle(colProto).Render(truncateCell(proto, colProto)) +
		cellStyle(colSrcPort).Render(truncateCell(srcPort, colSrcPort)) +
		cellStyle(colDest).Render(truncateCell(dest, colDest)) +
		comment
}

func (m model) viewList() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("nft-forward — IPv4 端口转发") + "  ")
	if systemd.Installed() {
		b.WriteString(okStyle.Render("● 开机持久化已启用") + "\n\n")
	} else {
		b.WriteString(warnStyle.Render("○ 开机持久化未启用") + "\n\n")
	}

	if len(m.rules) == 0 {
		b.WriteString(helpStyle.Render("  （暂无规则 — 按 a 新增）") + "\n")
	} else {
		// Header row uses the same column model as data rows.
		header := renderTableRow("协议", "监听", "→ 目标", "备注")
		b.WriteString(headerStyle.Render(header) + "\n")

		for i, r := range m.rules {
			target := r.DestIP
			if r.DestHost != "" {
				if r.DestIP != "" {
					target = fmt.Sprintf("%s (→ %s)", r.DestHost, r.DestIP)
				} else {
					target = r.DestHost
				}
			}
			destCell := fmt.Sprintf("→ %s:%d", target, r.DestPort)
			line := renderTableRow(
				strings.ToUpper(r.Proto),
				strconv.Itoa(r.SrcPort),
				destCell,
				r.Comment,
			)
			if i == m.cursor {
				// Determine the fixed portion width so the highlight spans the full row.
				fixedWidth := colProto + colSrcPort + colDest
				// Pad comment cell to fill the terminal width (or a sane default).
				termWidth := m.width
				if termWidth < fixedWidth+1 {
					termWidth = 80
				}
				commentWidth := termWidth - fixedWidth
				paddedLine := renderTableRow(
					strings.ToUpper(r.Proto),
					strconv.Itoa(r.SrcPort),
					destCell,
					cellStyle(commentWidth).Render(r.Comment),
				)
				b.WriteString(selectedStyle.Render(paddedLine) + "\n")
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

func (m model) viewForm() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.formTitle()) + "\n\n")

	labels := []string{
		"协议       ",
		"监听端口   ",
		"目标地址   ",
		"目标端口   ",
		"备注       ",
	}
	for i, ti := range m.inputs {
		marker := "  "
		if i == m.focusedInput {
			marker = "▌ "
		}
		b.WriteString(fmt.Sprintf("%s%s  %s\n", marker, labels[i], ti.View()))
	}

	b.WriteString("\n")
	if m.err != "" {
		b.WriteString(errStyle.Render("错误: "+m.err) + "\n\n")
	}
	b.WriteString(helpStyle.Render("Tab 下一项 • Shift+Tab 上一项 • Enter 保存 • Esc 取消"))
	return b.String()
}

func (m model) viewConfirm(question string) string {
	var b strings.Builder
	b.WriteString(warnStyle.Render("确认") + "\n\n")
	b.WriteString(question + "\n")
	b.WriteString(helpStyle.Render("y 确认 • n / Esc 取消"))
	return b.String()
}
