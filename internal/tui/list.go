package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"nft-forward/internal/nft"
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
