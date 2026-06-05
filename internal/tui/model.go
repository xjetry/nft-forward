package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
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
	// editingChainID is the chain a chain-hop edit targets (0 = the
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
	rules, err := loadInitialRules(client)
	if err != nil {
		return err
	}
	p := tea.NewProgram(initialModel(client, rules), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func initialModel(client daemonClient, rules []nft.Rule) model {
	return model{
		mode:   viewList,
		rules:  rules,
		inputs: buildInputs(),
		client: client,
	}
}

// loadInitialRules fetches the merged ruleset from the daemon. The TUI
// operates on a single unified segment ("panel"); legacy "tui" rules are
// included so nothing disappears during migration.
func loadInitialRules(client daemonClient) ([]nft.Rule, error) {
	owners, err := client.GetRuleset()
	if err != nil {
		return nil, fmt.Errorf("加载规则失败: %w", err)
	}
	var rules []nft.Rule
	rules = append(rules, owners["panel"]...)
	rules = append(rules, owners["tui"]...)
	if rules == nil {
		rules = []nft.Rule{}
	}
	return rules, nil
}

func (m model) totalRows() int {
	return len(m.rules)
}

func (m model) rowAt(i int) nft.Rule {
	return m.rules[i]
}

// lockedFields returns the form field indices that stay read-only for the
// row being edited. Standalone rows lock nothing — all fields are editable.
// Chain rows lock proto+target (the relay skeleton owned by the server) but
// free listen_port/mode/comment.
func (m model) lockedFields() map[int]bool {
	if m.editingChainID != 0 {
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

func (m *model) refresh() {
	rules, err := loadInitialRules(m.client)
	if err != nil {
		m.err = err.Error()
		return
	}
	m.rules = rules
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
	return commitOwner(client, "panel", rules)
}
