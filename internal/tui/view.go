package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"nft-forward/internal/nft"
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
