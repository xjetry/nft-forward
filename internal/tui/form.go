package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"nft-forward/internal/nft"
	"nft-forward/internal/resolver"
)

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
		return m.commitForm(true)
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

// commitForm collects form fields, validates, and either creates a new rule
// (isEdit=false, appends to the tui segment) or updates the existing rule at
// the cursor (isEdit=true, may target the tui or panel segment). Shared
// validation and duplicate-detection logic lives here once.
func (m model) commitForm(isEdit bool) (tea.Model, tea.Cmd) {
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

	// --- edit-only: chain hops are server-authoritative ---
	// Only listen_port/mode/comment are editable (proto/target are the locked
	// relay skeleton). Send a command and let the server re-dispatch — don't
	// optimistically mutate the local row, since the real result (including
	// upstream changes on other nodes) arrives via the next push. The locked
	// target port is never sent, so it is deliberately not parsed/validated
	// before this branch — a chain edit must not be blockable by a field the
	// operator cannot reach.
	if isEdit && m.editingOwner == "panel" && m.editingChainID != 0 {
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

	if isEdit {
		return m.commitEdit(proto, srcPort, destInput, destPort, comment)
	}
	return m.commitAdd(proto, srcPort, destInput, destPort, comment)
}

// commitAdd creates a new rule and appends it to the tui segment.
func (m model) commitAdd(proto string, srcPort int, destInput string, destPort int, comment string) (tea.Model, tea.Cmd) {
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

// commitEdit updates the existing rule at the cursor position. It handles
// both tui and panel segments, pinning reconcile-key fields for panel rows.
func (m model) commitEdit(proto string, srcPort int, destInput string, destPort int, comment string) (tea.Model, tea.Cmd) {
	owner := m.editingOwner

	var seg []nft.Rule
	var idx int
	if owner == "panel" {
		seg = m.panelRules
		idx = m.cursor - len(m.rules)
	} else {
		seg = m.rules
		idx = m.cursor
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

// submitAdd is the legacy entry point called by tests; delegates to commitForm.
func (m model) submitAdd() (tea.Model, tea.Cmd) {
	return m.commitForm(false)
}

// submitEdit is the legacy entry point called by tests; delegates to commitForm.
func (m model) submitEdit() (tea.Model, tea.Cmd) {
	return m.commitForm(true)
}
