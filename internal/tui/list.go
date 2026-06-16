package tui

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

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
		m.mode = viewConfirmDelete
		m.err = ""
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

func (m model) updateConfirmDelete(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		r := m.rowAt(m.cursor)
		id := r.ID
		if r.RuleID != 0 {
			id = strconv.FormatInt(r.RuleID, 10)
		}
		if err := m.client.DeleteRule(id); err != nil {
			m.err = err.Error()
			m.mode = viewList
			return m, nil
		}
		m.refresh()
		if m.cursor >= m.totalRows() && m.cursor > 0 {
			m.cursor--
		}
		m.status = fmt.Sprintf("已删除 %s/%d", r.Proto, r.SrcPort)
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
		for _, r := range m.rules {
			id := r.ID
			if r.RuleID != 0 {
				id = strconv.FormatInt(r.RuleID, 10)
			}
			if err := m.client.DeleteRule(id); err != nil {
				m.err = err.Error()
				m.mode = viewList
				return m, nil
			}
		}
		m.refresh()
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
