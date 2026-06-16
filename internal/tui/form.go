package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"nft-forward/internal/daemonclient"
)

func (m *model) enterAddMode() {
	m.mode = viewAdd
	m.editingRuleID = 0
	m.err = ""
	m.status = ""
	m.inputs = buildInputs()
	m.focusedInput = fProto
	m.protoIdx = 0 // default: tcp
	m.modeIdx = 0  // default: kernel
}

func (m *model) enterEditMode() {
	r := m.rowAt(m.cursor)
	m.editingRuleID = r.RuleID
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

func (m *model) cycleFocus(dir int) {
	m.inputs[m.focusedInput].Blur()
	m.focusedInput = (m.focusedInput + dir + len(m.inputs)) % len(m.inputs)
	m.inputs[m.focusedInput].Focus()
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
		return m.commitForm(false)
	}
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
		return m.commitForm(true)
	}
	if m.lockedFields()[m.focusedInput] {
		return m, nil
	}
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

// commitForm collects form fields, validates locally (basic sanity only),
// and delegates to the daemon via CreateRule or UpdateRule.
func (m model) commitForm(isEdit bool) (tea.Model, tea.Cmd) {
	proto := protoOptions[m.protoIdx]
	srcPortStr := strings.TrimSpace(m.inputs[fSrcPort].Value())
	destInput := strings.TrimSpace(m.inputs[fDestIP].Value())
	destPortStr := strings.TrimSpace(m.inputs[fDestPort].Value())
	comment := strings.TrimSpace(m.inputs[fComment].Value())

	// SrcPort 0 = auto-assign by daemon.
	srcPort := 0
	if srcPortStr != "" {
		var err error
		srcPort, err = strconv.Atoi(srcPortStr)
		if err != nil || srcPort < 1 || srcPort > 65535 {
			m.err = "端口必须为 1-65535 的数字"
			return m, nil
		}
	}

	destPort, err := strconv.Atoi(destPortStr)
	if err != nil {
		m.err = "目标端口必须为数字"
		return m, nil
	}

	if destInput == "" {
		m.err = "目标地址不能为空"
		return m, nil
	}

	if isEdit {
		return m.commitEdit(proto, srcPort, destInput, destPort, comment)
	}
	return m.commitAdd(proto, srcPort, destInput, destPort, comment)
}

func (m model) commitAdd(proto string, srcPort int, destInput string, destPort int, comment string) (tea.Model, tea.Cmd) {
	resp, err := m.client.CreateRule(daemonclient.CreateRuleReq{
		Proto:      proto,
		ExitHost:   destInput,
		ExitPort:   destPort,
		ListenPort: srcPort,
		Mode:       modeOptions[m.modeIdx],
		Comment:    comment,
	})
	if err != nil {
		m.err = err.Error()
		return m, nil
	}
	m.refresh()
	m.mode = viewList
	entry := resp.Entry
	if entry == "" {
		entry = fmt.Sprintf(":%d", resp.ListenPort)
	}
	m.status = fmt.Sprintf("已添加 %s → %s:%d 入口 %s", proto, destInput, destPort, entry)
	m.err = ""
	return m, nil
}

func (m model) commitEdit(proto string, srcPort int, destInput string, destPort int, comment string) (tea.Model, tea.Cmd) {
	r := m.rowAt(m.cursor)
	// Server rules use their numeric RuleID; local rules use their hex ID.
	id := r.ID
	if r.RuleID != 0 {
		id = strconv.FormatInt(r.RuleID, 10)
	}
	if err := m.client.UpdateRule(id, daemonclient.UpdateRuleReq{
		Proto:      proto,
		ExitHost:   destInput,
		ExitPort:   destPort,
		ListenPort: srcPort,
		Mode:       modeOptions[m.modeIdx],
		Comment:    comment,
	}); err != nil {
		m.err = err.Error()
		return m, nil
	}
	m.refresh()
	m.mode = viewList
	m.status = fmt.Sprintf("已更新 %s → %s:%d", proto, destInput, destPort)
	m.err = ""
	return m, nil
}

// submitAdd is the legacy entry point called by tests; delegates to commitForm.
func (m model) submitAdd() (tea.Model, tea.Cmd) {
	return m.commitForm(false)
}

// submitEdit is the legacy entry point called by tests; delegates to commitForm.
func (m model) submitEdit() (tea.Model, tea.Cmd) {
	return m.commitForm(true)
}
