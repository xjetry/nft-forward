package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
)

// daemonClient is the subset of daemonclient.Client the TUI relies on.
// Declared locally so the TUI test suite can substitute a fake.
type daemonClient interface {
	Status() (daemonclient.StatusResp, error)
	ListRules() ([]nft.Rule, error)
	CreateRule(daemonclient.CreateRuleReq) (daemonclient.CreateRuleResp, error)
	UpdateRule(id string, req daemonclient.UpdateRuleReq) error
	DeleteRule(id string) error
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

// protoOptions is the ordered list of protocol choices for the selector.
var protoOptions = []string{"tcp", "udp", "tcp+udp"}

// modeOptions is the ordered list of data-plane mode choices for the selector.
// The zero index (kernel) is the default so that existing rules without an
// explicit Mode field behave identically to before.
var modeOptions = []string{nft.ModeKernel, nft.ModeUserspace}

type model struct {
	mode   viewMode
	rules  []nft.Rule
	cursor int
	// editingRuleID is the rule a rule-hop edit targets (0 = the
	// row is not a rule hop). It routes submitEdit to the rule command path
	// and selects the field-lock set.
	editingRuleID int64

	inputs       []textinput.Model
	focusedInput int
	protoIdx     int // index into protoOptions; owned separately from inputs[fProto]
	modeIdx      int // index into modeOptions; owned separately from inputs[fMode]

	status string
	err    string

	width  int
	height int

	connected bool
	nodeName  string

	client daemonClient
}

// Run starts the TUI bound to the given daemon client. Caller (cmd) is
// responsible for verifying the daemon is reachable before invoking Run.
func Run(client daemonClient) error {
	rules, connected, nodeName, err := loadInitialRules(client)
	if err != nil {
		return err
	}
	p := tea.NewProgram(initialModel(client, rules, connected, nodeName), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func initialModel(client daemonClient, rules []nft.Rule, connected bool, nodeName string) model {
	return model{
		mode:      viewList,
		rules:     rules,
		inputs:    buildInputs(),
		client:    client,
		connected: connected,
		nodeName:  nodeName,
	}
}

// loadInitialRules fetches the active ruleset and connection status from the daemon.
func loadInitialRules(client daemonClient) ([]nft.Rule, bool, string, error) {
	st, _ := client.Status()
	rules, err := client.ListRules()
	if err != nil {
		return nil, false, "", fmt.Errorf("加载规则失败: %w", err)
	}
	if rules == nil {
		rules = []nft.Rule{}
	}
	return rules, st.Connected, st.NodeName, nil
}

func (m model) totalRows() int {
	return len(m.rules)
}

func (m model) rowAt(i int) nft.Rule {
	return m.rules[i]
}

// lockedFields returns the form field indices that stay read-only for the
// row being edited. Single-hop and local rows lock nothing — all fields are
// editable. Multi-hop chain rows lock proto+target (the relay skeleton owned
// by the server) but free listen_port/mode/comment.
func (m model) lockedFields() map[int]bool {
	if m.cursor >= len(m.rules) {
		return nil
	}
	r := m.rowAt(m.cursor)
	if r.HopCount > 1 {
		return map[int]bool{fProto: true, fDestIP: true, fDestPort: true}
	}
	return nil
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
		mk("监听端口（空=自动）", 12),
		mk("目标 IP 或域名", 32),
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

func (m *model) refresh() {
	rules, connected, nodeName, err := loadInitialRules(m.client)
	if err != nil {
		m.err = err.Error()
		return
	}
	m.rules = rules
	m.connected = connected
	m.nodeName = nodeName
	if m.cursor >= m.totalRows() {
		m.cursor = m.totalRows() - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.status = "已从 daemon 重新加载"
}
